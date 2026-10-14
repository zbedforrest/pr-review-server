package reconcile

import (
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

// External match bar: the shared tokens must cover this much of the shorter
// text, and number at least the floor, so two short remarks that share a
// couple of words never join. A headline against the other side's body is a
// shorter text, so its floor is one word higher.
const (
	ExternalThreshold         = 0.35
	ExternalMinShared         = 3
	externalMinSharedHeadline = 4
)

// Match is one PRism finding paired with the external root it restates.
type Match struct {
	FindingID string
	External  ExternalFinding
	Score     float64
}

// MatchOptions selects the texts a finding is scored by.
type MatchOptions struct {
	// NoteSentence also scores the sentence a summary bullet shows for a
	// finding the contract does not title (no headline, no current impact):
	// the first sentence of its comment. Off, such a finding is scored by
	// its impact sentence, which for a latent hazard concedes nothing today
	// and shares nothing with the thread that raised its point.
	NoteSentence bool
	// ShownSentence picks the sentence the summary shows for a finding, as
	// the renderer does (a praise opener or a quoted context sentence
	// skipped); nil takes the comment's first sentence.
	ShownSentence func(payload.Finding) string
}

// ExternalScore is ExternalScoreWith under the shipped options.
func ExternalScore(p payload.Finding, g ExternalFinding) float64 {
	return ExternalScoreWith(p, g, MatchOptions{NoteSentence: true})
}

// ExternalScoreWith is how much of the shorter text the two comments share:
// the best of whole-text containment, the PRism headline against the
// external body, the external title against the PRism text and, with
// NoteSentence, the note's own sentence against the external body. A pair
// under the token floor scores 0. Whole-text Jaccard at or above the alias
// bar, the rule this one replaced, still scores as a match under the same
// floor; a subject name alone, which the old rule also took, no longer does.
func ExternalScoreWith(p payload.Finding, g ExternalFinding, opts MatchOptions) float64 {
	pText := prismText(p)
	gText := strings.TrimSpace(g.Title + " " + g.Body)
	best := 0.0
	if score, shared := overlap(p.Comment, gText); score >= similarityThreshold && shared >= ExternalMinShared {
		best = ExternalThreshold
	}
	consider := func(a, b string, floor int) {
		if a == "" || b == "" {
			return
		}
		if score, shared := Containment(a, b); shared >= floor && score > best {
			best = score
		}
	}
	consider(pText, gText, ExternalMinShared)
	consider(prismHeadline(p), gText, externalMinSharedHeadline)
	consider(g.Title, pText, externalMinSharedHeadline)
	if opts.NoteSentence {
		consider(noteSentence(p, opts.ShownSentence), gText, externalMinSharedHeadline)
	}
	return best
}

// MatchExternal is MatchExternalWith under the shipped options.
func MatchExternal(prism []payload.Finding, external []ExternalFinding) []Match {
	return MatchExternalWith(prism, external, MatchOptions{NoteSentence: true})
}

// MatchExternalWith pairs each PRism finding with the one external root it
// restates: same file, the PRism line inside the root's range plus the
// tolerance, and ExternalScoreWith at or above the threshold. A root with no
// line at all (file-level, or outdated with no original anchor) never
// matches: without a line the text alone is not the same point. A contract subject
// named in the root never makes a match on its own (distinct defects share
// a symbol all the time); it only breaks ties between roots of equal score.
// Each side is used at most once, best score first, then subject, then line
// distance.
func MatchExternalWith(prism []payload.Finding, external []ExternalFinding, opts MatchOptions) []Match {
	type cand struct {
		pi, gi   int
		score    float64
		subject  bool
		distance int
	}
	var cands []cand
	for pi, p := range prism {
		if isPseudoFile(p.File) || p.Line <= 0 {
			continue
		}
		for gi, g := range external {
			if g.EndLine <= 0 || !sameFile(p.File, g.File) || p.Line < g.StartLine-lineTolerance || p.Line > g.EndLine+lineTolerance {
				continue
			}
			score := ExternalScoreWith(p, g, opts)
			if score < ExternalThreshold {
				continue
			}
			cands = append(cands, cand{pi, gi, score, mentionsSubject(p, g), lineDistance(p.Line, g)})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.subject != b.subject {
			return a.subject
		}
		if a.distance != b.distance {
			return a.distance < b.distance
		}
		return a.pi < b.pi
	})
	usedP, usedG := map[int]bool{}, map[int]bool{}
	var out []Match
	for _, c := range cands {
		if usedP[c.pi] || usedG[c.gi] {
			continue
		}
		usedP[c.pi], usedG[c.gi] = true, true
		out = append(out, Match{FindingID: prism[c.pi].ID, External: external[c.gi], Score: c.score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FindingID < out[j].FindingID })
	return out
}

func prismText(p payload.Finding) string {
	parts := []string{p.Comment}
	if c := p.FindingContract; c != nil {
		parts = append(parts, c.Headline, c.CurrentImpact)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// noteSentence is the sentence the summary shows for a finding its
// contract does not title (no headline, no current impact): what shown
// picks, else the first sentence of the comment. Empty for a finding the
// headline or impact already names.
func noteSentence(p payload.Finding, shown func(payload.Finding) string) string {
	if c := p.FindingContract; c != nil {
		if strings.TrimSpace(c.Headline) != "" || (c.Materiality == "current_impact" && strings.TrimSpace(c.CurrentImpact) != "") {
			return ""
		}
	}
	if shown != nil {
		if s := strings.TrimSpace(shown(p)); s != "" {
			return s
		}
	}
	first, _ := splitFirstSentence(strings.Join(strings.Fields(p.Comment), " "))
	return first
}

func prismHeadline(p payload.Finding) string {
	if c := p.FindingContract; c != nil {
		if h := strings.TrimSpace(c.Headline); h != "" {
			return h
		}
		if i := strings.TrimSpace(c.CurrentImpact); i != "" {
			return i
		}
	}
	first, _ := splitFirstSentence(strings.Join(strings.Fields(p.Comment), " "))
	return first
}
