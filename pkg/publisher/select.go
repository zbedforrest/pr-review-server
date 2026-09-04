package publisher

import (
	"sort"

	"pr-review-server/pkg/reviewer/payload"
)

const (
	DefaultInlineCap         = 5
	DefaultInlineMinSeverity = "medium"

	summaryFile = "SUMMARY"
	checkFile   = "CHECK"
)

type Policy struct {
	InlineCap         int
	InlineMinSeverity string
}

func (p Policy) withDefaults() Policy {
	if p.InlineCap <= 0 {
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
		if isNarrative(f) || alreadyPublished[f.ID] {
			continue
		}
		if severityRank(f.Severity) >= minRank && f.Line > 0 && commentable[f.File][f.Line] {
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
