// Package event is the Twalk v2 bus contract: CloudEvents envelopes, NATS
// subjects, source-side exclusions and a JetStream publisher shared by every
// collector. See docs/02-spec-twalk-v2.md, section 6.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Networks a collector may publish for.
const (
	WhatsApp   = "whatsapp"
	Telegram   = "telegram"
	Signal     = "signal"
	Mattermost = "mattermost"
	Matrix     = "matrix"
)

// Conversation kinds.
const (
	KindDM      = "dm"
	KindGroup   = "group"
	KindChannel = "channel"
)

// CloudEvents types (fr.linagora.twalk.*.v2).
const (
	TypeMessageReceived  = "fr.linagora.twalk.message.received.v2"
	TypeMessageSent      = "fr.linagora.twalk.message.sent.v2"
	TypeReactionAdded    = "fr.linagora.twalk.reaction.added.v2"
	TypeCollectorStatus  = "fr.linagora.twalk.collector.status.v2"
	TypeTriageLabeled    = "fr.linagora.twalk.triage.labeled.v2"
	TypeSuggestionProduc = "fr.linagora.twalk.suggestion.produced.v2"
)

// Collector states carried by collector.status.
const (
	StateConnected    = "connected"
	StateDisconnected = "disconnected"
	StateLoggedOut    = "logged_out" // session expired: the owner must pair again
	StateStopping     = "stopping"
)

var networks = map[string]bool{WhatsApp: true, Telegram: true, Signal: true, Mattermost: true, Matrix: true}
var kinds = map[string]bool{KindDM: true, KindGroup: true, KindChannel: true}

// Envelope is a structured-mode CloudEvent 1.0 with the `network` extension.
type Envelope struct {
	SpecVersion     string    `json:"specversion"`
	Type            string    `json:"type"`
	Source          string    `json:"source"`
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	Network         string    `json:"network"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

type Conversation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type Sender struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Known bool   `json:"known"` // in the owner's contacts on that network
}

type Attachment struct {
	Kind     string `json:"kind"` // image, video, audio, voice, document, sticker, location, contact, other
	Caption  string `json:"caption,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileName string `json:"file_name,omitempty"`
}

// Message is the data of message.received and message.sent.
type Message struct {
	Conversation  Conversation `json:"conversation"`
	Sender        Sender       `json:"sender"`
	Text          string       `json:"text,omitempty"`
	ReplyTo       string       `json:"reply_to,omitempty"` // network message id
	MentionsOwner bool         `json:"mentions_owner"`
	Forwarded     bool         `json:"forwarded,omitempty"`
	Edited        bool         `json:"edited,omitempty"`
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// Reaction is the data of reaction.added. An empty Emoji means removed.
type Reaction struct {
	Conversation Conversation `json:"conversation"`
	Sender       Sender       `json:"sender"`
	TargetID     string       `json:"target_id"`
	Emoji        string       `json:"emoji"`
	FromOwner    bool         `json:"from_owner"`
}

// Status is the data of collector.status.
type Status struct {
	State   string `json:"state"`
	Account string `json:"account,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// ConvHash is the short stable token used in subjects for a conversation id.
func ConvHash(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return hex.EncodeToString(sum[:])[:12]
}

// MessageSubject: twalk2.msg.<received|sent>.<network>.<kind>.<conv>
func MessageSubject(sent bool, network, kind, conversationID string) string {
	dir := "received"
	if sent {
		dir = "sent"
	}
	return fmt.Sprintf("twalk2.msg.%s.%s.%s.%s", dir, network, kind, ConvHash(conversationID))
}

// ReactionSubject: twalk2.reaction.<network>.<conv>
func ReactionSubject(network, conversationID string) string {
	return fmt.Sprintf("twalk2.reaction.%s.%s", network, ConvHash(conversationID))
}

// StatusSubject: twalk2.collector.status.<network>
func StatusSubject(network string) string {
	return "twalk2.collector.status." + network
}

func validate(network string, c *Conversation) error {
	if !networks[network] {
		return fmt.Errorf("unknown network %q", network)
	}
	if c == nil {
		return nil
	}
	if c.ID == "" {
		return fmt.Errorf("empty conversation id")
	}
	if !kinds[c.Kind] {
		return fmt.Errorf("unknown conversation kind %q", c.Kind)
	}
	return nil
}
