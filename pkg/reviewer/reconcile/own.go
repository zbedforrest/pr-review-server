package reconcile

import (
	"regexp"
	"sort"

	"pr-review-server/pkg/reviewer/payload"
)

// OwnComment is an inline comment PRism itself posted earlier on the PR,
// recognised by the finding marker in its body.
type OwnComment struct {
	CommentID int64
	FindingID string
	File      string
	Line      int
	Text      string
}

var ownMarkerRe = regexp.MustCompile(`<!-- prism:finding:([^\s>]+) -->`)

// ParseOwnComments extracts PRism's own prior inline comments.
func ParseOwnComments(cs []ExternalComment) []OwnComment {
	var out []OwnComment
	for _, c := range cs {
		m := ownMarkerRe.FindStringSubmatch(c.Body)
		if m == nil || c.InReplyToID != 0 {
			continue
		}
		out = append(out, OwnComment{
			CommentID: c.ID,
			FindingID: m[1],
			File:      c.Path,
			Line:      c.Line,
			Text:      ownMarkerRe.ReplaceAllString(c.Body, ""),
		})
	}
	return out
}

// AliasPrior maps the id of each current finding that restates a finding
// PRism already posted (same file, nearby line, similar text) to the id it
// was published under, so re-reviews keep one identity per defect instead of
// posting the reworded finding again. Matching is greedy by similarity, one
// prior comment per finding.
func AliasPrior(current []payload.Finding, own []OwnComment) map[string]string {
	type cand struct {
		cur, prior int
		score      float64
	}
	var cands []cand
	for ci, f := range current {
		if isPseudoFile(f.File) {
			continue
		}
		for oi, o := range own {
			if o.FindingID == f.ID || !sameFile(f.File, o.File) {
				continue
			}
			if o.Line > 0 && f.Line > 0 && abs(f.Line-o.Line) > lineTolerance {
				continue
			}
			if score := Similarity(f.Comment, o.Text); score >= similarityThreshold {
				cands = append(cands, cand{ci, oi, score})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	aliases := map[string]string{}
	usedPrior := map[int]bool{}
	for _, c := range cands {
		if _, done := aliases[current[c.cur].ID]; done || usedPrior[c.prior] {
			continue
		}
		aliases[current[c.cur].ID] = own[c.prior].FindingID
		usedPrior[c.prior] = true
	}
	return aliases
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
