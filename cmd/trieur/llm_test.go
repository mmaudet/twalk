package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

func TestParseLabel(t *testing.T) {
	cases := []struct {
		in, label string
		ok        bool
	}{
		{`{"label":"urgent","reason":"client bloqué"}`, "urgent", true},
		{"Voici ma réponse :\n```json\n{\"label\": \"Reply\", \"reason\": \"attend un retour\"}\n``` voilà", "reply", true},
		{`{"label":"important","reason":"x"}`, "", false},
		{`pas de json`, "", false},
		{`{"label": 3}`, "", false},
	}
	for _, c := range cases {
		l, _, err := ParseLabel(c.in)
		if (err == nil) != c.ok || l != c.label {
			t.Errorf("%q: got %q %v", c.in, l, err)
		}
	}
}

func llmFor(url string) *LLM {
	return &LLM{URL: url, Key: "sk-secret-key", Model: "test-model", HTTP: http.DefaultClient, Backoff: time.Millisecond}
}

func reply(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	return string(b)
}

func TestLLMRetryOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-secret-key" {
			t.Error("clé absente")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(reply(`{"label":"reply","reason":"question directe"}`)))
	}))
	defer srv.Close()
	l, reason, err := llmFor(srv.URL).Classify(context.Background(), "x")
	if err != nil || l != "reply" || reason == "" || calls.Load() != 2 {
		t.Fatalf("got %q %q %v après %d appels", l, reason, err, calls.Load())
	}
}

func TestLLMResponseFormatRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["response_format"]; has {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"response_format unsupported"}`))
			return
		}
		_, _ = w.Write([]byte(reply(`{"label":"info","reason":"ok"}`)))
	}))
	defer srv.Close()
	if l, _, err := llmFor(srv.URL).Classify(context.Background(), "x"); err != nil || l != "info" {
		t.Fatalf("got %q %v", l, err)
	}
}

// A budget error must degrade to the rule label and never leak the body in logs.
func TestBudgetErrorNoLeak(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"Budget has been exceeded! key=sk-LEAKED-FRAGMENT"}}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	tr := newTriager(&Rules{}, llmFor(srv.URL), slog.New(slog.NewJSONHandler(&logs, nil)))
	var got event.Triage
	tr.pub = func(_ context.Context, _, _ string, x event.Triage) error { got = x; return nil }
	env := inEnvelope{Type: event.TypeMessageReceived, ID: "whatsapp:c1:m1", Network: "whatsapp", Time: time.Now()}
	env.Data, _ = json.Marshal(msg("dm", "bonjour", true, false))
	raw, _ := json.Marshal(env)
	for i := 0; i < 2; i++ {
		if err := tr.handle(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
	if got.Label != "reply" || got.By != "rule:dm_known (llm indisponible)" {
		t.Fatalf("got %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("budget épuisé: pas de nouvel appel avant 1 min, %d appels", calls.Load())
	}
	if s := logs.String(); strings.Contains(s, "LEAKED") || strings.Contains(s, "sk-") || !strings.Contains(s, `"kind":"budget"`) {
		t.Fatalf("journal: %s", s)
	}
	_, _, err := llmFor(srv.URL).Classify(context.Background(), "x")
	var le *LLMError
	if !errors.As(err, &le) || strings.Contains(err.Error(), "LEAKED") {
		t.Fatalf("erreur: %v", err)
	}
}

func TestAlreadyReplied(t *testing.T) {
	now := time.Now()
	tr := newTriager(&Rules{ReplyKeywords: []string{"peux-tu"}}, nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	var got event.Triage
	tr.pub = func(_ context.Context, _, _ string, x event.Triage) error { got = x; return nil }
	mk := func(typ, id string, at time.Time) []byte {
		env := inEnvelope{Type: typ, ID: id, Network: "whatsapp", Time: at}
		env.Data, _ = json.Marshal(msg("dm", "peux-tu venir", true, false))
		b, _ := json.Marshal(env)
		return b
	}
	// rattrapage : l'envoi du propriétaire est déjà connu (lookup) et postérieur au message
	tr.lookup = func(context.Context, event.Conversation) (time.Time, error) { return now, nil }
	if err := tr.handle(context.Background(), mk(event.TypeMessageReceived, "whatsapp:c1:m1", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got.Label != "info" || got.Reason != "déjà répondu" {
		t.Fatalf("got %+v", got)
	}
	// message plus récent que l'envoi : toujours à répondre
	_ = tr.handle(context.Background(), mk(event.TypeMessageReceived, "whatsapp:c1:m2", now.Add(time.Minute)))
	if got.Label != "reply" {
		t.Fatalf("got %+v", got)
	}
}
