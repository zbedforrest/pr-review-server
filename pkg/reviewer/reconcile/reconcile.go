package reconcile

import (
	"path"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

const (
	lineTolerance       = 10
	similarityThreshold = 0.20
)

// Tagged is a PRism finding annotated with where else it was reported.
type Tagged struct {
	Finding          payload.Finding
	SourceTag        string
	MatchedCommentID int64
}

// Result pairs the tagged PRism findings (in input order) with the external
// findings no PRism finding matched.
type Result struct {
	Findings     []Tagged
	GreptileOnly []ExternalFinding
}

type candidate struct {
	prism, greptile int
	score           float64
	distance        int
}

// Reconcile tags each PRism finding against Greptile's posted findings. A pair
// matches when the files agree, the PRism line falls within the Greptile range
// plus a tolerance, and the texts are similar or a contract subject name is
// quoted. Each finding on either side is matched at most once, best Jaccard
// first, ties broken by line distance. SUMMARY and CHECK pseudo-files are
// always prism-only and never consume a Greptile finding.
func Reconcile(prism []payload.Finding, greptile []ExternalFinding) Result {
	var candidates []candidate
	for pi, p := range prism {
		if isPseudoFile(p.File) || p.Line <= 0 {
			continue
		}
		for gi, g := range greptile {
			if !sameFile(p.File, g.File) {
				continue
			}
			if p.Line < g.StartLine-lineTolerance || p.Line > g.EndLine+lineTolerance {
				continue
			}
			score := Similarity(p.Comment, g.Title+" "+g.Body)
			if score < similarityThreshold && !mentionsSubject(p, g) {
				continue
			}
			candidates = append(candidates, candidate{prism: pi, greptile: gi, score: score, distance: lineDistance(p.Line, g)})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.score != b.score {
			return a.score > b.score
		}
		return a.distance < b.distance
	})

	matchedPrism := make(map[int]int64, len(candidates))
	matchedGreptile := make(map[int]bool, len(candidates))
	for _, c := range candidates {
		if _, done := matchedPrism[c.prism]; done || matchedGreptile[c.greptile] {
			continue
		}
		matchedPrism[c.prism] = greptile[c.greptile].CommentID
		matchedGreptile[c.greptile] = true
	}

	res := Result{Findings: make([]Tagged, 0, len(prism))}
	for pi, p := range prism {
		t := Tagged{Finding: p, SourceTag: SourceTagPrismOnly}
		if id, ok := matchedPrism[pi]; ok {
			t.SourceTag = SourceTagBoth
			t.MatchedCommentID = id
		}
		res.Findings = append(res.Findings, t)
	}
	for gi, g := range greptile {
		if !matchedGreptile[gi] {
			res.GreptileOnly = append(res.GreptileOnly, g)
		}
	}
	return res
}

func isPseudoFile(file string) bool {
	return file == "SUMMARY" || file == "CHECK"
}

func mentionsSubject(p payload.Finding, g ExternalFinding) bool {
	if p.FindingContract == nil {
		return false
	}
	text := strings.ToLower(g.Title + " " + g.Body)
	for _, s := range p.FindingContract.Subjects {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		if name != "" && strings.Contains(text, name) {
			return true
		}
	}
	return false
}

func lineDistance(line int, g ExternalFinding) int {
	switch {
	case line < g.StartLine:
		return g.StartLine - line
	case line > g.EndLine:
		return line - g.EndLine
	default:
		return 0
	}
}

// sameFile matches paths exactly, by whole-path suffix, or by bare basename:
// different producers emit different amounts of leading path for one file.
func sameFile(a, b string) bool {
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a) || path.Base(a) == path.Base(b)
}
