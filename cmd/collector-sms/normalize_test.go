package main

import (
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/mmaudet/twalk/internal/event"
)

var (
	start = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	me    = &gmproto.Participant{ID: &gmproto.SmallInfo{ParticipantID: "1"}, IsMe: true}
	alice = &gmproto.Participant{ID: &gmproto.SmallInfo{Number: "+33 6 11 11 11 11", ParticipantID: "2"}, FullName: "Alice Martin", ContactID: "c-alice", IsVisible: true}
	bob   = &gmproto.Participant{ID: &gmproto.SmallInfo{Number: "06 22 22 22 22", ParticipantID: "3"}, IsVisible: true}
	carl  = &gmproto.Participant{ID: &gmproto.SmallInfo{ParticipantID: "4"}, IsVisible: true} // no number
)

func newConv() *converter {
	return &converter{cutoff: start.Add(-maxReplayAge), defaultCC: "33", seen: newSeenSet(16)}
}

func text(s string) *gmproto.MessageInfo {
	return &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: s}}}
}

func media(mime, name string, f gmproto.MediaFormats) *gmproto.MessageInfo {
	return &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MimeType: mime, MediaName: name, Format: f}}}
}

func msg(id, conv, from string, st gmproto.MessageStatusType, at time.Time, parts ...*gmproto.MessageInfo) *gmproto.Message {
	return &gmproto.Message{MessageID: id, ConversationID: conv, ParticipantID: from, Timestamp: at.UnixMicro(),
		MessageStatus: &gmproto.MessageStatus{Status: st}, MessageInfo: parts}
}

var (
	dm    = &gmproto.Conversation{ConversationID: "42", Name: "Alice Martin", Participants: []*gmproto.Participant{me, alice}}
	group = &gmproto.Conversation{ConversationID: "77", Name: "Voisins", IsGroupChat: true, Participants: []*gmproto.Participant{me, alice, bob, carl}}
)

func TestReceivedSMSInDM(t *testing.T) {
	c := newConv()
	m := msg("m1", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("Salut, tu es dispo ?"))
	r, ok := c.convert(m, dm)
	if !ok || r.Sent || r.ID != "sms:42:m1" || !r.Time.Equal(start) {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	if r.Msg.Conversation != (event.Conversation{ID: "sms:42", Kind: "dm", Name: "Alice Martin"}) {
		t.Fatalf("conversation %+v", r.Msg.Conversation)
	}
	if r.Msg.Sender != (event.Sender{ID: "sms:+33611111111", Name: "Alice Martin", Known: true}) {
		t.Fatalf("sender %+v", r.Msg.Sender)
	}
	if r.Msg.Text != "Salut, tu es dispo ?" || len(r.Msg.Attachments) != 0 || r.Msg.ReplyTo != "" {
		t.Fatalf("message %+v", r.Msg)
	}
}

func TestGroupMMSWithImage(t *testing.T) {
	c := newConv()
	m := msg("m2", "77", "3", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("Regardez"), media("image/jpeg", "IMG_01.jpg", gmproto.MediaFormats_IMAGE_JPEG))
	r, ok := c.convert(m, group)
	if !ok || r.Msg.Conversation.Kind != "group" || r.Msg.Conversation.ID != "sms:77" || r.Msg.Conversation.Name != "Voisins" {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	// Bob is not in the contacts and his number is national: +33 via TWALK_SMS_DEFAULT_CC.
	if r.Msg.Sender != (event.Sender{ID: "sms:+33622222222"}) {
		t.Fatalf("sender %+v", r.Msg.Sender)
	}
	if len(r.Msg.Attachments) != 1 || r.Msg.Attachments[0] != (event.Attachment{Kind: "image", MimeType: "image/jpeg", FileName: "IMG_01.jpg"}) {
		t.Fatalf("attachments %+v", r.Msg.Attachments)
	}
	// Participant without a number: the Google participant id is the sender id.
	r, _ = c.convert(msg("m3", "77", "4", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("hello")), group)
	if r.Msg.Sender.ID != "sms:4" || r.Msg.Sender.Known {
		t.Fatalf("sender %+v", r.Msg.Sender)
	}
	// Media-only MMS: no text, one attachment typed from the format.
	r, ok = c.convert(msg("m4", "77", "2", gmproto.MessageStatusType_INCOMING_DELIVERED, start, media("", "", gmproto.MediaFormats_VIDEO_MP4)), group)
	if !ok || r.Msg.Text != "" || r.Msg.Attachments[0].Kind != "video" {
		t.Fatalf("%+v", r)
	}
}

func TestSentMessage(t *testing.T) {
	c := newConv()
	m := msg("m5", "42", "1", gmproto.MessageStatusType_OUTGOING_COMPLETE, start, text("Oui, 18h"))
	m.ReplyMessage = &gmproto.ReplyMessage{MessageID: "m1"}
	r, ok := c.convert(m, dm)
	if !ok || !r.Sent || r.ID != "sms:42:m5" || r.Msg.ReplyTo != "m1" {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	if r.Msg.Sender != (event.Sender{ID: "sms:1", Known: true}) {
		t.Fatalf("sender %+v", r.Msg.Sender)
	}
}

func TestStatusUpdatesPublishOnce(t *testing.T) {
	c := newConv()
	for _, st := range []gmproto.MessageStatusType{
		gmproto.MessageStatusType_OUTGOING_YET_TO_SEND, gmproto.MessageStatusType_OUTGOING_SENDING,
		gmproto.MessageStatusType_OUTGOING_FAILED_GENERIC, gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD, gmproto.MessageStatusType_TOMBSTONE_PARTICIPANT_LEFT,
		gmproto.MessageStatusType_MESSAGE_STATUS_TOMBSTONE_ENCRYPTED_GROUP_CREATED, gmproto.MessageStatusType_INCOMING_DELETED,
	} {
		if c.wanted(msg("m6", "42", "1", st, start, text("x"))) {
			t.Errorf("status %v must not be published", st)
		}
	}
	// sending -> sent: published; delivered/displayed updates afterwards: not again.
	if !c.wanted(msg("m6", "42", "1", gmproto.MessageStatusType_OUTGOING_COMPLETE, start, text("x"))) {
		t.Fatal("complete not wanted")
	}
	c.seen.add("sms:42:m6")
	for _, st := range []gmproto.MessageStatusType{gmproto.MessageStatusType_OUTGOING_COMPLETE, gmproto.MessageStatusType_OUTGOING_DELIVERED, gmproto.MessageStatusType_OUTGOING_DISPLAYED} {
		if c.wanted(msg("m6", "42", "1", st, start, text("x"))) {
			t.Errorf("status %v republished", st)
		}
	}
	// An MMS that finishes downloading is published at INCOMING_COMPLETE only.
	if !c.wanted(msg("m7", "77", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("x"))) {
		t.Fatal("incoming complete not wanted")
	}
}

func TestOldMessagesIgnored(t *testing.T) {
	c := newConv()
	old := msg("m8", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start.Add(-maxReplayAge-time.Second), text("replay"))
	if c.wanted(old) {
		t.Fatal("message older than start-2min published")
	}
	if !c.wanted(msg("m9", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start.Add(-maxReplayAge+time.Second), text("ok"))) {
		t.Fatal("message within 2 minutes of start ignored")
	}
	if c.wanted(msg("", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("x"))) || c.wanted(msg("m", "", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("x"))) {
		t.Fatal("message without ids accepted")
	}
}

func TestNothingToPublish(t *testing.T) {
	c := newConv()
	if _, ok := c.convert(msg("a", "42", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start), dm); ok {
		t.Error("empty message published")
	}
	spam := &gmproto.Conversation{ConversationID: "9", Status: gmproto.ConversationStatus_SPAM_FOLDER, Participants: dm.Participants}
	if _, ok := c.convert(msg("b", "9", "2", gmproto.MessageStatusType_INCOMING_COMPLETE, start, text("promo")), spam); ok {
		t.Error("spam published")
	}
}

func TestSeenSetBounded(t *testing.T) {
	s := newSeenSet(2)
	s.add("a")
	s.add("b")
	s.add("c")
	if !s.has("c") || len(s.m) > 2 {
		t.Fatalf("%v", s.m)
	}
}

func TestE164(t *testing.T) {
	c := converter{defaultCC: "33"}
	for in, want := range map[string]string{
		"+33612345678": "+33612345678", "+33 6 12 34 56 78": "+33612345678", "0033612345678": "+33612345678",
		"06 12 34 56 78": "+33612345678", "(+1) 555-123-4567": "+15551234567", "36105": "", "GOOGLE": "", "a@b.fr": "", "": "",
	} {
		if got := c.e164(in); got != want {
			t.Errorf("e164(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (&converter{}).e164("06 12 34 56 78"); got != "" {
		t.Errorf("national number without default country code: %q", got)
	}
}

func TestAttKind(t *testing.T) {
	for _, c := range []struct {
		mime string
		f    gmproto.MediaFormats
		want string
	}{
		{"image/png", 0, "image"}, {"video/mp4", 0, "video"}, {"audio/amr", 0, "audio"}, {"text/x-vcard", 0, "contact"},
		{"application/pdf", gmproto.MediaFormats_APP_PDF, "document"}, {"", gmproto.MediaFormats_IMAGE_GIF, "image"},
		{"", gmproto.MediaFormats_AUDIO_AMR, "audio"}, {"", gmproto.MediaFormats_APP_ZIP, "document"}, {"", 0, "other"},
	} {
		if got := attKind(c.mime, c.f); got != c.want {
			t.Errorf("attKind(%q,%v) = %q, want %q", c.mime, c.f, got, c.want)
		}
	}
}

func TestFlagsStatus(t *testing.T) {
	for _, c := range []struct {
		f      flags
		state  string
		detail string
	}{
		{flags{}, event.StateDisconnected, "connexion en cours"},
		{flags{ready: true}, event.StateConnected, ""},
		{flags{ready: true, listenErr: true}, event.StateDisconnected, "écoute Google Messages interrompue, nouvelle tentative"},
		{flags{ready: true, phoneDown: true}, event.StateDisconnected, "téléphone injoignable (Google Messages ne répond pas)"},
		{flags{ready: true, inactive: detailElsewhere}, event.StateDisconnected, "session Messages Web active ailleurs"},
	} {
		if s := c.f.status(); s.State != c.state || s.Detail != c.detail {
			t.Errorf("%+v -> %+v", c.f, s)
		}
	}
}
