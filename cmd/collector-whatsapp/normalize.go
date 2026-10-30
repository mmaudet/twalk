package main

import (
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/mmaudet/twalk/internal/event"
)

// directory is what normalization needs from the live session; the real
// implementation (main.go) reads whatsmeow's stores, tests use a fake.
type directory interface {
	Own() (pn, lid types.JID)
	PNForLID(lid types.JID) types.JID              // empty when unknown
	Contact(j types.JID) (name string, known bool) // address-book name, else push name
	GroupName(j types.JID) string                  // empty when unavailable
}

// normalized is the result of converting one *events.Message.
type normalized struct {
	Sent     bool
	ID       string // event id (also Nats-Msg-Id)
	Time     time.Time
	Message  *event.Message
	Reaction *event.Reaction
}

func kindOf(chat types.JID) string {
	switch chat.Server {
	case types.GroupServer:
		return event.KindGroup
	case types.NewsletterServer:
		return event.KindChannel
	}
	return event.KindDM
}

// rawID is the stable raw id: bare number for phone JIDs, full JID otherwise.
func rawID(j types.JID) string {
	if j = j.ToNonAD(); j.Server == types.DefaultUserServer {
		return j.User
	}
	return j.String()
}

// canonical resolves a LID to its phone-number JID (alt first, then the LID
// store) so that one person keeps one id. Unknown LIDs are kept as is.
func canonical(j, alt types.JID, d directory) types.JID {
	if j = j.ToNonAD(); j.Server != types.HiddenUserServer {
		return j
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD()
	}
	if pn := d.PNForLID(j); pn.Server == types.DefaultUserServer && pn.User != "" {
		return pn.ToNonAD()
	}
	return j
}

// mentionsOwner: the context lists our phone-number or LID user among mentions.
func mentionsOwner(ci *waE2E.ContextInfo, own ...types.JID) bool {
	for _, m := range ci.GetMentionedJID() {
		if j, err := types.ParseJID(m); err == nil {
			for _, o := range own {
				if !o.IsEmpty() && j.User == o.User {
					return true
				}
			}
		}
	}
	return false
}

// extract returns text, attachments and context info of a message (already
// unwrapped from ephemeral/view-once/device-sent wrappers by whatsmeow).
func extract(m *waE2E.Message) (text string, atts []event.Attachment, ci *waE2E.ContextInfo) {
	if m == nil {
		return
	}
	text = m.GetConversation()
	if et := m.GetExtendedTextMessage(); et != nil {
		text, ci = et.GetText(), et.GetContextInfo()
	}
	add := func(a event.Attachment, c *waE2E.ContextInfo) {
		atts = append(atts, a)
		if ci == nil {
			ci = c
		}
	}
	if x := m.GetImageMessage(); x != nil {
		text = x.GetCaption()
		add(event.Attachment{Kind: "image", Caption: text, MimeType: x.GetMimetype()}, x.GetContextInfo())
	}
	for _, x := range []*waE2E.VideoMessage{m.GetVideoMessage(), m.GetPtvMessage()} {
		if x != nil {
			text = x.GetCaption()
			add(event.Attachment{Kind: "video", Caption: text, MimeType: x.GetMimetype()}, x.GetContextInfo())
		}
	}
	if x := m.GetDocumentMessage(); x != nil {
		text = x.GetCaption()
		name := x.GetFileName()
		if name == "" {
			name = x.GetTitle()
		}
		add(event.Attachment{Kind: "document", Caption: text, MimeType: x.GetMimetype(), FileName: name}, x.GetContextInfo())
	}
	if x := m.GetAudioMessage(); x != nil {
		add(event.Attachment{Kind: map[bool]string{true: "voice", false: "audio"}[x.GetPTT()], MimeType: x.GetMimetype()}, x.GetContextInfo())
	}
	if x := m.GetStickerMessage(); x != nil {
		add(event.Attachment{Kind: "sticker", MimeType: x.GetMimetype()}, x.GetContextInfo())
	}
	if x := m.GetLocationMessage(); x != nil {
		add(event.Attachment{Kind: "location", Caption: x.GetName()}, x.GetContextInfo())
	}
	if x := m.GetLiveLocationMessage(); x != nil {
		add(event.Attachment{Kind: "location", Caption: x.GetCaption()}, x.GetContextInfo())
	}
	if x := m.GetContactMessage(); x != nil {
		add(event.Attachment{Kind: "contact", Caption: x.GetDisplayName()}, x.GetContextInfo())
	}
	if x := m.GetContactsArrayMessage(); x != nil {
		add(event.Attachment{Kind: "contact", Caption: x.GetDisplayName()}, x.GetContextInfo())
	}
	// Other content (poll, invite, list, buttons…): type only, no text.
	if len(atts) == 0 && text == "" && (m.GetPollCreationMessage() != nil || m.GetGroupInviteMessage() != nil ||
		m.GetListMessage() != nil || m.GetButtonsMessage() != nil || m.GetTemplateMessage() != nil ||
		m.GetInteractiveMessage() != nil || m.GetProductMessage() != nil || m.GetOrderMessage() != nil || m.GetEventMessage() != nil) {
		atts = append(atts, event.Attachment{Kind: "other"})
	}
	return
}

// normalize converts a whatsmeow message event. ok is false when nothing must
// be published: status/broadcast, revocations and other protocol messages,
// empty or undecryptable content.
func normalize(evt *events.Message, d directory) (n normalized, ok bool) {
	info := evt.Info
	if info.Chat.Server == types.BroadcastServer || info.IsIncomingBroadcast() || evt.Message == nil {
		return n, false
	}
	ownPN, ownLID := d.Own()
	chatAlt := info.SenderAlt
	if info.IsFromMe {
		chatAlt = info.RecipientAlt
	}
	chat := canonical(info.Chat, chatAlt, d)
	conv := event.Conversation{ID: event.WhatsApp + ":" + rawID(chat), Kind: kindOf(chat)}
	if conv.Kind == event.KindGroup {
		conv.Name = d.GroupName(chat)
	} else if conv.Name, _ = d.Contact(chat); conv.Name == "" && !info.IsFromMe {
		conv.Name = info.PushName
	}

	senderJID := info.Sender
	if info.IsFromMe {
		senderJID = ownPN
	} else if senderJID.IsEmpty() {
		senderJID = info.Chat // channels
	}
	sj := canonical(senderJID, info.SenderAlt, d)
	sender := event.Sender{ID: event.WhatsApp + ":" + rawID(sj), Name: info.PushName, Known: info.IsFromMe}
	if name, known := d.Contact(sj); known && !info.IsFromMe {
		sender.Name, sender.Known = name, true
	}

	n.Sent, n.Time = info.IsFromMe, info.Timestamp
	n.ID = fmt.Sprintf("%s:%s:%s", event.WhatsApp, rawID(chat), info.ID)
	msg, edited := evt.Message, false
	if pm := msg.GetProtocolMessage(); pm != nil { // only edits are kept
		if pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || pm.GetEditedMessage() == nil || pm.GetKey().GetID() == "" {
			return n, false
		}
		msg, edited = pm.GetEditedMessage(), true
		n.ID = fmt.Sprintf("%s:%s:%s:edit:%d", event.WhatsApp, rawID(chat), pm.GetKey().GetID(), info.Timestamp.Unix())
	}
	if r := msg.GetReactionMessage(); r != nil && !edited {
		n.Reaction = &event.Reaction{Conversation: conv, Sender: sender, TargetID: r.GetKey().GetID(), Emoji: r.GetText(), FromOwner: info.IsFromMe}
		return n, r.GetKey().GetID() != ""
	}
	text, atts, ci := extract(msg)
	if text == "" && len(atts) == 0 {
		return n, false
	}
	n.Message = &event.Message{Conversation: conv, Sender: sender, Text: text, Attachments: atts, Edited: edited,
		ReplyTo: ci.GetStanzaID(), MentionsOwner: mentionsOwner(ci, ownPN, ownLID), Forwarded: ci.GetIsForwarded()}
	return n, true
}
