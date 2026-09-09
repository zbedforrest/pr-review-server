package html

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// surfaceAlertsEnabled reports whether the deterministic alerts are also
// pinned at the top of the report; they always render inside review details.
func surfaceAlertsEnabled() bool {
	return os.Getenv("SURFACE_ALERTS") == "true"
}

// AlertView is one row of the "Deterministic alerts" list.
type AlertView struct {
	Provenance string // "mechanical" or "required-check"
	FilePath   string
	LineNumber int
	Importance string
	Body       string // raw Markdown; the template runs it through markdownify
	CheckID    string // required-check id (CHK-...) when resolvable, else ""
	Verdict    string // VIOLATED | SAFE | NOT-APPLICABLE | UNANSWERED; "" when unknown
	Evidence   string // "reference resolved" when the check's evidence path resolved, else ""
}

// VerdictClass returns the CSS-class suffix for the verdict badge.
func (a AlertView) VerdictClass() string {
	return strings.ToLower(a.Verdict)
}

// Body-embedded check-id markers, written by service/checks.go:
// EnforceRequiredChecks appends "_Required check CHK-x was not answered with
// evidence ..." to unresolved gate alerts and synthesizes "**Required check
// CHK-x answered VIOLATED ...**" findings.
var (
	alertViolatedRe   = regexp.MustCompile(`Required check (CHK-[a-z0-9-]+) answered VIOLATED`)
	alertUnansweredRe = regexp.MustCompile(`Required check (CHK-[a-z0-9-]+) was not answered with evidence`)
	checkIDNumRe      = regexp.MustCompile(`^CHK-([a-z0-9-]+?)-([0-9]+)$`)
)

const evidenceResolvedLabel = "reference resolved"

// syncKindCounter advances the per-kind positional counter past an id that
// arrived embedded in an alert body, so later same-kind alerts without an
// embedded id keep the CHK-<kind>-N numbering BuildRequiredChecks assigned.
func syncKindCounter(perKind map[string]int, id string) {
	m := checkIDNumRe.FindStringSubmatch(id)
	if m == nil {
		return
	}
	if n, err := strconv.Atoi(m[2]); err == nil && n > perKind[m[1]] {
		perKind[m[1]] = n
	}
}

// buildDeterministicAlerts collects the mechanical / required-check findings
// in merge order and resolves each one's check verdict from the structured
// check records. An id embedded in the body (enforcement notes, VIOLATED
// syntheses) is authoritative; otherwise the alert's gate kind is matched
// positionally to the CHK-<kind>-N ids BuildRequiredChecks numbers in firing
// order. The verdict is best-effort display; the alert row renders regardless.
func buildDeterministicAlerts(comments []types.LineComment, checks []CheckRecord) []AlertView {
	byID := make(map[string]CheckRecord, len(checks))
	for _, c := range checks {
		if _, ok := byID[c.ID]; !ok {
			byID[c.ID] = c
		}
	}
	perKind := map[string]int{}
	var out []AlertView
	for _, c := range comments {
		if c.FilePath == "SUMMARY" || c.FilePath == "GENERAL" {
			continue
		}
		prov := payload.DeriveProvenance(c)
		if !payload.IsDeterministicProvenance(prov) {
			continue
		}
		v := AlertView{
			Provenance: prov,
			FilePath:   c.FilePath,
			LineNumber: c.LineNumber,
			Importance: c.Importance,
			Body:       payload.StripProvenanceNote(c.CommentBody),
		}
		if m := alertViolatedRe.FindStringSubmatch(c.CommentBody); m != nil {
			v.CheckID, v.Verdict = m[1], "VIOLATED"
			syncKindCounter(perKind, v.CheckID)
		} else if m := alertUnansweredRe.FindStringSubmatch(c.CommentBody); m != nil {
			v.CheckID, v.Verdict = m[1], "UNANSWERED"
			syncKindCounter(perKind, v.CheckID)
		} else if kind := payload.AlertKind(c.CommentBody); kind != "" {
			perKind[kind]++
			candidate := fmt.Sprintf("CHK-%s-%d", kind, perKind[kind])
			if _, ok := byID[candidate]; ok {
				v.CheckID = candidate
			}
		}
		if rec, ok := byID[v.CheckID]; ok && v.CheckID != "" {
			v.Verdict = rec.Verdict
			if rec.EvidenceResolved {
				v.Evidence = evidenceResolvedLabel
			}
		}
		out = append(out, v)
	}
	return out
}
