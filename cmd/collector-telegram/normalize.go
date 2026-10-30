package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/mmaudet/twalk/internal/event"
)

// normalized is one publishable message: the event id, its time, whether the
// owner wrote it, and the payload.
type normalized struct {
	ID   string
	Time time.Time
	Sent bool
	Msg  event.Message
}

// normalize converts a Telegram message into a Twalk event. ok is false for
// messages that must be ignored: service messages, empty messages, messages
// without text nor media, and edits that are not visible edits (no edit_date,
// or edit_hide set: poll results, media-unread flips, etc.).
// self is the owner's own account; e carries the users/chats/channels sent
// along with the update (an entity may be missing: names are then empty).
func normalize(mc tg.MessageClass, e tg.Entities, self *tg.User, edited bool) (n normalized, ok bool) {
	m, isMsg := mc.(*tg.Message)
	if !isMsg || m.PeerID == nil { // MessageService, MessageEmpty
		return n, false
	}
	conv, rawConv, ok := conversationOf(m, e)
	if !ok {
		return n, false
	}
	atts := attachmentsOf(m)
	if m.Message == "" && len(atts) == 0 {
		return n, false
	}
	if m.Message != "" {
		for i := range atts {
			atts[i].Caption = m.Message
		}
	}

	id := "telegram:" + rawConv + ":" + strconv.Itoa(m.ID)
	t := time.Unix(int64(m.Date), 0)
	if edited {
		d, has := m.GetEditDate()
		if !has || d == 0 || m.EditHide {
			return n, false
		}
		id += ":edit:" + strconv.Itoa(d)
		t = time.Unix(int64(d), 0)
	}

	msg := event.Message{
		Conversation:  conv,
		Sender:        senderOf(m, e, self),
		Text:          m.Message,
		ReplyTo:       replyToOf(m),
		MentionsOwner: m.Mentioned,
		Edited:        edited,
		Attachments:   atts,
	}
	_, msg.Forwarded = m.GetFwdFrom()
	return normalized{ID: id, Time: t, Sent: m.Out, Msg: msg}, true
}

// conversationOf maps the message peer to a conversation and returns its raw
// id ("user:42", "chat:7", "channel:9") used in event ids.
func conversationOf(m *tg.Message, e tg.Entities) (event.Conversation, string, bool) {
	switch p := m.PeerID.(type) {
	case *tg.PeerUser:
		raw := "user:" + strconv.FormatInt(p.UserID, 10)
		return event.Conversation{ID: "telegram:" + raw, Kind: event.KindDM, Name: userName(e.Users[p.UserID])}, raw, true
	case *tg.PeerChat:
		raw := "chat:" + strconv.FormatInt(p.ChatID, 10)
		name := ""
		if c := e.Chats[p.ChatID]; c != nil {
			name = c.Title
		}
		return event.Conversation{ID: "telegram:" + raw, Kind: event.KindGroup, Name: name}, raw, true
	case *tg.PeerChannel:
		raw := "channel:" + strconv.FormatInt(p.ChannelID, 10)
		kind, name := event.KindChannel, ""
		if c := e.Channels[p.ChannelID]; c != nil {
			name = c.Title
			if c.Megagroup {
				kind = event.KindGroup
			}
		} else if !m.Post {
			// Entity missing: a post is a broadcast channel; anything else
			// (a member speaking) can only be a supergroup.
			kind = event.KindGroup
		}
		return event.Conversation{ID: "telegram:" + raw, Kind: kind, Name: name}, raw, true
	}
	return event.Conversation{}, "", false
}

// senderOf: the owner for outgoing messages, else FromID, else (incoming DM
// without FromID) the peer itself. Users are "telegram:<id>"; a chat or a
// channel speaking (channel post, anonymous admin) is "telegram:channel:<id>"
// or "telegram:chat:<id>".
func senderOf(m *tg.Message, e tg.Entities, self *tg.User) event.Sender {
	if m.Out && self != nil {
		return event.Sender{ID: userID(self.ID), Name: userName(self), Known: true}
	}
	from, has := m.GetFromID()
	if !has {
		from = m.PeerID
	}
	switch p := from.(type) {
	case *tg.PeerUser:
		u := e.Users[p.UserID]
		return event.Sender{ID: userID(p.UserID), Name: userName(u), Known: u != nil && u.Contact}
	case *tg.PeerChat:
		s := event.Sender{ID: "telegram:chat:" + strconv.FormatInt(p.ChatID, 10)}
		if c := e.Chats[p.ChatID]; c != nil {
			s.Name = c.Title
		}
		return s
	case *tg.PeerChannel:
		s := event.Sender{ID: "telegram:channel:" + strconv.FormatInt(p.ChannelID, 10)}
		if c := e.Channels[p.ChannelID]; c != nil {
			s.Name = c.Title
		}
		return s
	}
	return event.Sender{}
}

func userID(id int64) string { return "telegram:" + strconv.FormatInt(id, 10) }

func userName(u *tg.User) string {
	if u == nil {
		return ""
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

// accountLabel is the owner's "@username", or the numeric id.
func accountLabel(u *tg.User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	for _, un := range u.Usernames {
		if un.Active {
			return "@" + un.Username
		}
	}
	return strconv.FormatInt(u.ID, 10)
}

func replyToOf(m *tg.Message) string {
	rc, has := m.GetReplyTo()
	if !has {
		return ""
	}
	if h, isHeader := rc.(*tg.MessageReplyHeader); isHeader {
		if id, has := h.GetReplyToMsgID(); has && id != 0 {
			return strconv.Itoa(id)
		}
	}
	return ""
}

// attachmentsOf lists the media of a message (no download). Link previews and
// empty media are not attachments.
func attachmentsOf(m *tg.Message) []event.Attachment {
	media, has := m.GetMedia()
	if !has {
		return nil
	}
	one := func(a event.Attachment) []event.Attachment { return []event.Attachment{a} }
	switch md := media.(type) {
	case *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return nil
	case *tg.MessageMediaPhoto:
		return one(event.Attachment{Kind: "image"})
	case *tg.MessageMediaDocument:
		return one(documentAttachment(md))
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return one(event.Attachment{Kind: "location"})
	case *tg.MessageMediaContact:
		return one(event.Attachment{Kind: "contact"})
	}
	return one(event.Attachment{Kind: "other"}) // poll, dice, game, invoice, unsupported…
}

func documentAttachment(md *tg.MessageMediaDocument) event.Attachment {
	doc, isDoc := md.Document.(*tg.Document)
	if !isDoc {
		return event.Attachment{Kind: "other"} // expired / empty document
	}
	a := event.Attachment{Kind: "document", MimeType: doc.MimeType}
	var sticker, voice, audio, video bool
	for _, attr := range doc.Attributes {
		switch x := attr.(type) {
		case *tg.DocumentAttributeFilename:
			a.FileName = x.FileName
		case *tg.DocumentAttributeSticker:
			sticker = true
		case *tg.DocumentAttributeAudio:
			if x.Voice {
				voice = true
			} else {
				audio = true
			}
		case *tg.DocumentAttributeVideo:
			video = true
		}
	}
	switch {
	case sticker:
		a.Kind = "sticker"
	case voice || md.Voice:
		a.Kind = "voice"
	case audio:
		a.Kind = "audio"
	case video || md.Video || strings.HasPrefix(doc.MimeType, "video/"):
		a.Kind = "video"
	}
	return a
}
