// collector-whatsapp: read-only WhatsApp collector for Twalk v2 (whatsmeow,
// linked-device mode). It never sends anything: no message, no read receipt,
// no presence, no typing indicator. See -h.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"github.com/mmaudet/twalk/internal/event"
)

const (
	displayName = "Twalk (Athena)"
	pairTimeout = 5 * time.Minute
	pairCmdHint = "collector-whatsapp pair --phone <numéro E.164 sans +>"
)

const usage = `collector-whatsapp : collecteur WhatsApp en lecture seule pour Twalk v2.

  collector-whatsapp run                      démon : se connecte, publie sur NATS
  collector-whatsapp pair --phone 33660469852 appairage par code (numéro E.164 sans +)
  collector-whatsapp pair --qr /tmp/qr.png    appairage par QR (PNG réécrit à chaque rotation)
  pair --force : remplace l'appareil déjà appairé (suppression locale)

Sur le téléphone : WhatsApp > Appareils connectés > Connecter un appareil
(> Connecter avec un numéro de téléphone pour saisir le code, ou scanner le QR).
Sans appareil appairé, "run" publie logged_out et sort avec le code 2.
Environnement : NATS_URL NATS_USER NATS_PASSWORD TWALK_EXCLUDE TWALK_LOG_LEVEL,
TWALK_STATE_DIR (défaut $HERMES_HOME/secrets/twalk2/whatsapp, whatsmeow.db en 0600).
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
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue %q\n\n%s", os.Args[1], usage)
	}
	stop()
	os.Exit(code)
}

// slogAdapter forwards whatsmeow warnings/errors only (its debug/info lines
// can carry message metadata; we never log content).
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Warnf(m string, args ...any)  { a.l.Warn(fmt.Sprintf(m, args...)) }
func (a slogAdapter) Errorf(m string, args ...any) { a.l.Error(fmt.Sprintf(m, args...)) }
func (a slogAdapter) Infof(string, ...any)         {}
func (a slogAdapter) Debugf(string, ...any)        {}
func (a slogAdapter) Sub(string) waLog.Logger      { return a }

// openStore opens the whatsmeow SQLite store (dir 0700, db 0600).
func openStore(ctx context.Context, log *slog.Logger) (*sqlstore.Container, error) {
	dir := os.Getenv("TWALK_STATE_DIR")
	if dir == "" {
		home := os.Getenv("HERMES_HOME")
		if home == "" {
			home = "/home/hermes/hermes-new"
		}
		dir = filepath.Join(home, "secrets", "twalk2", "whatsapp")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "whatsmeow.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	// dbutil.ParseDialect accepts any "sqlite*" name; whatsmeow requires foreign
	// keys, enabled through modernc's _pragma DSN parameter.
	return sqlstore.New(ctx, "sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)", slogAdapter{log})
}

// pairedDevice returns the paired device, or nil when none.
func pairedDevice(ctx context.Context, c *sqlstore.Container) (*store.Device, error) {
	devs, err := c.GetAllDevices(ctx)
	for _, d := range devs {
		if d.ID != nil {
			return d, err
		}
	}
	return nil, err
}

// liveDir implements directory on top of the live session.
type liveDir struct {
	ctx    context.Context
	cli    *whatsmeow.Client
	log    *slog.Logger
	mu     sync.Mutex
	groups map[types.JID]groupEntry
}
type groupEntry struct {
	name string
	at   time.Time // empty names (failures) are retried after 5 minutes
}

func (d *liveDir) Own() (types.JID, types.JID) {
	return d.cli.Store.GetJID().ToNonAD(), d.cli.Store.GetLID().ToNonAD()
}

func (d *liveDir) PNForLID(lid types.JID) types.JID {
	pn, _ := d.cli.Store.LIDs.GetPNForLID(d.ctx, lid)
	return pn
}

// Contact: address-book name (known) from the contact store, else the push name.
func (d *liveDir) Contact(j types.JID) (string, bool) {
	ci, err := d.cli.Store.Contacts.GetContact(d.ctx, j)
	if err != nil {
		return "", false
	}
	for _, n := range []string{ci.FullName, ci.FirstName} {
		if n != "" {
			return n, true
		}
	}
	return map[bool]string{true: ci.PushName, false: ci.BusinessName}[ci.PushName != ""], false
}

func (d *liveDir) setGroup(j types.JID, name string) {
	d.mu.Lock()
	d.groups[j] = groupEntry{name, time.Now()}
	d.mu.Unlock()
}

// GroupName is cached and refreshed on events.GroupInfo; a failure is not fatal.
func (d *liveDir) GroupName(j types.JID) string {
	d.mu.Lock()
	g, ok := d.groups[j]
	d.mu.Unlock()
	if ok && (g.name != "" || time.Since(g.at) < 5*time.Minute) {
		return g.name
	}
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	gi, err := d.cli.GetGroupInfo(ctx, j) // read-only query
	if err != nil {
		d.log.Warn("group info", "group", j.String(), "err", err)
		d.setGroup(j, "")
		return ""
	}
	d.setGroup(j, gi.Name)
	return gi.Name
}

type collector struct {
	ctx  context.Context
	log  *slog.Logger
	pub  *event.Publisher
	dir  *liveDir
	exit chan int
	mu   sync.Mutex
	cur  event.Status
}

func (c *collector) status() event.Status { c.mu.Lock(); defer c.mu.Unlock(); return c.cur }

// setStatus records and publishes the state; code != 0 also asks run() to exit.
func (c *collector) setStatus(state, detail string, code int) {
	c.mu.Lock()
	c.cur.State, c.cur.Detail = state, detail
	s := c.cur
	c.mu.Unlock()
	c.log.Info("status", "state", state, "detail", detail)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.pub.Status(ctx, s); err != nil {
		c.log.Warn("publish status", "err", err)
	}
	if code != 0 {
		select {
		case c.exit <- code:
		default:
		}
	}
}

// handle receives every whatsmeow event. Nothing here sends to the network.
func (c *collector) handle(raw any) {
	switch v := raw.(type) {
	case *events.Message:
		n, ok := normalize(v, c.dir)
		var err error
		if ok && n.Reaction != nil {
			err = c.pub.Reaction(c.ctx, n.ID, n.Time, *n.Reaction)
		} else if ok {
			err = c.pub.Message(c.ctx, n.Sent, n.ID, n.Time, *n.Message)
		}
		if err != nil { // never log content
			c.log.Warn("publish failed", "id", n.ID, "err", err)
		}
	case *events.GroupInfo:
		if v.Name != nil {
			c.dir.setGroup(v.JID, v.Name.Name)
		}
	case *events.HistorySync:
		c.log.Info("history sync ignored (lot 0)", "conversations", len(v.Data.GetConversations()))
	case *events.Connected, *events.KeepAliveRestored:
		c.setStatus(event.StateConnected, "", 0)
	case *events.Disconnected:
		c.setStatus(event.StateDisconnected, "websocket closed, reconnecting", 0)
	case *events.KeepAliveTimeout:
		c.setStatus(event.StateDisconnected, "keepalive timeout", 0)
	case *events.StreamReplaced:
		c.setStatus(event.StateDisconnected, "stream replaced by another client of this device", 1)
	case *events.ClientOutdated:
		c.setStatus(event.StateDisconnected, "whatsmeow too old for WhatsApp, update the collector", 1)
	case *events.LoggedOut:
		c.setStatus(event.StateLoggedOut, "appareil délié ; relancer : "+pairCmdHint, 2)
	}
}

func run(ctx context.Context, log *slog.Logger) int {
	pub, err := event.Connect(event.WhatsApp, log)
	if err != nil {
		log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	container, err := openStore(ctx, log)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	defer container.Close()
	dev, err := pairedDevice(ctx, container)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	if dev == nil {
		_ = pub.Status(ctx, event.Status{State: event.StateLoggedOut, Detail: "aucun appareil appairé ; lancer : " + pairCmdHint})
		log.Error("no paired device", "hint", pairCmdHint)
		return 2
	}

	cli := whatsmeow.NewClient(dev, slogAdapter{log})
	// Read-only: no presence is ever sent (delivery receipts go out as
	// "inactive", never "read"); no phone re-requests, no reporting tokens.
	cli.EnableAutoReconnect, cli.AutomaticMessageRerequestFromPhone, cli.SendReportingTokens = true, false, false
	c := &collector{ctx: ctx, log: log, pub: pub, exit: make(chan int, 1), cur: event.Status{Account: dev.ID.User}}
	c.dir = &liveDir{ctx: ctx, cli: cli, log: log, groups: map[types.JID]groupEntry{}}
	cli.AddEventHandler(c.handle)
	go pub.Heartbeat(ctx, 5*time.Minute, c.status)

	c.setStatus(event.StateDisconnected, "connecting", 0)
	for err := cli.Connect(); err != nil; err = cli.Connect() {
		log.Warn("connect failed", "err", err)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(15 * time.Second):
		}
	}
	code := 0
	select {
	case <-ctx.Done():
		c.setStatus(event.StateStopping, "", 0)
	case code = <-c.exit:
	}
	cli.Disconnect()
	return code
}
