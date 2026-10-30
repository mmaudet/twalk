package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

type fakeDir struct{}

func (fakeDir) groupName(id string) string {
	if id == "R3JvdXA/+Zz==" {
		return "Équipe Twalk"
	}
	return ""
}

func (fakeDir) contact(number, uuid string) (string, bool) {
	if number == "+33612345678" || uuid == "11111111-aaaa-bbbb-cccc-222222222222" {
		return "Alice Martin", true
	}
	return "", false
}

var me = self{Number: "+33699999999", UUID: "99999999-0000-0000-0000-000000000009"}

// notif wraps an envelope in a realistic `receive` JSON-RPC notification.
func notif(t *testing.T, envelopeJSON string) *envelope {
	t.Helper()
	line := `{"jsonrpc":"2.0","method":"receive","params":{"account":"+33699999999","envelope":` + envelopeJSON + `}}`
	var m rpcMsg
	var p struct{ Envelope *envelope }
	if err := json.Unmarshal([]byte(line), &m); err != nil || m.Method != "receive" {
		t.Fatalf("notification: %v", err)
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || p.Envelope == nil {
		t.Fatalf("params: %v", err)
	}
	return p.Envelope
}

func TestDirectMessage(t *testing.T) {
	n, ok := normalize(notif(t, `{"source":"+33612345678","sourceNumber":"+33612345678","sourceUuid":"11111111-aaaa-bbbb-cccc-222222222222",
	 "sourceName":"Alice","sourceDevice":1,"timestamp":1730000000123,
	 "dataMessage":{"timestamp":1730000000123,"message":"Salut Michel","expiresInSeconds":0,"viewOnce":false}}`), me, fakeDir{})
	if !ok || n.Msg == nil || n.Sent {
		t.Fatalf("n=%+v ok=%v", n, ok)
	}
	m := n.Msg
	if n.ID != "signal:+33612345678:1730000000123:+33612345678" || m.Conversation.ID != "signal:+33612345678" ||
		m.Conversation.Kind != event.KindDM || m.Conversation.Name != "Alice Martin" {
		t.Errorf("id/conv: %q %+v", n.ID, m.Conversation)
	}
	if m.Sender.ID != "signal:+33612345678" || m.Sender.Name != "Alice" || !m.Sender.Known || m.Text != "Salut Michel" || m.MentionsOwner {
		t.Errorf("sender/text: %+v", m)
	}
	if !n.Time.Equal(time.UnixMilli(1730000000123)) {
		t.Errorf("time %v", n.Time)
	}
}

func TestUnknownSenderUUIDOnly(t *testing.T) {
	n, ok := normalize(notif(t, `{"source":"33333333-aaaa-bbbb-cccc-444444444444","sourceNumber":null,
	 "sourceUuid":"33333333-aaaa-bbbb-cccc-444444444444","sourceName":"Inconnu","timestamp":5,"dataMessage":{"timestamp":5,"message":"coucou"}}`), me, fakeDir{})
	if !ok || n.Msg.Sender.ID != "signal:33333333-aaaa-bbbb-cccc-444444444444" || n.Msg.Sender.Known ||
		n.Msg.Conversation.ID != "signal:33333333-aaaa-bbbb-cccc-444444444444" {
		t.Errorf("%+v", n.Msg)
	}
}

func TestGroupWithMentionAndQuote(t *testing.T) {
	n, ok := normalize(notif(t, `{"sourceNumber":"+33612345678","sourceUuid":"11111111-aaaa-bbbb-cccc-222222222222","sourceName":"Alice","timestamp":1730000100000,
	 "dataMessage":{"timestamp":1730000100000,"message":"￼ tu valides ?",
	 "groupInfo":{"groupId":"R3JvdXA/+Zz==","type":"DELIVER"},
	 "mentions":[{"name":"Michel","number":"+33699999999","uuid":"99999999-0000-0000-0000-000000000009","start":0,"length":1}],
	 "quote":{"id":1729999999000,"author":"+33699999999","authorNumber":"+33699999999","text":"le plan"}}}`), me, fakeDir{})
	if !ok {
		t.Fatal("ignored")
	}
	m := n.Msg
	if m.Conversation.ID != "signal:group:R3JvdXA/+Zz==" || m.Conversation.Kind != event.KindGroup || m.Conversation.Name != "Équipe Twalk" {
		t.Errorf("conv %+v", m.Conversation)
	}
	if !m.MentionsOwner || m.ReplyTo != "1729999999000" || n.ID != "signal:group:R3JvdXA/+Zz==:1730000100000:+33612345678" {
		t.Errorf("%q %+v", n.ID, m)
	}
	// mention of someone else only
	n, _ = normalize(notif(t, `{"sourceNumber":"+33612345678","timestamp":1,"dataMessage":{"timestamp":1,"message":"x",
	 "groupInfo":{"groupId":"g","groupName":"Nom direct"},"mentions":[{"number":"+33600000000","uuid":"zzz"}]}}`), me, fakeDir{})
	if n.Msg.MentionsOwner || n.Msg.Conversation.Name != "Nom direct" {
		t.Errorf("%+v", n.Msg)
	}
}

func TestSyncSentMessage(t *testing.T) {
	n, ok := normalize(notif(t, `{"sourceNumber":"+33699999999","sourceUuid":"99999999-0000-0000-0000-000000000009","sourceDevice":1,"timestamp":1730000200000,
	 "syncMessage":{"sentMessage":{"destination":"+33612345678","destinationNumber":"+33612345678","destinationUuid":"11111111-aaaa-bbbb-cccc-222222222222",
	  "timestamp":1730000200000,"message":"Oui, c'est bon","expiresInSeconds":0,"viewOnce":false}}}`), me, fakeDir{})
	if !ok || !n.Sent {
		t.Fatalf("%+v %v", n, ok)
	}
	if n.Msg.Conversation.ID != "signal:+33612345678" || n.Msg.Sender.ID != "signal:+33699999999" || !n.Msg.Sender.Known ||
		n.ID != "signal:+33612345678:1730000200000:+33699999999" || n.Msg.Conversation.Name != "Alice Martin" {
		t.Errorf("%q %+v", n.ID, n.Msg)
	}
	// sent to a group
	n, ok = normalize(notif(t, `{"sourceNumber":"+33699999999","timestamp":9,"syncMessage":{"sentMessage":{"timestamp":9,"message":"ok","groupInfo":{"groupId":"R3JvdXA/+Zz=="}}}}`), me, fakeDir{})
	if !ok || !n.Sent || n.Msg.Conversation.ID != "signal:group:R3JvdXA/+Zz==" {
		t.Errorf("%+v", n)
	}
}

func TestEditMessage(t *testing.T) {
	n, ok := normalize(notif(t, `{"sourceNumber":"+33612345678","timestamp":1730000400000,
	 "editMessage":{"targetSentTimestamp":1730000000123,"dataMessage":{"timestamp":1730000400000,"message":"Salut Michel (corrigé)"}}}`), me, fakeDir{})
	if !ok || !n.Msg.Edited || n.ID != "signal:+33612345678:1730000000123:+33612345678:edit:1730000400000" || n.Msg.Text != "Salut Michel (corrigé)" {
		t.Errorf("%q %+v", n.ID, n.Msg)
	}
	// edit of a message sent by the owner from another device
	n, ok = normalize(notif(t, `{"sourceNumber":"+33699999999","timestamp":50,"syncMessage":{"sentMessage":{"destinationNumber":"+33612345678","timestamp":50,
	 "editMessage":{"targetSentTimestamp":40,"dataMessage":{"timestamp":50,"message":"v2"}}}}}`), me, fakeDir{})
	if !ok || !n.Sent || !n.Msg.Edited || n.ID != "signal:+33612345678:40:+33699999999:edit:50" {
		t.Errorf("%q %+v", n.ID, n)
	}
}

func TestReaction(t *testing.T) {
	n, ok := normalize(notif(t, `{"sourceNumber":"+33612345678","timestamp":1730000300000,
	 "dataMessage":{"timestamp":1730000300000,"reaction":{"emoji":"👍","targetAuthor":"+33699999999","targetAuthorNumber":"+33699999999","targetSentTimestamp":1730000200000,"isRemove":false}}}`), me, fakeDir{})
	if !ok || n.Reaction == nil || n.Msg != nil || n.Reaction.Emoji != "👍" || n.Reaction.TargetID != "1730000200000" || n.Reaction.FromOwner ||
		n.ID != "signal:+33612345678:1730000300000:+33612345678" {
		t.Errorf("%q %+v", n.ID, n.Reaction)
	}
	n, _ = normalize(notif(t, `{"sourceNumber":"+33699999999","timestamp":7,"syncMessage":{"sentMessage":{"destinationNumber":"+33612345678","timestamp":7,
	 "reaction":{"emoji":"👍","targetSentTimestamp":6,"isRemove":true}}}}`), me, fakeDir{})
	if n.Reaction == nil || n.Reaction.Emoji != "" || !n.Reaction.FromOwner {
		t.Errorf("%+v", n.Reaction)
	}
}

func TestIgnored(t *testing.T) {
	for name, env := range map[string]string{
		"typing":    `{"sourceNumber":"+33612345678","timestamp":1,"typingMessage":{"action":"STARTED","timestamp":1}}`,
		"receipt":   `{"sourceNumber":"+33612345678","timestamp":1,"receiptMessage":{"when":1,"isDelivery":true,"timestamps":[1]}}`,
		"story":     `{"sourceNumber":"+33612345678","timestamp":1,"storyMessage":{"allowsReplies":true}}`,
		"call":      `{"sourceNumber":"+33612345678","timestamp":1,"callMessage":{"offerMessage":{"id":1,"type":"AUDIO_CALL"}}}`,
		"sync-read": `{"sourceNumber":"+33699999999","timestamp":1,"syncMessage":{"readMessages":[{"sender":"+33612345678","timestamp":1}]}}`,
		"expiry":    `{"sourceNumber":"+33612345678","timestamp":1,"dataMessage":{"timestamp":1,"isExpirationUpdate":true,"expiresInSeconds":60}}`,
		"delete":    `{"sourceNumber":"+33612345678","timestamp":1,"dataMessage":{"timestamp":1,"remoteDelete":{"timestamp":0}}}`,
		"no-source": `{"timestamp":1,"dataMessage":{"timestamp":1,"message":"?"}}`,
	} {
		if n, ok := normalize(notif(t, env), me, fakeDir{}); ok {
			t.Errorf("%s: should be ignored, got %+v", name, n)
		}
	}
}

func TestAttachments(t *testing.T) {
	n, ok := normalize(notif(t, `{"sourceNumber":"+33612345678","timestamp":2,"dataMessage":{"timestamp":2,"attachments":[
	 {"contentType":"image/jpeg","filename":"IMG_1.jpg","id":"a1","size":1000,"caption":"la vue"},
	 {"contentType":"audio/aac","id":"a2","isVoiceNote":true},{"contentType":"audio/mpeg","filename":"x.mp3"},
	 {"contentType":"video/mp4"},{"contentType":"application/pdf","filename":"devis.pdf"},{"contentType":"weird/thing"}],
	 "sticker":{"packId":"p","stickerId":3}}}`), me, fakeDir{})
	if !ok || n.Msg.Text != "" {
		t.Fatalf("%+v", n)
	}
	want := []event.Attachment{{Kind: "image", Caption: "la vue", MimeType: "image/jpeg", FileName: "IMG_1.jpg"},
		{Kind: "voice", MimeType: "audio/aac"}, {Kind: "audio", MimeType: "audio/mpeg", FileName: "x.mp3"}, {Kind: "video", MimeType: "video/mp4"},
		{Kind: "document", MimeType: "application/pdf", FileName: "devis.pdf"}, {Kind: "other", MimeType: "weird/thing"}, {Kind: "sticker"}}
	if len(n.Msg.Attachments) != len(want) {
		t.Fatalf("%+v", n.Msg.Attachments)
	}
	for i, a := range want {
		if n.Msg.Attachments[i] != a {
			t.Errorf("attachment %d: %+v want %+v", i, n.Msg.Attachments[i], a)
		}
	}
}
