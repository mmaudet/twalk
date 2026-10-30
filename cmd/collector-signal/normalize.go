package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

// Subset of signal-cli's JSON envelope (see the release's json-schemas). Field
// names match the JSON keys case-insensitively. Typing, receipt, story and
// call messages have no field here: they are ignored.
type envelope struct {
	Source, SourceNumber, SourceUUID, SourceName string
	Timestamp                                    int64
	DataMessage                                  *dataMsg
	EditMessage                                  *editMsg
	SyncMessage                                  *struct{ SentMessage *sentMsg }
}

type sentMsg struct {
	dataMsg
	Destination, DestinationNumber, DestinationUUID string
	EditMessage                                     *editMsg
}

type editMsg struct {
	TargetSentTimestamp int64
	DataMessage         *dataMsg
}

type dataMsg struct {
	Timestamp   int64
	Message     string
	GroupInfo   *struct{ GroupID, GroupName string }
	Mentions    []struct{ Number, UUID string }
	Quote       *struct{ ID int64 }
	Attachments []struct {
		ContentType, Filename, Caption string
		IsVoiceNote                    bool
	}
	Sticker  *struct{}
	Reaction *struct {
		Emoji               string
		IsRemove            bool
		TargetSentTimestamp int64
	}
}

// self is the owner's identity (number from the account, uuid when known).
type self struct{ Number, UUID string }

// directory answers from the local caches (listGroups / listContacts).
type directory interface {
	groupName(groupID string) string
	contact(number, uuid string) (name string, known bool)
}

type normalized struct {
	ID       string
	Time     time.Time
	Sent     bool
	Msg      *event.Message
	Reaction *event.Reaction
}

func first(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// normalize converts one envelope; ok is false for everything that is not a
// text/attachment message or a reaction (typing, receipts, calls, stories,
// remote deletions, sync of read state, empty messages...).
func normalize(e *envelope, me self, dir directory) (n normalized, ok bool) {
	d, edit, dest := e.DataMessage, e.EditMessage, ""
	if e.SyncMessage != nil && e.SyncMessage.SentMessage != nil {
		s := e.SyncMessage.SentMessage
		n.Sent = true
		d, edit = &s.dataMsg, s.EditMessage
		dest = first(s.DestinationNumber, s.DestinationUUID, s.Destination)
	}
	if edit != nil {
		d = edit.DataMessage
	}
	if d == nil {
		return n, false
	}
	author := first(e.SourceNumber, e.SourceUUID, e.Source)
	if author == "" {
		return n, false
	}
	// Conversation: a group, or the other party of a direct chat.
	conv := event.Conversation{Kind: event.KindGroup}
	var raw string
	if g := d.GroupInfo; g != nil && g.GroupID != "" {
		raw = "group:" + g.GroupID
		conv.Name = first(g.GroupName, dir.groupName(g.GroupID))
	} else {
		raw = first(dest, author)
		if !n.Sent {
			raw = author
		}
		conv.Kind = event.KindDM
		conv.Name, _ = dir.contact(raw, raw)
		if !n.Sent {
			conv.Name = first(conv.Name, e.SourceName)
		}
	}
	if raw == "" {
		return n, false
	}
	conv.ID = event.Signal + ":" + raw

	name, known := dir.contact(e.SourceNumber, e.SourceUUID)
	sender := event.Sender{ID: event.Signal + ":" + author, Name: first(e.SourceName, name), Known: known || n.Sent}

	tsv := d.Timestamp
	if tsv == 0 {
		tsv = e.Timestamp
	}
	ts := strconv.FormatInt(tsv, 10)
	n.Time = time.UnixMilli(tsv)
	if r := d.Reaction; r != nil {
		n.ID = eventID(raw, ts, author)
		n.Reaction = &event.Reaction{Conversation: conv, Sender: sender,
			TargetID: strconv.FormatInt(r.TargetSentTimestamp, 10), FromOwner: n.Sent}
		if !r.IsRemove {
			n.Reaction.Emoji = r.Emoji
		}
		return n, true
	}
	msg := event.Message{Conversation: conv, Sender: sender, Text: d.Message, Edited: edit != nil}
	for _, a := range d.Attachments {
		msg.Attachments = append(msg.Attachments, event.Attachment{Kind: kindOf(a.ContentType, a.IsVoiceNote),
			Caption: a.Caption, MimeType: a.ContentType, FileName: a.Filename})
	}
	if d.Sticker != nil {
		msg.Attachments = append(msg.Attachments, event.Attachment{Kind: "sticker"})
	}
	if msg.Text == "" && len(msg.Attachments) == 0 {
		return n, false // group update, expiration timer, remote delete...
	}
	if d.Quote != nil && d.Quote.ID != 0 {
		msg.ReplyTo = strconv.FormatInt(d.Quote.ID, 10)
	}
	for _, m := range d.Mentions {
		msg.MentionsOwner = msg.MentionsOwner || (m.Number != "" && m.Number == me.Number) || (m.UUID != "" && m.UUID == me.UUID)
	}
	n.ID = eventID(raw, ts, author)
	if edit != nil { // the id stays that of the original message, plus the edit time
		n.ID = eventID(raw, strconv.FormatInt(edit.TargetSentTimestamp, 10), author) + ":edit:" + ts
	}
	n.Msg = &msg
	return n, true
}

func eventID(rawConv, ts, author string) string {
	return fmt.Sprintf("%s:%s:%s:%s", event.Signal, rawConv, ts, author)
}

func kindOf(contentType string, voice bool) string {
	ct := strings.ToLower(contentType)
	switch {
	case voice:
		return "voice"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "text/x-vcard"), strings.HasPrefix(ct, "text/vcard"):
		return "contact"
	case strings.HasPrefix(ct, "application/"), strings.HasPrefix(ct, "text/"):
		return "document"
	}
	return "other"
}
