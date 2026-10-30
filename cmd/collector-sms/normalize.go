package main

import (
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/mmaudet/twalk/internal/event"
)

// maxReplayAge: Google Messages replays recent updates on connection; only
// messages stamped after (start - maxReplayAge) are published.
const maxReplayAge = 2 * time.Minute

// seenSet remembers the event ids already published (only the worker uses it);
// it is simply reset when it grows past n, JetStream deduplicates the rest.
type seenSet struct {
	m map[string]struct{}
	n int
}

func newSeenSet(n int) *seenSet { return &seenSet{m: map[string]struct{}{}, n: n} }

func (s *seenSet) has(id string) bool { _, ok := s.m[id]; return ok }

func (s *seenSet) add(id string) {
	if len(s.m) >= s.n {
		s.m = map[string]struct{}{}
	}
	s.m[id] = struct{}{}
}

// converter turns libgm messages into Twalk events. defaultCC (e.g. "33")
// turns national numbers ("06 12…") into E.164; empty means "leave as is".
type converter struct {
	cutoff    time.Time
	defaultCC string
	seen      *seenSet
}

// result is one message ready for event.Publisher.Message.
type result struct {
	Sent bool
	ID   string // event id (also Nats-Msg-Id)
	Time time.Time
	Msg  event.Message
}

// complete reports whether the status is a final-enough one: outgoing 1/2/11
// (sent, delivered, displayed) and incoming 100/108/109. Every other status
// (draft, sending, failed, MMS still downloading, tombstones, deleted…) is
// skipped; a later update with a complete status is published instead.
func complete(s gmproto.MessageStatusType) (sent, ok bool) {
	switch s {
	case gmproto.MessageStatusType_OUTGOING_COMPLETE, gmproto.MessageStatusType_OUTGOING_DELIVERED, gmproto.MessageStatusType_OUTGOING_DISPLAYED:
		return true, true
	case gmproto.MessageStatusType_INCOMING_COMPLETE, gmproto.MessageStatusType_INCOMING_DELIVERED, gmproto.MessageStatusType_INCOMING_DISPLAYED:
		return false, true
	}
	return false, false
}

func eventID(m *gmproto.Message) string {
	return event.SMS + ":" + m.GetConversationID() + ":" + m.GetMessageID()
}

// wanted is the cheap filter run before any conversation lookup: complete
// status, recent enough, not yet published.
func (c *converter) wanted(m *gmproto.Message) bool {
	if m.GetMessageID() == "" || m.GetConversationID() == "" {
		return false
	}
	if _, ok := complete(m.GetMessageStatus().GetStatus()); !ok {
		return false
	}
	return !time.UnixMicro(m.GetTimestamp()).Before(c.cutoff) && !c.seen.has(eventID(m))
}

// convert builds the event; ok is false when there is nothing to publish
// (empty message, spam/deleted/blocked conversation).
func (c *converter) convert(m *gmproto.Message, conv *gmproto.Conversation) (r result, ok bool) {
	sent, ok := complete(m.GetMessageStatus().GetStatus())
	switch conv.GetStatus() {
	case gmproto.ConversationStatus_SPAM_FOLDER, gmproto.ConversationStatus_DELETED,
		gmproto.ConversationStatus_TRASH_FOLDER, gmproto.ConversationStatus_BLOCKED_FOLDER:
		ok = false
	}
	if !ok {
		return r, false
	}
	var text []string
	if s := m.GetSubject(); s != "" {
		text = append(text, s)
	}
	var atts []event.Attachment
	for _, part := range m.GetMessageInfo() {
		switch d := part.GetData().(type) {
		case *gmproto.MessageInfo_MessageContent:
			if t := d.MessageContent.GetContent(); t != "" {
				text = append(text, t)
			}
		case *gmproto.MessageInfo_MediaContent:
			mc := d.MediaContent
			atts = append(atts, event.Attachment{Kind: attKind(mc.GetMimeType(), mc.GetFormat()), MimeType: mc.GetMimeType(), FileName: mc.GetMediaName()})
		}
	}
	body := strings.Join(text, "\n")
	if body == "" && len(atts) == 0 {
		return r, false
	}
	others, self := 0, (*gmproto.Participant)(nil)
	var from *gmproto.Participant
	for _, p := range conv.GetParticipants() {
		switch {
		case p.GetIsMe():
			self = p
		case p.GetIsVisible():
			others++
		}
		if p.GetID().GetParticipantID() == m.GetParticipantID() && !p.GetIsMe() {
			from = p
		}
	}
	kind := event.KindDM
	if conv.GetIsGroupChat() || others > 1 {
		kind = event.KindGroup
	}
	if sent {
		from = self
	} else if from == nil {
		from = m.GetSenderParticipant()
	}
	r = result{Sent: sent, ID: eventID(m), Time: time.UnixMicro(m.GetTimestamp()), Msg: event.Message{
		Conversation: event.Conversation{ID: event.SMS + ":" + m.GetConversationID(), Kind: kind, Name: conv.GetName()},
		Sender:       c.sender(from, m.GetParticipantID(), sent),
		Text:         body, Attachments: atts, ReplyTo: m.GetReplyMessage().GetMessageID(),
	}}
	return r, true
}

// sender: id is the E.164 number when known, else the Google participant id.
// Known = the participant is linked to a phone contact (ContactID set).
func (c *converter) sender(p *gmproto.Participant, participantID string, sent bool) event.Sender {
	id := c.e164(p.GetID().GetNumber())
	if id == "" {
		id = p.GetID().GetParticipantID()
	}
	if id == "" {
		id = participantID
	}
	if id == "" && sent {
		id = "me"
	}
	name := p.GetFullName()
	if name == "" {
		name = p.GetFirstName()
	}
	return event.Sender{ID: event.SMS + ":" + id, Name: name, Known: sent || p.GetContactID() != ""}
}

// e164 returns "+<digits>" for an international number, converts a national
// one ("0612…") with defaultCC, and returns "" for anything else (short
// codes, alphanumeric senders, e-mail addresses).
func (c *converter) e164(raw string) string {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(" -.()/\u00a0", r) {
			return -1
		}
		return r
	}, raw)
	if strings.HasPrefix(s, "00") {
		s = "+" + s[2:]
	}
	digits := func(t string) bool {
		for _, r := range t {
			if r < '0' || r > '9' {
				return false
			}
		}
		return t != ""
	}
	switch {
	case strings.HasPrefix(s, "+") && digits(s[1:]) && len(s) >= 8 && len(s) <= 16:
		return s
	case c.defaultCC != "" && strings.HasPrefix(s, "0") && digits(s) && len(s) >= 9 && len(s) <= 12:
		return "+" + c.defaultCC + s[1:]
	}
	return ""
}

func attKind(mime string, f gmproto.MediaFormats) string {
	switch {
	case strings.HasPrefix(mime, "image/"), f >= gmproto.MediaFormats_IMAGE_JPEG && f <= gmproto.MediaFormats_IMAGE_UNSPECIFIED:
		return "image"
	case strings.HasPrefix(mime, "video/"), f >= gmproto.MediaFormats_VIDEO_MP4 && f <= gmproto.MediaFormats_VIDEO_UNSPECIFIED:
		return "video"
	case strings.HasPrefix(mime, "audio/"), f >= gmproto.MediaFormats_AUDIO_AAC && f <= gmproto.MediaFormats_AUDIO_UNSPECIFIED:
		return "audio"
	case mime == "text/vcard", mime == "text/x-vcard", f == gmproto.MediaFormats_TEXT_VCARD:
		return "contact"
	case mime != "", f != gmproto.MediaFormats_UNSPECIFIED_TYPE:
		return "document"
	}
	return "other"
}

// flags is the connection picture; status() derives the published state.
type flags struct {
	ready, listenErr, phoneDown bool
	inactive                    string // non-empty: Messages Web session lost (detail)
}

func (f flags) status() event.Status {
	dis := func(detail string) event.Status { return event.Status{State: event.StateDisconnected, Detail: detail} }
	switch {
	case f.inactive != "":
		return dis(f.inactive)
	case f.listenErr:
		return dis("écoute Google Messages interrompue, nouvelle tentative")
	case f.phoneDown:
		return dis("téléphone injoignable (Google Messages ne répond pas)")
	case !f.ready:
		return dis("connexion en cours")
	}
	return event.Status{State: event.StateConnected}
}
