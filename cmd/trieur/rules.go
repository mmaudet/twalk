package main

import (
	"os"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"

	"github.com/mmaudet/twalk/internal/event"
)

// Rules is the content of rules.yaml (see rules.example.yaml).
type Rules struct {
	Muted          []string `yaml:"muted"`
	VIP            []string `yaml:"vip"`
	UrgentKeywords []string `yaml:"urgent_keywords"`
	ReplyKeywords  []string `yaml:"reply_keywords"`

	urgentRe, replyRe *regexp.Regexp // compiled on first use
	compiled          bool
}

var defaultRules = Rules{
	UrgentKeywords: []string{"urgent", "urgence", "asap", "incident", "en panne", "critique"},
	ReplyKeywords:  []string{"peux-tu", "pouvez-vous", "tu peux", "ton avis", "votre avis", "tiens-moi au courant", "merci de confirmer"},
}

// LoadRules reads path; a missing file gives the default rules.
func LoadRules(path string) (*Rules, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		r := defaultRules
		return &r, nil
	}
	if err != nil {
		return nil, err
	}
	var r Rules
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// fold lowercases and strips accents: "Échéance" -> "echeance".
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func inList(list []string, vals ...string) bool {
	for _, item := range list {
		for _, v := range vals {
			if v != "" && fold(strings.TrimSpace(item)) == fold(v) {
				return true
			}
		}
	}
	return false
}

// keywordRe matches any keyword as a whole word on folded text (an optional
// plural "s" is tolerated), so that "urgent" does not match "urgentiste".
func keywordRe(keywords []string) *regexp.Regexp {
	var alts []string
	for _, k := range keywords {
		if k = fold(strings.TrimSpace(k)); k != "" {
			alts = append(alts, regexp.QuoteMeta(k))
		}
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(` + strings.Join(alts, "|") + `)s?(?:[^\p{L}\p{N}]|$)`)
}

func hasKeyword(re *regexp.Regexp, text string) (string, bool) {
	if re == nil {
		return "", false
	}
	m := re.FindStringSubmatch(fold(text))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Decision is the outcome of the rules. Candidate means a DM label the LLM may refine.
type Decision struct {
	Label, Reason, By string
	Candidate         bool
}

// Classify applies the rules in order. replyToOwner is true when the message
// answers one of the owner's own messages.
func (r *Rules) Classify(m event.Message, replyToOwner bool) Decision {
	d := func(label, reason, rule string) Decision {
		return Decision{Label: label, Reason: reason, By: "rule:" + rule}
	}
	text := m.Text
	for _, a := range m.Attachments {
		text += " " + a.Caption
	}
	dm := m.Conversation.Kind == event.KindDM
	switch {
	case inList(r.Muted, m.Conversation.ID, m.Conversation.Name):
		return d(event.LabelNoise, "conversation en sourdine", "muted")
	case dm && inList(r.VIP, m.Sender.ID, m.Sender.Name):
		return d(event.LabelUrgent, "message direct d'un expéditeur prioritaire", "vip")
	}
	if !r.compiled {
		r.urgentRe, r.replyRe, r.compiled = keywordRe(r.UrgentKeywords), keywordRe(r.ReplyKeywords), true
	}
	if kw, ok := hasKeyword(r.urgentRe, text); ok {
		return d(event.LabelUrgent, "mot-clé urgent : "+kw, "urgent_keyword")
	}
	if kw, ok := hasKeyword(r.replyRe, text); ok {
		return d(event.LabelReply, "mot-clé de demande : "+kw, "reply_keyword")
	}
	switch {
	case !dm && m.MentionsOwner:
		return d(event.LabelReply, "vous êtes mentionné dans une conversation de groupe", "mention")
	case !dm && replyToOwner:
		return d(event.LabelReply, "réponse à un de vos messages", "reply_to_owner")
	case dm && m.Sender.Known:
		x := d(event.LabelReply, "message direct d'un contact", "dm_known")
		x.Candidate = true
		return x
	case dm:
		x := d(event.LabelInfo, "message direct d'un inconnu", "dm_unknown")
		x.Candidate = true
		return x
	case strings.TrimSpace(m.Text) == "":
		return d(event.LabelNoise, "média sans texte dans un groupe", "empty_group")
	}
	return d(event.LabelInfo, "conversation de groupe sans mention", "group_default")
}
