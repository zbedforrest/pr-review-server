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
	externalMinShared         = 3
	externalMinSharedHeadline = 4
)

// Match is one PRism finding paired with the external root it restates.
type Match struct {
	FindingID string
	External  ExternalFinding
	Score     float64
}

// ExternalScore is how much of the shorter text the two comments share: the
// best of whole-text containment, the PRism headline against the external
// body and the external title against the PRism text. A pair under the
// token floor scores 0. Whole-text Jaccard at or above the alias bar, the
// rule this one replaced, still scores as a match so nothing the old rule
// caught is lost.
func ExternalScore(p payload.Finding, g ExternalFinding) float64 {
	pText := prismText(p)
	gText := strings.TrimSpace(g.Title + " " + g.Body)
	best := 0.0
	if Similarity(p.Comment, gText) >= similarityThreshold {
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
	consider(pText, gText, externalMinShared)
	consider(prismHeadline(p), gText, externalMinSharedHeadline)
	consider(g.Title, pText, externalMinSharedHeadline)
	return best
}

// MatchExternal pairs each PRism finding with the one external root it
// restates: same file, the PRism line inside the root's range plus the
// tolerance, and either ExternalScore at or above the threshold or a
// contract subject named in the root. Each side is used at most once, best
// score first, ties broken by line distance.
func MatchExternal(prism []payload.Finding, external []ExternalFinding) []Match {
	type cand struct {
		pi, gi   int
		score    float64
		distance int
	}
	var cands []cand
	for pi, p := range prism {
		if isPseudoFile(p.File) || p.Line <= 0 {
			continue
		}
		for gi, g := range external {
			if !sameFile(p.File, g.File) || p.Line < g.StartLine-lineTolerance || p.Line > g.EndLine+lineTolerance {
				continue
			}
			score := ExternalScore(p, g)
			if score < ExternalThreshold && !mentionsSubject(p, g) {
				continue
			}
			cands = append(cands, cand{pi, gi, score, lineDistance(p.Line, g)})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
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
