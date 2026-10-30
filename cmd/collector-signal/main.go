// Command collector-signal is the read-only Signal collector of Twalk v2. It
// adapts the signal-cli JSON-RPC interface: it runs `signal-cli jsonRpc` as a
// child process (or connects to an existing daemon) on a device linked to the
// owner's account, and publishes what it receives on the bus. Its JSON-RPC
// client only knows two read methods (listGroups, listContacts).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

const usage = `collector-signal: collecteur Signal (lecture seule) de Twalk v2, adaptateur de signal-cli.

  collector-signal link [--qr /tmp/signal-qr.png]   lie cet appareil (QR à scanner dans Signal >
                                                    Réglages > Appareils liés), 5 min max
  collector-signal run                              démon: publie sur NATS (sortie 2 si appareil
                                                    non lié ou délié)

Environnement: TWALK_STATE_DIR (défaut $HERMES_HOME/secrets/twalk2/signal, config signal-cli, 0700),
SIGNAL_CLI_BIN (défaut signal-cli), SIGNAL_ACCOUNT (numéro; sinon lu dans la config),
SIGNAL_RPC_SOCKET (chemin unix ou hôte:port d'un démon existant; sinon processus enfant),
NATS_URL, NATS_USER, NATS_PASSWORD, TWALK_EXCLUDE, TWALK_LOG_LEVEL (debug|info|warn|error).
Envoyé automatiquement par signal-cli (protocole, non désactivable): accusés de DISTRIBUTION des
messages reçus, demandes de renvoi si un message ne se déchiffre pas. Jamais: accusés de lecture,
frappe, réactions, messages.
`

var errLoggedOut = errors.New("appareil Signal non lié ou délié")

// signal-cli stderr lines that mean the device is no longer linked.
var loggedOutRE = regexp.MustCompile(`(?i)authorization ?failed|not registered|unlinked|deauthori[sz]ed`)

func main() { os.Exit(realMain()) }

func realMain() int {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Fprint(os.Stderr, usage)
		if len(os.Args) < 2 {
			return 1
		}
		return 0
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL")))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	c := &collector{log: log, cfg: os.Getenv("TWALK_STATE_DIR"), bin: os.Getenv("SIGNAL_CLI_BIN"), names: &cache{}}
	if c.cfg == "" {
		c.cfg = filepath.Join(first(os.Getenv("HERMES_HOME"), "/home/hermes/hermes-new"), "secrets", "twalk2", "signal")
	}
	if c.bin == "" {
		c.bin = "signal-cli"
		if _, err := exec.LookPath(c.bin); err != nil {
			c.bin = filepath.Join(os.Getenv("HOME"), ".local", "bin", "signal-cli")
		}
	}
	if err := os.MkdirAll(c.cfg, 0o700); err != nil || os.Chmod(c.cfg, 0o700) != nil {
		log.Error("dossier d'état", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "link":
		fs := flag.NewFlagSet("link", flag.ExitOnError)
		qr := fs.String("qr", "/tmp/signal-qr.png", "fichier PNG du QR code")
		_ = fs.Parse(os.Args[2:])
		if err := c.link(ctx, *qr); err != nil {
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

type collector struct {
	log   *slog.Logger
	cfg   string // signal-cli --config directory
	bin   string
	acct  self
	pub   *event.Publisher
	names *cache
	mu    sync.Mutex
	cur   event.Status
}

// loadAccount reads the linked account from signal-cli's accounts.json.
func (c *collector) loadAccount() {
	c.acct = self{Number: os.Getenv("SIGNAL_ACCOUNT")}
	b, _ := os.ReadFile(filepath.Join(c.cfg, "data", "accounts.json"))
	var f struct {
		Accounts []struct{ Number, UUID string }
	}
	if json.Unmarshal(b, &f) == nil && len(f.Accounts) > 0 && (c.acct.Number == "" || c.acct.Number == f.Accounts[0].Number) {
		c.acct = self{Number: f.Accounts[0].Number, UUID: f.Accounts[0].UUID}
	}
}

// set publishes s unless it equals the current status.
func (c *collector) set(ctx context.Context, state, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := event.Status{State: state, Account: c.acct.Number, Detail: detail}
	if s == c.cur {
		return
	}
	c.cur = s
	if err := c.pub.Status(context.WithoutCancel(ctx), s); err != nil {
		c.log.Warn("publication du statut", "state", state, "err", err)
	}
}

func (c *collector) status() event.Status { c.mu.Lock(); defer c.mu.Unlock(); return c.cur }

func (c *collector) run(ctx context.Context) int {
	pub, err := event.Connect(event.Signal, c.log)
	if err != nil {
		c.log.Error("nats", "err", err)
		return 1
	}
	defer pub.Close()
	c.pub = pub
	c.set(ctx, event.StateDisconnected, "démarrage")
	go pub.Heartbeat(ctx, 5*time.Minute, c.status)

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		c.loadAccount()
		start, err := time.Now(), errLoggedOut
		if c.acct.Number != "" || os.Getenv("SIGNAL_RPC_SOCKET") != "" {
			err = c.runOnce(ctx)
		}
		if errors.Is(err, errLoggedOut) {
			c.log.Error("appareil non lié ou délié: lancer « collector-signal link »")
			c.set(ctx, event.StateLoggedOut, "collector-signal link")
			return 2
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > 2*time.Minute {
			backoff = 5 * time.Second
		}
		c.log.Warn("signal-cli arrêté, nouvelle tentative", "err", err, "in", backoff.String())
		c.set(ctx, event.StateDisconnected, "relance de signal-cli")
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	c.set(ctx, event.StateStopping, "")
	return 0
}

// runOnce runs one signal-cli session (existing daemon or child process).
func (c *collector) runOnce(ctx context.Context) error {
	if sock := os.Getenv("SIGNAL_RPC_SOCKET"); sock != "" {
		network := "tcp"
		if strings.HasPrefix(sock, "/") {
			network = "unix"
		}
		conn, err := net.Dial(network, sock)
		if err != nil {
			return err
		}
		defer conn.Close()
		go func() { <-ctx.Done(); conn.Close() }()
		return c.session(ctx, conn, conn, c.acct.Number)
	}
	// No --send-read-receipts, no typing: signal-cli only sends its default
	// delivery receipts. Attachments, stories, avatars, stickers are not fetched.
	cmd := exec.CommandContext(ctx, c.bin, "--config", c.cfg, "-a", c.acct.Number, "-o", "json", "jsonRpc",
		"--receive-mode", "on-start", "--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) } // clean stop of the child
	cmd.WaitDelay = 15 * time.Second
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	var loggedOut atomic.Bool
	go func() { // stderr text is only logged at debug level: it may name contacts
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if loggedOutRE.MatchString(sc.Text()) {
				loggedOut.Store(true)
			}
			c.log.Debug("signal-cli stderr", "text", sc.Text())
		}
	}()
	err := c.session(ctx, stdin, stdout, "")
	_ = stdin.Close()
	werr := cmd.Wait()
	if loggedOut.Load() {
		return errLoggedOut
	}
	return errors.Join(err, werr)
}

// session pumps notifications until the stream ends.
func (c *collector) session(ctx context.Context, w io.Writer, r io.Reader, account string) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc := newRPC(w, account)
	c.names.rc = rc
	evs, done := make(chan rpcMsg, 512), make(chan struct{})
	go func() {
		defer close(done)
		for m := range evs {
			c.handle(sctx, m)
		}
	}()
	go func() { // connected once signal-cli answers a local read
		for sctx.Err() == nil {
			err := c.names.refresh(sctx)
			if err == nil {
				c.set(ctx, event.StateConnected, "")
				return
			}
			c.log.Warn("signal-cli ne répond pas encore", "err", err)
			select {
			case <-sctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}()
	err := rc.readLoop(r, func(m rpcMsg) {
		select {
		case evs <- m:
		default:
			c.log.Warn("file de notifications pleine, message ignoré")
		}
	})
	close(evs)
	<-done
	return err
}

// handle normalizes one `receive` notification and publishes it.
func (c *collector) handle(ctx context.Context, m rpcMsg) {
	var p struct{ Envelope *envelope }
	if m.Method != "receive" || json.Unmarshal(m.Params, &p) != nil || p.Envelope == nil {
		return
	}
	e := p.Envelope
	if e.SyncMessage != nil && c.acct.UUID == "" {
		c.acct.UUID = e.SourceUUID // our own sync messages carry our uuid
	}
	c.names.refreshIfStale(ctx, e.DataMessage)
	n, ok := normalize(e, c.acct, c.names)
	if !ok {
		return
	}
	var err error
	if n.Reaction != nil {
		err = c.pub.Reaction(ctx, n.ID, n.Time, *n.Reaction)
	} else {
		err = c.pub.Message(ctx, n.Sent, n.ID, n.Time, *n.Msg)
	}
	if err != nil {
		c.log.Warn("publication", "event", n.ID, "err", err)
	}
}

// cache holds group and contact names (listGroups, listContacts: local reads).
// Keys: "group:<id>", phone number, uuid.
type cache struct {
	mu    sync.Mutex
	rc    *rpcClient
	names map[string]string
	at    time.Time
}

func (d *cache) lookup(k string) string { d.mu.Lock(); defer d.mu.Unlock(); return d.names[k] }

func (d *cache) groupName(id string) string { return d.lookup("group:" + id) }

func (d *cache) contact(number, uuid string) (string, bool) {
	name := first(d.lookup(number), d.lookup(uuid))
	return name, name != ""
}

func (d *cache) refresh(ctx context.Context) error {
	var gs []struct{ ID, Name string }
	var cs []struct{ Number, UUID, Name, NickName string }
	if err := d.rc.call(ctx, "listGroups", &gs); err != nil {
		return err
	}
	if err := d.rc.call(ctx, "listContacts", &cs); err != nil {
		return err
	}
	m := map[string]string{}
	for _, g := range gs {
		m["group:"+g.ID] = g.Name
	}
	for _, c := range cs {
		if n := first(c.Name, c.NickName); n != "" {
			m[c.Number], m[c.UUID] = n, n
		}
	}
	d.mu.Lock()
	d.names, d.at = m, time.Now()
	d.mu.Unlock()
	return nil
}

// refreshIfStale reloads the caches for an unknown group (at most once a
// minute) or when they are older than 10 minutes.
func (d *cache) refreshIfStale(ctx context.Context, dm *dataMsg) {
	gid := ""
	if dm != nil && dm.GroupInfo != nil {
		gid = dm.GroupInfo.GroupID
	}
	d.mu.Lock()
	age, rc := time.Since(d.at), d.rc
	_, has := d.names["group:"+gid]
	known := gid == "" || has
	d.mu.Unlock()
	if rc != nil && (age > 10*time.Minute || (!known && age > time.Minute)) {
		_ = d.refresh(ctx)
	}
}
