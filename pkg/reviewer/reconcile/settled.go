package reconcile

import (
	"path"
	"sort"
	"strings"
	"unicode"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// SettledPoint is a point PRism already settled on the PR: a dismissed,
// contested, external or fixed ledger row, with everything PRism said in
// its thread (the root's prose and PRism's own replies). A finding that
// restates it from another file, or from another part of the same file, is
// a repeat whatever its wording, line or severity.
type SettledPoint struct {
	FindingID string
	File      string
	Kind      string
	Subjects  []string
	Text      string
}

// A settled point has no line anchor to lean on, so the text bar is the
// external one with a higher floor, and a headline alone must sit mostly
// inside the thread.
const (
	settledMinShared         = 8
	settledHeadlineThreshold = 0.5
	settledHeadlineMinShared = 4
	settledMinSubjectLen     = 4
)

// MatchSettled maps the id of each finding that restates a settled point to
// that point's id. A pair matches when the kinds agree (or one is unknown),
// a specific subject of the finding (a symbol, selector or key, never a bare
// file or a workflow) is one of the point's subjects or is named in the
// thread, and the finding's text sits inside the thread text (containment
// at or above the external bar with eight shared words, or its headline at
// or above half with four). Pseudo-files never match. Several findings may
// restate one point; each finding matches at most the best-scoring point.
func MatchSettled(current []payload.Finding, settled []SettledPoint) map[string]string {
	out := map[string]string{}
	for _, f := range current {
		if isPseudoFile(f.File) {
			continue
		}
		best, bestScore := "", 0.0
		for _, s := range settled {
			if score, ok := SamePoint(f, s); ok && (best == "" || score > bestScore || (score == bestScore && s.FindingID < best)) {
				best, bestScore = s.FindingID, score
			}
		}
		if best != "" {
			out[f.ID] = best
		}
	}
	return out
}

// SamePoint reports whether finding f restates the settled point s, with the
// text score behind the decision.
func SamePoint(f payload.Finding, s SettledPoint) (float64, bool) {
	kind := ""
	if f.FindingContract != nil {
		kind = f.FindingContract.FindingKind
	}
	if kind != "" && s.Kind != "" && kind != s.Kind {
		return 0, false
	}
	if !sharesSubject(SpecificSubjects(f.FindingContract), s) {
		return 0, false
	}
	return settledTextScore(prismText(f), prismHeadline(f), s.Text)
}

// settledTextScore is the containment of a finding's text, or of its
// headline, inside a settled thread's text.
func settledTextScore(text, headline, thread string) (float64, bool) {
	if thread == "" {
		return 0, false
	}
	if score, shared := Containment(text, thread); shared >= settledMinShared && score >= ExternalThreshold {
		return score, true
	}
	if headline != "" {
		if score, shared := Containment(headline, thread); shared >= settledHeadlineMinShared && score >= settledHeadlineThreshold {
			return score, true
		}
	}
	return 0, false
}

func sharesSubject(subjects []string, s SettledPoint) bool {
	if len(subjects) == 0 {
		return false
	}
	named := make(map[string]bool, len(s.Subjects))
	for _, name := range s.Subjects {
		named[name] = true
	}
	for _, name := range subjects {
		if named[strings.ToLower(name)] || NamesIdentifier(s.Text, name) {
			return true
		}
	}
	return false
}

// SpecificSubjects are the contract's subject names that identify code
// rather than a place: every kind but file and workflow, four characters or
// longer, in the case the agent wrote them, de-duplicated and sorted. A
// symbol named after the file it lives in (a module, hook or component
// default export) is as broad as the file and is left out too.
func SpecificSubjects(c *types.FindingContract) []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Subjects {
		if s.Kind == "file" || s.Kind == "workflow" {
			continue
		}
		name := strings.TrimSpace(s.Name)
		lower := strings.ToLower(name)
		if len(name) < settledMinSubjectLen || seen[lower] || lower == fileStem(s.Path) {
			continue
		}
		seen[lower] = true
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func fileStem(p string) string {
	base := path.Base(p)
	return strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))
}

// NamesIdentifier reports whether text names the identifier as a whole
// word: the characters around each occurrence are not identifier
// characters, so a shorter name inside a longer one does not count. A name
// with an upper-case letter is matched in its own case, so a class or hook
// name is not found in the ordinary word it resembles; an all-lower name is
// matched case-insensitively.
func NamesIdentifier(text, name string) bool {
	if name == "" {
		return false
	}
	if strings.ToLower(name) == name {
		text = strings.ToLower(text)
	}
	for start := 0; ; {
		i := strings.Index(text[start:], name)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(name)
		before := i == 0 || !identChar(rune(text[i-1]))
		after := end == len(text) || !identChar(rune(text[end]))
		if before && after {
			return true
		}
		start = i + 1
	}
}

func identChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
