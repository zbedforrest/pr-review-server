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
// contested or fixed ledger row, with the root's prose (Text) and
// what PRism itself said in the thread afterwards (Replies: the concession,
// withdrawal or verification). A finding that restates what those replies
// settled, from another file or another part of the same file, is a repeat
// whatever its wording, line or severity.
type SettledPoint struct {
	FindingID string
	File      string
	Kind      string
	Text      string
	Replies   string
}

func (s SettledPoint) thread() string { return strings.TrimSpace(s.Text + " " + s.Replies) }

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
// file or a workflow) is named in what PRism itself replied in the thread,
// and the finding's text sits inside the thread text (containment at or
// above the external bar with eight shared words, or its headline at or
// above half with four). A subject the root alone names, or one the row
// merely shares, is not enough: distinct defects in one function share its
// name all the time, and only the reply says what PRism conceded. Pseudo-files
// never match. Several findings may restate one point; each finding matches
// at most the best-scoring point.
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

// PointScore is the evidence behind a settled-point decision: the
// containment of the finding's text and of its headline in the thread text
// with the shared word counts, and how many of the finding's specific
// subjects PRism's own replies name.
type PointScore struct {
	Text            float64
	TextShared      int
	Headline        float64
	HeadlineShared  int
	SubjectsNamed   int
	SubjectsInTotal int
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
	ps := ScorePoint(f, s)
	if ps.SubjectsNamed == 0 {
		return 0, false
	}
	return ps.pass()
}

// ScorePoint measures finding f against a settled point without deciding:
// the subjects its replies name, and the text scores against the whole
// thread.
func ScorePoint(f payload.Finding, s SettledPoint) PointScore {
	return scoreAgainst(f, s.Replies, s.thread())
}

// scoreAgainst counts the finding's specific subjects named in naming and
// scores its text and headline against text.
func scoreAgainst(f payload.Finding, naming, text string) PointScore {
	var ps PointScore
	specific := SpecificSubjects(f.FindingContract)
	ps.SubjectsInTotal = len(specific)
	for _, name := range specific {
		if NamesIdentifier(naming, name) {
			ps.SubjectsNamed++
		}
	}
	if text == "" {
		return ps
	}
	ps.Text, ps.TextShared = Containment(prismText(f), text)
	if h := prismHeadline(f); h != "" {
		ps.Headline, ps.HeadlineShared = Containment(h, text)
	}
	return ps
}

// pass applies the text bars: the whole text at the external bar with
// eight shared words, or the headline at half with four.
func (ps PointScore) pass() (float64, bool) {
	if ps.TextShared >= settledMinShared && ps.Text >= ExternalThreshold {
		return ps.Text, true
	}
	if ps.HeadlineShared >= settledHeadlineMinShared && ps.Headline >= settledHeadlineThreshold {
		return ps.Headline, true
	}
	return 0, false
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
