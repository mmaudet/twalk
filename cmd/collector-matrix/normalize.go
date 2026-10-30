package main

import (
	"slices"
	"strconv"
	"strings"
	"time"

	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/mmaudet/twalk/internal/event"
)

// room is what we know about a room: enough to name the conversation and to
// decide dm/group and "bridged". It is filled from /state and kept current
// from the m.room.name and m.room.member events of the sync stream.
type room struct {
	Name    string               // m.room.name, empty if unset
	Members map[id.UserID]string // joined members -> displayname (may be empty)
	Bridged bool                 // m.bridge / uk.half-shot.bridge state present
	Direct  bool                 // listed in the owner's m.direct
}

func newRoom() *room { return &room{Members: map[id.UserID]string{}} }

func (r *room) joined() int { return len(r.Members) }

// apply folds one state event into the room.
func (r *room) apply(e *mevent.Event) {
	switch e.Type {
	case mevent.StateRoomName:
		if c, ok := e.Content.Parsed.(*mevent.RoomNameEventContent); ok {
			r.Name = c.Name
		}
	case mevent.StateMember:
		c, ok := e.Content.Parsed.(*mevent.MemberEventContent)
		if !ok || e.StateKey == nil {
			return
		}
		u := id.UserID(*e.StateKey)
		if c.Membership == mevent.MembershipJoin {
			r.Members[u] = c.Displayname
		} else {
			delete(r.Members, u)
		}
	case mevent.StateBridge, mevent.StateHalfShotBridge:
		r.Bridged = true
	}
}

// owner is the account the collector follows.
type owner struct {
	ID      id.UserID
	Name    string             // global displayname
	Directs map[id.UserID]bool // users the owner shares a DM with (m.direct)
}

// isDM: the room is in m.direct, or has exactly two joined members.
func isDM(r *room) bool { return r.Direct || r.joined() == 2 }

// convName: the room name, else the other members' names (up to three).
func convName(r *room, me id.UserID) string {
	if r.Name != "" {
		return r.Name
	}
	var names []string
	for u, n := range r.Members {
		if u == me {
			continue
		}
		if n == "" {
			n = string(u)
		}
		names = append(names, n)
	}
	slices.Sort(names)
	if len(names) > 3 {
		names = names[:3]
	}
	return strings.Join(names, ", ")
}

type normalized struct {
	ID       string
	Time     time.Time
	Sent     bool
	Msg      *event.Message
	Reaction *event.Reaction
}

func conversation(e *mevent.Event, r *room, me id.UserID) event.Conversation {
	kind := event.KindGroup
	if isDM(r) {
		kind = event.KindDM
	}
	return event.Conversation{ID: "matrix:" + string(e.RoomID), Kind: kind, Name: convName(r, me)}
}

func sender(e *mevent.Event, r *room, o *owner) event.Sender {
	name := r.Members[e.Sender]
	if e.Sender == o.ID && name == "" {
		name = o.Name
	}
	return event.Sender{ID: "matrix:" + string(e.Sender), Name: name, Known: e.Sender == o.ID || o.Directs[e.Sender]}
}

// normalize converts a decrypted m.room.message or m.reaction event. ok is
// false for events that must be ignored: bridged rooms (the bridged network
// has its own collector), empty messages, malformed events.
func normalize(e *mevent.Event, r *room, o *owner) (n normalized, ok bool) {
	if r.Bridged || e.ID == "" || e.RoomID == "" || e.Sender == "" {
		return n, false
	}
	t := time.UnixMilli(e.Timestamp)
	conv := conversation(e, r, o.ID)
	base := "matrix:" + string(e.RoomID) + ":"

	if e.Type == mevent.EventReaction {
		c, isR := e.Content.Parsed.(*mevent.ReactionEventContent)
		if !isR || c.RelatesTo.Type != mevent.RelAnnotation || c.RelatesTo.EventID == "" {
			return n, false
		}
		return normalized{ID: base + string(e.ID), Time: t, Reaction: &event.Reaction{
			Conversation: conv, Sender: sender(e, r, o), TargetID: string(c.RelatesTo.EventID),
			Emoji: c.RelatesTo.Key, FromOwner: e.Sender == o.ID,
		}}, true
	}

	c, isM := e.Content.Parsed.(*mevent.MessageEventContent)
	if !isM {
		return n, false
	}
	eid := base + string(e.ID)
	edited := false
	if rel := c.RelatesTo; rel != nil && rel.Type == mevent.RelReplace && rel.EventID != "" {
		// An edit event: the new text is in m.new_content.
		edited = true
		eid = base + string(rel.EventID) + ":edit:" + strconv.FormatInt(e.Timestamp, 10)
		if c.NewContent != nil {
			c = &mevent.MessageEventContent{MsgType: c.NewContent.MsgType, Body: c.NewContent.Body,
				FileName: c.NewContent.FileName, Info: c.NewContent.Info, GeoURI: c.NewContent.GeoURI,
				Mentions: c.NewContent.Mentions, RelatesTo: c.RelatesTo.Copy()}
		}
	}
	reply := ""
	if c.RelatesTo != nil && c.RelatesTo.Type != mevent.RelReplace {
		reply = string(c.RelatesTo.GetReplyTo())
	}
	body := c.Body
	if reply != "" {
		body = mevent.TrimReplyFallbackText(body)
	}

	text, atts := body, []event.Attachment(nil)
	switch c.MsgType {
	case mevent.MsgImage, mevent.MsgVideo, mevent.MsgAudio, mevent.MsgFile, mevent.MsgLocation:
		text, atts = "", []event.Attachment{attachment(c, body)}
		// Since Matrix 1.10 a media body differs from filename when it is a caption.
		if c.MsgType != mevent.MsgLocation && c.FileName != "" && body != c.FileName {
			text, atts[0].Caption = body, body
		}
	}
	if text == "" && len(atts) == 0 {
		return n, false
	}
	msg := &event.Message{Conversation: conv, Sender: sender(e, r, o), Text: text, ReplyTo: reply,
		Edited: edited, Attachments: atts, MentionsOwner: mentions(c, body, r, o)}
	return normalized{ID: eid, Time: t, Sent: e.Sender == o.ID, Msg: msg}, true
}

func attachment(c *mevent.MessageEventContent, body string) event.Attachment {
	a := event.Attachment{FileName: c.FileName}
	if c.Info != nil {
		a.MimeType = c.Info.MimeType
	}
	switch c.MsgType {
	case mevent.MsgImage:
		a.Kind = "image"
	case mevent.MsgVideo:
		a.Kind = "video"
	case mevent.MsgAudio:
		a.Kind = "audio"
		if c.MSC3245Voice != nil {
			a.Kind = "voice"
		}
	case mevent.MsgLocation:
		a.Kind, a.Caption = "location", body
	default:
		a.Kind = "document"
	}
	if a.FileName == "" && c.MsgType != mevent.MsgLocation {
		a.FileName = body // legacy: the body of a media message is its file name
	}
	return a
}

// mentions: m.mentions lists the owner, or the text contains the user id or
// (case-insensitively) a displayname of the owner.
func mentions(c *mevent.MessageEventContent, body string, r *room, o *owner) bool {
	if c.Mentions.Has(o.ID) {
		return true
	}
	low := strings.ToLower(body)
	if strings.Contains(low, strings.ToLower(string(o.ID))) {
		return true
	}
	for _, n := range []string{o.Name, r.Members[o.ID]} {
		if len(n) >= 3 && strings.Contains(low, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
