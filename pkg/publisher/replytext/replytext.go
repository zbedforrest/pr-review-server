// Package replytext shapes the text PRism posts under an author's reply: it
// reads the author's words for the shape of their pushback and enforces the
// reply conventions the model is asked to follow but sometimes does not.
package replytext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Context is what the renderer knows about the thread besides the reply body.
type Context struct {
	AuthorComment string
	FindingBody   string
	Decision      string
}

const (
	NoteIntentAcknowledged = "intent_acknowledged"

	TicketAsk = "If there is a ticket for this, reply with its key and this thread can be closed against it."
)

// agreementOpenerRe matches an agreement formula at the head of the body when
// it stands alone: followed by punctuation, by "that", or by nothing. "Right
// now the guard..." is not a formula and is left alone.
var agreementOpenerRe = regexp.MustCompile(`(?i)^(?:you(?:'|’)?re right|you are right|that(?:'|’)?s (?:right|correct|fair)|correct|agreed|agree|good point|fair point|good catch|nice catch|fair enough|fair|right|yes|yep|indeed|exactly|true)(?:\s*[,.:;!]+\s*|\s+that\s+|\s*$)(?:(?:and|but|so)\s+)?`)

// StripAgreementOpener drops agreement formulas from the head of the body and
// capitalizes what remains. ok is false when nothing but formulas was there.
func StripAgreementOpener(body string) (out string, ok bool) {
	out = strings.TrimSpace(body)
	for i := 0; i < 4; i++ {
		m := agreementOpenerRe.FindStringIndex(out)
		if m == nil {
			break
		}
		out = strings.TrimSpace(out[m[1]:])
	}
	if out == "" {
		return "", false
	}
	return capitalize(out), true
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

var (
	intentRe = regexp.MustCompile(`(?i)\b(?:intentional(?:ly)?|intended|by design|on purpose|deliberate(?:ly)?|product (?:decision|call)|design (?:decision|choice)|not a bug|working as (?:intended|designed|expected)|(?:keep(?:ing)?|leav(?:e|ing)) (?:it|this|that|them|these)?\s*as[- ]is)\b`)
	changeRe = regexp.MustCompile(`(?i)\b(?:fix(?:ed|es)?|added|changed|moved|updated|removed|replaced|pushed|commit(?:ted)?|patched|[0-9a-f]{7,40})\b`)
	deferRe  = regexp.MustCompile(`(?i)\b(?:out of scope|follow[- ]?up|later (?:pr|change|commit)|(?:separate|another|different|future|new|its own) (?:pr|ticket|change|issue)|not (?:touching|addressing|fixing|changing|doing) (?:it|this|that) here|(?:in|as) a ticket|track(?:ed|ing)? (?:it |this |that )?(?:separately|elsewhere))\b`)
	ticketRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-\d{1,6})\b`)
	// Uppercase-dash-digits that are not issue keys.
	notTicket  = map[string]bool{"SHA": true, "UTF": true, "ISO": true, "RFC": true, "MD": true, "AES": true, "HTTP": true, "TLS": true, "CVE": true, "PR": true, "UTC": true, "RSA": true, "ES": true, "HTML": true}
	severityRe = regexp.MustCompile(`(?i)(?:\[(critical|high|medium|low)\]|alt="(critical|high|medium|low)")`)
	withdrawRe = regexp.MustCompile(`(?i)\bwithdr[ae]w`)
	// A sentence ends at terminal punctuation followed by whitespace, so the
	// dots in retry.go:41 do not split it.
	sentenceRe  = regexp.MustCompile(`.*?[.!?]+["')\]]*(?:\s+|$)|.+$`)
	keyAskRe    = regexp.MustCompile(`(?i)\bkey\b`)
	descAskRe   = regexp.MustCompile(`(?i)\bdescription\b`)
	riskAskBare = "Should this be noted in the PR description as accepted risk?"
)

// AssertsIntent reports an author reply that defends the behavior as
// deliberate without describing a code change.
func AssertsIntent(comment string) bool {
	return intentRe.MatchString(comment) && !changeRe.MatchString(comment)
}

// Defers reports an author reply that sends the fix elsewhere: out of scope,
// a follow-up, a later PR, a separate ticket.
func Defers(comment string) bool {
	return deferRe.MatchString(comment)
}

// TicketKeys returns the issue keys (ABC-123) named in the text, in order,
// without duplicates.
func TicketKeys(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ticketRe.FindAllStringSubmatch(text, -1) {
		key := m[1]
		project := key[:strings.IndexByte(key, '-')]
		if notTicket[project] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// FindingSeverity reads the severity label PRism put at the head of the
// inline comment: "**[MEDIUM] ...**" or a badge image. Empty when absent.
func FindingSeverity(findingBody string) string {
	m := severityRe.FindStringSubmatch(findingBody)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1] + m[2])
}

// SeverityAtLeastMedium is the bar for naming a kept behavior as accepted risk.
func SeverityAtLeastMedium(severity string) bool {
	switch strings.ToLower(severity) {
	case "critical", "high", "medium":
		return true
	}
	return false
}

// Note names the sub-path a concession took, for telemetry; empty otherwise.
func Note(ctx Context) string {
	if ctx.Decision == "concede" && AssertsIntent(ctx.AuthorComment) {
		return NoteIntentAcknowledged
	}
	return ""
}

// Render applies the posting conventions to a reply body: no agreement
// formula opener; no withdrawal when the author asserted intent (and, at
// medium severity or above, the accepted-risk question); the ticket key
// repeated or asked for when the author deferred the fix. ok is false when
// nothing postable remains. Rendering an already rendered body is a no-op.
func Render(body string, ctx Context) (out string, ok bool) {
	out, ok = StripAgreementOpener(body)
	if !ok {
		return "", false
	}
	if Note(ctx) == NoteIntentAcknowledged {
		out, ok = dropWithdrawal(out)
		if !ok {
			return "", false
		}
		if SeverityAtLeastMedium(FindingSeverity(ctx.FindingBody)) && !descAskRe.MatchString(out) {
			out = out + " " + riskAskBare
		}
	}
	if Defers(ctx.AuthorComment) {
		keys := TicketKeys(ctx.AuthorComment)
		switch {
		case len(keys) > 0 && !strings.Contains(out, keys[0]):
			out = out + " Tracking this against " + keys[0] + "."
		case len(keys) == 0 && len(TicketKeys(out)) == 0 && !keyAskRe.MatchString(out):
			out = out + " " + TicketAsk
		}
	}
	return out, true
}

func dropWithdrawal(body string) (string, bool) {
	var kept []string
	for _, s := range sentenceRe.FindAllString(body, -1) {
		if s = strings.TrimSpace(s); s != "" && !withdrawRe.MatchString(s) {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, " "), true
}
