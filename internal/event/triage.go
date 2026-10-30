package event

import (
	"fmt"
	"time"
)

// Triage labels carried by triage.labeled.
const (
	LabelUrgent = "urgent"
	LabelReply  = "reply"
	LabelInfo   = "info"
	LabelNoise  = "noise"
)

// Triage is the data of triage.labeled (published by the trieur).
type Triage struct {
	SourceID      string       `json:"source_id"` // id of the message.received event
	Network       string       `json:"network"`
	Conversation  Conversation `json:"conversation"`
	Sender        Sender       `json:"sender"`
	Label         string       `json:"label"`
	Reason        string       `json:"reason"`
	By            string       `json:"by"` // "rule:<name>" or "llm:<model>"
	Excerpt       string       `json:"excerpt,omitempty"`
	MentionsOwner bool         `json:"mentions_owner"`
	ReceivedAt    time.Time    `json:"received_at"`
}

// TriageSubject: twalk2.triage.<label>.<network>.<conv>
func TriageSubject(label, network, conversationID string) string {
	return fmt.Sprintf("twalk2.triage.%s.%s.%s", label, network, ConvHash(conversationID))
}
