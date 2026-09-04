package publisher

import (
	"sort"

	"pr-review-server/pkg/reviewer/payload"
)

// Greptile posts about half an inline comment per PR; the cap and the
// current-impact gate below are what keep PRism in that range.
const (
	DefaultInlineCap         = 3
	DefaultInlineMinSeverity = "medium"

	summaryFile = "SUMMARY"
	checkFile   = "CHECK"
)

type Policy struct {
	InlineCap         int
	InlineMinSeverity string
}

// DefaultPolicy is the shipped posting policy: five inline comments per
// round, medium severity and above.
func DefaultPolicy() Policy {
	return Policy{InlineCap: DefaultInlineCap, InlineMinSeverity: DefaultInlineMinSeverity}
}

// withDefaults fills unset fields only. A zero cap is a real setting (post
// nothing inline); negative means unset.
func (p Policy) withDefaults() Policy {
	if p.InlineCap < 0 {
		p.InlineCap = DefaultInlineCap
	}
	if p.InlineMinSeverity == "" {
		p.InlineMinSeverity = DefaultInlineMinSeverity
	}
	return p
}

type Selection struct {
	Inline      []payload.Finding
	Annotations []payload.Finding
}

// severityRank orders critical > medium > low > unknown; higher is worse.
func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func isNarrative(f payload.Finding) bool {
	return f.File == summaryFile || f.File == checkFile
}

// Publishable reports whether a finding may appear on GitHub at all. Only
// findings the review agent produced or answered for (agent, required-check,
// carried from an earlier agent round, or legacy sidecars without provenance)
// qualify; first-pass re-admissions and raw gate alerts stay on the dashboard,
// where their unconfirmed status is explained.
func Publishable(f payload.Finding) bool {
	if isNarrative(f) {
		return false
	}
	switch f.Provenance {
	case "first-pass", "mechanical":
		return false
	}
	return true
}

func sortBySeverity(fs []payload.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}

func Select(findings []payload.Finding, alreadyPublished map[string]bool, commentable map[string]map[int]bool, p Policy) Selection {
	p = p.withDefaults()
	minRank := severityRank(p.InlineMinSeverity)

	var candidates, rest []payload.Finding
	for _, f := range findings {
		if !Publishable(f) || alreadyPublished[f.ID] {
			continue
		}
		if severityRank(f.Severity) >= minRank && f.Line > 0 && commentable[f.File][f.Line] && worthInline(f) {
			candidates = append(candidates, f)
		} else {
			rest = append(rest, f)
		}
	}
	sortBySeverity(candidates)

	var sel Selection
	if len(candidates) > p.InlineCap {
		rest = append(rest, candidates[p.InlineCap:]...)
		candidates = candidates[:p.InlineCap]
	}
	sel.Inline = candidates
	sortBySeverity(rest)
	sel.Annotations = rest
	return sel
}

// worthInline is the Greptile-style bar for occupying a reviewer's diff view:
// the agent must assert an impact that exists today on a behavior or security
// finding. Latent hazards, design and test notes, and anything with unknown
// materiality stay in the folded summary table.
func worthInline(f payload.Finding) bool {
	c := f.FindingContract
	if c == nil || f.FindingContractStatus != "valid" || c.Materiality != "current_impact" {
		return false
	}
	return c.FindingKind == "production_behavior" || c.FindingKind == "security_risk"
}
