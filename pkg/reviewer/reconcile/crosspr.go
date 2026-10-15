package reconcile

import (
	"regexp"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

// EarlierPoint is a point the PR author settled on an earlier pull request
// of the repo: its specific subject names (lower-cased) and the prose of
// the settled thread (root, impact, the author's answer).
type EarlierPoint struct {
	Subjects []string
	Text     string
}

// A settled point from another PR has neither a line nor a file to lean on,
// so a match needs both a shared subject and text at the external bar:
// the headline or the impact sentence inside the thread with four shared
// words, or the whole text with eight.
const (
	earlierMinSharedSentence = 4
	earlierMinSharedText     = 8
)

// EarlierScore is the containment behind a cross-PR match: the best of
// the finding's headline, impact sentence and whole text against the
// settled thread, and the subjects both name.
type EarlierScore struct {
	Text    float64
	Subject string
}

// SameEarlierPoint reports whether finding f raises again the point p: a
// specific subject of f (symbol, key, endpoint; never a bare file or a
// workflow) is one of p's subjects or is named as a whole identifier in
// p's text (a thread another reviewer opened carries no contract, only the
// names its prose quotes), and f's headline or impact sentence sits inside
// p's text at the external bar (or its whole text with eight shared
// words). Both conditions, never one: distinct defects share a symbol all
// the time, and two findings about one switch can read alike.
func SameEarlierPoint(f payload.Finding, p EarlierPoint) (EarlierScore, bool) {
	var s EarlierScore
	s.Subject = sharedEarlierSubject(f, p)
	if s.Subject == "" {
		return s, false
	}
	if p.Text == "" {
		return s, false
	}
	consider := func(text string, floor int) {
		if text == "" {
			return
		}
		if score, shared := Containment(text, p.Text); shared >= floor && score >= ExternalThreshold && score > s.Text {
			s.Text = score
		}
	}
	consider(prismHeadline(f), earlierMinSharedSentence)
	if c := f.FindingContract; c != nil {
		consider(strings.TrimSpace(c.CurrentImpact), earlierMinSharedSentence)
	}
	consider(prismText(f), earlierMinSharedText)
	return s, s.Text > 0
}

// MatchEarlier pairs finding f with the best-scoring earlier point it
// raises again, if any; ties go to the earlier index.
func MatchEarlier(f payload.Finding, points []EarlierPoint) (int, EarlierScore, bool) {
	best, bestScore := -1, EarlierScore{}
	for i, p := range points {
		if s, ok := SameEarlierPoint(f, p); ok && (best < 0 || s.Text > bestScore.Text) {
			best, bestScore = i, s
		}
	}
	return best, bestScore, best >= 0
}

func sharedEarlierSubject(f payload.Finding, p EarlierPoint) string {
	theirs := map[string]bool{}
	for _, name := range p.Subjects {
		for _, key := range SubjectKeys(name) {
			theirs[key] = true
		}
	}
	text := strings.ToLower(p.Text)
	for _, name := range SpecificSubjects(f.FindingContract) {
		for _, key := range SubjectKeys(name) {
			if theirs[key] || NamesIdentifier(text, key) {
				return key
			}
		}
	}
	return ""
}

// SubjectKeys are the lower-cased forms a subject name is compared under:
// the name itself and, for a dotted or qualified name, its last segment,
// so "DeleteView._purchase_ids" and "_purchase_ids" are one subject.
func SubjectKeys(name string) []string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	keys := []string{name}
	if i := strings.LastIndexAny(name, ".:#"); i >= 0 && i+1 < len(name) && len(name[i+1:]) >= settledMinSubjectLen {
		keys = append(keys, name[i+1:])
	}
	return keys
}

var codeSpanRe = regexp.MustCompile("`([^`\\n]{4,120})`")

// CodeIdentifiers are the identifier-like code spans a comment names
// (`record_fanclub_bulk`, `PURCHASER_READ_SWAP`, `DeleteView.delete`), as
// the subjects of a thread that carries no contract: lower-cased, four
// characters or longer, without spaces or call syntax, de-duplicated and
// sorted.
func CodeIdentifiers(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range codeSpanRe.FindAllStringSubmatch(text, -1) {
		span := strings.TrimSpace(m[1])
		span = strings.TrimSuffix(span, "()")
		if !identifierLike(span) {
			continue
		}
		key := strings.ToLower(span)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func identifierLike(s string) bool {
	if len(s) < settledMinSubjectLen {
		return false
	}
	letters := 0
	for _, r := range s {
		switch {
		case identChar(r):
			if r != '_' && !('0' <= r && r <= '9') {
				letters++
			}
		case r == '.' || r == ':' || r == '-' || r == '#':
		default:
			return false
		}
	}
	return letters >= 2
}
