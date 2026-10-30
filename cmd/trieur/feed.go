package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mmaudet/twalk/internal/event"
)

// Feed is the live view for the control-tower pane: collector states and the
// latest messages (both directions) with their triage label.
type Feed struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Since       time.Time           `json:"since"`
	Collectors  []CollectorState    `json:"collectors"`
	Labels      map[string]int      `json:"labels"`
	Networks    map[string]*NetStat `json:"networks"`
	Messages    []FeedMessage       `json:"messages"`
	Total       int                 `json:"total"`
}

type CollectorState struct {
	Network string    `json:"network"`
	State   string    `json:"state"`
	Detail  string    `json:"detail,omitempty"`
	Account string    `json:"account,omitempty"`
	Time    time.Time `json:"time"`
}

type FeedMessage struct {
	ID            string    `json:"id"`
	Time          time.Time `json:"time"`
	Network       string    `json:"network"`
	Sent          bool      `json:"sent"`
	Kind          string    `json:"kind"`
	Conversation  string    `json:"conversation"`
	Sender        string    `json:"sender"`
	Known         bool      `json:"known"`
	Text          string    `json:"text"`
	Attachments   []string  `json:"attachments,omitempty"`
	MentionsOwner bool      `json:"mentions_owner"`
	Label         string    `json:"label,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Replied       bool      `json:"replied"`
}

var feedNetworks = []string{event.WhatsApp, event.Telegram, event.Signal, event.Mattermost, event.Matrix, event.SMS, event.LinkedIn}

// BuildFeed joins messages with their triage, newest first, capped at limit.
func BuildFeed(evs []inEnvelope, since time.Time, limit int) *Feed {
	f := &Feed{GeneratedAt: time.Now().UTC(), Since: since.UTC(), Networks: map[string]*NetStat{},
		Labels: map[string]int{"urgent": 0, "reply": 0, "info": 0, "noise": 0}, Messages: []FeedMessage{}}
	tri := map[string]event.Triage{}
	lastSent, convOf := map[string]time.Time{}, map[string]string{}
	var msgs []FeedMessage
	for _, e := range evs {
		var m event.Message
		var t event.Triage
		switch {
		case e.Type == event.TypeTriageLabeled && json.Unmarshal(e.Data, &t) == nil:
			if _, dup := tri[t.SourceID]; !dup {
				tri[t.SourceID] = t
				f.Labels[t.Label]++
			}
		case (e.Type == event.TypeMessageReceived || e.Type == event.TypeMessageSent) && json.Unmarshal(e.Data, &m) == nil:
			sent := e.Type == event.TypeMessageSent
			ns := f.Networks[e.Network]
			if ns == nil {
				ns = &NetStat{}
				f.Networks[e.Network] = ns
			}
			if sent {
				ns.Sent++
				if e.Time.After(lastSent[m.Conversation.ID]) {
					lastSent[m.Conversation.ID] = e.Time
				}
			} else {
				ns.Received++
			}
			fm := FeedMessage{ID: e.ID, Time: e.Time, Network: e.Network, Sent: sent, Kind: m.Conversation.Kind,
				Conversation: cmp.Or(m.Conversation.Name, m.Conversation.ID), Sender: cmp.Or(m.Sender.Name, m.Sender.ID),
				Known: m.Sender.Known, Text: truncate(m.Text, 400), MentionsOwner: m.MentionsOwner}
			for _, a := range m.Attachments {
				fm.Attachments = append(fm.Attachments, a.Kind)
			}
			msgs = append(msgs, fm)
			convOf[e.ID] = m.Conversation.ID
		}
	}
	for i := range msgs {
		if t, ok := tri[msgs[i].ID]; ok {
			msgs[i].Label, msgs[i].Reason = t.Label, t.Reason
		}
		if !msgs[i].Sent {
			msgs[i].Replied = lastSent[convOf[msgs[i].ID]].After(msgs[i].Time)
		}
	}
	slices.SortStableFunc(msgs, func(a, b FeedMessage) int { return b.Time.Compare(a.Time) })
	f.Total = len(msgs)
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[:limit]
	}
	f.Messages = msgs
	return f
}

// collectorStates reads the last collector.status of each network (direct get).
func collectorStates(ctx context.Context, js jetstream.JetStream) []CollectorState {
	out := []CollectorState{}
	st, err := js.Stream(ctx, stream)
	if err != nil {
		return out
	}
	for _, n := range feedNetworks {
		raw, err := st.GetLastMsgForSubject(ctx, event.StatusSubject(n))
		if err != nil {
			continue // collector never started
		}
		var e struct {
			Time time.Time    `json:"time"`
			Data event.Status `json:"data"`
		}
		if json.Unmarshal(raw.Data, &e) == nil {
			out = append(out, CollectorState{Network: n, State: e.Data.State, Detail: e.Data.Detail, Account: e.Data.Account, Time: e.Time})
		}
	}
	return out
}

func runFeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("feed", flag.ContinueOnError)
	since := fs.String("since", "12h", "fenêtre (2h, 12h, 24h)")
	limit := fs.Int("limit", 150, "nombre maximal de messages")
	account := fs.String("account", "hermes", "compte NATS")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dur, err := time.ParseDuration(*since)
	if err != nil || dur <= 0 {
		return fmt.Errorf("--since invalide")
	}
	nc, js, err := connect(*account)
	if err != nil {
		return err
	}
	defer nc.Close()
	start := time.Now().Add(-dur)
	evs, err := readBus(ctx, js, start)
	if err != nil {
		return err
	}
	f := BuildFeed(evs, start, *limit)
	f.Collectors = collectorStates(ctx, js)
	return json.NewEncoder(os.Stdout).Encode(f)
}
