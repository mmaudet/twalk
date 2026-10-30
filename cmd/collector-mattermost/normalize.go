package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/mmaudet/twalk/internal/event"
)

// wsEvent is one Mattermost WebSocket event (API v4). data.post, data.mentions
// and data.reaction are JSON documents encoded inside strings.
type wsEvent struct {
	Event string `json:"event"`
	Data  struct {
		ChannelDisplayName string `json:"channel_display_name"`
		ChannelType        string `json:"channel_type"`
		Mentions           string `json:"mentions"`
		Post               string `json:"post"`
		Reaction           string `json:"reaction"`
		SenderName         string `json:"sender_name"`
	} `json:"data"`
	Broadcast struct {
		ChannelID string `json:"channel_id"`
	} `json:"broadcast"`
	// Authentication reply (no "event" field).
	Status   string `json:"status"`
	SeqReply int    `json:"seq_reply"`
}

type post struct {
	ID        string `json:"id"`
	CreateAt  int64  `json:"create_at"`
	UpdateAt  int64  `json:"update_at"`
	EditAt    int64  `json:"edit_at"`
	DeleteAt  int64  `json:"delete_at"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	Message   string `json:"message"`
	Type      string `json:"type"`
	Metadata  struct {
		Files []struct {
			Name     string `json:"name"`
			MimeType string `json:"mime_type"`
		} `json:"files"`
	} `json:"metadata"`
}

type reaction struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
	CreateAt  int64  `json:"create_at"`
}

// chanInfo is what GET /channels/{id} tells us (type O, P, D or G).
type chanInfo struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
}

// resolver looks channels and users up (cached, read-only GETs). A miss
// returns ok=false / "" and the event is then published with empty names.
type resolver interface {
	channel(id string) (chanInfo, bool)
	userName(id string) string
}

// normalized is one publishable event: exactly one of Msg / React is set.
type normalized struct {
	ID    string
	Time  time.Time
	Sent  bool
	Msg   *event.Message
	React *event.Reaction
}

// normalize converts a posted / post_edited / reaction_added event. ok is
// false for everything else: other events, system posts, deleted or empty
// posts, unknown channels.
func normalize(ev wsEvent, selfID string, res resolver) (n normalized, ok bool) {
	switch ev.Event {
	case "posted", "post_edited":
		return normalizePost(ev, selfID, res)
	case "reaction_added":
		return normalizeReaction(ev, selfID, res)
	}
	return n, false
}

func normalizePost(ev wsEvent, selfID string, res resolver) (n normalized, ok bool) {
	var p post
	if json.Unmarshal([]byte(ev.Data.Post), &p) != nil || p.ID == "" || p.DeleteAt != 0 {
		return n, false
	}
	if p.Type != "" && strings.HasPrefix(p.Type, "system_") {
		return n, false
	}
	atts := attachmentsOf(p)
	if p.Message == "" && len(atts) == 0 {
		return n, false
	}
	chID := p.ChannelID
	if chID == "" {
		chID = ev.Broadcast.ChannelID
	}
	conv, ok := conversationOf(chID, p.UserID, ev, selfID, res)
	if !ok {
		return n, false
	}
	if p.Message != "" {
		for i := range atts {
			atts[i].Caption = p.Message
		}
	}
	edited := ev.Event == "post_edited"
	id, t := "mattermost:"+chID+":"+p.ID, time.UnixMilli(p.CreateAt)
	if edited {
		at := p.EditAt
		if at == 0 {
			at = p.UpdateAt
		}
		id, t = id+":edit:"+itoa(at), time.UnixMilli(at)
	}
	name := strings.TrimPrefix(ev.Data.SenderName, "@")
	if name == "" {
		name = res.userName(p.UserID)
	}
	mine := p.UserID == selfID
	msg := event.Message{
		Conversation:  conv,
		Sender:        event.Sender{ID: "mattermost:" + p.UserID, Name: name, Known: mine || conv.Kind == event.KindDM},
		Text:          p.Message,
		ReplyTo:       p.RootID,
		MentionsOwner: mentions(ev.Data.Mentions, selfID),
		Edited:        edited,
		Attachments:   atts,
	}
	return normalized{ID: id, Time: t, Sent: mine, Msg: &msg}, true
}

func normalizeReaction(ev wsEvent, selfID string, res resolver) (n normalized, ok bool) {
	var r reaction
	if json.Unmarshal([]byte(ev.Data.Reaction), &r) != nil || r.PostID == "" || r.EmojiName == "" {
		return n, false
	}
	conv, ok := conversationOf(ev.Broadcast.ChannelID, r.UserID, ev, selfID, res)
	if !ok {
		return n, false
	}
	mine := r.UserID == selfID
	return normalized{
		ID:   "mattermost:" + ev.Broadcast.ChannelID + ":" + r.PostID + ":react:" + r.UserID + ":" + r.EmojiName + ":" + itoa(r.CreateAt),
		Time: time.UnixMilli(r.CreateAt),
		React: &event.Reaction{
			Conversation: conv,
			Sender:       event.Sender{ID: "mattermost:" + r.UserID, Name: res.userName(r.UserID), Known: mine || conv.Kind == event.KindDM},
			TargetID:     r.PostID,
			Emoji:        r.EmojiName,
			FromOwner:    mine,
		},
	}, true
}

// conversationOf maps a channel to a conversation. D → dm (named after the
// other participant), G → group, O and P → group. The type comes from the
// event when it has one (posted), else from GET /channels/{id}.
func conversationOf(chID, fromID string, ev wsEvent, selfID string, res resolver) (event.Conversation, bool) {
	if chID == "" {
		return event.Conversation{}, false
	}
	typ, name := ev.Data.ChannelType, ev.Data.ChannelDisplayName
	var info chanInfo
	if typ == "" || typ == "D" || name == "" {
		var known bool
		if info, known = res.channel(chID); known && typ == "" {
			typ = info.Type
		}
	}
	if typ == "" {
		return event.Conversation{}, false
	}
	conv := event.Conversation{ID: "mattermost:" + chID, Kind: event.KindGroup, Name: name}
	if conv.Name == "" {
		conv.Name = info.DisplayName
	}
	if typ == "D" {
		conv.Kind, conv.Name = event.KindDM, ""
		for _, uid := range strings.Split(info.Name, "__") {
			if uid != selfID && uid != "" {
				conv.Name = res.userName(uid)
			}
		}
		if conv.Name == "" && fromID != selfID { // unresolved: the other side is the sender
			conv.Name = strings.TrimPrefix(ev.Data.SenderName, "@")
		}
	}
	return conv, true
}

// mentions reports whether the JSON array of user ids contains id.
func mentions(raw, id string) bool {
	var ids []string
	if raw == "" || json.Unmarshal([]byte(raw), &ids) != nil {
		return false
	}
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func attachmentsOf(p post) []event.Attachment {
	var out []event.Attachment
	for _, f := range p.Metadata.Files {
		out = append(out, event.Attachment{Kind: kindOf(f.MimeType), MimeType: f.MimeType, FileName: f.Name})
	}
	return out
}

func kindOf(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case mime == "":
		return "other"
	}
	return "document"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
