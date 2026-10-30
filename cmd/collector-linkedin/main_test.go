package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mmaudet/twalk/internal/event"
)

// fakeJMAP is a minimal JMAP server: Session, Email/query (filter after, sorted
// by receivedAt) and Email/get. It records the methods and "after" values it
// sees and rejects every token other than the one in good.
type fakeJMAP struct {
	*httptest.Server
	mu       sync.Mutex
	emails   []mail
	good     string // accepted Bearer token ("" = reject everything)
	methods  map[string]int
	afters   []string
	maxGet   int
	excluded []string // last inMailboxOtherThan seen
}

func newFake(t *testing.T, good string, emails ...mail) *fakeJMAP {
	f := &fakeJMAP{good: good, emails: emails, methods: map[string]int{}}
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods[r.Method+" "+r.URL.Path]++
		if f.good == "" || r.Header.Get("Authorization") != "Bearer "+f.good {
			w.WriteHeader(401)
			return false
		}
		return true
	}
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			fmt.Fprintf(w, `{"apiUrl":%q,"username":"owner@example.org","primaryAccounts":{"urn:ietf:params:jmap:mail":"acc1"}}`, f.URL+"/api")
		}
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var req struct{ MethodCalls [][]json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		var resp [][]any
		for _, c := range req.MethodCalls {
			var name string
			_ = json.Unmarshal(c[0], &name)
			f.methods[name]++
			var args struct {
				Filter struct {
					From, After        string
					InMailboxOtherThan []string
				}
				IDs []string
			}
			_ = json.Unmarshal(c[1], &args)
			switch name {
			case "Email/query":
				f.afters = append(f.afters, args.Filter.After)
				f.excluded = args.Filter.InMailboxOtherThan
				after, _ := time.Parse(time.RFC3339, args.Filter.After)
				var ids []string
				for _, m := range f.emails {
					if !m.Received.Before(after) && strings.Contains(m.Addr, args.Filter.From) {
						ids = append(ids, m.ID)
					}
				}
				resp = append(resp, []any{name, map[string]any{"ids": ids}, "q"})
			case "Mailbox/get":
				resp = append(resp, []any{name, map[string]any{"list": []map[string]string{{"id": "inbox", "role": "inbox"}, {"id": "trash", "role": "trash"}}}, "m"})
			case "Email/get":
				f.maxGet = max(f.maxGet, len(args.IDs))
				var list []map[string]any
				for _, id := range args.IDs {
					for _, m := range f.emails {
						if m.ID == id {
							list = append(list, map[string]any{"id": m.ID, "receivedAt": m.Received.Format(time.RFC3339), "subject": m.Subject,
								"from": []map[string]string{{"name": m.From, "email": m.Addr}}, "textBody": []map[string]string{{"partId": "1"}},
								"bodyValues": map[string]any{"1": map[string]any{"value": m.Body}}})
						}
					}
				}
				resp = append(resp, []any{name, map[string]any{"list": list}, "g"})
			default:
				resp = append(resp, []any{"error", map[string]any{"type": "unknownMethod"}, "x"})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": resp})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeJMAP) add(m mail) { f.mu.Lock(); f.emails = append(f.emails, m); f.mu.Unlock() }

// assertReadOnly: only the session GET, Mailbox/get, Email/query and Email/get were ever called.
func (f *fakeJMAP) assertReadOnly(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for m := range f.methods {
		if m != "GET /session" && m != "POST /api" && m != "Mailbox/get" && m != "Email/query" && m != "Email/get" {
			t.Errorf("forbidden call %q", m)
		}
	}
	if f.methods["Email/query"] > 0 && (len(f.excluded) != 1 || f.excluded[0] != "trash") {
		t.Errorf("Trash not excluded from Email/query: %v", f.excluded)
	}
}

// tokenScript writes a token command; each line of outputs is the stdout of
// successive calls (the last one repeats); "exit78" exits with 78.
func tokenScript(t *testing.T, outputs ...string) string {
	dir := t.TempDir()
	var sh strings.Builder
	sh.WriteString("#!/bin/sh\nn=$(cat " + dir + "/n 2>/dev/null || echo 0)\necho $((n+1)) > " + dir + "/n\ncase $n in\n")
	for i, o := range outputs {
		pat := fmt.Sprint(i)
		if i == len(outputs)-1 {
			pat = "*"
		}
		if o == "exit78" {
			sh.WriteString(pat + ") exit 78;;\n")
		} else {
			sh.WriteString(pat + ") echo " + o + ";;\n")
		}
	}
	sh.WriteString("esac\n")
	p := filepath.Join(dir, "token.sh")
	if err := os.WriteFile(p, []byte(sh.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func client(f *fakeJMAP, tokenCmd string) *jmapClient {
	return &jmapClient{hc: http.DefaultClient, sessionURL: f.URL + "/session", tokenCmd: []string{tokenCmd}}
}

func em(id, addr, subject string, at time.Time) mail {
	return mail{ID: id, Received: at, Addr: addr + "@linkedin.com", From: "Ana Lopez via LinkedIn", Subject: subject, Body: "corps\n"}
}

func TestFetch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var all []mail
	for i := 0; i < 130; i++ {
		all = append(all, em(fmt.Sprint("e", i), "invitations", "Je souhaite que nous nous connections", now.Add(time.Duration(i-200)*time.Minute)))
	}
	all = append(all, mail{ID: "other", Received: now, Addr: "someone@example.org"}) // not LinkedIn
	f := newFake(t, "good", all...)
	ms, err := client(f, tokenScript(t, "good")).fetch(context.Background(), now.Add(-24*time.Hour), func(id string) bool { return id == "e0" })
	if err != nil || len(ms) != 129 { // 130 - skipped e0, no foreign sender
		t.Fatalf("%d emails, %v", len(ms), err)
	}
	if !sort.SliceIsSorted(ms, func(i, j int) bool { return ms[i].Received.Before(ms[j].Received) }) || ms[0].ID != "e1" || ms[0].Body != "corps\n\n" {
		t.Errorf("order or body: %+v", ms[0])
	}
	if f.maxGet > 50 {
		t.Errorf("Email/get with %d ids (server limit is 100 for full bodies)", f.maxGet)
	}
	f.assertReadOnly(t)

	// Stale token: 401, a new one is requested and works.
	f = newFake(t, "good", all[:2]...)
	if ms, err = client(f, tokenScript(t, "stale", "good")).fetch(context.Background(), now.Add(-24*time.Hour), func(string) bool { return false }); err != nil || len(ms) != 2 {
		t.Errorf("after 401: %d %v", len(ms), err)
	}
	// 401 even with a new token, and a revoked grant: logged out.
	f = newFake(t, "")
	if _, err = client(f, tokenScript(t, "a", "b")).fetch(context.Background(), now, nil); err == nil || !strings.Contains(err.Error(), errLoggedOut.Error()) {
		t.Errorf("persistent 401: %v", err)
	}
	if _, err = client(f, tokenScript(t, "exit78")).fetch(context.Background(), now, nil); err != errLoggedOut {
		t.Errorf("exit 78: %v", err)
	}
	// Broker broken (other failure) or server down: plain error, not logged out.
	if _, err = client(f, "/nonexistent-token-cmd").fetch(context.Background(), now, nil); err == nil || err == errLoggedOut {
		t.Errorf("broker failure: %v", err)
	}
	f.Close()
	if _, err = client(f, tokenScript(t, "a")).fetch(context.Background(), now, nil); err == nil || strings.Contains(err.Error(), errLoggedOut.Error()) {
		t.Errorf("server down: %v", err)
	}
}

func TestCursorFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if c, err := loadCursor(dir); err != nil || !c.IsZero() {
		t.Fatalf("no cursor yet: %v %v", c, err)
	}
	at := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ { // second save replaces the file, same permissions
		if err := saveCursor(dir, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for p, perm := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "cursor.json"): 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != perm {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if c, _ := loadCursor(dir); !c.Equal(at.Add(time.Minute)) {
		t.Errorf("cursor %v", c)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover files: %v", ents)
	}
	_ = os.WriteFile(filepath.Join(dir, "cursor.json"), []byte("{oops"), 0o600)
	if _, err := loadCursor(dir); err == nil {
		t.Error("corrupt cursor accepted")
	}
	t.Setenv("TWALK_STATE_DIR", "")
	t.Setenv("HERMES_HOME", "/h")
	if stateDir() != "/h/secrets/twalk2/linkedin" {
		t.Error(stateDir())
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

// runFor starts run() against the fake server and returns a stop function and the exit code channel.
func runFor(t *testing.T, ctx context.Context, f *fakeJMAP, token, state string) (stop func(), code chan int) {
	t.Setenv("TWALK_JMAP_SESSION_URL", f.URL+"/session")
	t.Setenv("TWALK_TWAKE_TOKEN_CMD", token)
	t.Setenv("TWALK_STATE_DIR", state)
	t.Setenv("TWALK_LINKEDIN_POLL", "1s")
	t.Setenv("TWALK_EXCLUDE", "")
	ctx, cancel := context.WithCancel(ctx)
	code = make(chan int, 1)
	go func() { code <- run(ctx, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))) }()
	return cancel, code
}

// collect reads what is on the stream: "subject id state" lines.
func collect(t *testing.T, ctx context.Context, js jetstream.JetStream, want int) []string {
	cons, err := js.OrderedConsumer(ctx, "twalk2", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < want {
		m, err := cons.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			break
		}
		var env event.Envelope
		_ = json.Unmarshal(m.Data(), &env)
		d, _ := json.Marshal(env.Data)
		got = append(got, m.Subject()+" "+env.ID+" "+string(d))
	}
	return got
}

// End to end on a throwaway NATS with a fake JMAP: publication, cursor,
// catch-up after a stop, deduplication, statuses and exit codes.
func TestRunThroughNATS(t *testing.T) {
	url := startServer(t)
	t.Setenv("NATS_URL", url)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	nc, _ := nats.Connect(url)
	defer nc.Close()
	js, _ := jetstream.New(nc)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "twalk2", Subjects: []string{"twalk2.>"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	e1 := em("e1", "newsletters-noreply", "Les actus de la semaine", now.Add(-3*time.Hour))
	e2 := em("e2", "messaging-digest-noreply", "Ana vient de vous envoyer un message", now.Add(-2*time.Hour))
	e3 := em("e3", "invitations", "Je souhaite que nous nous connections", now.Add(-30*time.Minute))
	old := em("old", "invitations", "Je souhaite que nous nous connections", now.Add(-48*time.Hour)) // before the first window
	f := newFake(t, "good", old, e1, e2, e3)
	state := filepath.Join(t.TempDir(), "linkedin")
	token := tokenScript(t, "good")

	// First start: no cursor, window of 24 h. e2 and e3 are published, e1 (newsletter) and old are not.
	stop, code := runFor(t, ctx, f, token, state)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := loadCursor(state); c.Equal(e3.Received) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond) // two more cycles: nothing must be published again
	stop()
	if c := <-code; c != 0 {
		t.Fatalf("exit %d", c)
	}
	if c, _ := loadCursor(state); !c.Equal(e3.Received) {
		t.Fatalf("cursor %v, want %v", c, e3.Received)
	}
	if st, _ := os.Stat(filepath.Join(state, "cursor.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("cursor perm %v", st.Mode())
	}
	if first, _ := time.Parse(time.RFC3339, f.afters[0]); now.Add(-24*time.Hour).Sub(first).Abs() > 10*time.Second {
		t.Errorf("first query after %s, want now-24h", f.afters[0])
	}
	if last, _ := time.Parse(time.RFC3339, f.afters[len(f.afters)-1]); !last.Equal(e3.Received.Add(-time.Hour)) {
		t.Errorf("later queries after %s, want cursor-1h", f.afters[len(f.afters)-1])
	}

	// A new email arrives while the collector is stopped; restart: caught up, e3 (inside the 1 h overlap) not published twice.
	e4 := em("e4", "messages-noreply", "Ana a commenté votre post", now.Add(-5*time.Minute))
	f.add(e4)
	stop, code = runFor(t, ctx, f, token, state)
	time.Sleep(3500 * time.Millisecond)
	stop()
	<-code

	got := collect(t, ctx, js, 20)
	all := strings.Join(got, "\n")
	msgs := 0
	for _, g := range got {
		if strings.HasPrefix(g, "twalk2.msg.received.linkedin.") {
			msgs++
		}
	}
	for _, want := range []string{
		"twalk2.msg.received.linkedin.dm." + event.ConvHash("linkedin:message") + " linkedin:e2 ",
		"twalk2.msg.received.linkedin.group." + event.ConvHash("linkedin:invitation") + " linkedin:e3 ",
		"twalk2.msg.received.linkedin.group." + event.ConvHash("linkedin:comment") + " linkedin:e4 ",
		`"mentions_owner":true`, `"account":"owner@example.org","state":"connected"`, `"state":"stopping"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if msgs != 3 || strings.Contains(all, "linkedin:e1") || strings.Contains(all, "linkedin:old") {
		t.Errorf("%d messages (want 3: e2 e3 e4, once each; no newsletter, nothing older than 24 h):\n%s", msgs, all)
	}
	f.assertReadOnly(t)

	// Revoked grant (broker exit 78): logged_out with the command to run, exit 2.
	_, code = runFor(t, ctx, f, tokenScript(t, "exit78"), t.TempDir())
	select {
	case c := <-code:
		if c != 2 {
			t.Errorf("exit %d after exit 78", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop on a revoked grant")
	}
	// Server rejecting every token: same.
	_, code = runFor(t, ctx, newFake(t, ""), token, t.TempDir())
	if c := <-code; c != 2 {
		t.Errorf("exit %d after persistent 401", c)
	}
	got = collect(t, ctx, js, 30)
	all = strings.Join(got, "\n")
	if strings.Count(all, `"state":"logged_out"`) != 2 || !strings.Contains(all, "twake-token authorize --renew") {
		t.Errorf("logged_out statuses:\n%s", all)
	}
	// JMAP down: disconnected with an explicit detail, then the loop keeps trying (no exit).
	down := newFake(t, "good")
	down.Close()
	stop, code = runFor(t, ctx, down, token, t.TempDir())
	time.Sleep(2500 * time.Millisecond)
	select {
	case c := <-code:
		t.Fatalf("run stopped (%d) while JMAP is down", c)
	default:
	}
	stop()
	<-code
	if all = strings.Join(collect(t, ctx, js, 40), "\n"); !strings.Contains(all, `"state":"disconnected"`) || !strings.Contains(all, "injoignable") {
		t.Errorf("no disconnected status:\n%s", all)
	}
}
