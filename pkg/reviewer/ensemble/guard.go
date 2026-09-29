package ensemble

import (
	"fmt"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// Draft is a proposed merged review: findings whose Sources are member ids,
// plus an optional structured summary.
type Draft struct {
	Findings []types.LineComment
	Summary  *types.SummaryBlock
}

// Report records every correction the guard made, for telemetry.
type Report struct {
	// Reinserted are members no draft finding cited; each came back as its
	// own finding.
	Reinserted []string `json:"reinserted,omitempty"`
	// Invented counts draft findings that cited no known member; they were
	// dropped, because the merge may reword claims but never add them.
	Invented int `json:"invented,omitempty"`
	// UnknownSources are cited ids that matched no member.
	UnknownSources []string `json:"unknown_sources,omitempty"`
	// Relocated counts findings moved to a cited member's location because
	// the draft named a file none of its sources did.
	Relocated int `json:"relocated,omitempty"`
	// SeverityCapped counts findings lowered to the highest severity any of
	// their sources carried.
	SeverityCapped int `json:"severity_capped,omitempty"`
	// SupportDowngraded counts criticals lowered to MEDIUM by the support
	// threshold.
	SupportDowngraded int `json:"support_downgraded,omitempty"`
}

// Result is the guarded merged review.
type Result struct {
	Findings []types.LineComment
	// Support[i] is the number of distinct runs behind Findings[i].
	Support []int
	Summary *types.SummaryBlock
	Report  Report

	draftIDs []string
}

// Options tunes the guard.
type Options struct {
	// MinSupportCritical downgrades a CRITICAL cited by fewer runs than this
	// to MEDIUM. Zero or one disables it.
	MinSupportCritical int
}

// Guard turns a draft into a Result that loses no member's finding:
//   - sources that name no member are dropped, and a finding left with no
//     sources is dropped as invented;
//   - a finding placed on a file none of its sources named takes its best
//     source's location;
//   - severity never exceeds the highest severity among its sources;
//   - a finding without a contract takes its best source's contract;
//   - every member no finding cites is reinserted as its own finding.
func Guard(d Draft, members []Member, opts Options) Result {
	byID := make(map[string]Member, len(members))
	for _, m := range members {
		byID[m.ID] = m
	}
	var res Result
	cited := map[string]bool{}
	for _, f := range d.Findings {
		var srcs []Member
		for _, id := range f.Sources {
			id = strings.TrimSpace(id)
			m, ok := byID[id]
			if !ok {
				res.Report.UnknownSources = append(res.Report.UnknownSources, id)
				continue
			}
			srcs = append(srcs, m)
		}
		if len(srcs) == 0 {
			res.Report.Invented++
			continue
		}
		best := bestMember(srcs)
		if !citesFile(srcs, f.FilePath) {
			f.FilePath, f.LineNumber = best.Finding.FilePath, best.Finding.LineNumber
			res.Report.Relocated++
		}
		maxImp := best.Finding.Importance
		if f.Importance == "" {
			f.Importance = maxImp
		} else if ImportanceRank(f.Importance) > ImportanceRank(maxImp) {
			f.Importance = maxImp
			res.Report.SeverityCapped++
		}
		if f.FindingContract == nil {
			f.FindingContract = best.Finding.FindingContract
		}
		f.Sources = memberIDs(srcs)
		for _, m := range srcs {
			cited[m.ID] = true
		}
		res.add(f, supportOf(srcs), f.ID)
	}
	for _, m := range members {
		if cited[m.ID] {
			continue
		}
		f := m.Finding
		f.Sources = []string{m.ID}
		res.add(f, 1, "")
		res.Report.Reinserted = append(res.Report.Reinserted, m.ID)
	}
	res.applySupportThreshold(opts.MinSupportCritical)
	renamed := res.assignIDs()
	res.Summary = guardSummary(d.Summary, res.Findings, renamed)
	return res
}

func (r *Result) add(f types.LineComment, support int, draftID string) {
	f.Provenance = "agent"
	f.State, f.Inactive = "", false
	r.Findings = append(r.Findings, f)
	r.Support = append(r.Support, support)
	r.draftIDs = append(r.draftIDs, draftID)
}

func (r *Result) applySupportThreshold(minSupport int) {
	if minSupport <= 1 {
		return
	}
	for i := range r.Findings {
		if strings.EqualFold(r.Findings[i].Importance, "CRITICAL") && r.Support[i] < minSupport {
			r.Findings[i].Importance = "MEDIUM"
			r.Report.SupportDowngraded++
		}
	}
}

// assignIDs gives merged findings fresh ids ("E-1") and returns the map from
// each draft finding's own id to its new one.
func (r *Result) assignIDs() map[string]string {
	renamed := map[string]string{}
	for i := range r.Findings {
		r.Findings[i].ID = fmt.Sprintf("E-%d", i+1)
		if id := r.draftIDs[i]; id != "" {
			renamed[id] = r.Findings[i].ID
		}
	}
	return renamed
}

// guardSummary keeps the author's summary with its priority ids translated to
// merged ids (dropping any that name no surviving finding), and derives a
// summary when the author gave none. A review that keeps an active critical
// always asks for changes.
func guardSummary(s *types.SummaryBlock, findings []types.LineComment, renamed map[string]string) *types.SummaryBlock {
	hasCritical := false
	for _, f := range findings {
		if strings.EqualFold(f.Importance, "CRITICAL") {
			hasCritical = true
		}
	}
	out := &types.SummaryBlock{}
	if s != nil {
		*out = *s
		out.PriorityIDs = nil
		for _, id := range s.PriorityIDs {
			if merged, ok := renamed[id]; ok {
				out.PriorityIDs = append(out.PriorityIDs, merged)
			}
		}
	}
	switch {
	case hasCritical:
		out.Verdict = "request_changes"
	case out.Verdict == "" && len(findings) > 0:
		out.Verdict = "approve_suggestions"
	case out.Verdict == "":
		out.Verdict = "approve"
	}
	return out
}

func citesFile(srcs []Member, file string) bool {
	for _, m := range srcs {
		if normPath(m.Finding.FilePath) == normPath(file) {
			return true
		}
	}
	return false
}

// bestMember is the highest-severity member, then the one with the longest
// comment (the most specific statement of the issue), then the earliest run.
func bestMember(ms []Member) Member {
	best := ms[0]
	for _, m := range ms[1:] {
		rm, rb := ImportanceRank(m.Finding.Importance), ImportanceRank(best.Finding.Importance)
		switch {
		case rm > rb:
			best = m
		case rm == rb && len(m.Finding.CommentBody) > len(best.Finding.CommentBody):
			best = m
		}
	}
	return best
}

func supportOf(ms []Member) int {
	runs := map[int]bool{}
	for _, m := range ms {
		runs[m.Run] = true
	}
	return len(runs)
}

func memberIDs(ms []Member) []string {
	ids := make([]string, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		if !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Deterministic merges without a model: one finding per cluster, worded by
// the cluster's best member and citing every member.
func Deterministic(clusters []Cluster) Draft {
	d := Draft{}
	for _, c := range clusters {
		f := bestMember(c.Members).Finding
		f.Sources = memberIDs(c.Members)
		d.Findings = append(d.Findings, f)
	}
	return d
}
