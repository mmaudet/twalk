// collector-linkedin: read-only LinkedIn collector for Twalk v2. LinkedIn has
// no API for a personal account, so it reads the LinkedIn notification emails
// that reach the owner's Twake mailbox (JMAP) and publishes them on the bus.
// The mailbox is only read: no Email/set, no keyword ($seen), no move. See -h.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

const (
	reauthHint  = "réautoriser Twake : twake-token authorize --renew"
	lookback    = time.Hour      // a stopped collector is caught up; ids are deduplicated
	firstWindow = 24 * time.Hour // first start: only the last 24 h
)

const usage = `collector-linkedin : collecteur LinkedIn en lecture seule pour Twalk v2 (pas d'API LinkedIn :
lit les emails de notification LinkedIn de la boîte Twake en JMAP ; Session, Email/query, Email/get
seulement, jamais Email/set ni $seen ni déplacement).

  collector-linkedin run                             démon : interroge la boîte toutes les TWALK_LINKEDIN_POLL (2m)
  collector-linkedin dry-run [--since 72h] [--mask]  affiche les événements qui SERAIENT publiés, sans NATS ni
                                                     curseur ; --mask : ni nom ni texte

Ignoré (compté) : newsletters, groupes, éditeurs, suggestions, fil d'actualité.
Jeton : TWALK_TWAKE_TOKEN_CMD (défaut : node $HERMES_HOME/tools/twake-token/twake-token.mjs token) ;
code 78 = grant révoqué → logged_out, sortie 2 (` + reauthHint + `).
Environnement : NATS_URL NATS_USER NATS_PASSWORD TWALK_EXCLUDE TWALK_LOG_LEVEL TWALK_LINKEDIN_POLL
TWALK_JMAP_SESSION_URL TWALK_STATE_DIR (défaut $HERMES_HOME/secrets/twalk2/linkedin, cursor.json 0600).
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(map[bool]int{true: 0, false: 64}[len(os.Args) > 1])
	}
	var lvl slog.Level
	if lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL"))) != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := 64
	switch os.Args[1] {
	case "run":
		code = run(ctx, log)
	case "dry-run":
		code = dryRun(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue %q\n\n%s", os.Args[1], usage)
	}
	stop()
	os.Exit(code)
}

func stateDir() string {
	if d := os.Getenv("TWALK_STATE_DIR"); d != "" {
		return d
	}
	home := os.Getenv("HERMES_HOME")
	if home == "" {
		home = "/home/hermes/hermes-new"
	}
	return filepath.Join(home, "secrets", "twalk2", "linkedin")
}

// loadCursor returns the receivedAt of the last processed email; zero when none.
func loadCursor(dir string) (t time.Time, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "cursor.json"))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	} else if err != nil {
		return t, err
	}
	var c struct{ Cursor time.Time }
	if err = json.Unmarshal(b, &c); err != nil {
		return t, errors.New("cursor.json illisible")
	}
	return c.Cursor, nil
}

func saveCursor(dir string, t time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]time.Time{"cursor": t.UTC()})
	tmp := filepath.Join(dir, "cursor.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "cursor.json"))
}

type collector struct {
	log           *slog.Logger
	pub           *event.Publisher
	jm            *jmapClient
	dir           string
	cursor, saved time.Time
	seen          map[string]time.Time // email id -> receivedAt, for the lookback window

	mu  sync.Mutex
	cur event.Status
}

func (c *collector) status() event.Status { c.mu.Lock(); defer c.mu.Unlock(); return c.cur }

// setStatus publishes a status when it differs from the current one.
func (c *collector) setStatus(ctx context.Context, s event.Status) {
	c.mu.Lock()
	changed := s != c.cur
	c.cur = s
	c.mu.Unlock()
	if !changed {
		return
	}
	c.log.Info("status", "state", s.State, "detail", s.Detail)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.pub.Status(ctx, s); err != nil {
		c.log.Warn("publish status", "err", err)
	}
}

// cycle fetches the emails since cursor-lookback and publishes the new ones.
// The cursor only moves past emails that were published or deliberately ignored.
func (c *collector) cycle(ctx context.Context) (published int, ignored map[string]int, err error) {
	ignored = map[string]int{}
	ms, err := c.jm.fetch(ctx, c.cursor.Add(-lookback), func(id string) bool { _, ok := c.seen[id]; return ok })
	defer c.persist()
	for _, m := range ms {
		if id, msg, _, ok := convert(m); !ok {
			_, why := classify(m)
			ignored[why]++
		} else if perr := c.pub.Message(ctx, false, id, m.Received, msg); perr != nil {
			c.log.Warn("publish failed, retry next cycle", "id", id, "err", perr) // never log content
			return published, ignored, nil
		} else {
			published++
		}
		c.seen[m.ID] = m.Received
		if m.Received.After(c.cursor) {
			c.cursor = m.Received
		}
	}
	for id, t := range c.seen { // keep only what the next lookback window can return again
		if t.Before(c.cursor.Add(-2 * lookback)) {
			delete(c.seen, id)
		}
	}
	return published, ignored, err
}

// persist writes the cursor when it moved (or was never written).
func (c *collector) persist() {
	if c.cursor.Equal(c.saved) {
		return
	}
	if err := saveCursor(c.dir, c.cursor); err != nil {
		c.log.Warn("save cursor", "err", err)
		return
	}
	c.saved = c.cursor
}

func pollInterval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TWALK_LINKEDIN_POLL")); err == nil && d >= time.Second {
		return d
	}
	return 2 * time.Minute
}

func run(ctx context.Context, log *slog.Logger) int {
	pub, err := event.Connect(event.LinkedIn, log)
	if err != nil {
		log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	c := &collector{log: log, pub: pub, jm: newJMAP(), dir: stateDir(), seen: map[string]time.Time{},
		cur: event.Status{State: event.StateDisconnected, Detail: "démarrage"}}
	if c.cursor, err = loadCursor(c.dir); err != nil {
		log.Error("cursor", "err", err)
		return 1
	}
	c.saved = c.cursor
	if c.cursor.IsZero() {
		c.cursor = time.Now().Add(lookback - firstWindow) // the first fetch starts at cursor-lookback = now-24h
	}
	go pub.Heartbeat(ctx, 5*time.Minute, c.status)
	t := time.NewTicker(pollInterval())
	defer t.Stop()
	for {
		n, ign, err := c.cycle(ctx)
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, errLoggedOut):
			log.Error("logged out", "err", err)
			c.setStatus(ctx, event.Status{State: event.StateLoggedOut, Account: c.jm.account, Detail: reauthHint})
			return 2
		case err != nil:
			log.Warn("cycle failed", "err", err)
			c.setStatus(ctx, event.Status{State: event.StateDisconnected, Account: c.jm.account, Detail: "boîte Twake injoignable (" + err.Error() + "), nouvel essai"})
		default:
			c.setStatus(ctx, event.Status{State: event.StateConnected, Account: c.jm.account})
			if n > 0 || len(ign) > 0 {
				log.Info("cycle", "published", n, "ignored", ign)
			}
		}
		select {
		case <-ctx.Done():
			c.setStatus(ctx, event.Status{State: event.StateStopping, Account: c.jm.account})
			return 0
		case <-t.C:
		}
	}
}

// dryRun prints, as JSON lines, the events run would publish for the emails
// of the last --since; nothing goes to NATS and the cursor is untouched.
func dryRun(ctx context.Context, args []string) int {
	since, mask := 72*time.Hour, false
	for i := 0; i < len(args); i++ {
		var err error
		switch {
		case args[i] == "--since" && i+1 < len(args):
			i++
			if since, err = time.ParseDuration(args[i]); err != nil || since <= 0 {
				fmt.Fprintln(os.Stderr, "--since invalide (ex. 72h, 720h)")
				return 64
			}
		case args[i] == "--mask":
			mask = true
		default:
			fmt.Fprint(os.Stderr, usage)
			return 64
		}
	}
	ms, err := newJMAP().fetch(ctx, time.Now().Add(-since), func(string) bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "erreur :", err)
		return map[bool]int{true: 2, false: 1}[errors.Is(err, errLoggedOut)]
	}
	counts, enc := map[string]int{}, json.NewEncoder(os.Stdout)
	for _, m := range ms {
		id, msg, cat, ok := convert(m)
		if !ok {
			_, why := classify(m)
			counts["ignoré:"+why]++
			continue
		}
		counts[string(cat)]++
		out := map[string]any{"id": id, "time": m.Received, "kind": msg.Conversation.Kind, "mentions_owner": msg.MentionsOwner}
		if mask {
			out["id"], out["category"], out["text_len"], out["slug_known"] = event.ConvHash(id), string(cat), len([]rune(msg.Text)), msg.Sender.ID != "linkedin:linkedin"
		} else {
			out["conversation"], out["sender"], out["text"] = msg.Conversation, msg.Sender, msg.Text
		}
		_ = enc.Encode(out)
	}
	fmt.Fprintln(os.Stderr, len(ms), "emails sur", since, counts) // fmt prints maps sorted by key
	return 0
}
