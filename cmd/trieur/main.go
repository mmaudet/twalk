// Command trieur labels the messages of the Twalk v2 bus (urgent, reply, info,
// noise) with rules then an optional LLM, and prints a digest of the bus.
// It never contacts a messaging network: it only reads and writes NATS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mmaudet/twalk/internal/event"
)

const usage = `trieur: classement des messages du bus Twalk v2.

  trieur run                                  démon: étiquette les messages reçus (triage.labeled)
  trieur digest [--since 24h] [--format md|json] [--account hermes]   analyse du bus, lecture seule
  trieur feed [--since 12h] [--limit 150] [--account hermes]          flux JSON pour la tour de contrôle (lecture seule)

Environnement: NATS_URL, NATS_USER, NATS_PASSWORD (sinon $HERMES_HOME/secrets/twalk2/<compte>.env),
TWALK_RULES (rules.yaml, voir rules.example.yaml), TWALK_LLM_URL, TWALK_LLM_KEY_FILE (absent = pas
de LLM), TWALK_LLM_MODEL (mistral-small-2603), TWALK_LOG_LEVEL.
`

const stream = "twalk2"

func main() { os.Exit(realMain()) }

func realMain() int {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Fprint(os.Stderr, usage)
		return 0
	}
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(os.Getenv("TWALK_LOG_LEVEL")))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = runDaemon(ctx, log)
	case "digest":
		err = runDigest(ctx, os.Args[2:])
	case "feed":
		err = runFeed(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("arrêt sur erreur", "err", err)
		return 1
	}
	return 0
}

func secretsDir() string {
	home := os.Getenv("HERMES_HOME")
	if home == "" {
		home = "/home/hermes/hermes-new"
	}
	return filepath.Join(home, "secrets", "twalk2")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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

func connect(account string) (*nats.Conn, jetstream.JetStream, error) {
	if os.Getenv("NATS_USER") == "" {
		loadEnvFile(filepath.Join(secretsDir(), account+".env"))
	}
	nc, err := nats.Connect(envOr("NATS_URL", "nats://127.0.0.1:4250"),
		nats.UserInfo(os.Getenv("NATS_USER"), os.Getenv("NATS_PASSWORD")),
		nats.Name("twalk2-trieur"), nats.MaxReconnects(-1), nats.ReconnectWait(2*time.Second))
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return nc, js, nil
}

// inEnvelope is a CloudEvent read from the bus.
type inEnvelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Time    time.Time       `json:"time"`
	Network string          `json:"network"`
	Data    json.RawMessage `json:"data"`
}

func runDaemon(ctx context.Context, log *slog.Logger) error {
	rules, err := LoadRules(envOr("TWALK_RULES", filepath.Join(secretsDir(), "rules.yaml")))
	if err != nil {
		return fmt.Errorf("règles: %w", err)
	}
	llm := NewLLM(envOr("TWALK_LLM_URL", "https://ai-api.linagora.com/v1"),
		envOr("TWALK_LLM_KEY_FILE", filepath.Join(secretsDir(), "gateway-key")),
		envOr("TWALK_LLM_MODEL", "mistral-small-2603"))
	nc, js, err := connect("trieur")
	if err != nil {
		return err
	}
	defer nc.Drain()
	st, err := js.Stream(ctx, stream)
	if err != nil {
		return err
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable: "trieur", AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubjects: []string{"twalk2.msg.received.>", "twalk2.msg.sent.>"},
		AckWait:        3 * time.Minute, MaxAckPending: 100,
	})
	if err != nil {
		return fmt.Errorf("consommateur: %w", err)
	}
	host, _ := os.Hostname()
	t := newTriager(rules, llm, log)
	t.lookup = func(ctx context.Context, conv event.Conversation) (time.Time, error) {
		return lastSentTime(ctx, st, event.ConvHash(conv.ID))
	}
	t.pub = func(ctx context.Context, subject, id string, tr event.Triage) error {
		return publishTriage(ctx, js, "twalk2://"+host+"/trieur", subject, id, tr)
	}
	iter, err := cons.Messages()
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); iter.Stop() }()
	log.Info("trieur démarré", "llm", llm != nil)
	for {
		msg, err := iter.Next()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				log.Info("trieur arrêté")
				return nil
			}
			log.Warn("lecture du consommateur", "err", err)
			time.Sleep(time.Second)
			continue
		}
		switch err := t.handle(ctx, msg.Data()); {
		case err == nil:
			_ = msg.Ack()
		case errors.Is(err, errPoison):
			log.Warn("message illisible ignoré", "subject", msg.Subject(), "err", err)
			_ = msg.Term()
		default:
			log.Warn("publication échouée, nouvel essai", "err", err)
			_ = msg.NakWithDelay(10 * time.Second)
		}
	}
}

func publishTriage(ctx context.Context, js jetstream.JetStream, source, subject, id string, tr event.Triage) error {
	b, err := json.Marshal(event.Envelope{SpecVersion: "1.0", Type: event.TypeTriageLabeled, Source: source, ID: id,
		Time: time.Now().UTC(), Network: tr.Network, DataContentType: "application/json", Data: tr})
	if err != nil {
		return err
	}
	m := nats.NewMsg(subject)
	m.Data = b
	m.Header.Set("Content-Type", "application/cloudevents+json")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = js.PublishMsg(ctx, m, jetstream.WithMsgID(id))
	return err
}

// lastSentTime returns the time of the last message.sent of a conversation (zero if none).
func lastSentTime(ctx context.Context, st jetstream.Stream, convHash string) (time.Time, error) {
	raw, err := st.GetLastMsgForSubject(ctx, "twalk2.msg.sent.*.*."+convHash)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return raw.Time, nil // stream storage time, close enough to the event time
}
