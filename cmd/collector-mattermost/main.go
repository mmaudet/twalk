// Command collector-mattermost is the read-only Mattermost collector of
// Twalk v2. It follows the owner's account with a personal access token over
// the API v4 WebSocket (events) and REST (GET only, to resolve names).
// The WebSocket only ever carries the authentication challenge; the collector
// never posts, never marks a channel as read, never sends typing or status.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/mmaudet/twalk/internal/event"
)

const usage = `collector-mattermost: collecteur Mattermost (lecture seule) de Twalk v2.

  collector-mattermost login   connexion interactive (mot de passe, MFA) si l'instance refuse les tokens personnels
  collector-mattermost check   vérifie MM_URL et MM_TOKEN (GET /users/me et /users/me/teams)
  collector-mattermost run     démon: publie sur NATS (sortie 2 si token refusé)

Environnement: MM_URL, MM_TOKEN (token d'accès personnel; aussi lus dans
$HERMES_HOME/secrets/twalk2/mattermost.env, sinon dans la session de login), NATS_URL, NATS_USER, NATS_PASSWORD,
TWALK_EXCLUDE, TWALK_LOG_LEVEL. Les messages arrivés pendant une coupure ne
sont pas rattrapés.
`

var errLoggedOut = errors.New("token mattermost refusé (401)")

func main() { os.Exit(realMain()) }

func realMain() int {
	help := len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help")
	if len(os.Args) != 2 || help {
		fmt.Fprint(os.Stderr, usage)
		if help {
			return 0
		}
		return 1
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL")))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	loadEnvFile(filepath.Join(filepath.Dir(stateDir()), "mattermost.env"))
	if os.Args[1] == "login" {
		if err := login(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "échec:", err)
			return 1
		}
		return 0
	}
	loadSession()
	c, err := newCollector(os.Getenv("MM_URL"), os.Getenv("MM_TOKEN"), log)
	if err != nil {
		log.Error("configuration", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "check":
		if err := c.check(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "échec:", err)
			return 1
		}
		return 0
	case "run":
		return c.run(ctx)
	}
	fmt.Fprint(os.Stderr, usage)
	return 1
}

// loadEnvFile sets KEY=VALUE pairs from path for variables not already set.
func loadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, strings.Trim(v, `"'`))
		}
	}
}

func stateDir() string {
	if d := os.Getenv("TWALK_STATE_DIR"); d != "" {
		return d
	}
	home := os.Getenv("HERMES_HOME")
	if home == "" {
		home = "/home/hermes/hermes-new"
	}
	return filepath.Join(home, "secrets", "twalk2", "mattermost")
}

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type collector struct {
	base, token string
	log         *slog.Logger
	hc          *http.Client
	emit        func(context.Context, normalized)
	st          *statusKeeper // nil in tests

	mu    sync.Mutex
	ctx   context.Context // for the resolver's GETs
	chans map[string]chanInfo
	users map[string]user
}

func newCollector(rawURL, token string, log *slog.Logger) (*collector, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("MM_URL absente ou invalide (attendu: https://hôte)")
	}
	if token == "" {
		return nil, errors.New("MM_TOKEN absent")
	}
	return &collector{
		base: u.String(), token: token, log: log, ctx: context.Background(),
		hc:    &http.Client{Timeout: 20 * time.Second},
		chans: map[string]chanInfo{}, users: map[string]user{},
	}, nil
}

// get performs a read-only GET on the REST API. 401 → errLoggedOut.
func (c *collector) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errLoggedOut
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *collector) check(ctx context.Context) error {
	var me user
	if err := c.get(ctx, "/api/v4/users/me", &me); err != nil {
		return err
	}
	var teams []json.RawMessage
	if err := c.get(ctx, "/api/v4/users/me/teams", &teams); err != nil {
		return err
	}
	fmt.Printf("connecté en tant que @%s (%s), %d équipes\n", me.Username, me.ID, len(teams))
	return nil
}

// cached returns m[id], else fetches path (a read-only GET) and caches it.
// Failures are not cached.
func cached[T any](c *collector, m map[string]T, id, path string) (v T, ok bool) {
	c.mu.Lock()
	v, ok = m[id]
	ctx := c.ctx
	c.mu.Unlock()
	if ok || id == "" || c.get(ctx, path, &v) != nil {
		return v, ok
	}
	c.mu.Lock()
	m[id] = v
	c.mu.Unlock()
	return v, true
}

func (c *collector) channel(id string) (chanInfo, bool) {
	return cached(c, c.chans, id, "/api/v4/channels/"+url.PathEscape(id))
}

func (c *collector) userName(id string) string {
	u, _ := cached(c, c.users, id, "/api/v4/users/"+url.PathEscape(id))
	return u.Username
}

func (c *collector) run(ctx context.Context) int {
	pub, err := event.Connect(event.Mattermost, c.log)
	if err != nil {
		c.log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	c.st = &statusKeeper{pub: pub, log: c.log}
	c.emit = func(ctx context.Context, n normalized) {
		var err error
		if n.React != nil {
			err = pub.Reaction(ctx, n.ID, n.Time, *n.React)
		} else {
			err = pub.Message(ctx, n.Sent, n.ID, n.Time, *n.Msg)
		}
		if err != nil {
			c.log.Warn("publication", "event", n.ID, "err", err)
		}
	}
	c.st.set(event.Status{State: event.StateDisconnected, Detail: "démarrage"})
	go pub.Heartbeat(ctx, 5*time.Minute, c.st.current)

	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.runOnce(ctx)
		if errors.Is(err, errLoggedOut) {
			c.log.Error("token refusé: relancer collector-mattermost login")
			c.st.set(event.Status{State: event.StateLoggedOut, Detail: "collector-mattermost login"})
			return 2
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		c.log.Warn("connexion perdue, nouvelle tentative", "err", err, "in", backoff.String())
		c.st.set(event.Status{State: event.StateDisconnected, Account: c.st.account(), Detail: "reconnexion en cours"})
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	c.st.set(event.Status{State: event.StateStopping, Account: c.st.account()})
	return 0
}

// runOnce runs one WebSocket session until it fails or ctx ends.
func (c *collector) runOnce(ctx context.Context) error {
	var me user
	if err := c.get(ctx, "/api/v4/users/me", &me); err != nil {
		return err
	}
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	acct := "@" + me.Username

	conn, _, err := websocket.Dial(ctx, c.base+"/api/v4/websocket", nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	// The one and only message this client ever writes on the socket.
	challenge, _ := json.Marshal(map[string]any{
		"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": c.token},
	})
	if err := conn.Write(ctx, websocket.MessageText, challenge); err != nil {
		return err
	}
	for {
		// The server pings about every minute: silence means a dead link.
		rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, b, err := conn.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		var ev wsEvent
		if json.Unmarshal(b, &ev) != nil {
			continue
		}
		switch {
		case ev.SeqReply == 1 && ev.Status != "OK":
			return errLoggedOut
		case ev.SeqReply == 1 || ev.Event == "hello":
			c.log.Info("connecté", "account_id", me.ID)
			c.st.set(event.Status{State: event.StateConnected, Account: acct})
		default:
			if n, ok := normalize(ev, me.ID, c); ok {
				c.emit(ctx, n)
			}
		}
	}
}

// statusKeeper remembers the current status and publishes changes. All
// methods are no-ops on a nil keeper (tests).
type statusKeeper struct {
	mu  sync.Mutex
	cur event.Status
	pub *event.Publisher
	log *slog.Logger
}

func (k *statusKeeper) set(s event.Status) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if s == k.cur {
		return
	}
	k.cur = s
	if err := k.pub.Status(context.Background(), s); err != nil {
		k.log.Warn("publication du statut", "state", s.State, "err", err)
	}
}

func (k *statusKeeper) current() event.Status {
	if k == nil {
		return event.Status{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cur
}

func (k *statusKeeper) account() string { return k.current().Account }
