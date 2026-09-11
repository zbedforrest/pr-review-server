package publisher

import (
	"sort"
	"strings"

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
	// ShowUnverified folds the active first-pass claims the agent did not
	// confirm into the summary comment; they are never inline.
	ShowUnverified bool
}

// DefaultPolicy is the shipped posting policy: three inline comments per
// round, medium severity and above, unverified first-pass claims folded.
func DefaultPolicy() Policy {
	return Policy{InlineCap: DefaultInlineCap, InlineMinSeverity: DefaultInlineMinSeverity, ShowUnverified: true}
}

// withDefaults fills unset fields only. A zero cap is a real setting (post
// nothing inline); negative means unset. ShowUnverified is taken as given.
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

// Publishable reports whether a finding may be asserted on GitHub as a
// bullet or inline comment. Only active confirmed findings from the agent or
// a VIOLATED check qualify; unverified claims (first-pass re-admissions and
// carried findings) have their own fold, first-pass and mechanical alerts
// never post, and a raw bug-memory alert is not a finding.
func Publishable(f payload.Finding) bool {
	// Only confirmed claims are asserted as bullets; unverified ones have
	// their own fold (UnverifiedNote) and must not appear twice.
	if !f.Active || isNarrative(f) || (f.State != "" && f.State != "confirmed") {
		return false
	}
	switch f.Provenance {
	case "first-pass", "mechanical":
		return false
	case "required-check":
		return !isBugMemoryAlert(f)
	}
	return true
}

// An unanswered memory-derived check is re-admitted as its raw alert text;
// only checks the agent answered VIOLATED are confirmed.
func isBugMemoryAlert(f payload.Finding) bool {
	return strings.HasPrefix(commentText(f), "**Bug-memory alert")
}

// UnverifiedNote reports whether a finding belongs in the summary's folded
// unverified section: an active first-pass claim the agent left unverified or
// disputed. These are shown with a status marker, never asserted or inline.
func UnverifiedNote(f payload.Finding) bool {
	return f.Active && f.State == "unverified" && !isNarrative(f) && !isBugMemoryAlert(f)
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
		if !Shown(f) || alreadyPublished[f.ID] {
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

// Shown is the bar for appearing on GitHub at all: a critical finding, or one
// whose contract asserts current production impact (the inline bar). Lower
// findings live only on the dashboard.
func Shown(f payload.Finding) bool {
	if !Publishable(f) {
		return false
	}
	return f.Severity == "critical" || (f.Severity == "medium" && worthInline(f))
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
