// collector-sms: read-only SMS/MMS/RCS collector for Twalk v2. It behaves like
// Google Messages for web (libgm from mautrix-gmessages) paired with the
// owner's phone. It never sends a message, read receipt or typing indicator
// and never changes a setting. See -h.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"

	"github.com/mmaudet/twalk/internal/event"
)

const (
	pairCmdHint     = "collector-sms pair-google --cookies FICHIER"
	detailElsewhere = "session Messages Web active ailleurs"
)

const usage = `collector-sms : collecteur SMS/MMS/RCS en lecture seule pour Twalk v2
(Google Messages pour le web, via libgm de mautrix-gmessages).

  collector-sms run                          démon : se connecte, publie sur NATS
  collector-sms pair-google --cookies FICHIER [--keep]
                                             appairage par compte Google (recommandé) : cookies
                                             de messages.google.com (JSON, en-tête Cookie ou
                                             « Copier en cURL »), puis emoji à toucher sur le
                                             téléphone ; FICHIER supprimé après succès
  collector-sms pair [--qr /tmp/sms-qr.png]  appairage par QR (désactivé par Google depuis
                                             mautrix-gmessages v26.05, gardé au cas où)

Les cookies du compte Google restent dans session.json (0600) : libgm en a besoin à chaque
requête. Se déconnecter de cette session Google sur le navigateur donne logged_out.
Sans session valide, "run" publie logged_out et sort avec le code 2.
"run" prend la session Messages Web au démarrage (une session ouverte ailleurs est éjectée).
Si une autre la reprend ensuite : disconnected (« ` + detailElsewhere + ` »), déconnexion,
pas de reprise automatique ; relancer le service reprend la session.
Environnement : NATS_URL NATS_USER NATS_PASSWORD TWALK_EXCLUDE TWALK_LOG_LEVEL, TWALK_STATE_DIR
(défaut $HERMES_HOME/secrets/twalk2/sms, session.json 0600), TWALK_SMS_DEFAULT_CC (ex. 33 : 06… → +33 6…).
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
	case "pair":
		code = pair(ctx, log, os.Args[2:])
	case "pair-google":
		code = pairGoogle(ctx, log, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue %q\n\n%s", os.Args[1], usage)
	}
	stop()
	os.Exit(code)
}

// convDir caches conversations (events or GET_CONVERSATION, a read-only query to the phone).
type convDir struct {
	cli *libgm.Client
	log *slog.Logger
	m   sync.Map // conversation id -> convEntry
}
type convEntry struct {
	c  *gmproto.Conversation
	at time.Time
}

func (d *convDir) put(c *gmproto.Conversation) {
	d.m.Store(c.GetConversationID(), convEntry{c, time.Now()})
}

// get returns the cached conversation (refreshed after 6 h), else asks the
// phone up to 3 times (now, +5 s, +20 s). nil when unavailable.
func (d *convDir) get(ctx context.Context, id string) *gmproto.Conversation {
	e, _ := d.m.Load(id)
	ent, _ := e.(convEntry)
	if ent.c != nil && time.Since(ent.at) < 6*time.Hour {
		return ent.c
	}
	for _, wait := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
		select {
		case <-ctx.Done():
			return ent.c
		case <-time.After(wait):
		}
		c, err := d.cli.GetConversation(ctx, id)
		if err == nil && c.GetConversationID() != "" {
			d.put(c)
			return c
		}
		d.log.Warn("get conversation", "conversation", id, "err", err)
	}
	return ent.c // possibly stale, possibly nil
}

type collector struct {
	ctx   context.Context
	log   *slog.Logger
	pub   *event.Publisher
	cli   *libgm.Client
	dir   *convDir
	cv    *converter
	state string
	queue chan *libgm.WrappedMessage
	exit  chan int

	mu    sync.Mutex
	f     flags
	cur   event.Status
	final bool // logged_out / stopping: no further automatic status
}

func (c *collector) status() event.Status { c.mu.Lock(); defer c.mu.Unlock(); return c.cur }

func (c *collector) publish(s event.Status) {
	c.log.Info("status", "state", s.State, "detail", s.Detail)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.pub.Status(ctx, s); err != nil {
		c.log.Warn("publish status", "err", err)
	}
}

// update changes the connection flags and publishes the derived status when it changed.
func (c *collector) update(fn func(*flags)) {
	c.mu.Lock()
	if c.final {
		c.mu.Unlock()
		return
	}
	fn(&c.f)
	s := c.f.status()
	changed := s != c.cur
	c.cur = s
	c.mu.Unlock()
	if changed {
		c.publish(s)
	}
}

// terminal publishes a final state; code != 0 asks run() to exit with it.
func (c *collector) terminal(s event.Status, code int) {
	c.mu.Lock()
	c.final, c.cur = true, s
	c.mu.Unlock()
	c.publish(s)
	if code != 0 {
		select {
		case c.exit <- code:
		default:
		}
	}
}

func (c *collector) loggedOut(why string) {
	c.log.Error("logged out", "why", why)
	c.terminal(event.Status{State: event.StateLoggedOut, Detail: "appareil dissocié ou session invalide ; relancer : " + pairCmdHint}, 2)
}

func (c *collector) save() {
	if err := saveSession(c.state, c.cli.AuthData); err != nil {
		c.log.Warn("save session", "err", err)
	}
}

// inactive: another Messages Web session took over (or the phone dropped
// ours). The collector stops talking to Google (Disconnect also stops the
// pings, GET_UPDATES catch-ups and reconnects that would re-claim the
// session) and waits for a restart.
func (c *collector) inactive(detail string) {
	c.update(func(f *flags) { f.inactive, f.ready = detail, false })
	go c.cli.Disconnect()
}

// onEvent receives every libgm event, synchronously: it must not block or call the network.
func (c *collector) onEvent(raw any) {
	switch v := raw.(type) {
	case *libgm.WrappedMessage:
		select {
		case c.queue <- v:
		default:
			c.log.Warn("message queue full, dropped", "conversation", v.GetConversationID(), "id", v.GetMessageID())
		}
	case *gmproto.Conversation:
		c.dir.put(v)
	case *events.AuthTokenRefreshed:
		c.save()
	case *events.ListenTemporaryError:
		c.update(func(f *flags) { f.listenErr = true })
	case *events.ListenRecovered:
		c.update(func(f *flags) { f.listenErr = false })
	case *events.ListenFatalError:
		var he events.HTTPError
		if errors.Is(v.Error, events.ErrInvalidCredentials) || errors.Is(v.Error, events.ErrRequestedEntityNotFound) ||
			errors.As(v.Error, &he) && (he.Resp.StatusCode == 401 || he.Resp.StatusCode == 403) {
			c.loggedOut("listen: " + v.Error.Error())
			return
		}
		c.log.Error("fatal listen error", "err", v.Error)
		c.terminal(event.Status{State: event.StateDisconnected, Detail: "erreur fatale d'écoute Google Messages, redémarrage"}, 1)
	case *events.PhoneNotResponding:
		c.update(func(f *flags) { f.phoneDown = true })
	case *events.PhoneRespondingAgain:
		c.update(func(f *flags) { f.phoneDown = false })
	case *gmproto.RevokePairData, *events.GaiaLoggedOut:
		c.loggedOut("unpaired on the phone")
	case *gmproto.UserAlertEvent:
		switch v.GetAlertType() {
		case gmproto.AlertType_BROWSER_ACTIVE:
			c.update(func(f *flags) { f.ready, f.inactive = true, "" })
		case gmproto.AlertType_BROWSER_INACTIVE:
			c.inactive(detailElsewhere)
		case gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT:
			c.inactive("session Messages Web inactive (délai dépassé)")
		case gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
			c.inactive("session Messages Web inactive (inactivité)")
		}
	}
}

// work publishes queued messages one by one (conversation lookups may block).
func (c *collector) work() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case m := <-c.queue:
			c.process(m)
		}
	}
}

func (c *collector) process(m *libgm.WrappedMessage) {
	if !c.cv.wanted(m.Message) {
		return
	}
	c.update(func(f *flags) { f.ready = true }) // data is flowing
	conv := c.dir.get(c.ctx, m.GetConversationID())
	if conv == nil {
		c.log.Warn("conversation unavailable, message not published", "conversation", m.GetConversationID(), "id", m.GetMessageID())
		return
	}
	r, ok := c.cv.convert(m.Message, conv)
	if !ok {
		return
	}
	if err := c.pub.Message(c.ctx, r.Sent, r.ID, r.Time, r.Msg); err != nil { // never log content
		c.log.Warn("publish failed", "id", r.ID, "err", err)
		return
	}
	c.cv.seen.add(r.ID)
}

func run(ctx context.Context, log *slog.Logger) int {
	pub, err := event.Connect(event.SMS, log)
	if err != nil {
		log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	dir := stateDir()
	ad, err := loadSession(dir)
	if err != nil {
		log.Error("session", "err", err)
		return 1
	}
	if ad == nil || ad.Browser == nil || ad.TachyonAuthToken == nil {
		_ = pub.Status(ctx, event.Status{State: event.StateLoggedOut, Detail: "aucune session ; lancer : " + pairCmdHint})
		log.Error("no session", "hint", pairCmdHint)
		return 2
	}

	cli := libgm.NewClient(ad, nil, libgmLogger(log), exhttp.SensibleClientSettings) // nil push keys: never registers push
	cli.SetPingInterval(20 * time.Minute)                                            // upstream default 1 min drains the phone
	cli.SetDataReceiveCheckInterval(7 * 24 * time.Hour)                              // its extra GET_UPDATES would re-claim the session
	c := &collector{ctx: ctx, log: log, pub: pub, cli: cli, state: dir, queue: make(chan *libgm.WrappedMessage, 1024), exit: make(chan int, 1)}
	c.dir = &convDir{cli: cli, log: log}
	c.cv = &converter{cutoff: time.Now().Add(-maxReplayAge), defaultCC: os.Getenv("TWALK_SMS_DEFAULT_CC"), seen: newSeenSet(4096)}
	c.cur = c.f.status()
	cli.SetEventHandler(c.onEvent)
	go pub.Heartbeat(ctx, 5*time.Minute, c.status)
	go c.work()
	go func() { // Google rotates cookies in responses (UpdateCookiesFromResponse): persist them
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.save()
			}
		}
	}()
	c.publish(c.cur)

	if err := cli.FetchConfig(ctx); err != nil { // read-only GET; the bridge also tolerates failure
		log.Warn("fetch config", "err", err)
	}
	for err := cli.Connect(ctx); err != nil; err = cli.Connect(ctx) {
		if errors.Is(err, events.ErrInvalidCredentials) || errors.Is(err, events.ErrRequestedEntityNotFound) {
			c.loggedOut("connect: " + err.Error())
			return 2
		}
		log.Warn("connect failed", "err", err)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(30 * time.Second):
		}
	}
	c.save()

	code := 0
	select {
	case <-ctx.Done():
		c.terminal(event.Status{State: event.StateStopping}, 0)
	case code = <-c.exit:
	}
	cli.Disconnect()
	c.save()
	return code
}
