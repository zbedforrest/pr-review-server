package reconcile

import (
	"regexp"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// OwnComment is a finding PRism itself published earlier on the PR: an
// inline comment recognised by the marker in its body, or a ledger row. Text
// is the raw agent prose when the ledger kept it, else the stripped body;
// Kind and Subjects come from the finding contract when known.
type OwnComment struct {
	CommentID int64
	FindingID string
	File      string
	Line      int
	Text      string
	Kind      string
	Subjects  []string
}

var ownMarkerRe = regexp.MustCompile(`<!-- prism:finding:([^\s>]+) -->`)

// ParseOwnComments extracts PRism's own prior inline comments. Ownership is
// established by the ledger's comment ids, not by the marker: a quote-reply
// copies the marker verbatim and anyone can type one.
func ParseOwnComments(cs []ExternalComment, ledgerCommentIDs map[int64]bool) []OwnComment {
	var out []OwnComment
	for _, c := range cs {
		m := ownMarkerRe.FindStringSubmatch(c.Body)
		if m == nil || c.InReplyToID != 0 || !ledgerCommentIDs[c.ID] {
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

// SubjectNames returns the contract's subject names lower-cased, trimmed,
// de-duplicated and sorted, so two findings about the same symbols compare
// equal whatever order the agent listed them in.
func SubjectNames(c *types.FindingContract) []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range c.Subjects {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Alias match tiers, best first: raw text overlap near the same line, the
// line-independent (file, kind, subjects) key, and the same kind and
// subjects in another file after a re-anchoring.
const (
	tierText = iota
	tierKey
	tierCrossFile
)

// minSharedTokens keeps one shared word in two short comments from passing
// the Jaccard bar.
const minSharedTokens = 2

// AliasPrior maps the id of each current finding that restates a finding
// PRism already published to the id it was published under, so re-reviews
// keep one identity per defect instead of posting the reworded finding again.
// A prior whose id a current finding still carries verbatim belongs to that
// finding alone. Matching is greedy, one prior per finding: text matches
// (same file, line within ten, raw Jaccard at or above 0.20) win over the
// subject key (same file, kind and subjects, any line), which wins over a
// cross-file match (different file, kind known and equal on both sides, and
// either two or more subjects or two shared words, so one enclosing
// function alone never joins two files).
func AliasPrior(current []payload.Finding, own []OwnComment) map[string]string {
	type cand struct {
		cur, prior int
		tier       int
		score      float64
		distance   int
	}
	var cands []cand
	currentIDs := make(map[string]bool, len(current))
	for _, f := range current {
		currentIDs[f.ID] = true
	}
	for ci, f := range current {
		if isPseudoFile(f.File) {
			continue
		}
		subjects := SubjectNames(f.FindingContract)
		kind := ""
		if f.FindingContract != nil {
			kind = f.FindingContract.FindingKind
		}
		for oi, o := range own {
			if currentIDs[o.FindingID] {
				continue
			}
			keyed := len(subjects) > 0 && len(o.Subjects) > 0 && sameSubjects(subjects, o.Subjects) && (kind == "" || o.Kind == "" || kind == o.Kind)
			if !sameFile(f.File, o.File) {
				if keyed && kind != "" && kind == o.Kind && crossFileEvidence(subjects, f.Comment, o.Text) {
					cands = append(cands, cand{ci, oi, tierCrossFile, 0, 0})
				}
				continue
			}
			distance := 0
			if o.Line > 0 && f.Line > 0 {
				distance = abs(f.Line - o.Line)
			}
			if distance <= lineTolerance {
				if score, shared := overlap(f.Comment, o.Text); score >= similarityThreshold && shared >= minSharedTokens {
					cands = append(cands, cand{ci, oi, tierText, score, distance})
					continue
				}
			}
			if keyed {
				cands = append(cands, cand{ci, oi, tierKey, 0, distance})
			}
		}
	}
	// A higher-severity restatement claims the row before a lower one with
	// more word overlap: the row's severity would otherwise be handed to a
	// note while the real restatement posts again.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if ra, rb := severityRank(current[a.cur].Severity), severityRank(current[b.cur].Severity); ra != rb {
			return ra > rb
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.distance < b.distance
	})
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

func crossFileEvidence(subjects []string, text, oText string) bool {
	if len(subjects) >= 2 {
		return true
	}
	_, shared := overlap(text, oText)
	return shared >= minSharedTokens
}

func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func sameSubjects(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
