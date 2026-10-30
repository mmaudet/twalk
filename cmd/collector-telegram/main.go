// Command collector-telegram is the read-only Telegram collector of Twalk v2.
// It follows the owner's user account over MTProto (gotd) and publishes every
// message it sees on the bus. It never sends, never marks anything as read and
// never touches the presence ("online") status.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"golang.org/x/term"

	"github.com/mmaudet/twalk/internal/event"
)

const usage = `collector-telegram: collecteur Telegram (lecture seule) de Twalk v2.

  collector-telegram login   appairage interactif (téléphone, code Telegram, 2FA sans écho)
  collector-telegram run     démon: publie les messages sur NATS (sortie 2 si session expirée)

Environnement: TG_API_ID, TG_API_HASH, TWALK_STATE_DIR (défaut
$HERMES_HOME/secrets/twalk2/telegram, session.json en 0600), NATS_URL, NATS_USER,
NATS_PASSWORD, TWALK_EXCLUDE, TWALK_LOG_LEVEL (debug|info|warn|error).
Lot 0: état des mises à jour en mémoire (messages reçus pendant un arrêt non
rattrapés); réactions non collectées.
`

var errLoggedOut = errors.New("session telegram absente ou expirée")

func main() {
	os.Exit(realMain())
}

func realMain() int {
	if len(os.Args) != 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Fprint(os.Stderr, usage)
		if len(os.Args) == 2 {
			return 0
		}
		return 1
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL")))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	loadEnvFile(filepath.Join(filepath.Dir(stateDir()), "telegram.env"))
	apiID, err := strconv.Atoi(os.Getenv("TG_API_ID"))
	apiHash := os.Getenv("TG_API_HASH")
	if err != nil || apiID == 0 || apiHash == "" {
		log.Error("TG_API_ID et TG_API_HASH sont requis")
		return 1
	}
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Error("dossier d'état", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c := &collector{apiID: apiID, apiHash: apiHash, session: filepath.Join(dir, "session.json"), log: log}

	switch os.Args[1] {
	case "login":
		if err := c.login(ctx); err != nil {
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

// loadEnvFile sets KEY=VALUE pairs from path for variables not already set
// (the API id and hash live in secrets/twalk2/telegram.env).
func loadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
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
	return filepath.Join(home, "secrets", "twalk2", "telegram")
}

type collector struct {
	apiID   int
	apiHash string
	session string
	log     *slog.Logger
	pub     *event.Publisher
	st      *statusKeeper
	self    atomic.Pointer[tg.User]
}

// newClient never sets a logger (gotd logs nothing by default, so no content
// can leak) and never calls any "online" method: gotd has none.
func (c *collector) newClient(h telegram.UpdateHandler, onState func(telegram.ConnectionState)) *telegram.Client {
	return telegram.NewClient(c.apiID, c.apiHash, telegram.Options{
		UpdateHandler:     h,
		SessionStorage:    &session.FileStorage{Path: c.session},
		OnConnectionState: onState,
		Device: telegram.DeviceConfig{
			DeviceModel: "Twalk collector (lecture seule)", SystemVersion: "linux", AppVersion: "twalk2",
			LangCode: "fr", SystemLangCode: "fr",
		},
	})
}

// login runs the interactive authentication and stores the session.
func (c *collector) login(ctx context.Context) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("login doit être lancé dans un terminal interactif")
	}
	client := c.newClient(nil, nil) // no handler: no updates, nothing is read
	return client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(&terminalAuth{in: bufio.NewReader(os.Stdin), fd: int(os.Stdin.Fd())}, auth.SendCodeOptions{})
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		_ = os.Chmod(c.session, 0o600)
		fmt.Printf("connecté en tant que %s (id %d)\n", userName(self), self.ID)
		return nil
	})
}

// terminalAuth asks the owner on the terminal. Existing accounts only.
type terminalAuth struct {
	in *bufio.Reader
	fd int
}

func (t *terminalAuth) ask(prompt string) (string, error) {
	fmt.Print(prompt)
	s, err := t.in.ReadString('\n')
	return strings.TrimSpace(s), err
}

func (t *terminalAuth) Phone(context.Context) (string, error) {
	return t.ask("Numéro de téléphone (format international, ex. +33612345678) : ")
}

func (t *terminalAuth) Code(context.Context, *tg.AuthSentCode) (string, error) {
	return t.ask("Code reçu dans l'application Telegram : ")
}

func (t *terminalAuth) Password(context.Context) (string, error) {
	fmt.Print("Mot de passe 2FA (saisie masquée) : ")
	b, err := term.ReadPassword(t.fd)
	fmt.Println()
	return string(b), err
}

func (t *terminalAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("ce numéro n'a pas de compte Telegram: inscription refusée")
}

func (t *terminalAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("ce numéro n'a pas de compte Telegram: inscription refusée")
}

// run is the daemon. Returns the process exit code.
func (c *collector) run(ctx context.Context) int {
	pub, err := event.Connect(event.Telegram, c.log)
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

	code := 0
	if _, err := os.Stat(c.session); err != nil {
		code = 2 // never paired: do not even open a connection
	} else {
		for ctx.Err() == nil {
			err := c.runOnce(ctx)
			if errors.Is(err, errLoggedOut) {
				code = 2
				break
			}
			if ctx.Err() != nil {
				break
			}
			c.log.Warn("client arrêté, nouvelle tentative", "err", err)
			c.st.set(event.Status{State: event.StateDisconnected, Account: c.st.account(), Detail: "reconnexion en cours"})
			select {
			case <-ctx.Done():
			case <-time.After(15 * time.Second):
			}
		}
	}
	if code == 2 {
		c.log.Error("session absente ou expirée: lancer « collector-telegram login »")
		c.st.set(event.Status{State: event.StateLoggedOut, Detail: "collector-telegram login"})
		return 2
	}
	c.st.set(event.Status{State: event.StateStopping, Account: c.st.account()})
	return 0
}

// runOnce runs one client session until it fails or ctx ends.
func (c *collector) runOnce(ctx context.Context) error {
	d := tg.NewUpdateDispatcher()
	// Lot 0: update state (pts/qts/seq) lives in memory (gotd's default). The
	// fuller file-backed storage does not exist in gotd, so messages received
	// while the collector was stopped are not caught up.
	mgr := updates.New(updates.Config{
		Handler:          d,
		OnChannelTooLong: func(id int64) { c.log.Warn("trou non comblé dans un canal", "channel", id) },
		OnTooLong:        func() { c.log.Warn("trou non comblé (differenceTooLong)") },
	})
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		c.handle(ctx, e, u.Message, false)
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		c.handle(ctx, e, u.Message, false)
		return nil
	})
	d.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		c.handle(ctx, e, u.Message, true)
		return nil
	})
	d.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		c.handle(ctx, e, u.Message, true)
		return nil
	})

	var ready atomic.Bool // authorized and synchronised
	client := c.newClient(mgr, func(s telegram.ConnectionState) {
		switch s {
		case telegram.ConnectionStateDisconnected:
			c.st.set(event.Status{State: event.StateDisconnected, Account: c.st.account(), Detail: "connexion perdue"})
		case telegram.ConnectionStateReady:
			if ready.Load() {
				c.st.set(event.Status{State: event.StateConnected, Account: c.st.account()})
			}
		}
	})
	err := client.Run(ctx, func(ctx context.Context) error {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !st.Authorized || st.User == nil {
			return errLoggedOut
		}
		c.self.Store(st.User)
		acct := accountLabel(st.User)
		return mgr.Run(ctx, client.API(), st.User.ID, updates.AuthOptions{OnStart: func(context.Context) {
			ready.Store(true)
			c.st.set(event.Status{State: event.StateConnected, Account: acct})
			c.log.Info("connecté", "account_id", st.User.ID)
		}})
	})
	if auth.IsUnauthorized(err) {
		return errLoggedOut
	}
	return err
}

func (c *collector) handle(ctx context.Context, e tg.Entities, mc tg.MessageClass, edited bool) {
	n, ok := normalize(mc, e, c.self.Load(), edited)
	if !ok {
		return
	}
	if err := c.pub.Message(ctx, n.Sent, n.ID, n.Time, n.Msg); err != nil {
		c.log.Warn("publication", "event", n.ID, "err", err)
	}
}

// statusKeeper remembers the current status and publishes changes from a
// single goroutine, so that callbacks which must not block can call set.
type statusKeeper struct {
	mu   sync.Mutex
	cur  event.Status
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

// set records and publishes s unless it equals the current status.
func (k *statusKeeper) set(s event.Status) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s == k.cur {
		return
	}
	k.cur = s
	select {
	case k.ch <- s:
	default: // queue full: heartbeat will republish the current state
	}
}

func (k *statusKeeper) current() event.Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cur
}

func (k *statusKeeper) account() string { return k.current().Account }

// close flushes the pending statuses.
func (k *statusKeeper) close() {
	close(k.ch)
	<-k.done
}
