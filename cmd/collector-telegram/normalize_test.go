package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/mmaudet/twalk/internal/event"
)

var self = &tg.User{ID: 1, FirstName: "Michel-Marie", LastName: "Maudet", Username: "mmaudet", Self: true}

func entities() tg.Entities {
	return tg.Entities{
		Users: map[int64]*tg.User{
			1:  self,
			42: {ID: 42, FirstName: "Gilles", LastName: "Lehoux", Contact: true},
			43: {ID: 43, FirstName: "Inconnu"},
		},
		Chats:    map[int64]*tg.Chat{7: {ID: 7, Title: "Famille"}},
		Channels: map[int64]*tg.Channel{9: {ID: 9, Title: "Comex", Megagroup: true}, 10: {ID: 10, Title: "Actus", Broadcast: true}},
	}
}

func TestNormalizeConversationAndSender(t *testing.T) {
	cases := []struct {
		name     string
		m        *tg.Message
		wantConv event.Conversation
		wantSnd  event.Sender
		wantSent bool
		wantID   string
	}{
		{"dm entrant sans FromID", &tg.Message{ID: 5, Date: 100, Message: "salut", PeerID: &tg.PeerUser{UserID: 42}},
			event.Conversation{ID: "telegram:user:42", Kind: event.KindDM, Name: "Gilles Lehoux"},
			event.Sender{ID: "telegram:42", Name: "Gilles Lehoux", Known: true}, false, "telegram:user:42:5"},
		{"dm entrant, inconnu", &tg.Message{ID: 6, Message: "x", PeerID: &tg.PeerUser{UserID: 43}, FromID: &tg.PeerUser{UserID: 43}},
			event.Conversation{ID: "telegram:user:43", Kind: event.KindDM, Name: "Inconnu"},
			event.Sender{ID: "telegram:43", Name: "Inconnu", Known: false}, false, "telegram:user:43:6"},
		{"dm sortant (autre appareil)", &tg.Message{ID: 7, Out: true, Message: "ok", PeerID: &tg.PeerUser{UserID: 42}},
			event.Conversation{ID: "telegram:user:42", Kind: event.KindDM, Name: "Gilles Lehoux"},
			event.Sender{ID: "telegram:1", Name: "Michel-Marie Maudet", Known: true}, true, "telegram:user:42:7"},
		{"groupe basique", &tg.Message{ID: 8, Message: "hello", PeerID: &tg.PeerChat{ChatID: 7}, FromID: &tg.PeerUser{UserID: 42}},
			event.Conversation{ID: "telegram:chat:7", Kind: event.KindGroup, Name: "Famille"},
			event.Sender{ID: "telegram:42", Name: "Gilles Lehoux", Known: true}, false, "telegram:chat:7:8"},
		{"supergroupe", &tg.Message{ID: 9, Message: "hello", PeerID: &tg.PeerChannel{ChannelID: 9}, FromID: &tg.PeerUser{UserID: 43}},
			event.Conversation{ID: "telegram:channel:9", Kind: event.KindGroup, Name: "Comex"},
			event.Sender{ID: "telegram:43", Name: "Inconnu"}, false, "telegram:channel:9:9"},
		{"canal diffusion (le canal parle)", &tg.Message{ID: 10, Post: true, Message: "news", PeerID: &tg.PeerChannel{ChannelID: 10}},
			event.Conversation{ID: "telegram:channel:10", Kind: event.KindChannel, Name: "Actus"},
			event.Sender{ID: "telegram:channel:10", Name: "Actus"}, false, "telegram:channel:10:10"},
		{"canal inconnu, post", &tg.Message{ID: 11, Post: true, Message: "news", PeerID: &tg.PeerChannel{ChannelID: 99}},
			event.Conversation{ID: "telegram:channel:99", Kind: event.KindChannel},
			event.Sender{ID: "telegram:channel:99"}, false, "telegram:channel:99:11"},
		{"canal inconnu, membre qui parle", &tg.Message{ID: 12, Message: "yo", PeerID: &tg.PeerChannel{ChannelID: 99}, FromID: &tg.PeerUser{UserID: 77}},
			event.Conversation{ID: "telegram:channel:99", Kind: event.KindGroup},
			event.Sender{ID: "telegram:77"}, false, "telegram:channel:99:12"},
		{"admin anonyme (le groupe parle)", &tg.Message{ID: 13, Message: "yo", PeerID: &tg.PeerChannel{ChannelID: 9}, FromID: &tg.PeerChannel{ChannelID: 9}},
			event.Conversation{ID: "telegram:channel:9", Kind: event.KindGroup, Name: "Comex"},
			event.Sender{ID: "telegram:channel:9", Name: "Comex"}, false, "telegram:channel:9:13"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := norm(c.m, entities(), self, false)
			if !ok {
				t.Fatal("ignored")
			}
			if n.Msg.Conversation != c.wantConv {
				t.Errorf("conversation %+v, want %+v", n.Msg.Conversation, c.wantConv)
			}
			if n.Msg.Sender != c.wantSnd {
				t.Errorf("sender %+v, want %+v", n.Msg.Sender, c.wantSnd)
			}
			if n.Sent != c.wantSent || n.ID != c.wantID {
				t.Errorf("sent=%v id=%q, want sent=%v id=%q", n.Sent, n.ID, c.wantSent, c.wantID)
			}
		})
	}
}

func TestNormalizeEntityMissing(t *testing.T) {
	m := &tg.Message{ID: 1, Message: "x", PeerID: &tg.PeerUser{UserID: 500}}
	n, ok := norm(m, tg.Entities{}, self, false)
	if !ok || n.Msg.Conversation.Name != "" || n.Msg.Sender.Name != "" || n.Msg.Sender.Known {
		t.Fatalf("%+v ok=%v", n, ok)
	}
	if n.Msg.Conversation.ID != "telegram:user:500" || n.Msg.Sender.ID != "telegram:500" {
		t.Fatalf("%+v", n.Msg)
	}
}

func TestNormalizeIgnored(t *testing.T) {
	peer := &tg.PeerUser{UserID: 42}
	cases := map[string]tg.MessageClass{
		"service":       &tg.MessageService{ID: 1, PeerID: peer},
		"empty":         &tg.MessageEmpty{ID: 2},
		"sans contenu":  &tg.Message{ID: 3, PeerID: peer},
		"webpage seule": &tg.Message{ID: 4, PeerID: peer, Media: &tg.MessageMediaWebPage{}},
		"media vide":    &tg.Message{ID: 5, PeerID: peer, Media: &tg.MessageMediaEmpty{}},
	}
	for name, m := range cases {
		if _, ok := norm(m, entities(), self, false); ok {
			t.Errorf("%s: should be ignored", name)
		}
	}
}

func TestNormalizeFlagsAndReply(t *testing.T) {
	m := &tg.Message{ID: 20, Date: 1700000000, Mentioned: true, Message: "@mmaudet regarde", PeerID: &tg.PeerChannel{ChannelID: 9},
		FromID: &tg.PeerUser{UserID: 42}}
	hdr := &tg.MessageReplyHeader{}
	hdr.SetReplyToMsgID(15)
	m.SetReplyTo(hdr)
	m.SetFwdFrom(tg.MessageFwdHeader{Date: 1})
	n, ok := norm(m, entities(), self, false)
	if !ok {
		t.Fatal("ignored")
	}
	if !n.Msg.MentionsOwner || !n.Msg.Forwarded || n.Msg.ReplyTo != "15" || n.Msg.Edited || n.Sent {
		t.Errorf("%+v", n)
	}
	if !n.Time.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("time %v", n.Time)
	}
	// neither forwarded, replied, nor mentioned by default
	n, _ = norm(&tg.Message{ID: 21, Message: "a", PeerID: &tg.PeerUser{UserID: 42}}, entities(), self, false)
	if n.Msg.MentionsOwner || n.Msg.Forwarded || n.Msg.ReplyTo != "" {
		t.Errorf("%+v", n)
	}
	// reply to a story: no message id
	m2 := &tg.Message{ID: 22, Message: "a", PeerID: &tg.PeerUser{UserID: 42}}
	m2.SetReplyTo(&tg.MessageReplyStoryHeader{})
	if n, _ = norm(m2, entities(), self, false); n.Msg.ReplyTo != "" {
		t.Errorf("story reply: %q", n.Msg.ReplyTo)
	}
}

func TestNormalizeEdit(t *testing.T) {
	m := &tg.Message{ID: 30, Date: 100, Message: "corrigé", PeerID: &tg.PeerUser{UserID: 42}}
	if _, ok := norm(m, entities(), self, true); ok {
		t.Fatal("edit without edit_date must be ignored")
	}
	m.SetEditDate(250)
	n, ok := norm(m, entities(), self, true)
	if !ok || !n.Msg.Edited || n.ID != "telegram:user:42:30:edit:250" || !n.Time.Equal(time.Unix(250, 0)) {
		t.Fatalf("%+v ok=%v", n, ok)
	}
	m.EditHide = true
	if _, ok := norm(m, entities(), self, true); ok {
		t.Fatal("edit_hide must be ignored")
	}
	// the same message, new: no suffix
	m.EditHide = false
	if n, _ = norm(m, entities(), self, false); n.ID != "telegram:user:42:30" || n.Msg.Edited {
		t.Fatalf("%+v", n)
	}
}

func doc(mime string, attrs ...tg.DocumentAttributeClass) *tg.Document {
	return &tg.Document{MimeType: mime, Attributes: attrs}
}

func TestAttachments(t *testing.T) {
	cases := []struct {
		name  string
		media tg.MessageMediaClass
		want  []event.Attachment
	}{
		{"photo", &tg.MessageMediaPhoto{}, []event.Attachment{{Kind: "image", Caption: "txt"}}},
		{"voix", &tg.MessageMediaDocument{Document: doc("audio/ogg", &tg.DocumentAttributeAudio{Voice: true})},
			[]event.Attachment{{Kind: "voice", MimeType: "audio/ogg", Caption: "txt"}}},
		{"audio", &tg.MessageMediaDocument{Document: doc("audio/mpeg", &tg.DocumentAttributeAudio{}, &tg.DocumentAttributeFilename{FileName: "a.mp3"})},
			[]event.Attachment{{Kind: "audio", MimeType: "audio/mpeg", FileName: "a.mp3", Caption: "txt"}}},
		{"video", &tg.MessageMediaDocument{Document: doc("video/mp4", &tg.DocumentAttributeVideo{})},
			[]event.Attachment{{Kind: "video", MimeType: "video/mp4", Caption: "txt"}}},
		{"gif", &tg.MessageMediaDocument{Document: doc("video/mp4", &tg.DocumentAttributeAnimated{})},
			[]event.Attachment{{Kind: "video", MimeType: "video/mp4", Caption: "txt"}}},
		{"sticker", &tg.MessageMediaDocument{Document: doc("image/webp", &tg.DocumentAttributeSticker{})},
			[]event.Attachment{{Kind: "sticker", MimeType: "image/webp", Caption: "txt"}}},
		{"pdf", &tg.MessageMediaDocument{Document: doc("application/pdf", &tg.DocumentAttributeFilename{FileName: "devis.pdf"})},
			[]event.Attachment{{Kind: "document", MimeType: "application/pdf", FileName: "devis.pdf", Caption: "txt"}}},
		{"document expiré", &tg.MessageMediaDocument{Document: &tg.DocumentEmpty{}}, []event.Attachment{{Kind: "other", Caption: "txt"}}},
		{"geo", &tg.MessageMediaGeo{}, []event.Attachment{{Kind: "location", Caption: "txt"}}},
		{"lieu", &tg.MessageMediaVenue{}, []event.Attachment{{Kind: "location", Caption: "txt"}}},
		{"position live", &tg.MessageMediaGeoLive{}, []event.Attachment{{Kind: "location", Caption: "txt"}}},
		{"contact", &tg.MessageMediaContact{}, []event.Attachment{{Kind: "contact", Caption: "txt"}}},
		{"sondage", &tg.MessageMediaPoll{}, []event.Attachment{{Kind: "other", Caption: "txt"}}},
		{"dé", &tg.MessageMediaDice{}, []event.Attachment{{Kind: "other", Caption: "txt"}}},
		{"aperçu de lien", &tg.MessageMediaWebPage{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &tg.Message{ID: 1, Message: "txt", PeerID: &tg.PeerUser{UserID: 42}, Media: c.media}
			n, ok := norm(m, entities(), self, false)
			if !ok {
				t.Fatal("ignored")
			}
			if !reflect.DeepEqual(n.Msg.Attachments, c.want) {
				t.Errorf("got %+v, want %+v", n.Msg.Attachments, c.want)
			}
		})
	}
	// media without text: published, no caption
	m := &tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: 42}, Media: &tg.MessageMediaPhoto{}}
	n, ok := norm(m, entities(), self, false)
	if !ok || n.Msg.Text != "" || !reflect.DeepEqual(n.Msg.Attachments, []event.Attachment{{Kind: "image"}}) {
		t.Errorf("%+v ok=%v", n, ok)
	}
	// MessageMediaDocument.Voice flag alone
	if d := documentAttachment(&tg.MessageMediaDocument{Voice: true, Document: doc("audio/ogg")}); d.Kind != "voice" {
		t.Errorf("voice flag: %+v", d)
	}
}

func TestAccountLabel(t *testing.T) {
	if got := accountLabel(self); got != "@mmaudet" {
		t.Error(got)
	}
	if got := accountLabel(&tg.User{ID: 5, Usernames: []tg.Username{{Username: "old"}, {Username: "new", Active: true}}}); got != "@new" {
		t.Error(got)
	}
	if got := accountLabel(&tg.User{ID: 5}); got != "5" {
		t.Error(got)
	}
}

// norm recomputes the TL flags of hand-built messages, as decoding or the
// gotd Set* helpers do for real ones, then normalizes them.
func norm(mc tg.MessageClass, e tg.Entities, self *tg.User, edited bool) (normalized, bool) {
	if m, ok := mc.(*tg.Message); ok {
		m.SetFlags()
	}
	return normalize(mc, e, self, edited)
}
