package main

import (
	"strings"
	"testing"
	"time"
)

// Subjects and bodies are anonymised copies of the shapes seen in real
// LinkedIn emails (oct. 2026), except the ones marked "plausible", which are
// guesses: no real mention/comment/reaction email had been received yet.

const footer = "\r\n----------------------------------------\r\n\r\nCet e-mail est destiné à Jean Dupont (Consultant)\r\nVous recevez des e-mails de notifications de LinkedIn.\r\nSe désabonner : https://www.linkedin.com/comm/mypreferences/u/emailunsub?lipi=urn%3Ali%3Apage\r\n"

const trk = "?lipi=urn%3Ali%3Apage%3Aemail_x%3BAAA%3D%3D&midToken=AQE&midSig=1q&trk=eml-x&trkEmail=eml-y-null&eid=2734q-a-3x&otpToken=ZTI5MmU4"

var digestBody = "Vous avez 1 nouveau message\r\n\r\n.....................................\r\n\r\nPavana S (Founder @ Acme | Building things for…)\r\n(https://in.linkedin.com/in/pavana-s-a25a28217" + trk + ")\r\n\r\nVoir le message https://www.linkedin.com/comm/messaging/thread/2-MmJjMzhjMWQtYjI2MC00ZDVm==" + trk + "\r\n\r\n....................................." + footer

var inmailBody = "L’InMail de Sam attend votre réponse\r\n\r\nSam Martin (Network Specialist)\r\n(https://ma.linkedin.com/in/sam-martin-81384b)\r\n\r\nmessage :\r\n\r\nBonjour Monsieur,\r\nJe me permets de vous contacter pour un stage.\r\n\r\nVoir le message :\r\n\r\nhttps://www.linkedin.com/comm/messaging/thread/2-ZDVmY2QxYz==" + trk + footer

var inviteBody = "Léa, Platform Engineer chez Acme, attend votre réponse\r\n\r\nBonjour Jean, je souhaiterais rejoindre votre réseau professionnel\r\n\r\nLéa Durand\r\nPlatform Engineer\r\nToulouse, France\r\n12 relations en commun\r\nVoir toutes les relations en commun :https://www.linkedin.com/comm/search/results/people/?facetNetwork=1&" + trk[1:] + "\r\n\r\nAccepter :https://www.linkedin.com/comm/mynetwork/invite-accept/invitationId/75129/sharedKey/bwth/?inviterVanityName=lea-durand-39b67221a&" + trk[1:] + footer

func lm(addr, from, subject, body string) mail {
	return mail{ID: "id-" + addr, Received: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Addr: addr + "@linkedin.com", From: from, Subject: subject, Body: body}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		m    mail
		cat  category
		why  string
		note string
	}{
		{lm("messaging-digest-noreply", "Pavana S via LinkedIn", "Pavana vient de vous envoyer un message", digestBody), catMessage, "", "real"},
		{lm("messages-noreply", "Sam Martin via LinkedIn", "Sam vient de vous envoyer un message", inmailBody), catMessage, "", "real"},
		{lm("invitations", "Léa Durand", "Je souhaite que nous nous connections", inviteBody), catInvitation, "", "real"},
		{lm("invitations", "Khouloud K via LinkedIn", "Khouloud a accepté votre invitation, découvrez son réseau", "Invitation acceptée"), catAccepted, "", "real"},
		{lm("invitations", "LinkedIn", "Voir les relations, l’expérience et plus encore de Florence  et d’autres personnes", ""), catIgnored, "suggestion", "real"},
		{lm("notifications-noreply", "LinkedIn", "Jean, vos posts ont obtenu 5,417 impressions la semaine dernière", ""), catStats, "", "real"},
		{lm("messages-noreply", "LinkedIn", "Jean, vous avez plus de 4 000 impressions du post. Et maintenant ?", ""), catStats, "", "real"},
		{lm("messages-noreply", "LinkedIn", "Focus apprentissage : la cybersécurité commence par vous", ""), catStats, "", "real"},
		{lm("messages-noreply", "LinkedIn", "Vous participez à Data Spaces: the next generation", ""), catEvent, "", "real"},
		{lm("messages-noreply", "LinkedIn", "Découvrez ce que vous avez manqué chez Acme la semaine dernière", ""), catIgnored, "digest", "real"},
		{lm("newsletters-noreply", "LinkedIn", "Jean, vous avez commenté ? vos posts vous mentionnent", ""), catIgnored, "newsletter", "newsletter wins even with trigger words"},
		{lm("groups-noreply", "LinkedIn", "Ne manquez pas les discussions dans Python Developers", ""), catIgnored, "newsletter", "real"},
		{lm("editors-noreply", "LinkedIn", "Jean, comment les professionnels peuvent-ils bénéficier de l’IA?", ""), catIgnored, "newsletter", "real"},
		{lm("updates-noreply", "LinkedIn", "Sylvain R, Consultant, a réagi à un post : Si vous avez téléchargé…", ""), catIgnored, "feed", "real: reaction on someone else's post"},
		{lm("messages-noreply", "LinkedIn", "Quelque chose d'inattendu", ""), catOther, "", "fallback"},
		// plausible, not validated
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez vous a mentionné dans une publication", ""), catMention, "", "plausible FR"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez mentioned you in a post", ""), catMention, "", "plausible EN"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez a commenté votre post", ""), catComment, "", "plausible FR"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez a répondu à votre commentaire", ""), catComment, "", "plausible FR"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez commented on your post", ""), catComment, "", "plausible EN"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez replied to your comment", ""), catComment, "", "plausible EN"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez a réagi à votre publication", ""), catReaction, "", "plausible FR"},
		{lm("updates-noreply", "LinkedIn", "Ana Lopez a réagi à votre post : « Nous lançons… »", ""), catReaction, "", "plausible: updates sender but 'votre'"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez reacted to your post", ""), catReaction, "", "plausible EN"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez a partagé votre post", ""), catReaction, "", "plausible share"},
		{lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez reposted your post", ""), catReaction, "", "plausible share EN"},
		{lm("notifications-noreply", "LinkedIn", "Ana a commenté un post que vous suivez", ""), catOther, "", "not on the owner's post: not a comment"},
	} {
		cat, why := classify(c.m)
		if cat != c.cat || why != c.why {
			t.Errorf("%q (%s): got %q/%q, want %q/%q", c.m.Subject, c.note, cat, why, c.cat, c.why)
		}
	}
}

func TestCleanURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.linkedin.com/comm/messaging/thread/2-Abc==" + trk:           "https://www.linkedin.com/messaging/thread/2-Abc==",
		"https://www.linkedin.com/comm/feed/update/urn:li:activity:75?trk=x&a=1": "https://www.linkedin.com/feed/update/urn:li:activity:75?a=1",
		"https://www.linkedin.com/comm/in/jane-doe-1a2?lipi=x).":                 "https://www.linkedin.com/in/jane-doe-1a2",
		"https://evil.example.com/linkedin.com/in/x":                             "",
		"https://notlinkedin.com/in/x":                                           "",
	} {
		if got := cleanURL(in); got != want {
			t.Errorf("cleanURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThreadAndSlug(t *testing.T) {
	if got := threadID(digestBody); got != "2-MmJjMzhjMWQtYjI2MC00ZDVm==" {
		t.Errorf("thread %q", got)
	}
	if threadID("rien") != "" {
		t.Error("thread without url")
	}
	for body, want := range map[string]string{
		digestBody: "pavana-s-a25a28217",
		inmailBody: "sam-martin-81384b",
		inviteBody: "lea-durand-39b67221a", // from inviterVanityName
		"https://www.linkedin.com/comm/in/Jo%C3%A9?x": "joé",
		"aucun lien": "",
	} {
		if got := profileSlug(body); got != want {
			t.Errorf("slug %q, want %q", got, want)
		}
	}
	for in, want := range map[string]string{"Éloïse Dupont-Martin": "eloise-dupont-martin", "  Jean  ": "jean", "": ""} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvert(t *testing.T) {
	check := func(m mail, cat category, convID, kind, name, sender, senderName string, mentions bool, contains ...string) {
		t.Helper()
		id, msg, got, ok := convert(m)
		if !ok || got != cat {
			t.Fatalf("%q: ok=%v cat=%q, want %q", m.Subject, ok, got, cat)
		}
		if id != "linkedin:"+m.ID || msg.Conversation.ID != convID || msg.Conversation.Kind != kind || msg.Conversation.Name != name ||
			msg.Sender.ID != sender || msg.Sender.Name != senderName || msg.Sender.Known || msg.MentionsOwner != mentions {
			t.Errorf("%q: %q %+v mentions=%v", id, m.Subject, msg, msg.MentionsOwner)
		}
		for _, bad := range []string{"lipi", "midToken", "otpToken", "trkEmail", "eid=", "unsub", "Cet e-mail", ".....", "<p"} {
			if strings.Contains(msg.Text, bad) {
				t.Errorf("%q: text contains %q: %q", m.Subject, bad, msg.Text)
			}
		}
		for _, want := range contains {
			if !strings.Contains(msg.Text, want) {
				t.Errorf("%q: text lacks %q: %q", m.Subject, want, msg.Text)
			}
		}
		if i := strings.Index(msg.Text, "\nhttps://"); i > 0 && len([]rune(msg.Text[:i])) > len([]rune(m.Subject))+maxExcerptRunes+10 {
			t.Errorf("text too long: %d", len(msg.Text))
		}
	}
	check(lm("messaging-digest-noreply", "Pavana S via LinkedIn", "Pavana vient de vous envoyer un message", digestBody), catMessage,
		"linkedin:thread:2-MmJjMzhjMWQtYjI2MC00ZDVm==", "dm", "LinkedIn · Pavana S", "linkedin:pavana-s-a25a28217", "Pavana S", false,
		"Founder @ Acme", "\nhttps://www.linkedin.com/messaging/thread/2-MmJjMzhjMWQtYjI2MC00ZDVm==")
	check(lm("messages-noreply", "Sam Martin via LinkedIn", "Sam vient de vous envoyer un message", inmailBody), catMessage,
		"linkedin:thread:2-ZDVmY2QxYz==", "dm", "LinkedIn · Sam Martin", "linkedin:sam-martin-81384b", "Sam Martin", false,
		"Je me permets de vous contacter pour un stage") // an InMail carries the text
	check(lm("invitations", "Léa Durand", "Je souhaite que nous nous connections", inviteBody), catInvitation,
		"linkedin:invitation", "group", "LinkedIn · invitations", "linkedin:lea-durand-39b67221a", "Léa Durand", false,
		"Platform Engineer", "\nhttps://www.linkedin.com/in/lea-durand-39b67221a")
	check(lm("invitations", "Khouloud K via LinkedIn", "Khouloud a accepté votre invitation, découvrez son réseau", "Invitation acceptée"+footer), catAccepted,
		"linkedin:invitation_accepted", "group", "LinkedIn · invitations acceptées", "linkedin:khouloud-k", "Khouloud K", false)
	check(lm("notifications-noreply", "LinkedIn", "Jean, vos posts ont obtenu 5,417 impressions la semaine dernière", "<p style=\"x\">html junk</p>"+footer), catStats,
		"linkedin:stats", "group", "LinkedIn · statistiques", "linkedin:linkedin", "LinkedIn", false)
	check(lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez vous a mentionné dans une publication", "Ana Lopez vous a mentionné : « Merci Jean pour ton aide »\r\nVoir la publication https://www.linkedin.com/comm/feed/update/urn:li:activity:75"+trk+footer), catMention,
		"linkedin:mention", "group", "LinkedIn · mentions", "linkedin:ana-lopez", "Ana Lopez", true,
		"Merci Jean pour ton aide", "\nhttps://www.linkedin.com/feed/update/urn:li:activity:75")
	check(lm("notifications-noreply", "LinkedIn", "Ana Lopez commented on your post", "Ana Lopez commented: \"Great news!\""+footer), catComment,
		"linkedin:comment", "group", "LinkedIn · commentaires", "linkedin:ana-lopez", "Ana Lopez", true, "Great news!")
	check(lm("notifications-noreply", "Ana Lopez via LinkedIn", "Ana Lopez reacted to your post", ""), catReaction,
		"linkedin:reaction", "group", "LinkedIn · réactions", "linkedin:ana-lopez", "Ana Lopez", false)

	if _, _, _, ok := convert(lm("newsletters-noreply", "LinkedIn", "Les actus", "x")); ok {
		t.Error("newsletter converted")
	}
	long := lm("messaging-digest-noreply", "Pavana S via LinkedIn", "Pavana vient de vous envoyer un message", strings.Repeat("une ligne utile ", 100))
	if _, msg, _, _ := convert(long); len([]rune(msg.Text)) > len([]rune(long.Subject))+maxExcerptRunes+10 {
		t.Errorf("long text not cut: %d", len(msg.Text))
	}
	group := lm("messaging-digest-noreply", "Stephane A via LinkedIn", "Stephane vient de vous envoyer un message", "Vous avez 1 nouveau message\r\nA et Stephane\r\nConversation de groupe\r\nVoir le message https://www.linkedin.com/comm/messaging/thread/2-G=="+trk)
	if _, msg, _, _ := convert(group); msg.Conversation.Kind != "group" || msg.Conversation.ID != "linkedin:thread:2-G==" || !strings.HasSuffix(msg.Conversation.Name, "(groupe)") {
		t.Errorf("group conversation: %+v", msg.Conversation)
	}
}
