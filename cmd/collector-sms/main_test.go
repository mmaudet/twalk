package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/mmaudet/twalk/internal/event"
)

func TestSessionPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sms")
	if ad, err := loadSession(dir); ad != nil || err != nil {
		t.Fatalf("no session yet: %v %v", ad, err)
	}
	ad := libgm.NewAuthData()
	ad.Browser = &gmproto.Device{SourceID: "browser-1"}
	ad.Mobile = &gmproto.Device{SourceID: "phone-1"}
	ad.TachyonAuthToken = []byte("token-secret")
	ad.TachyonExpiry = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	if err := saveSession(dir, ad); err != nil {
		t.Fatal(err)
	}
	for p, perm := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "session.json"): 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != perm {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	// A token refresh rewrites the file: still 0600, no temp file left behind.
	ad.TachyonAuthToken = []byte("token-refreshed")
	if err := saveSession(dir, ad); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(dir)
	if err != nil || got == nil {
		t.Fatal(got, err)
	}
	if string(got.TachyonAuthToken) != "token-refreshed" || got.Browser.GetSourceID() != "browser-1" || got.Mobile.GetSourceID() != "phone-1" ||
		!bytes.Equal(got.RequestCrypto.AESKey, ad.RequestCrypto.AESKey) || got.RefreshKey == nil || !got.TachyonExpiry.Equal(ad.TachyonExpiry) {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if st, _ := os.Stat(filepath.Join(dir, "session.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", st.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	if os.WriteFile(filepath.Join(dir, "session.json"), []byte("{oops"), 0o600) != nil {
		t.Fatal()
	}
	if _, err := loadSession(dir); err == nil {
		t.Fatal("corrupt session accepted")
	}
}

func TestStateDirDefault(t *testing.T) {
	t.Setenv("TWALK_STATE_DIR", "")
	t.Setenv("HERMES_HOME", "/h")
	if stateDir() != "/h/secrets/twalk2/sms" {
		t.Fatal(stateDir())
	}
	t.Setenv("TWALK_STATE_DIR", "/x")
	if stateDir() != "/x" {
		t.Fatal(stateDir())
	}
}

// libgm's own fields (ids, base64 payloads of unknown events…) must not reach our logs.
func TestLibgmLogDropsFields(t *testing.T) {
	var buf bytes.Buffer
	l := libgmLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	l.Debug().Str("data", "AAAA").Msg("debug is dropped")
	l.Warn().Str("evt_data", "c2VjcmV0").Str("conv", "42").Err(fmt.Errorf("boom")).Msg("Got unknown event type")
	out := buf.String()
	if strings.Contains(out, "c2VjcmV0") || strings.Contains(out, "AAAA") || strings.Contains(out, "42") || strings.Contains(out, "debug is dropped") {
		t.Fatalf("leak: %s", out)
	}
	if !strings.Contains(out, "Got unknown event type") || !strings.Contains(out, "boom") {
		t.Fatalf("lost message: %s", out)
	}
}

func startServer(t *testing.T) string {
	bin := "/home/hermes/.local/bin/nats-server"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("nats-server absent")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, "-js", "-a", "127.0.0.1", "-p", fmt.Sprint(port), "-sd", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	for i := 0; i < 50; i++ {
		if nc, err := nats.Connect(url); err == nil {
			nc.Close()
			return url
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("nats-server ne démarre pas")
	return ""
}

// End to end on a throwaway NATS: libgm events in, bus events out. No network access to Google.
func TestPublishThroughNATS(t *testing.T) {
	url := startServer(t)
	t.Setenv("NATS_URL", url)
	t.Setenv("TWALK_EXCLUDE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, _ := nats.Connect(url)
	defer nc.Close()
	js, _ := jetstream.New(nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "twalk2", Subjects: []string{"twalk2.>"}}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	pub, err := event.Connect(event.SMS, log)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	c := &collector{ctx: ctx, log: log, pub: pub, state: t.TempDir(), queue: make(chan *libgm.WrappedMessage, 8), exit: make(chan int, 1),
		dir: &convDir{log: log}, cv: newConv()}
	c.cv.cutoff = time.Now().Add(-maxReplayAge)
	c.dir.put(dm)
	c.cur = c.f.status()
	now := time.Now()
	wrap := func(m *gmproto.Message) *libgm.WrappedMessage { return &libgm.WrappedMessage{Message: m} }

	c.onEvent(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_ACTIVE})
	c.onEvent(wrap(msg("m1", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, now, text("bonjour"))))
	c.onEvent(wrap(msg("m1", "42", "2", gmproto.MessageStatusType_INCOMING_DISPLAYED, now, text("bonjour")))) // status update
	c.onEvent(wrap(msg("m2", "42", "1", gmproto.MessageStatusType_OUTGOING_SENDING, now, text("réponse"))))   // not complete
	c.onEvent(wrap(msg("m2", "42", "1", gmproto.MessageStatusType_OUTGOING_COMPLETE, now, text("réponse"))))
	for len(c.queue) > 0 {
		c.process(<-c.queue)
	}
	c.onEvent(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_INACTIVE})
	c.onEvent(&events.ListenRecovered{}) // must not clear the "active elsewhere" state
	c.onEvent(&gmproto.RevokePairData{})
	c.onEvent(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_ACTIVE}) // ignored once logged out
	if code := <-c.exit; code != 2 {
		t.Fatalf("exit code %d", code)
	}

	cons, err := js.OrderedConsumer(ctx, "twalk2", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 5 {
		m, err := cons.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			break
		}
		var env event.Envelope
		_ = json.Unmarshal(m.Data(), &env)
		d, _ := json.Marshal(env.Data)
		got = append(got, m.Subject()+" "+env.ID+" "+env.Network+" "+string(d))
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"twalk2.msg.received.sms.dm." + event.ConvHash("sms:42") + " sms:42:m1 sms",
		"twalk2.msg.sent.sms.dm." + event.ConvHash("sms:42") + " sms:42:m2 sms",
		`"state":"connected"`, `"detail":"session Messages Web active ailleurs"`, `"state":"logged_out"`, "collector-sms pair",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Count(all, "sms:42:m1")+strings.Count(all, "sms:42:m2") != 2 {
		t.Errorf("message published several times:\n%s", all)
	}
	if len(got) != 5 { // 2 messages + connected + inactive + logged_out
		t.Errorf("%d events:\n%s", len(got), all)
	}
}
