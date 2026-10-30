package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

var errPoison = errors.New("événement illisible")

type remembered struct{ who, text string }

// triager holds the in-memory conversation state (last owner message, last 3 messages).
type triager struct {
	rules *Rules
	llm   *LLM
	log   *slog.Logger

	lookup func(ctx context.Context, c event.Conversation) (time.Time, error) // last message.sent time
	pub    func(ctx context.Context, subject, id string, tr event.Triage) error

	lastSent map[string]time.Time // conversation id -> last owner message
	sentIDs  map[string]bool      // conversation id + "\x00" + network message id
	recent   map[string][]remembered
}

func newTriager(r *Rules, l *LLM, log *slog.Logger) *triager {
	return &triager{rules: r, llm: l, log: log, lastSent: map[string]time.Time{}, sentIDs: map[string]bool{}, recent: map[string][]remembered{}}
}

// netMsgID extracts the network message id from an event id "<net>:<conv>:<msg>[:edit:<ts>]".
func netMsgID(eventID string) string {
	if i := strings.Index(eventID, ":edit:"); i >= 0 {
		eventID = eventID[:i]
	}
	return eventID[strings.LastIndex(eventID, ":")+1:]
}

func (t *triager) remember(convID, who, text string) {
	t.recent[convID] = append(t.recent[convID], remembered{who, truncate(text, 500)})
	if r := t.recent[convID]; len(r) > 3 {
		t.recent[convID] = r[1:]
	}
}

// handle processes one message.received or message.sent. A nil error means ack.
func (t *triager) handle(ctx context.Context, data []byte) error {
	var env inEnvelope
	var m event.Message
	if json.Unmarshal(data, &env) != nil || json.Unmarshal(env.Data, &m) != nil || env.ID == "" || env.Network == "" {
		return errPoison
	}
	conv := m.Conversation.ID
	switch env.Type {
	case event.TypeMessageSent:
		if env.Time.After(t.lastSent[conv]) {
			t.lastSent[conv] = env.Time
		}
		if len(t.sentIDs) > 100000 {
			t.sentIDs = map[string]bool{}
		}
		t.sentIDs[conv+"\x00"+netMsgID(env.ID)] = true
		t.remember(conv, "moi", m.Text)
		return nil
	case event.TypeMessageReceived:
	default:
		return nil
	}
	if _, seen := t.lastSent[conv]; !seen && t.lookup != nil { // catch-up: has the owner answered since?
		ts, err := t.lookup(ctx, m.Conversation)
		if err != nil {
			t.log.Warn("recherche du dernier envoi", "err", err)
		} else {
			t.lastSent[conv] = ts
		}
	}
	d := t.rules.Classify(m, m.ReplyTo != "" && t.sentIDs[conv+"\x00"+m.ReplyTo])
	if d.Candidate && t.llm != nil && strings.TrimSpace(m.Text) != "" {
		if label, reason, err := t.llm.Classify(ctx, buildPrompt(env.Network, m, t.recent[conv])); err == nil {
			d = Decision{Label: label, Reason: reason, By: "llm:" + t.llm.Model}
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var le *LLMError
			if errors.As(err, &le) {
				t.log.Warn("llm indisponible", "kind", le.Kind, "status", le.Status)
			}
			d.By += " (llm indisponible)"
		}
	}
	if d.Label == event.LabelReply && t.lastSent[conv].After(env.Time) {
		d.Label, d.Reason = event.LabelInfo, "déjà répondu"
	}
	tr := event.Triage{SourceID: env.ID, Network: env.Network, Conversation: m.Conversation, Sender: m.Sender,
		Label: d.Label, Reason: d.Reason, By: d.By, Excerpt: truncate(m.Text, 200), MentionsOwner: m.MentionsOwner, ReceivedAt: env.Time}
	if err := t.pub(ctx, event.TriageSubject(d.Label, tr.Network, conv), "triage:"+env.ID, tr); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	t.log.Info("étiqueté", "label", d.Label, "by", strings.SplitN(d.By, " ", 2)[0], "network", tr.Network)
	t.remember(conv, m.Sender.Name, m.Text)
	return nil
}

func buildPrompt(network string, m event.Message, prev []remembered) string {
	known := "inconnu"
	if m.Sender.Known {
		known = "contact connu"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Réseau : %s\nConversation : %s « %s »\nExpéditeur : %s (%s)\n", network, m.Conversation.Kind, m.Conversation.Name, m.Sender.Name, known)
	if len(prev) > 0 {
		b.WriteString("Messages précédents :\n")
		for _, p := range prev {
			fmt.Fprintf(&b, "- %s : %s\n", p.who, p.text)
		}
	}
	fmt.Fprintf(&b, "Message à classer :\n<<<\n%s\n>>>", truncate(m.Text, 1500))
	return b.String()
}
