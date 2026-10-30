package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mmaudet/twalk/internal/event"
)

type NetStat struct {
	Received int `json:"received"`
	Sent     int `json:"sent"`
}

// Item is a labeled (or unlabeled) received message.
type Item struct {
	Time         time.Time `json:"time"`
	Network      string    `json:"network"`
	Conversation string    `json:"conversation"`
	Sender       string    `json:"sender"`
	Excerpt      string    `json:"excerpt"`
	Label        string    `json:"label,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	By           string    `json:"by,omitempty"`
	Replied      bool      `json:"replied"`
}

type GroupStat struct {
	Network  string `json:"network"`
	Name     string `json:"name"`
	Messages int    `json:"messages"`
	Senders  int    `json:"senders"`
}

type Digest struct {
	Since               time.Time           `json:"since"`
	Until               time.Time           `json:"until"`
	Networks            map[string]*NetStat `json:"networks"`
	ActiveConversations int                 `json:"active_conversations"`
	Labels              map[string]int      `json:"labels"`
	Priority            []Item              `json:"priority"` // urgent then reply
	TopGroups           []GroupStat         `json:"top_groups"`
	UnlabeledCount      int                 `json:"unlabeled_count"`
	Unlabeled           []Item              `json:"unlabeled"`
}

// BuildDigest aggregates bus events (message.received/sent, triage.labeled).
func BuildDigest(evs []inEnvelope, since, until time.Time) *Digest {
	d := &Digest{Since: since, Until: until, Networks: map[string]*NetStat{}, Priority: []Item{}, Unlabeled: []Item{}, TopGroups: []GroupStat{},
		Labels: map[string]int{"urgent": 0, "reply": 0, "info": 0, "noise": 0}}
	type recv struct {
		env inEnvelope
		m   event.Message
	}
	var recvs []recv
	var triages []event.Triage
	lastSent := map[string]time.Time{}
	convs, seenTriage := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		var m event.Message
		var t event.Triage
		switch {
		case e.Type == event.TypeTriageLabeled && json.Unmarshal(e.Data, &t) == nil:
			if !seenTriage[t.SourceID] {
				seenTriage[t.SourceID] = true
				triages = append(triages, t)
			}
		case (e.Type == event.TypeMessageReceived || e.Type == event.TypeMessageSent) && json.Unmarshal(e.Data, &m) == nil:
			ns := d.Networks[e.Network]
			if ns == nil {
				ns = &NetStat{}
				d.Networks[e.Network] = ns
			}
			convs[m.Conversation.ID] = true
			if e.Type == event.TypeMessageSent {
				ns.Sent++
				if e.Time.After(lastSent[m.Conversation.ID]) {
					lastSent[m.Conversation.ID] = e.Time
				}
			} else {
				ns.Received++
				recvs = append(recvs, recv{e, m})
			}
		}
	}
	d.ActiveConversations = len(convs)
	item := func(t time.Time, network string, c event.Conversation, s event.Sender, text string) Item {
		return Item{Time: t, Network: network, Conversation: cmp.Or(c.Name, c.ID), Sender: cmp.Or(s.Name, s.ID), Excerpt: truncate(text, 200), Replied: lastSent[c.ID].After(t)}
	}
	for _, t := range triages {
		d.Labels[t.Label]++
		if t.Label == event.LabelUrgent || t.Label == event.LabelReply {
			it := item(t.ReceivedAt, t.Network, t.Conversation, t.Sender, t.Excerpt)
			it.Label, it.Reason, it.By = t.Label, t.Reason, t.By
			d.Priority = append(d.Priority, it)
		}
	}
	slices.SortStableFunc(d.Priority, func(a, b Item) int { // urgent first, then chronological
		return cmp.Or(cmp.Compare(a.Label, b.Label)*-1, a.Time.Compare(b.Time))
	})
	groups, senders := map[string]*GroupStat{}, map[string]map[string]bool{}
	for _, r := range recvs {
		if c := r.m.Conversation; c.Kind != event.KindDM {
			if groups[c.ID] == nil {
				groups[c.ID], senders[c.ID] = &GroupStat{Network: r.env.Network, Name: cmp.Or(c.Name, c.ID)}, map[string]bool{}
			}
			groups[c.ID].Messages++
			senders[c.ID][r.m.Sender.ID] = true
			groups[c.ID].Senders = len(senders[c.ID])
		}
		if !seenTriage[r.env.ID] {
			d.UnlabeledCount++
			d.Unlabeled = append(d.Unlabeled, item(r.env.Time, r.env.Network, r.m.Conversation, r.m.Sender, r.m.Text))
		}
	}
	for _, g := range groups {
		d.TopGroups = append(d.TopGroups, *g)
	}
	slices.SortFunc(d.TopGroups, func(a, b GroupStat) int {
		return cmp.Or(cmp.Compare(b.Messages, a.Messages), strings.Compare(a.Name, b.Name))
	})
	if len(d.TopGroups) > 10 {
		d.TopGroups = d.TopGroups[:10]
	}
	return d
}

func oneLine(s string, n int) string { return strings.ReplaceAll(truncate(s, n), "\n", " ") }

// Markdown renders the digest in French.
func (d *Digest) Markdown() string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	p("# Analyse des messageries\n\nDu %s au %s.\n\n## Volumes\n\n", d.Since.Local().Format("02/01/2006 15:04"), d.Until.Local().Format("02/01/2006 15:04"))
	for _, n := range slices.Sorted(maps.Keys(d.Networks)) {
		s := d.Networks[n]
		p("- %s : %d reçus, %d envoyés\n", n, s.Received, s.Sent)
	}
	if len(d.Networks) == 0 {
		p("- aucun message sur la période\n")
	}
	p("\nConversations actives : %d.\n\n## Étiquettes\n\n", d.ActiveConversations)
	for _, l := range []string{"urgent", "reply", "info", "noise"} {
		p("- %s : %d\n", l, d.Labels[l])
	}
	p("\n## Urgent et à répondre\n\n")
	for _, it := range d.Priority {
		p("- **%s** · %s · %s · %s · « %s » · %s (%s) · %s\n", it.Label, it.Network, it.Conversation, it.Sender, oneLine(it.Excerpt, 200), it.Reason, it.By, map[bool]string{true: "répondu", false: "sans réponse"}[it.Replied])
	}
	if len(d.Priority) == 0 {
		p("Rien d'urgent ni à répondre.\n")
	}
	p("\n## Groupes les plus actifs\n\n")
	for i, g := range d.TopGroups {
		p("%d. %s (%s) : %d messages, %d expéditeurs\n", i+1, g.Name, g.Network, g.Messages, g.Senders)
	}
	if len(d.TopGroups) == 0 {
		p("Aucun message de groupe.\n")
	}
	p("\n## Non encore triés\n\n%d message(s) reçu(s) sans étiquette", d.UnlabeledCount)
	if d.UnlabeledCount == 0 {
		p(".\n")
	} else {
		p(" (le trieur est peut-être en retard) :\n\n")
	}
	for i, it := range d.Unlabeled {
		if i == 20 {
			p("- … et %d autres\n", d.UnlabeledCount-20)
			break
		}
		p("- %s · %s · %s · « %s »\n", it.Network, it.Conversation, it.Sender, oneLine(it.Excerpt, 80))
	}
	return b.String()
}

func runDigest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	since := fs.String("since", "24h", "fenêtre d'analyse (2h, 24h, 168h)")
	format := fs.String("format", "md", "md ou json")
	account := fs.String("account", "hermes", "compte NATS (fichier <compte>.env si NATS_USER est vide)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dur, err := time.ParseDuration(*since)
	if err != nil || dur <= 0 || (*format != "md" && *format != "json") {
		return fmt.Errorf("--since ou --format invalide")
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
	d := BuildDigest(evs, start, time.Now())
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	_, err = fmt.Print(d.Markdown())
	return err
}

// readBus reads msg.> and triage.> from start with an ephemeral ordered consumer, until caught up.
func readBus(ctx context.Context, js jetstream.JetStream, start time.Time) ([]inEnvelope, error) {
	cons, err := js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"twalk2.msg.>", "twalk2.triage.>"},
		DeliverPolicy:  jetstream.DeliverByStartTimePolicy, OptStartTime: &start,
	})
	if err != nil {
		return nil, fmt.Errorf("consommateur ordonné: %w", err)
	}
	iter, err := cons.Messages()
	if err != nil {
		return nil, err
	}
	defer iter.Stop()
	var evs []inEnvelope
	for {
		msg, err := iter.Next(jetstream.NextMaxWait(3 * time.Second))
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
			return evs, nil // nothing (more) to read
		}
		if err != nil {
			return nil, err
		}
		var e inEnvelope
		if json.Unmarshal(msg.Data(), &e) == nil {
			evs = append(evs, e)
		}
		if md, err := msg.Metadata(); err == nil && md.NumPending == 0 {
			return evs, nil
		}
	}
}
