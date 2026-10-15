package reconcile

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"pr-review-server/pkg/reviewer/payload"
)

// EarlierPoint is a point the PR author settled on an earlier pull request
// of the repo: its specific subject names (lower-cased), the prose of the
// settled thread (root, impact, the author's answer) and its contract kind
// when the point came from a PRism finding (empty for another reviewer's
// thread, which carries no contract).
type EarlierPoint struct {
	Subjects []string
	Text     string
	Kind     string
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
// the time, and two findings about one switch can read alike. Kinds must
// agree when both are known, as on the same PR.
func SameEarlierPoint(f payload.Finding, p EarlierPoint) (EarlierScore, bool) {
	var s EarlierScore
	if kind := findingKind(f); kind != "" && p.Kind != "" && !strings.EqualFold(kind, p.Kind) {
		return s, false
	}
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
			// A generic key is passed over, not fatal: the next shared key
			// may name the point.
			if (theirs[key] || NamesIdentifier(text, key)) && !GenericSubject(key, f.File) {
				return key
			}
		}
	}
	return ""
}

// GenericSubject reports whether the shared subject is too plain to carry a
// match: a short name (start, main, lookup, user_id) that distinct defects
// name all the time, a token with fewer than five letters, or the file's
// own stem (creators for creators.py). A dotted name or a long one is
// specific enough.
func GenericSubject(subject, file string) bool {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return true
	}
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)))
	if subject == stem {
		return true
	}
	letters := 0
	for _, r := range subject {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < genericSubjectMinLetters {
		return true
	}
	// A short qualified name (user_id, created_at) is as common as a
	// short word; a long one, or a dotted one, names something.
	return utf8.RuneCountInString(subject) < genericSubjectMaxLen && !strings.ContainsAny(subject, ".:#")
}

// genericSubjectMinLetters is the letter count under which a token (v2,
// s3, md5) is generic whatever else it carries.
const genericSubjectMinLetters = 5

// genericSubjectMaxLen is the length under which a name is generic.
const genericSubjectMaxLen = 10

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

func findingKind(f payload.Finding) string {
	if f.FindingContract == nil {
		return ""
	}
	return strings.TrimSpace(f.FindingContract.FindingKind)
}
