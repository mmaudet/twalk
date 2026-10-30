package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const secret = "tok-secret-123"

// fakeMattermost serves the few endpoints the collector may touch and
// records every request and every WebSocket frame the client writes.
type fakeMattermost struct {
	mu      sync.Mutex
	reqs    []string // "METHOD path"
	frames  []string // text frames received from the client
	authOK  bool
	srv     *httptest.Server
	posted  string
	chDone  chan struct{}
	tokenOK func(*http.Request) bool
}

func newFake(t *testing.T, authOK bool) *fakeMattermost {
	f := &fakeMattermost{authOK: authOK, chDone: make(chan struct{})}
	mux := http.NewServeMux()
	rec := func(r *http.Request) bool {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		return r.Header.Get("Authorization") == "Bearer "+secret
	}
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, r *http.Request) {
		if !rec(r) {
			http.Error(w, "{}", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"id":"`+me+`","username":"michel"}`)
	})
	mux.HandleFunc("/api/v4/users/me/teams", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		io.WriteString(w, `[{"id":"t1"},{"id":"t2"}]`)
	})
	mux.HandleFunc("/api/v4/users/", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		io.WriteString(w, `{"id":"bob0000000000000000000000b","username":"bob"}`)
	})
	mux.HandleFunc("/api/v4/channels/", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		io.WriteString(w, `{"id":"chD","type":"D","display_name":"","name":"bob0000000000000000000000b__`+me+`"}`)
	})
	mux.HandleFunc("/api/v4/websocket", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, string(b))
		f.mu.Unlock()
		if !f.authOK {
			conn.Write(ctx, websocket.MessageText, []byte(`{"status":"FAIL","seq_reply":1,"error":{"id":"api.web_socket_router.bad_auth.app_error"}}`))
			conn.Close(websocket.StatusPolicyViolation, "bad auth")
			return
		}
		conn.Write(ctx, websocket.MessageText, []byte(`{"status":"OK","seq_reply":1}`))
		conn.Write(ctx, websocket.MessageText, []byte(`{"event":"hello","data":{"server_version":"9.11.0"},"broadcast":{},"seq":0}`))
		conn.Write(ctx, websocket.MessageText, []byte(`{"event":"typing","data":{"parent_id":"","user_id":"x"},"broadcast":{"channel_id":"chD"},"seq":1}`))
		conn.Write(ctx, websocket.MessageText, []byte(f.posted))
		// Keep reading: any further client frame is recorded.
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.frames = append(f.frames, string(b))
			f.mu.Unlock()
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMattermost) snapshot() (reqs, frames []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...), append([]string(nil), f.frames...)
}

func newTestCollector(t *testing.T, f *fakeMattermost) *collector {
	c, err := newCollector(f.srv.URL+"/", secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWebSocketSendsOnlyTheChallenge(t *testing.T) {
	f := newFake(t, true)
	f.posted = `{"event":"posted","data":{"channel_type":"D","sender_name":"@bob","mentions":"[\"` + me + `\"]","post":` +
		enc(enc(map[string]any{"id": "p1", "create_at": 1761800000123, "user_id": "bob0000000000000000000000b", "channel_id": "chD", "message": "coucou"})) +
		`},"broadcast":{"channel_id":"chD"},"seq":2}`
	c := newTestCollector(t, f)
	got := make(chan normalized, 4)
	c.emit = func(_ context.Context, n normalized) { got <- n }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.runOnce(ctx) }()

	select {
	case n := <-got:
		if n.Msg == nil || n.Msg.Text != "coucou" || !n.Msg.MentionsOwner || n.Msg.Conversation.Kind != "dm" || n.Msg.Conversation.Name != "bob" {
			t.Fatalf("n = %+v msg = %+v", n, n.Msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
	time.Sleep(300 * time.Millisecond) // let any stray client frame arrive
	cancel()
	<-done

	reqs, frames := f.snapshot()
	if len(frames) != 1 {
		t.Fatalf("client wrote %d frames: %v", len(frames), frames)
	}
	var ch struct {
		Seq    int               `json:"seq"`
		Action string            `json:"action"`
		Data   map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Action != "authentication_challenge" || ch.Seq != 1 || ch.Data["token"] != secret || len(ch.Data) != 1 {
		t.Errorf("challenge = %+v", ch)
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("non-GET request: %s", r)
		}
		for _, bad := range []string{"/view", "/status", "typing"} {
			if strings.Contains(r, bad) {
				t.Errorf("forbidden endpoint: %s", r)
			}
		}
	}
}

func TestWebSocketAuthFailureIsLoggedOut(t *testing.T) {
	f := newFake(t, false)
	c := newTestCollector(t, f)
	c.emit = func(context.Context, normalized) { t.Error("unexpected event") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.runOnce(ctx); err != errLoggedOut {
		t.Fatalf("err = %v", err)
	}
}

func TestRestUnauthorizedIsLoggedOut(t *testing.T) {
	f := newFake(t, true)
	c := newTestCollector(t, f)
	c.token = "mauvais"
	if err := c.check(context.Background()); err != errLoggedOut {
		t.Fatalf("err = %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := newFake(t, true)
	c := newTestCollector(t, f)
	if err := c.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs, _ := f.snapshot()
	if len(reqs) != 2 || reqs[0] != "GET /api/v4/users/me" || reqs[1] != "GET /api/v4/users/me/teams" {
		t.Errorf("reqs = %v", reqs)
	}
}

func TestNewCollectorConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, u := range []string{"", "chat.exemple.com", "ftp://x", "https://"} {
		if _, err := newCollector(u, "t", log); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	if _, err := newCollector("https://chat.exemple.com", "", log); err == nil {
		t.Error("empty token accepted")
	}
	c, err := newCollector("https://chat.exemple.com/", "t", log)
	if err != nil || c.base != "https://chat.exemple.com" {
		t.Errorf("c = %+v err = %v", c, err)
	}
}
