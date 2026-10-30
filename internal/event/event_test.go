package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubjects(t *testing.T) {
	id := "whatsapp:120363000000000000@g.us"
	h := ConvHash(id)
	if len(h) != 12 || h != ConvHash(id) {
		t.Fatalf("hash not stable/short: %q", h)
	}
	if got := MessageSubject(false, WhatsApp, KindGroup, id); got != "twalk2.msg.received.whatsapp.group."+h {
		t.Fatal(got)
	}
	if got := MessageSubject(true, Telegram, KindDM, "telegram:42"); !strings.HasPrefix(got, "twalk2.msg.sent.telegram.dm.") {
		t.Fatal(got)
	}
	if got := ReactionSubject(Signal, id); got != "twalk2.reaction.signal."+h {
		t.Fatal(got)
	}
	if StatusSubject(Matrix) != "twalk2.collector.status.matrix" {
		t.Fatal("status subject")
	}
}

func TestValidate(t *testing.T) {
	if validate("irc", nil) == nil {
		t.Fatal("unknown network accepted")
	}
	if validate(WhatsApp, &Conversation{ID: "x", Kind: "room"}) == nil {
		t.Fatal("unknown kind accepted")
	}
	if validate(WhatsApp, &Conversation{Kind: KindDM}) == nil {
		t.Fatal("empty id accepted")
	}
	if err := validate(WhatsApp, &Conversation{ID: "x", Kind: KindDM}); err != nil {
		t.Fatal(err)
	}
}

func TestExclusions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exclude.yaml")
	os.WriteFile(p, []byte("networks: [signal]\nconversations: [\"Famille\", \"telegram:7\"]\nsenders: [\"whatsapp:336\"]\n"), 0o600)
	ex, err := LoadExclusions(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		net  string
		c    Conversation
		s    Sender
		want bool
	}{
		{Signal, Conversation{ID: "a"}, Sender{}, true},
		{WhatsApp, Conversation{ID: "b", Name: "Famille"}, Sender{}, true},
		{Telegram, Conversation{ID: "telegram:7"}, Sender{}, true},
		{WhatsApp, Conversation{ID: "c"}, Sender{ID: "whatsapp:336"}, true},
		{WhatsApp, Conversation{ID: "c", Name: "Comex"}, Sender{ID: "whatsapp:337"}, false},
	}
	for i, c := range cases {
		if got := ex.Excluded(c.net, c.c, c.s); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
	none, err := LoadExclusions(filepath.Join(dir, "missing.yaml"))
	if err != nil || none.Excluded(WhatsApp, Conversation{ID: "x"}, Sender{}) {
		t.Fatal("missing file must exclude nothing")
	}
}

func TestEnvelopeJSON(t *testing.T) {
	env := Envelope{SpecVersion: "1.0", Type: TypeMessageReceived, Source: "twalk2://h/collector/whatsapp",
		ID: "whatsapp:c:1", Time: time.Unix(0, 0).UTC(), Network: WhatsApp, DataContentType: "application/json",
		Data: Message{Conversation: Conversation{ID: "c", Kind: KindDM}, Sender: Sender{ID: "s"}, Text: "salut"}}
	b, _ := json.Marshal(env)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"specversion", "type", "source", "id", "time", "network", "datacontenttype", "data"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	d := m["data"].(map[string]any)
	if d["mentions_owner"] != false || d["text"] != "salut" {
		t.Errorf("data: %v", d)
	}
}
