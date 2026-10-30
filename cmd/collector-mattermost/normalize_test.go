package main

import (
	"encoding/json"
	"testing"

	"github.com/mmaudet/twalk/internal/event"
)

const me = "me00000000000000000000000a"

type fakeRes struct {
	chans map[string]chanInfo
	users map[string]string
}

func (f fakeRes) channel(id string) (chanInfo, bool) { c, ok := f.chans[id]; return c, ok }
func (f fakeRes) userName(id string) string          { return f.users[id] }

var res = fakeRes{
	chans: map[string]chanInfo{
		"chD": {Type: "D", Name: "bob0000000000000000000000b__" + me},
		"chG": {Type: "G", DisplayName: "alice, bob"},
		"chP": {Type: "P", DisplayName: "Équipe privée", Name: "equipe"},
	},
	users: map[string]string{"bob0000000000000000000000b": "bob", me: "michel"},
}

// wsJSON builds a WebSocket frame the way the server does (nested JSON
// documents in strings), then decodes it with the production struct.
func wsJSON(t *testing.T, evName, channelID string, data map[string]any) wsEvent {
	t.Helper()
	frame := map[string]any{
		"event": evName, "data": data, "seq": 12,
		"broadcast": map[string]any{"omit_users": nil, "user_id": "", "channel_id": channelID, "team_id": ""},
	}
	b, _ := json.Marshal(frame)
	var ev wsEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func enc(v any) string { b, _ := json.Marshal(v); return string(b) }

func postData(p map[string]any, extra map[string]any) map[string]any {
	d := map[string]any{"post": enc(p), "set_online": true, "team_id": "t1"}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func TestPostedKinds(t *testing.T) {
	tests := []struct {
		name, chID, typ, display, sender string
		wantKind, wantName               string
	}{
		{"dm", "chD", "D", "bob", "@bob", event.KindDM, "bob"},
		{"group", "chG", "G", "alice, bob", "@alice", event.KindGroup, "alice, bob"},
		{"open", "chO", "O", "Town Square", "@alice", event.KindGroup, "Town Square"},
		{"private", "chP", "P", "Équipe privée", "@alice", event.KindGroup, "Équipe privée"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := wsJSON(t, "posted", tc.chID, postData(
				map[string]any{"id": "p1", "create_at": 1761800000123, "user_id": "u1", "channel_id": tc.chID, "message": "salut", "type": ""},
				map[string]any{"channel_type": tc.typ, "channel_display_name": tc.display, "sender_name": tc.sender, "mentions": "[]"}))
			n, ok := normalize(ev, me, res)
			if !ok || n.Msg == nil {
				t.Fatal("ignored")
			}
			m := n.Msg
			if m.Conversation.Kind != tc.wantKind || m.Conversation.Name != tc.wantName || m.Conversation.ID != "mattermost:"+tc.chID {
				t.Errorf("conversation = %+v", m.Conversation)
			}
			if n.ID != "mattermost:"+tc.chID+":p1" || n.Sent || m.Edited || m.Text != "salut" {
				t.Errorf("n = %+v msg = %+v", n, m)
			}
			if m.Sender.ID != "mattermost:u1" || m.Sender.Name != tc.sender[1:] {
				t.Errorf("sender = %+v", m.Sender)
			}
			if n.Time.UnixMilli() != 1761800000123 {
				t.Errorf("time = %v", n.Time)
			}
		})
	}
}

func TestPostedDMNameFromCache(t *testing.T) {
	// No channel_display_name: the DM is named after the other participant.
	ev := wsJSON(t, "posted", "chD", postData(
		map[string]any{"id": "p1", "user_id": me, "channel_id": "chD", "message": "x"},
		map[string]any{"channel_type": "D", "sender_name": "@michel"}))
	n, ok := normalize(ev, me, res)
	if !ok || n.Msg.Conversation.Name != "bob" || !n.Sent || !n.Msg.Sender.Known {
		t.Fatalf("n = %+v ok=%v", n, ok)
	}
}

func TestMentionAndReply(t *testing.T) {
	p := map[string]any{"id": "p2", "user_id": "u1", "channel_id": "chG", "message": "@michel ?", "root_id": "p0"}
	for _, tc := range []struct {
		mentions string
		want     bool
	}{
		{enc([]string{"zzz", me}), true},
		{enc([]string{"zzz"}), false},
		{"", false},
		{"pas du json", false},
	} {
		ev := wsJSON(t, "posted", "chG", postData(p, map[string]any{"channel_type": "G", "channel_display_name": "g", "sender_name": "@a", "mentions": tc.mentions}))
		n, ok := normalize(ev, me, res)
		if !ok || n.Msg.MentionsOwner != tc.want || n.Msg.ReplyTo != "p0" {
			t.Errorf("mentions %q: %+v ok=%v", tc.mentions, n.Msg, ok)
		}
	}
}

func TestSystemAndEmptyIgnored(t *testing.T) {
	base := func(p map[string]any) wsEvent {
		return wsJSON(t, "posted", "chG", postData(p, map[string]any{"channel_type": "G", "channel_display_name": "g"}))
	}
	for name, p := range map[string]map[string]any{
		"system_join":  {"id": "p", "user_id": "u", "channel_id": "chG", "message": "a rejoint", "type": "system_join_channel"},
		"system_other": {"id": "p", "user_id": "u", "channel_id": "chG", "message": "x", "type": "system_header_change"},
		"empty":        {"id": "p", "user_id": "u", "channel_id": "chG", "message": ""},
		"deleted":      {"id": "p", "user_id": "u", "channel_id": "chG", "message": "x", "delete_at": 5},
		"no id":        {"user_id": "u", "channel_id": "chG", "message": "x"},
	} {
		if _, ok := normalize(base(p), me, res); ok {
			t.Errorf("%s not ignored", name)
		}
	}
	// A non-system custom type is kept.
	if _, ok := normalize(base(map[string]any{"id": "p", "user_id": "u", "channel_id": "chG", "message": "x", "type": "slack_attachment"}), me, res); !ok {
		t.Error("slack_attachment ignored")
	}
	if _, ok := normalize(wsJSON(t, "typing", "chG", map[string]any{"user_id": "u"}), me, res); ok {
		t.Error("typing not ignored")
	}
}

func TestAttachments(t *testing.T) {
	ev := wsJSON(t, "posted", "chP", postData(map[string]any{
		"id": "p3", "user_id": "u1", "channel_id": "chP", "message": "voir",
		"file_ids": []string{"f1", "f2", "f3", "f4", "f5"},
		"metadata": map[string]any{"files": []map[string]any{
			{"id": "f1", "name": "a.png", "extension": "png", "size": 10, "mime_type": "image/png"},
			{"id": "f2", "name": "b.mp4", "mime_type": "video/mp4"},
			{"id": "f3", "name": "c.ogg", "mime_type": "audio/ogg"},
			{"id": "f4", "name": "d.pdf", "mime_type": "application/pdf"},
			{"id": "f5", "name": "e", "mime_type": ""},
		}},
	}, map[string]any{"channel_type": "P", "channel_display_name": "Équipe privée", "sender_name": "@a"}))
	n, ok := normalize(ev, me, res)
	if !ok {
		t.Fatal("ignored")
	}
	var kinds []string
	for _, a := range n.Msg.Attachments {
		kinds = append(kinds, a.Kind)
		if a.Caption != "voir" {
			t.Errorf("caption %q", a.Caption)
		}
	}
	want := []string{"image", "video", "audio", "document", "other"}
	if len(kinds) != 5 {
		t.Fatalf("kinds = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds = %v", kinds)
		}
	}
	if n.Msg.Attachments[0].FileName != "a.png" || n.Msg.Attachments[0].MimeType != "image/png" {
		t.Errorf("att0 = %+v", n.Msg.Attachments[0])
	}
}

func TestPostEdited(t *testing.T) {
	// post_edited carries only data.post: the channel and sender are resolved.
	ev := wsJSON(t, "post_edited", "chD", map[string]any{"post": enc(map[string]any{
		"id": "p1", "create_at": 1761800000123, "update_at": 1761800999000, "edit_at": 1761800999000,
		"user_id": "bob0000000000000000000000b", "channel_id": "chD", "message": "corrigé",
	})})
	n, ok := normalize(ev, me, res)
	if !ok {
		t.Fatal("ignored")
	}
	if n.ID != "mattermost:chD:p1:edit:1761800999000" || !n.Msg.Edited || n.Msg.Conversation.Kind != event.KindDM ||
		n.Msg.Sender.Name != "bob" || n.Msg.Conversation.Name != "bob" || n.Time.UnixMilli() != 1761800999000 {
		t.Errorf("n = %+v msg = %+v", n, n.Msg)
	}
	// Unknown channel and no type in the event: nothing can be said.
	ev = wsJSON(t, "post_edited", "chX", map[string]any{"post": enc(map[string]any{"id": "p", "user_id": "u", "channel_id": "chX", "message": "x", "edit_at": 1})})
	if _, ok := normalize(ev, me, res); ok {
		t.Error("unknown channel not ignored")
	}
}

func TestReactionAdded(t *testing.T) {
	ev := wsJSON(t, "reaction_added", "chG", map[string]any{"reaction": enc(map[string]any{
		"user_id": me, "post_id": "p9", "emoji_name": "+1", "create_at": 1761801000000,
	})})
	n, ok := normalize(ev, me, res)
	if !ok || n.React == nil {
		t.Fatal("ignored")
	}
	r := n.React
	if r.TargetID != "p9" || r.Emoji != "+1" || !r.FromOwner || r.Conversation.ID != "mattermost:chG" ||
		r.Conversation.Kind != event.KindGroup || r.Sender.ID != "mattermost:"+me || r.Sender.Name != "michel" {
		t.Errorf("r = %+v", r)
	}
	if n.ID != "mattermost:chG:p9:react:"+me+":+1:1761801000000" {
		t.Errorf("id = %s", n.ID)
	}
	// reaction_removed is not collected.
	ev.Event = "reaction_removed"
	if _, ok := normalize(ev, me, res); ok {
		t.Error("reaction_removed collected")
	}
}
