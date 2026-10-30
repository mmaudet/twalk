package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mmaudet/twalk/internal/event"
)

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

func TestRunAndDigest(t *testing.T) {
	url := startServer(t)
	t.Setenv("NATS_URL", url)
	t.Setenv("NATS_USER", "x")
	t.Setenv("HERMES_HOME", t.TempDir()) // ni règles ni clé LLM : règles par défaut, pas de LLM
	t.Setenv("TWALK_RULES", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, js, err := connect("trieur")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "twalk2", Subjects: []string{"twalk2.>"}, AllowDirect: true}); err != nil {
		t.Fatal(err)
	}
	pub, err := event.Connect(event.WhatsApp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	now := time.Now()
	comex := event.Conversation{ID: "whatsapp:comex@g.us", Kind: "group", Name: "Comex"}
	gilles := event.Sender{ID: "whatsapp:3361", Name: "Gilles", Known: true}
	dmG := event.Conversation{ID: "whatsapp:3361@s", Kind: "dm", Name: "Gilles"}
	inconnu := event.Conversation{ID: "whatsapp:3399@s", Kind: "dm"}
	for _, f := range []struct {
		sent bool
		id   string
		at   time.Duration
		m    event.Message
	}{
		{false, "whatsapp:comex:1", -3 * time.Hour, event.Message{Conversation: comex, Sender: gilles, Text: "Michel-Marie, ton avis ?", MentionsOwner: true}},
		{false, "whatsapp:3361:2", -2 * time.Hour, event.Message{Conversation: dmG, Sender: gilles, Text: "Appelle-moi quand tu peux"}},
		{true, "whatsapp:3361:3", -time.Hour, event.Message{Conversation: dmG, Sender: event.Sender{ID: "whatsapp:me"}, Text: "ok je t'appelle"}},
		{false, "whatsapp:3399:4", -30 * time.Minute, event.Message{Conversation: inconnu, Sender: event.Sender{ID: "whatsapp:3399", Name: "Bob"}, Text: "URGENT : serveur en panne"}},
	} {
		if err := pub.Message(ctx, f.sent, f.id, now.Add(f.at), f.m); err != nil {
			t.Fatal(err)
		}
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runDaemon(runCtx, newTestLogger()) }()

	var d *Digest
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		evs, err := readBus(ctx, js, now.Add(-4*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if d = BuildDigest(evs, now.Add(-4*time.Hour), time.Now()); d.UnlabeledCount == 0 && d.Labels["reply"]+d.Labels["urgent"]+d.Labels["info"] == 3 {
			break
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("arrêt: %v", err)
	}
	if d.UnlabeledCount != 0 || d.Labels["urgent"] != 1 || d.Labels["reply"] != 1 || d.Labels["info"] != 1 {
		t.Fatalf("étiquettes: %+v", d.Labels)
	}
	if len(d.Priority) != 2 || d.Priority[0].Label != "urgent" || d.Priority[1].Conversation != "Comex" || d.Priority[1].Replied {
		t.Fatalf("priorités: %+v", d.Priority)
	}
	md := d.Markdown()
	for _, want := range []string{"whatsapp : 3 reçus, 1 envoyés", "serveur en panne", "Comex (whatsapp) : 1 messages, 1 expéditeurs", "0 message(s) reçu(s) sans étiquette"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown sans %q:\n%s", want, md)
		}
	}
	// le message « Appelle-moi » (déjà répondu) est info
	if lbl := d.Labels["info"]; lbl != 1 {
		t.Errorf("info = %d", lbl)
	}
}
