package main

import (
	"testing"

	"github.com/mmaudet/twalk/internal/event"
)

func msg(kind, text string, known, mention bool) event.Message {
	return event.Message{
		Conversation:  event.Conversation{ID: "whatsapp:c1", Kind: kind, Name: "Comex"},
		Sender:        event.Sender{ID: "whatsapp:33600", Name: "Paul Durand", Known: known},
		Text:          text,
		MentionsOwner: mention,
	}
}

func TestClassify(t *testing.T) {
	r := &Rules{
		Muted:          []string{"Annonces Mairie", "whatsapp:muted@g.us"},
		VIP:            []string{"gilles lehoux", "telegram:42"},
		UrgentKeywords: []string{"urgent", "échéance", "signature"},
		ReplyKeywords:  []string{"peux-tu"},
	}
	muted := msg("group", "urgent !", false, true)
	muted.Conversation.Name = "annonces mairie"
	vipGroup := msg("group", "bonjour à tous", true, false)
	vipDM := msg("dm", "salut", true, false)
	vipDM.Sender.Name = "Gilles LEHOUX"
	vipGroup.Sender.Name = "Gilles LEHOUX"
	vipByID := msg("dm", "salut", false, false)
	vipByID.Sender = event.Sender{ID: "telegram:42"}
	vipByID.Sender.Name = "Inconnu"

	cases := []struct {
		name      string
		m         event.Message
		toOwner   bool
		label, by string
		candidate bool
	}{
		{"muted bat urgent et mention", muted, false, "noise", "rule:muted", false},
		{"vip en dm", vipDM, false, "urgent", "rule:vip", false},
		{"vip par id en dm", vipByID, false, "urgent", "rule:vip", false},
		{"vip en groupe n'est pas urgent", vipGroup, false, "info", "rule:group_default", false},
		{"mot-clé accentué, casse différente", msg("group", "L'ÉCHÉANCE approche", false, false), false, "urgent", "rule:urgent_keyword", false},
		{"mot-clé sans accent dans le texte", msg("group", "echeance vendredi", false, false), false, "urgent", "rule:urgent_keyword", false},
		{"mot-clé au pluriel", msg("group", "des signatures à faire", false, false), false, "urgent", "rule:urgent_keyword", false},
		{"faux ami : urgentiste", msg("group", "l'urgentiste est passé", false, false), false, "info", "rule:group_default", false},
		{"faux ami : designature", msg("group", "la designature du dossier", false, false), false, "info", "rule:group_default", false},
		{"mot-clé reply", msg("group", "Peux-tu regarder ?", false, false), false, "reply", "rule:reply_keyword", false},
		{"urgent avant reply", msg("dm", "urgent, peux-tu m'appeler", true, false), false, "urgent", "rule:urgent_keyword", false},
		{"mention en groupe", msg("group", "salut", false, true), false, "reply", "rule:mention", false},
		{"réponse à un message du propriétaire", msg("group", "d'accord", false, false), true, "reply", "rule:reply_to_owner", false},
		{"dm connu = candidat reply", msg("dm", "ça va ?", true, false), false, "reply", "rule:dm_known", true},
		{"dm inconnu = candidat info", msg("dm", "offre spéciale", false, false), false, "info", "rule:dm_unknown", true},
		{"groupe sans mention = info sans LLM", msg("channel", "bonjour", false, false), false, "info", "rule:group_default", false},
		{"média seul en groupe = noise", msg("group", "  ", false, false), false, "noise", "rule:empty_group", false},
		{"média seul avec mention reste reply", msg("group", "", false, true), false, "reply", "rule:mention", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Classify(c.m, c.toOwner)
			if d.Label != c.label || d.By != c.by || d.Candidate != c.candidate || d.Reason == "" {
				t.Fatalf("got %+v, want %s %s candidate=%v", d, c.label, c.by, c.candidate)
			}
		})
	}
}

func TestLoadRulesMissingFile(t *testing.T) {
	r, err := LoadRules(t.TempDir() + "/absent.yaml")
	if err != nil || len(r.UrgentKeywords) == 0 {
		t.Fatalf("règles par défaut attendues: %v %v", r, err)
	}
}

func TestExampleRulesParse(t *testing.T) {
	r, err := LoadRules("rules.example.yaml")
	if err != nil || len(r.Muted) == 0 || len(r.VIP) == 0 || len(r.UrgentKeywords) == 0 || len(r.ReplyKeywords) == 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
