// Command collector-matrix is the read-only Matrix collector of Twalk v2. It
// syncs the owner's account (end-to-end encryption included, pure-Go Olm) and
// publishes every message it sees on the bus. It never sends a message, a
// read receipt, a typing notice or a presence update, and never joins a room.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"maunium.net/go/mautrix"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/mmaudet/twalk/internal/event"
)

const usage = `collector-matrix: collecteur Matrix (lecture seule, E2EE) de Twalk v2.

  collector-matrix login   interactif: identifiant, mot de passe et clé de récupération (sans écho)
  collector-matrix run     démon: publie les messages sur NATS (sortie 2 si session expirée)

Build sans CGO: CGO_ENABLED=0 go build -tags goolm -o bin/collector-matrix ./cmd/collector-matrix
Environnement: TWALK_STATE_DIR (défaut $HERMES_HOME/secrets/twalk2/matrix: session.json,
crypto.db, pickle.key en 0600), NATS_URL, NATS_USER, NATS_PASSWORD, TWALK_EXCLUDE,
TWALK_LOG_LEVEL (debug|info|warn|error).
`

func main() { os.Exit(realMain()) }

func realMain() int {
	if len(os.Args) != 2 || (os.Args[1] != "login" && os.Args[1] != "run") {
		fmt.Fprint(os.Stderr, usage)
		if len(os.Args) == 2 && strings.Contains("-h --help help", os.Args[1]) {
			return 0
		}
		return 1
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL")))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	dir := os.Getenv("TWALK_STATE_DIR")
	if dir == "" {
		home := os.Getenv("HERMES_HOME")
		if home == "" {
			home = "/home/hermes/hermes-new"
		}
		dir = filepath.Join(home, "secrets", "twalk2", "matrix")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Error("dossier d'état", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "login" {
		if err := login(ctx, dir); err != nil {
			fmt.Fprintln(os.Stderr, "échec:", err)
			return 1
		}
		return 0
	}
	c := &collector{dir: dir, log: log, rooms: map[id.RoomID]*room{}, directRooms: map[id.RoomID]bool{}}
	return c.run(ctx)
}

type collector struct {
	dir string
	log *slog.Logger
	pub *event.Publisher
	st  *statusKeeper
	cli *mautrix.Client

	mu          sync.Mutex // guards the fields below
	me          owner
	rooms       map[id.RoomID]*room
	directRooms map[id.RoomID]bool

	initial     atomic.Bool  // the sync being processed is the initial one
	decryptFail atomic.Int64 // events that could not be decrypted
}

func (c *collector) run(ctx context.Context) int {
	pub, err := event.Connect(event.Matrix, c.log)
	if err != nil {
		c.log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	c.pub = pub
	c.st = newStatusKeeper(ctx, pub, c.log)
	defer c.st.close()
	c.st.set(event.Status{State: event.StateDisconnected, Detail: "démarrage"})
	go pub.Heartbeat(ctx, 5*time.Minute, c.st.current)

	for ctx.Err() == nil {
		err := c.runOnce(ctx)
		if errors.Is(err, errLoggedOut) || errors.Is(err, mautrix.MUnknownToken) {
			c.log.Error("session absente ou expirée: lancer « collector-matrix login »")
			c.st.set(event.Status{State: event.StateLoggedOut, Detail: "collector-matrix login"})
			return 2
		}
		if ctx.Err() != nil {
			break
		}
		c.log.Warn("synchronisation arrêtée, nouvelle tentative", "err", err)
		c.st.set(event.Status{State: event.StateDisconnected, Account: c.st.account(), Detail: "reconnexion en cours"})
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
	c.st.set(event.Status{State: event.StateStopping, Account: c.st.account()})
	return 0
}

// runOnce runs one sync session until it fails or ctx ends.
func (c *collector) runOnce(ctx context.Context) error {
	s, err := loadSession(c.dir)
	if err != nil {
		return err
	}
	cli, err := newClient(s)
	if err != nil {
		return err
	}
	c.cli = cli
	acct := string(s.UserID)
	// Status follows the /sync responses.
	cli.ResponseHook = func(req *http.Request, resp *http.Response, err error, _ time.Duration) {
		if !strings.HasSuffix(req.URL.Path, "/sync") {
			return
		}
		if err != nil && ctx.Err() == nil {
			c.st.set(event.Status{State: event.StateDisconnected, Account: acct, Detail: "connexion perdue"})
		} else if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			c.st.set(event.Status{State: event.StateConnected, Account: acct, Detail: c.st.detail()})
		}
	}

	detail := ""
	ch, err := openCrypto(ctx, cli, c.dir)
	if noKeys(err) { // pasted token of a device that has keys elsewhere
		c.log.Error("appareil déjà doté de clés: chiffrement indisponible, rooms non chiffrées seulement")
		cli.Store, detail = mautrix.NewMemorySyncStore(), "chiffrement indisponible"
	} else if err != nil {
		return fmt.Errorf("chiffrement: %w", err)
	} else {
		defer ch.Close()
		ch.DecryptErrorCallback = func(e *mevent.Event, err error) {
			c.decryptFail.Add(1)
			lvl := slog.LevelWarn
			if c.initial.Load() { // history of the first sync: expected, not worth a warning
				lvl = slog.LevelDebug
			}
			c.log.Log(context.Background(), lvl, "déchiffrement impossible", "event", e.ID, "failures", c.decryptFail.Load(), "err", err)
		}
	}
	c.st.setDetail(detail)

	if dn, err := cli.GetOwnDisplayName(ctx); err == nil {
		c.me = owner{ID: s.UserID, Name: dn.DisplayName}
	} else {
		c.me = owner{ID: s.UserID}
	}
	var direct mevent.DirectChatsEventContent
	if err := cli.GetAccountData(ctx, "m.direct", &direct); err == nil {
		c.setDirect(direct)
	}

	syn := cli.Syncer.(*mautrix.DefaultSyncer)
	syn.OnSync(func(_ context.Context, _ *mautrix.RespSync, since string) bool {
		c.initial.Store(since == "")
		return true
	})
	syn.OnEventType(mevent.EventMessage, c.onEvent)
	syn.OnEventType(mevent.EventReaction, c.onEvent)
	syn.OnEventType(mevent.StateRoomName, c.onState)
	syn.OnEventType(mevent.StateMember, c.onState)
	syn.OnEventType(mevent.AccountDataDirectChats, func(_ context.Context, e *mevent.Event) {
		if d, ok := e.Content.Parsed.(*mevent.DirectChatsEventContent); ok {
			c.setDirect(*d)
		}
	})
	// Invitations are ignored: nothing registers a member/invite handler that joins.
	c.log.Info("synchronisation", "account", acct, "device", s.DeviceID, "e2ee", ch != nil)
	return cli.SyncWithContext(ctx)
}

func (c *collector) setDirect(d mevent.DirectChatsEventContent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.me.Directs, c.directRooms = map[id.UserID]bool{}, map[id.RoomID]bool{}
	for u, rooms := range d {
		c.me.Directs[u] = true
		for _, r := range rooms {
			c.directRooms[r] = true
		}
	}
	for rid, r := range c.rooms {
		r.Direct = c.directRooms[rid]
	}
}

// onState keeps cached rooms current without refetching their state.
func (c *collector) onState(_ context.Context, e *mevent.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rooms[e.RoomID]; r != nil {
		r.apply(e)
	}
}

// roomInfo returns the cached room, fetching its state once (read-only GET).
// Called with c.mu held.
func (c *collector) roomInfo(ctx context.Context, rid id.RoomID) (*room, error) {
	if r := c.rooms[rid]; r != nil {
		return r, nil
	}
	state, err := c.cli.State(ctx, rid)
	if err != nil {
		return nil, err
	}
	r := newRoom()
	r.Direct = c.directRooms[rid]
	for _, byKey := range state {
		for _, e := range byKey {
			r.apply(e)
		}
	}
	c.rooms[rid] = r
	return r, nil
}

func (c *collector) onEvent(ctx context.Context, e *mevent.Event) {
	if since, _ := ctx.Value(mautrix.SyncTokenContextKey).(string); since == "" {
		return // initial sync: history is not republished
	}
	c.mu.Lock()
	r, err := c.roomInfo(ctx, e.RoomID)
	var n normalized
	ok := false
	if err == nil {
		n, ok = normalize(e, r, &c.me)
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("état de la room indisponible", "event", e.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	if n.Reaction != nil {
		err = c.pub.Reaction(ctx, n.ID, n.Time, *n.Reaction)
	} else {
		err = c.pub.Message(ctx, n.Sent, n.ID, n.Time, *n.Msg)
	}
	if err != nil { // never log content
		c.log.Warn("publication", "event", n.ID, "err", err)
	}
}

// statusKeeper remembers the current status and publishes changes from a
// single goroutine, so that callbacks which must not block can call set.
type statusKeeper struct {
	mu   sync.Mutex
	cur  event.Status
	note string
	ch   chan event.Status
	done chan struct{}
}

func newStatusKeeper(ctx context.Context, pub *event.Publisher, log *slog.Logger) *statusKeeper {
	k := &statusKeeper{ch: make(chan event.Status, 32), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		for s := range k.ch {
			if err := pub.Status(context.WithoutCancel(ctx), s); err != nil {
				log.Warn("publication du statut", "state", s.State, "err", err)
			}
		}
	}()
	return k
}

func (k *statusKeeper) set(s event.Status) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s == k.cur {
		return
	}
	k.cur = s
	select {
	case k.ch <- s:
	default: // queue full: the heartbeat republishes the current state
	}
}

func (k *statusKeeper) current() event.Status { k.mu.Lock(); defer k.mu.Unlock(); return k.cur }
func (k *statusKeeper) account() string       { return k.current().Account }
func (k *statusKeeper) detail() string        { k.mu.Lock(); defer k.mu.Unlock(); return k.note }
func (k *statusKeeper) setDetail(d string)    { k.mu.Lock(); defer k.mu.Unlock(); k.note = d }
func (k *statusKeeper) close()                { close(k.ch); <-k.done }
