package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/mmaudet/twalk/internal/event"
)

var (
	ownPN    = types.NewJID("33660469852", types.DefaultUserServer)
	ownLID   = types.NewJID("111222333", types.HiddenUserServer)
	alicePN  = types.NewJID("33611111111", types.DefaultUserServer)
	aliceLID = types.NewJID("999888777", types.HiddenUserServer)
	bobLID   = types.NewJID("555444333", types.HiddenUserServer)
	group    = types.NewJID("120363000000000001", types.GroupServer)
	ts       = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
)

type fakeDir struct{}

func (fakeDir) Own() (types.JID, types.JID) { return ownPN, ownLID }
func (fakeDir) PNForLID(l types.JID) types.JID {
	if l.User == aliceLID.User {
		return alicePN
	}
	return types.EmptyJID
}
func (fakeDir) Contact(j types.JID) (string, bool) {
	if j.User == alicePN.User {
		return "Alice Martin", true
	}
	return "", false
}
func (fakeDir) GroupName(j types.JID) string {
	if j == group {
		return "Comex"
	}
	return ""
}

func ev(chat, sender types.JID, fromMe bool, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            "3EB0A1C2D3", PushName: "Push", Timestamp: ts,
		},
		Message: m,
	}
}

func TestKindOf(t *testing.T) {
	for jid, want := range map[types.JID]string{
		group:                              event.KindGroup,
		types.NewJID("1203", "newsletter"): event.KindChannel,
		alicePN:                            event.KindDM,
		aliceLID:                           event.KindDM,
	} {
		if got := kindOf(jid); got != want {
			t.Errorf("kindOf(%s) = %s, want %s", jid, got, want)
		}
	}
}

func TestIDs(t *testing.T) {
	if got := rawID(alicePN); got != "33611111111" {
		t.Errorf("pn: %s", got)
	}
	if got := rawID(types.NewADJID("33611111111", 0, 7)); got != "33611111111" {
		t.Errorf("device stripped: %s", got)
	}
	if got := rawID(group); got != "120363000000000001@g.us" {
		t.Errorf("group: %s", got)
	}
	// LID resolved through the store → same id as the phone number.
	if got := rawID(canonical(aliceLID, types.EmptyJID, fakeDir{})); got != "33611111111" {
		t.Errorf("lid via store: %s", got)
	}
	// LID resolved through the alt JID carried by the event.
	if got := rawID(canonical(bobLID, types.NewJID("33622222222", types.DefaultUserServer), fakeDir{})); got != "33622222222" {
		t.Errorf("lid via alt: %s", got)
	}
	// Unknown LID stays a LID.
	if got := rawID(canonical(bobLID, types.EmptyJID, fakeDir{})); got != "555444333@lid" {
		t.Errorf("unknown lid: %s", got)
	}
}

func TestMentionsOwner(t *testing.T) {
	ci := func(j ...string) *waE2E.ContextInfo { return &waE2E.ContextInfo{MentionedJID: j} }
	cases := []struct {
		name string
		ci   *waE2E.ContextInfo
		want bool
	}{
		{"nil", nil, false},
		{"pn", ci("33660469852@s.whatsapp.net"), true},
		{"pn with device", ci("33660469852:3@s.whatsapp.net"), true},
		{"lid", ci("111222333@lid"), true},
		{"other", ci("33611111111@s.whatsapp.net", "garbage"), false},
	}
	for _, c := range cases {
		if got := mentionsOwner(c.ci, ownPN, ownLID); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestExtract(t *testing.T) {
	cases := []struct {
		name     string
		msg      *waE2E.Message
		text     string
		atts     []event.Attachment
		forwards bool
	}{
		{"conversation", &waE2E.Message{Conversation: proto.String("salut")}, "salut", nil, false},
		{"extended text + forwarded", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("voir https://x.y"), ContextInfo: &waE2E.ContextInfo{IsForwarded: proto.Bool(true)}}}, "voir https://x.y", nil, true},
		{"image caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("photo"), Mimetype: proto.String("image/jpeg")}},
			"photo", []event.Attachment{{Kind: "image", Caption: "photo", MimeType: "image/jpeg"}}, false},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4")}},
			"", []event.Attachment{{Kind: "video", MimeType: "video/mp4"}}, false},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg")}},
			"", []event.Attachment{{Kind: "audio", MimeType: "audio/mpeg"}}, false},
		{"voice note", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus")}},
			"", []event.Attachment{{Kind: "voice", MimeType: "audio/ogg; codecs=opus"}}, false},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("devis.pdf"), Mimetype: proto.String("application/pdf"), Caption: proto.String("le devis")}},
			"le devis", []event.Attachment{{Kind: "document", Caption: "le devis", MimeType: "application/pdf", FileName: "devis.pdf"}}, false},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}},
			"", []event.Attachment{{Kind: "sticker", MimeType: "image/webp"}}, false},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Bureau")}},
			"", []event.Attachment{{Kind: "location", Caption: "Bureau"}}, false},
		{"contact", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Bob")}},
			"", []event.Attachment{{Kind: "contact", Caption: "Bob"}}, false},
		{"poll → other", &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Midi ?")}},
			"", []event.Attachment{{Kind: "other"}}, false},
		{"empty", &waE2E.Message{}, "", nil, false},
		{"nil", nil, "", nil, false},
	}
	for _, c := range cases {
		text, atts, ci := extract(c.msg)
		if text != c.text {
			t.Errorf("%s: text %q, want %q", c.name, text, c.text)
		}
		if len(atts) != len(c.atts) {
			t.Errorf("%s: atts %+v, want %+v", c.name, atts, c.atts)
			continue
		}
		for i := range atts {
			if atts[i] != c.atts[i] {
				t.Errorf("%s: att %+v, want %+v", c.name, atts[i], c.atts[i])
			}
		}
		if ci.GetIsForwarded() != c.forwards {
			t.Errorf("%s: forwarded = %v", c.name, ci.GetIsForwarded())
		}
	}
}

func TestNormalizeGroupMessage(t *testing.T) {
	m := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("@33660469852 tu valides ?"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID: proto.String("QUOTED1"), MentionedJID: []string{"111222333@lid"},
		}}}
	e := ev(group, aliceLID, false, m)
	e.Info.SenderAlt = alicePN
	n, ok := normalize(e, fakeDir{})
	if !ok || n.Message == nil || n.Sent {
		t.Fatalf("ok=%v n=%+v", ok, n)
	}
	got := *n.Message
	want := event.Message{
		Conversation:  event.Conversation{ID: "whatsapp:120363000000000001@g.us", Kind: "group", Name: "Comex"},
		Sender:        event.Sender{ID: "whatsapp:33611111111", Name: "Alice Martin", Known: true},
		Text:          "@33660469852 tu valides ?",
		ReplyTo:       "QUOTED1",
		MentionsOwner: true,
	}
	if got.Conversation != want.Conversation || got.Sender != want.Sender || got.Text != want.Text ||
		got.ReplyTo != want.ReplyTo || got.MentionsOwner != want.MentionsOwner || got.Edited || got.Forwarded {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if n.ID != "whatsapp:120363000000000001@g.us:3EB0A1C2D3" || !n.Time.Equal(ts) {
		t.Errorf("id/time: %s %v", n.ID, n.Time)
	}
}

func TestNormalizeDM(t *testing.T) {
	// Incoming DM from an unknown LID: keeps the LID and the push name.
	n, ok := normalize(ev(bobLID, bobLID, false, &waE2E.Message{Conversation: proto.String("hello")}), fakeDir{})
	if !ok {
		t.Fatal("not ok")
	}
	if c := n.Message.Conversation; c.ID != "whatsapp:555444333@lid" || c.Kind != "dm" || c.Name != "Push" {
		t.Errorf("conv %+v", c)
	}
	if s := n.Message.Sender; s.ID != "whatsapp:555444333@lid" || s.Name != "Push" || s.Known {
		t.Errorf("sender %+v", s)
	}
	// Message sent by the owner from another device, to Alice's LID chat.
	e := ev(aliceLID, types.NewADJID("33660469852", 0, 5), true, &waE2E.Message{Conversation: proto.String("ok")})
	n, ok = normalize(e, fakeDir{})
	if !ok || !n.Sent {
		t.Fatalf("sent: ok=%v n=%+v", ok, n)
	}
	if c := n.Message.Conversation; c.ID != "whatsapp:33611111111" || c.Name != "Alice Martin" {
		t.Errorf("conv %+v", c)
	}
	if s := n.Message.Sender; s.ID != "whatsapp:33660469852" || !s.Known {
		t.Errorf("sender %+v", s)
	}
}

func TestNormalizeReaction(t *testing.T) {
	react := func(emoji string) *waE2E.Message {
		return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{ID: proto.String("TARGET1")}, Text: proto.String(emoji)}}
	}
	n, ok := normalize(ev(group, aliceLID, false, react("👍")), fakeDir{})
	if !ok || n.Reaction == nil || n.Message != nil {
		t.Fatalf("ok=%v n=%+v", ok, n)
	}
	if r := n.Reaction; r.TargetID != "TARGET1" || r.Emoji != "👍" || r.FromOwner || r.Conversation.Kind != "group" {
		t.Errorf("reaction %+v", r)
	}
	n, _ = normalize(ev(alicePN, ownPN, true, react("")), fakeDir{}) // removal by owner
	if n.Reaction == nil || n.Reaction.Emoji != "" || !n.Reaction.FromOwner {
		t.Errorf("removal %+v", n.Reaction)
	}
}

func TestNormalizeEdit(t *testing.T) {
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("ORIG1")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("corrigé")},
	}}
	n, ok := normalize(ev(alicePN, alicePN, false, edit), fakeDir{})
	if !ok || n.Message == nil || !n.Message.Edited || n.Message.Text != "corrigé" {
		t.Fatalf("ok=%v n=%+v", ok, n)
	}
	if want := "whatsapp:33611111111:ORIG1:edit:" + strconv.FormatInt(ts.Unix(), 10); n.ID != want {
		t.Errorf("id %s, want %s", n.ID, want)
	}
}

func TestNormalizeIgnored(t *testing.T) {
	text := &waE2E.Message{Conversation: proto.String("x")}
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("ORIG1")}}}
	cases := map[string]*events.Message{
		"status":      ev(types.StatusBroadcastJID, alicePN, false, text),
		"broadcast":   ev(types.NewJID("1699999999", types.BroadcastServer), alicePN, false, text),
		"revoke":      ev(alicePN, alicePN, false, revoke),
		"empty":       ev(alicePN, alicePN, false, &waE2E.Message{}),
		"undecrypted": ev(alicePN, alicePN, false, nil),
	}
	for name, e := range cases {
		if _, ok := normalize(e, fakeDir{}); ok {
			t.Errorf("%s must be ignored", name)
		}
	}
}

// The real store: modernc SQLite through sqlstore, no network involved.
func TestOpenStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("TWALK_STATE_DIR", dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	c, err := openStore(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if d, err := pairedDevice(ctx, c); err != nil || d != nil {
		t.Fatalf("empty store: %v %v", d, err)
	}
	dev := c.NewDevice()
	id := types.NewADJID("33660469852", 0, 3)
	dev.ID, dev.Account = &id, &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := dev.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if d, err := pairedDevice(ctx, c); err != nil || d == nil || d.ID.User != "33660469852" {
		t.Fatalf("paired device: %v %v", d, err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "whatsmeow.db"): 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v", path, st, err)
		}
	}
}
