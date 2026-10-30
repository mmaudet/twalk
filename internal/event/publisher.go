package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publisher publishes Twalk v2 events to JetStream. The Nats-Msg-Id header is
// the event id, so a redelivered network message is stored once.
type Publisher struct {
	network string
	source  string
	nc      *nats.Conn
	js      jetstream.JetStream
	ex      *Exclusions
	log     *slog.Logger
}

// Connect uses NATS_URL, NATS_USER, NATS_PASSWORD and TWALK_EXCLUDE.
func Connect(network string, log *slog.Logger) (*Publisher, error) {
	if !networks[network] {
		return nil, fmt.Errorf("unknown network %q", network)
	}
	ex, err := LoadExclusions(os.Getenv("TWALK_EXCLUDE"))
	if err != nil {
		return nil, err
	}
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://127.0.0.1:4250"
	}
	nc, err := nats.Connect(url,
		nats.UserInfo(os.Getenv("NATS_USER"), os.Getenv("NATS_PASSWORD")),
		nats.Name("twalk2-collector-"+network),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	host, _ := os.Hostname()
	return &Publisher{
		network: network,
		source:  fmt.Sprintf("twalk2://%s/collector/%s", host, network),
		nc:      nc, js: js, ex: ex, log: log,
	}, nil
}

func (p *Publisher) Close() {
	if p != nil && p.nc != nil {
		_ = p.nc.Drain()
	}
}

func (p *Publisher) publish(ctx context.Context, subject, typ, id string, t time.Time, data any) error {
	if t.IsZero() {
		t = time.Now()
	}
	env := Envelope{
		SpecVersion: "1.0", Type: typ, Source: p.source, ID: id, Time: t.UTC(),
		Network: p.network, DataContentType: "application/json", Data: data,
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	msg := nats.NewMsg(subject)
	msg.Data = b
	// CloudEvents NATS binding: structured mode, content type in a header.
	msg.Header.Set("Content-Type", "application/cloudevents+json")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(id))
	return err
}

// Message publishes message.received (sent=false) or message.sent (sent=true).
// The id must be stable for the network message, e.g. "whatsapp:<chat>:<msgid>".
func (p *Publisher) Message(ctx context.Context, sent bool, id string, t time.Time, m Message) error {
	if err := validate(p.network, &m.Conversation); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("empty event id")
	}
	if p.ex.Excluded(p.network, m.Conversation, m.Sender) {
		return nil
	}
	typ := TypeMessageReceived
	if sent {
		typ = TypeMessageSent
	}
	return p.publish(ctx, MessageSubject(sent, p.network, m.Conversation.Kind, m.Conversation.ID), typ, id, t, m)
}

// Reaction publishes reaction.added.
func (p *Publisher) Reaction(ctx context.Context, id string, t time.Time, r Reaction) error {
	if err := validate(p.network, &r.Conversation); err != nil {
		return err
	}
	if p.ex.Excluded(p.network, r.Conversation, r.Sender) {
		return nil
	}
	return p.publish(ctx, ReactionSubject(p.network, r.Conversation.ID), TypeReactionAdded, id, t, r)
}

// Status publishes collector.status. Its id is unique per publication.
func (p *Publisher) Status(ctx context.Context, s Status) error {
	now := time.Now()
	id := fmt.Sprintf("%s:status:%d", p.network, now.UnixNano())
	return p.publish(ctx, StatusSubject(p.network), TypeCollectorStatus, id, now, s)
}

// Heartbeat republishes the current status every interval until ctx ends.
func (p *Publisher) Heartbeat(ctx context.Context, interval time.Duration, current func() Status) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Status(ctx, current()); err != nil {
				p.log.Warn("status heartbeat", "err", err)
			}
		}
	}
}
