package main

import (
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/mmaudet/twalk/internal/event"
)

// mail is one LinkedIn notification email, reduced to what we use.
type mail struct {
	ID       string
	Received time.Time
	Addr     string // sender address
	From     string // sender display name ("Prénom Nom via LinkedIn")
	Subject  string
	Body     string // text body, possibly truncated
}

type category string

const (
	catMessage      category = "message"
	catMention      category = "mention"
	catComment      category = "comment"
	catReaction     category = "reaction" // reaction to, or share of, the owner's post
	catInvitation   category = "invitation"
	catAccepted     category = "invitation_accepted"
	catEvent        category = "event"
	catStats        category = "stats"
	catOther        category = "other"
	catIgnored      category = "" // not published, only counted under rule.why
	maxExcerptRunes          = 400
)

// rule: the first rule whose three patterns (nil = any) all match wins.
// cat == catIgnored drops the email and counts it under why.
// Validated on real emails: ignored kinds, message, invitation, accepted, event,
// stats. NOT validated (no such email received yet): mention, comment, reaction
// (reaction/share of the owner's own post): plausible FR/EN patterns only.
type rule struct {
	cat                 category
	why                 string
	from, subject, body *regexp.Regexp
}

func re(p string) *regexp.Regexp {
	if p == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)` + p)
}

func R(cat category, why, from, subject, body string) rule {
	return rule{cat, why, re(from), re(subject), re(body)}
}

var rules = []rule{
	R(catIgnored, "newsletter", `^(newsletters|groups|editors)-noreply@`, "", ""),
	R(catMessage, "", `^(messaging-digest|messages)-noreply@`, `vient de vous envoyer un message|vous a envoyé un message|sent you a message|nouveaux? messages?|new messages?`, ""),
	R(catMention, "", "", `vous a (mentionné|identifié|cité)|mentioned you|tagged you`, ""),
	R(catComment, "", "", `a commenté (votre|sur votre|sous votre)|a répondu à (votre|vos) (commentaire|post|publication)|commented on your|replied to your (comment|post)`, ""),
	R(catReaction, "", "", `a réagi à (votre|vos)|(a aimé|aime) (votre|vos)|reacted to your|likes? your|liked your|a (partagé|republié) (votre|vos)|(shared|reposted) your`, ""),
	R(catAccepted, "", "", `a accepté votre invitation|accepted your invitation`, ""),
	R(catIgnored, "suggestion", "", `^(voir les relations|see (the )?connections)|vous connaissez peut-être|people you may know`, ""),
	R(catInvitation, "", `^invitations@`, `souhaite que nous nous connections|d.like to connect|souhaite se connecter|wants to connect`, ""),
	R(catEvent, "", "", `^(vous participez à|vous êtes inscrit|you.re attending|you are attending)|event reminder|rappel.*événement`, ""),
	R(catStats, "", "", `impressions|focus apprentissage|learning focus|vues? de votre profil|profile views|apparu dans|appeared in`, ""),
	R(catIgnored, "digest", "", `découvrez ce que vous avez manqué|what you missed`, ""),
	R(catIgnored, "feed", `^updates-noreply@`, "", ""), // "X a réagi à un post" on someone else's post
	R(catOther, "", "", "", ""),
}

// classify applies the rules table; for an ignored email it returns the reason.
func classify(m mail) (cat category, why string) {
	for _, r := range rules {
		if (r.from == nil || r.from.MatchString(m.Addr)) && (r.subject == nil || r.subject.MatchString(m.Subject)) && (r.body == nil || r.body.MatchString(m.Body)) {
			return r.cat, r.why
		}
	}
	return catOther, ""
}

var catLabels = map[category]string{
	catMessage: "messages", catMention: "mentions", catComment: "commentaires", catReaction: "réactions", catInvitation: "invitations",
	catAccepted: "invitations acceptées", catEvent: "événements", catStats: "statistiques", catOther: "autres notifications",
}

var (
	reVia       = regexp.MustCompile(`(?i)\s+via\s+LinkedIn\s*$`)
	reSubjName  = regexp.MustCompile(`(?i)^(.{2,60}?)(?:,[^,]{1,80},)? (?:vous a |a (?:commenté|réagi|partagé|répondu|aimé|accepté|republié)|mentioned|commented|replied|reacted|liked|shared|reposted|vient de)`)
	reThread    = regexp.MustCompile(`/messaging/thread/([^/?&\s"<>)]+)`)
	reSlug      = regexp.MustCompile(`linkedin\.com/(?:comm/)?in/([^/?&\s"<>)]+)`)
	reVanity    = regexp.MustCompile(`inviterVanityName=([^&?\s"<>)]+)`)
	reTag       = regexp.MustCompile(`<[^>]*>`)
	reURL       = regexp.MustCompile(`https?://[^\s<>")]+`)
	reGroupConv = regexp.MustCompile(`(?i)conversation de groupe|group conversation`)
	reStop      = regexp.MustCompile(`(?i)^-{10,}|cet e-mail est destiné|this email was intended for`)
	reSkipLine  = regexp.MustCompile(`(?i)^(vous avez \d+ nouveaux? messages?|you have \d+ new messages?|voir (le message|les? relations|le profil|toutes les|l.événement)|view (the )?message|accepter|accept|message ?:)`)
	trackParams = []string{"lipi", "midToken", "midSig", "trk", "trkEmail", "eid", "otpToken"}
)

// cleanURL returns a canonical linkedin.com URL without tracking parameters
// and without the /comm/ email redirect prefix; "" for any other host.
func cleanURL(raw string) string {
	u, err := url.Parse(strings.TrimRight(raw, ".,;:)]}"))
	if err != nil || (u.Host != "linkedin.com" && !strings.HasSuffix(u.Host, ".linkedin.com")) {
		return ""
	}
	q := u.Query()
	for _, p := range trackParams {
		q.Del(p)
	}
	u.RawQuery, u.Fragment = q.Encode(), ""
	u.Path = strings.Replace(u.Path, "/comm/", "/", 1)
	return u.String()
}

func threadID(body string) string {
	if m := reThread.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// profileSlug: /in/<slug> of the first profile link, or the vanity name of an invitation link.
func profileSlug(body string) string {
	for _, r := range []*regexp.Regexp{reSlug, reVanity} {
		if m := r.FindStringSubmatch(body); m != nil {
			s, err := url.PathUnescape(m[1])
			if err != nil {
				s = m[1]
			}
			return strings.ToLower(s)
		}
	}
	return ""
}

// slugify: "Éloïse Dupont-Martin" → "eloise-dupont-martin".
func slugify(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func personName(m mail) string {
	n := strings.TrimSpace(reVia.ReplaceAllString(m.From, ""))
	if n != "" && !strings.EqualFold(n, "linkedin") {
		return n
	}
	if s := reSubjName.FindStringSubmatch(m.Subject); s != nil {
		return strings.TrimSpace(s[1])
	}
	return ""
}

// excerptLines: stats and other notifications only carry boilerplate (and sometimes HTML).
var excerptLines = map[category]int{catStats: 0, catOther: 0, catAccepted: 0, catEvent: 2}

// excerpt keeps the useful text lines of the body: no URLs, no footer, no boilerplate.
func excerpt(body, subject string, maxLines int) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(reTag.ReplaceAllString(reURL.ReplaceAllString(l, ""), ""))
		if reStop.MatchString(l) {
			break
		}
		if strings.IndexFunc(l, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 || reSkipLine.MatchString(l) || l == subject {
			continue
		}
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	s := strings.Join(out[:min(len(out), maxLines)], " · ")
	if r := []rune(s); len(r) > maxExcerptRunes {
		s = string(r[:maxExcerptRunes]) + "…"
	}
	return s
}

// link picks the canonical link that best matches the category.
func link(cat category, body, slug string) string {
	if (cat == catInvitation || cat == catAccepted) && slug != "" {
		return "https://www.linkedin.com/in/" + slug
	}
	want := map[category]*regexp.Regexp{
		catMessage: regexp.MustCompile(`/messaging/thread/`), catEvent: regexp.MustCompile(`/events/`),
		catMention: regexp.MustCompile(`/feed/update/|/posts/`), catComment: regexp.MustCompile(`/feed/update/|/posts/`), catReaction: regexp.MustCompile(`/feed/update/|/posts/`),
	}[cat]
	if want == nil {
		return ""
	}
	for _, raw := range reURL.FindAllString(body, -1) {
		if u := cleanURL(raw); u != "" && want.MatchString(u) {
			return u
		}
	}
	return ""
}

// convert builds the bus message for a published category. The id is
// "linkedin:<JMAP email id>"; ok is false for an ignored email.
func convert(m mail) (id string, msg event.Message, cat category, ok bool) {
	cat, _ = classify(m)
	if cat == catIgnored {
		return "", msg, cat, false
	}
	body := m.Body
	name := personName(m)
	person := cat != catEvent && cat != catStats && cat != catOther
	slug := ""
	if person {
		slug = profileSlug(body)
	}
	sender := event.Sender{ID: "linkedin:linkedin", Name: "LinkedIn"}
	if person && (slug != "" || name != "") {
		if slug == "" {
			slug = slugify(name)
		}
		sender = event.Sender{ID: "linkedin:" + slug, Name: name}
	}
	conv := event.Conversation{ID: "linkedin:" + string(cat), Kind: event.KindGroup, Name: "LinkedIn · " + catLabels[cat]}
	if cat == catMessage {
		conv.ID, conv.Kind, conv.Name = "linkedin:thread:"+threadID(body), event.KindDM, "LinkedIn · "+name
		if threadID(body) == "" {
			conv.ID = "linkedin:message"
		}
		if name == "" {
			conv.Name = "LinkedIn · messages"
		}
		if reGroupConv.MatchString(body) {
			conv.Kind, conv.Name = event.KindGroup, conv.Name+" (groupe)"
		}
	}
	text := m.Subject
	n, limited := excerptLines[cat]
	if !limited {
		n = 99
	}
	if x := excerpt(body, m.Subject, n); x != "" {
		text += " — " + x
	}
	if l := link(cat, body, slug); l != "" {
		text += "\n" + l
	}
	return "linkedin:" + m.ID, event.Message{
		Conversation: conv, Sender: sender, Text: text,
		MentionsOwner: cat == catMention || cat == catComment,
	}, cat, true
}
