package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"maunium.net/go/mautrix/crypto"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/mmaudet/twalk/internal/event"
)

const (
	me    = id.UserID("@mmaudet:linagora.com")
	alice = id.UserID("@alice:linagora.com")
	bob   = id.UserID("@bob:example.org")
	rid   = id.RoomID("!abc:linagora.com")
)

func owner0() *owner {
	return &owner{ID: me, Name: "Michel-Marie Maudet", Directs: map[id.UserID]bool{alice: true}}
}

func group() *room {
	r := newRoom()
	r.Members = map[id.UserID]string{me: "Michel-Marie Maudet", alice: "Alice", bob: "Bob"}
	return r
}

func msg(sender id.UserID, c *mevent.MessageEventContent) *mevent.Event {
	return &mevent.Event{ID: "$ev1", RoomID: rid, Sender: sender, Type: mevent.EventMessage,
		Timestamp: 1_790_000_000_000, Content: mevent.Content{Parsed: c}}
}

func TestNormalizeMessage(t *testing.T) {
	text := func(b string) *mevent.MessageEventContent {
		return &mevent.MessageEventContent{MsgType: mevent.MsgText, Body: b}
	}
	tests := []struct {
		name  string
		evt   *mevent.Event
		room  *room
		check func(t *testing.T, n normalized)
	}{
		{"text group", msg(bob, text("salut")), group(), func(t *testing.T, n normalized) {
			if n.ID != "matrix:!abc:linagora.com:$ev1" || n.Sent || n.Msg.Text != "salut" {
				t.Fatalf("%+v", n)
			}
			c := n.Msg.Conversation
			if c.ID != "matrix:"+string(rid) || c.Kind != event.KindGroup || c.Name != "Alice, Bob" {
				t.Fatalf("conv %+v", c)
			}
			if s := n.Msg.Sender; s.ID != "matrix:@bob:example.org" || s.Name != "Bob" || s.Known {
				t.Fatalf("sender %+v", s)
			}
			if !n.Time.Equal(time.UnixMilli(1_790_000_000_000)) {
				t.Fatal(n.Time)
			}
		}},
		{"sent by owner, known peer", msg(me, text("ok")), group(), func(t *testing.T, n normalized) {
			if !n.Sent || !n.Msg.Sender.Known {
				t.Fatalf("%+v", n)
			}
		}},
		{"known sender via m.direct", msg(alice, text("hi")), group(), func(t *testing.T, n normalized) {
			if !n.Msg.Sender.Known {
				t.Fatal("alice shares a DM with the owner")
			}
		}},
		{"notice from bot published", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgNotice, Body: "build ok"}), group(),
			func(t *testing.T, n normalized) {
				if n.Msg.Text != "build ok" {
					t.Fatal(n.Msg.Text)
				}
			}},
		{"edit", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgText, Body: "* corrigé",
			RelatesTo:  (&mevent.RelatesTo{}).SetReplace("$orig"),
			NewContent: text("corrigé")}), group(), func(t *testing.T, n normalized) {
			if n.ID != "matrix:!abc:linagora.com:$orig:edit:1790000000000" || !n.Msg.Edited || n.Msg.Text != "corrigé" {
				t.Fatalf("%+v %+v", n, n.Msg)
			}
		}},
		{"reply strips fallback", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgText,
			Body:      "> <@mmaudet:linagora.com> question\n\nréponse",
			RelatesTo: (&mevent.RelatesTo{}).SetReplyTo("$q")}), group(), func(t *testing.T, n normalized) {
			if n.Msg.ReplyTo != "$q" || n.Msg.Text != "réponse" || n.Msg.MentionsOwner {
				t.Fatalf("%+v", n.Msg)
			}
		}},
		{"mention via m.mentions", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgText, Body: "hey",
			Mentions: &mevent.Mentions{UserIDs: []id.UserID{me}}}), group(), func(t *testing.T, n normalized) {
			if !n.Msg.MentionsOwner {
				t.Fatal("expected mention")
			}
		}},
		{"mention via user id in body", msg(bob, text("ping @mmaudet:linagora.com")), group(), func(t *testing.T, n normalized) {
			if !n.Msg.MentionsOwner {
				t.Fatal("expected mention")
			}
		}},
		{"mention via displayname", msg(bob, text("merci michel-marie maudet !")), group(), func(t *testing.T, n normalized) {
			if !n.Msg.MentionsOwner {
				t.Fatal("expected mention")
			}
		}},
		{"no mention", msg(bob, text("bonjour tout le monde")), group(), func(t *testing.T, n normalized) {
			if n.Msg.MentionsOwner {
				t.Fatal("unexpected mention")
			}
		}},
		{"image with caption", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgImage, Body: "la photo",
			FileName: "p.jpg", Info: &mevent.FileInfo{MimeType: "image/jpeg"}}), group(), func(t *testing.T, n normalized) {
			a := n.Msg.Attachments
			if len(a) != 1 || a[0].Kind != "image" || a[0].MimeType != "image/jpeg" || a[0].FileName != "p.jpg" ||
				a[0].Caption != "la photo" || n.Msg.Text != "la photo" {
				t.Fatalf("%+v", n.Msg)
			}
		}},
		{"legacy file: body is the name", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgFile, Body: "doc.pdf"}), group(),
			func(t *testing.T, n normalized) {
				a := n.Msg.Attachments
				if len(a) != 1 || a[0].Kind != "document" || a[0].FileName != "doc.pdf" || n.Msg.Text != "" {
					t.Fatalf("%+v", n.Msg)
				}
			}},
		{"voice note", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgAudio, Body: "v.ogg",
			MSC3245Voice: &mevent.MSC3245Voice{}}), group(), func(t *testing.T, n normalized) {
			if n.Msg.Attachments[0].Kind != "voice" {
				t.Fatalf("%+v", n.Msg)
			}
		}},
		{"location", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgLocation, Body: "Linagora, Paris",
			GeoURI: "geo:48.8,2.3"}), group(), func(t *testing.T, n normalized) {
			a := n.Msg.Attachments
			if len(a) != 1 || a[0].Kind != "location" || a[0].Caption != "Linagora, Paris" {
				t.Fatalf("%+v", n.Msg)
			}
		}},
		{"video", msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgVideo, Body: "v.mp4"}), group(), func(t *testing.T, n normalized) {
			if n.Msg.Attachments[0].Kind != "video" {
				t.Fatalf("%+v", n.Msg)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := normalize(tc.evt, tc.room, owner0())
			if !ok {
				t.Fatal("event dropped")
			}
			tc.check(t, n)
		})
	}
}

func TestNormalizeReaction(t *testing.T) {
	e := &mevent.Event{ID: "$r1", RoomID: rid, Sender: me, Type: mevent.EventReaction, Timestamp: 5000,
		Content: mevent.Content{Parsed: &mevent.ReactionEventContent{RelatesTo: *(&mevent.RelatesTo{}).SetAnnotation("$ev1", "👍")}}}
	n, ok := normalize(e, group(), owner0())
	if !ok || n.Reaction == nil || n.Msg != nil {
		t.Fatalf("%+v %v", n, ok)
	}
	r := n.Reaction
	if n.ID != "matrix:!abc:linagora.com:$r1" || r.TargetID != "$ev1" || r.Emoji != "👍" || !r.FromOwner {
		t.Fatalf("%+v id=%s", r, n.ID)
	}
	// A reaction-typed event without an annotation relation is dropped.
	e.Content.Parsed = &mevent.ReactionEventContent{}
	if _, ok := normalize(e, group(), owner0()); ok {
		t.Fatal("malformed reaction kept")
	}
}

func TestNormalizeIgnored(t *testing.T) {
	text := &mevent.MessageEventContent{MsgType: mevent.MsgText, Body: "x"}
	bridged := group()
	bridged.apply(&mevent.Event{Type: mevent.StateHalfShotBridge, StateKey: ptr("whatsapp")})
	if _, ok := normalize(msg(bob, text), bridged, owner0()); ok {
		t.Fatal("bridged room must be ignored")
	}
	if _, ok := normalize(msg(bob, &mevent.MessageEventContent{MsgType: mevent.MsgText}), group(), owner0()); ok {
		t.Fatal("empty message kept")
	}
	if _, ok := normalize(&mevent.Event{ID: "$x", RoomID: rid, Sender: bob, Type: mevent.EventMessage}, group(), owner0()); ok {
		t.Fatal("event without parsed content kept")
	}
}

func ptr(s string) *string { return &s }

func TestDMDetection(t *testing.T) {
	two := newRoom()
	two.Members = map[id.UserID]string{me: "M", alice: "Alice"}
	if !isDM(two) {
		t.Fatal("two joined members is a DM")
	}
	if convName(two, me) != "Alice" {
		t.Fatalf("name %q", convName(two, me))
	}
	n, _ := normalize(msg(alice, &mevent.MessageEventContent{MsgType: mevent.MsgText, Body: "yo"}), two, owner0())
	if n.Msg.Conversation.Kind != event.KindDM {
		t.Fatalf("%+v", n.Msg.Conversation)
	}

	// m.direct wins even when the room has three members.
	three := group()
	three.Direct = true
	if !isDM(three) {
		t.Fatal("room listed in m.direct is a DM")
	}
	if isDM(group()) {
		t.Fatal("three members, not in m.direct: group")
	}

	// A room name beats the computed name; state events update the room.
	three.apply(&mevent.Event{Type: mevent.StateRoomName, Content: mevent.Content{Parsed: &mevent.RoomNameEventContent{Name: "Équipe"}}})
	if convName(three, me) != "Équipe" {
		t.Fatal(convName(three, me))
	}
	three.apply(&mevent.Event{Type: mevent.StateMember, StateKey: ptr(string(bob)),
		Content: mevent.Content{Parsed: &mevent.MemberEventContent{Membership: mevent.MembershipLeave}}})
	if three.joined() != 2 {
		t.Fatal("leave not applied")
	}
}

// TestCryptoStoreOpens opens crypto.db with the pure-Go SQLite driver and the
// goolm Olm machine against a fake homeserver, then reopens it: the device
// keys and the pickle key survive, file modes are 0600.
func TestCryptoStoreOpens(t *testing.T) {
	var uploads int
	var devKeys json.RawMessage // what the fake server stores for our device
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/keys/query":
			resp := map[string]any{}
			if devKeys != nil {
				resp["device_keys"] = map[string]any{string(me): map[string]any{"TWALKTEST": devKeys}}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/_matrix/client/v3/keys/upload":
			var up struct {
				DeviceKeys json.RawMessage `json:"device_keys"`
			}
			_ = json.NewDecoder(r.Body).Decode(&up)
			if len(up.DeviceKeys) > 0 {
				devKeys = up.DeviceKeys
			}
			uploads++
			_ = json.NewEncoder(w).Encode(map[string]any{"one_time_key_counts": map[string]int{"signed_curve25519": 50}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "{}", 404)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	s := &session{UserID: me, DeviceID: "TWALKTEST", AccessToken: "tok", Homeserver: srv.URL}
	if err := saveSession(dir, s); err != nil {
		t.Fatal(err)
	}
	cli, err := newClient(s)
	if err != nil {
		t.Fatal(err)
	}
	if cli.SyncPresence != mevent.PresenceOffline {
		t.Fatal("sync must be offline")
	}
	ctx := context.Background()
	ch, err := openCrypto(ctx, cli, dir)
	if err != nil {
		t.Fatal(err)
	}
	if uploads != 1 {
		t.Fatalf("device keys uploads = %d", uploads)
	}
	key1, _ := os.ReadFile(filepath.Join(dir, "pickle.key"))
	ch.Close()
	for _, f := range []string{"session.json", "crypto.db", "pickle.key"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	got, err := loadSession(dir)
	if err != nil || *got != *s {
		t.Fatalf("session %+v %v", got, err)
	}

	// Reopen: the server now knows our device keys; nothing is uploaded again
	// and the pickle key is unchanged.
	cli2, _ := newClient(s)
	ch2, err := openCrypto(ctx, cli2, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ch2.Close()
	if uploads != 1 {
		t.Fatalf("uploads after reopen = %d", uploads)
	}
	if _, isSQL := cli2.Store.(*crypto.SQLCryptoStore); !isSQL {
		t.Fatalf("sync token store is %T, want the crypto database", cli2.Store)
	}
	key2, _ := os.ReadFile(filepath.Join(dir, "pickle.key"))
	if string(key1) != string(key2) {
		t.Fatal("pickle key regenerated")
	}
}
