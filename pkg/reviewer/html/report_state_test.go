package html

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pr-review-server/pkg/reviewer/types"
)

// stateFixtureInput is a Phase B review: state on every finding, a structured
// SUMMARY, one disputed claim and three inactive records.
func stateFixtureInput() ReportInput {
	in := layoutFixtureInput()
	in.Comments = []types.LineComment{
		{ID: "A-1", FilePath: "app.go", LineNumber: 13, Importance: "CRITICAL", Provenance: "agent", State: "confirmed",
			CommentBody: "Nil dereference when the config is missing. The handler reads cfg.Name before the nil check."},
		{FilePath: "app.go", LineNumber: 0, Importance: "MEDIUM", Provenance: "first-pass", State: "unverified",
			CommentBody:     "**Retry loop never backs off.** Each attempt fires immediately.",
			FindingContract: &types.FindingContract{Headline: "Retry loop has no backoff"}},
		{FilePath: "GENERAL", LineNumber: 0, Importance: "LOW", Provenance: "carried", State: "unverified",
			CommentBody: "Consider documenting the new env var in the deployment guide."},
		{FilePath: "worker.py", LineNumber: 0, Importance: "MEDIUM", Provenance: "mechanical",
			CommentBody: "**Mechanical alert: task payload contract changed.** The worker signature gained a field."},
		{FilePath: "handlers.py", LineNumber: 0, Importance: "MEDIUM", Provenance: "required-check", State: "confirmed",
			CommentBody: "**Required check CHK-settings-ref-1 answered VIOLATED without an accompanying finding.** See handlers.py:42."},
		{FilePath: "SUMMARY", LineNumber: 0,
			Summary: &types.SummaryBlock{Verdict: "request_changes", Upshot: "The nil dereference is a real crash path.",
				PriorityIDs: []string{"A-1", "FP-9", "A-2"}, Notes: "Coverage is otherwise adequate."},
			CommentBody: "Verdict: request changes.\n\nThe nil dereference is a real crash path.\n\nFix first:\n1. Nil dereference when the config is missing (app.go:13)\n\nCoverage is otherwise adequate."},
		{FilePath: "lib.go", LineNumber: 7, Importance: "CRITICAL", Provenance: "first-pass", State: "unverified",
			CommentBody: "Cache key ignores the tenant. Two tenants share one cache entry.",
			Assessment: &types.Disposition{SourceID: "FP-2", State: "rejected", Reason: "The cache key includes the tenant id.",
				Evidence: []types.EvidenceRef{{File: "cache.go", Line: 22}}}},
		{FilePath: "api.go", LineNumber: 8, Importance: "MEDIUM", Provenance: "first-pass", State: "rejected", Inactive: true,
			CommentBody: "Missing error check on Decode.",
			Original:    &types.OriginalClaim{SourceID: "FP-3", FilePath: "api.go", LineNumber: 8, Comment: "Missing error check on Decode."},
			Assessment: &types.Disposition{SourceID: "FP-3", State: "rejected", Reason: "Decode errors are handled by the caller.",
				Evidence: []types.EvidenceRef{{File: "api.go", Line: 30}, {File: "api.go"}}}},
		{FilePath: "util.go", LineNumber: 3, Importance: "LOW", Provenance: "first-pass", State: "unverified", Inactive: true,
			CommentBody: "Unexamined low claim about naming."},
		{FilePath: "app.go", LineNumber: 13, Importance: "CRITICAL", Provenance: "first-pass", State: "merged", Inactive: true, MergedInto: "A-1",
			CommentBody: "Restated nil dereference claim."},
		{ID: "A-2", FilePath: "db.go", LineNumber: 5, Importance: "MEDIUM", Provenance: "agent", State: "confirmed",
			CommentBody: "Connection leak on error. The conn is never closed when Ping fails."},
	}
	return in
}

func indexSection(t *testing.T, report string) string {
	t.Helper()
	start := strings.Index(report, "<h2>Findings</h2>")
	end := strings.Index(report, "<h2>General Comments</h2>")
	require.Greater(t, start, -1)
	require.Greater(t, end, start)
	return report[start:end]
}

func detailsSection(t *testing.T, report string) string {
	t.Helper()
	start := strings.Index(report, "<summary>Review details</summary>")
	require.Greater(t, start, -1)
	return report[start:]
}

func TestGenerateReport_FindingsIndexGroupsByState(t *testing.T) {
	report := renderLayoutFixture(t, stateFixtureInput())
	index := indexSection(t, report)

	idx := sectionIndices(t, index, "<h3>Confirmed</h3>", "<h3>Needs verification</h3>", "<h3>Mechanical signals</h3>")
	confirmed, needs, mech := index[idx[0]:idx[1]], index[idx[1]:idx[2]], index[idx[2]:]

	assert.Contains(t, confirmed, "app.go:13")
	assert.Contains(t, confirmed, "handlers.py")
	assert.Contains(t, confirmed, "db.go:5")
	assert.NotContains(t, confirmed, "UNVERIFIED")
	assert.NotContains(t, confirmed, "DISPUTED")

	assert.Contains(t, needs, "FIRST PASS · UNVERIFIED")
	assert.Contains(t, needs, "Retry loop has no backoff")
	assert.Contains(t, needs, "CARRIED · UNVERIFIED")
	assert.Contains(t, needs, "GENERAL")
	assert.Contains(t, needs, "FIRST PASS · DISPUTED")
	assert.Contains(t, needs, "lib.go:7")

	assert.Contains(t, mech, ">MECHANICAL<")
	assert.Contains(t, mech, "worker.py")

	for _, inactive := range []string{"api.go:8", "util.go:3", "Restated nil dereference", "Missing error check", "Unexamined low claim"} {
		assert.NotContains(t, index, inactive, "inactive records stay out of the findings index")
	}
}

func TestGenerateReport_LegacyCommentsWithoutStateStillGroupByProvenance(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	index := indexSection(t, report)
	idx := sectionIndices(t, index, "<h3>Confirmed</h3>", "<h3>Needs verification</h3>")
	assert.Contains(t, index[idx[1]:], "FIRST PASS · UNVERIFIED")
	assert.Contains(t, index[idx[1]:], "CARRIED · UNVERIFIED")
	assert.NotContains(t, index[idx[0]:idx[1]], "GENERAL")
}

func TestGenerateReport_DisputedFindingShowsCounterargument(t *testing.T) {
	report := renderLayoutFixture(t, stateFixtureInput())
	markup := report[:strings.Index(report, "<summary>Review details</summary>")]

	assert.Equal(t, 2, strings.Count(markup, "FIRST PASS · DISPUTED"), "index row plus detailed comment")
	require.Greater(t, strings.Index(markup, `class="counterargument"`), strings.Index(markup, "Two tenants share one cache entry."), "the counterargument follows the claim body")
	counter := markup[strings.Index(markup, `class="counterargument"`):]
	counter = counter[:strings.Index(counter, "</div>")]
	assert.Contains(t, counter, "Agent rejected:")
	assert.Contains(t, counter, "The cache key includes the tenant id.")
	assert.Contains(t, counter, "cache.go:22")
	assert.Equal(t, 1, strings.Count(markup, `class="counterargument"`), "only the disputed finding carries a counterargument")
}

func TestGenerateReport_InactiveRecordsExcludedFromCountsAndDetail(t *testing.T) {
	report := renderLayoutFixture(t, stateFixtureInput())
	start := strings.Index(report, `class="verdict-block"`)
	end := strings.Index(report, "<h2>Review Summary</h2>")
	require.Greater(t, end, start)
	block := report[start:end]
	assert.Contains(t, block, "3 confirmed")
	assert.Contains(t, block, "3 unverified")
	assert.Contains(t, block, "1 mechanical")
	assert.Contains(t, block, "1 rejected")

	body := report[:strings.Index(report, "<summary>Review details</summary>")]
	for _, inactive := range []string{"Missing error check on Decode", "Unexamined low claim", "Restated nil dereference"} {
		assert.NotContains(t, body, inactive, "inactive records must not render as comments")
	}
	assert.Contains(t, report, "Comment 1 of 8", "the counter covers active comments only")
	assert.NotContains(t, report, "of 11")
}

func TestGenerateReport_RejectedCountHiddenWhenZero(t *testing.T) {
	in := stateFixtureInput()
	var kept []types.LineComment
	for _, c := range in.Comments {
		if c.State != "rejected" {
			kept = append(kept, c)
		}
	}
	in.Comments = kept
	report := renderLayoutFixture(t, in)
	assert.NotContains(t, report, " rejected</span>")
	assert.NotContains(t, report, "<h3>Rejected after investigation</h3>")
}

func TestGenerateReport_ReviewDetailsListsInactiveRecords(t *testing.T) {
	report := renderLayoutFixture(t, stateFixtureInput())
	details := detailsSection(t, report)

	idx := sectionIndices(t, details,
		"<h3>Rejected after investigation</h3>",
		"<h3>First-pass claims not examined</h3>",
		"<h3>Merged claims</h3>",
		"<h3>Required checks</h3>",
	)
	for i := 1; i < len(idx); i++ {
		assert.Greater(t, idx[i], idx[i-1], "record section %d out of order", i)
	}

	rejected := details[idx[0]:idx[1]]
	assert.Contains(t, rejected, `class="sev-pill sev-medium"`)
	assert.Contains(t, rejected, "api.go:8")
	assert.Contains(t, rejected, "Missing error check on Decode.")
	assert.Contains(t, rejected, "Rejected because: Decode errors are handled by the caller.")
	assert.Contains(t, rejected, "api.go:30")

	unexamined := details[idx[1]:idx[2]]
	assert.Contains(t, unexamined, `class="sev-pill sev-low"`)
	assert.Contains(t, unexamined, "util.go:3")
	assert.Contains(t, unexamined, "Unexamined low claim about naming.")

	merged := details[idx[2]:idx[3]]
	assert.Contains(t, merged, "app.go:13")
	assert.Contains(t, merged, "Restated nil dereference claim.")
	assert.Contains(t, merged, "folded into")
	assert.Contains(t, merged, `href="#finding-1"`)
}

func TestGenerateReport_ReviewDetailsOmitsEmptyRecordSections(t *testing.T) {
	report := renderLayoutFixture(t, layoutFixtureInput())
	details := detailsSection(t, report)
	assert.NotContains(t, details, "Rejected after investigation")
	assert.NotContains(t, details, "First-pass claims not examined")
	assert.NotContains(t, details, "Merged claims")
}

func TestGenerateReport_StructuredSummaryDrivesVerdictUpshotAndFixFirst(t *testing.T) {
	report := renderLayoutFixture(t, stateFixtureInput())
	start := strings.Index(report, `class="verdict-block"`)
	end := strings.Index(report, `class="fix-first"`)
	require.Greater(t, start, -1)
	require.Greater(t, end, start, "Fix first lives inside the verdict card")
	block := report[start:end]
	assert.Contains(t, block, `data-verdict="changes"`)
	assert.Contains(t, block, "Verdict: request changes.")
	assert.Contains(t, block, "The nil dereference is a real crash path.")

	assert.NotContains(t, report, "<h2>Suggestions</h2>")
	assert.NotContains(t, report, "Next actions")
	actionsEnd := strings.Index(report, "<h2>Review Summary</h2>")
	actions := report[end:actionsEnd]
	assert.Contains(t, actions, "Fix first")
	assert.Contains(t, actions, "The reviewer's pick of the findings below; the full list follows.")

	first := strings.Index(actions, `href="#finding-1"`)
	second := strings.Index(actions, `href="#finding-8"`)
	assert.Greater(t, first, -1, "A-1 resolves to the first finding")
	assert.Greater(t, second, first, "A-2 follows in priority order")
	assert.Contains(t, actions, "Nil dereference when the config is missing.")
	assert.Contains(t, actions, "Connection leak on error.")
	assert.Contains(t, actions, "db.go:5")
	assert.NotContains(t, actions, "FP-9", "unresolvable ids are skipped")
	assert.Equal(t, 2, strings.Count(actions, "<li"))

	summary := report[actionsEnd:strings.Index(report, "<h2>Findings</h2>")]
	assert.Contains(t, summary, "Coverage is otherwise adequate.")
	assert.NotContains(t, summary, "Verdict:")
	assert.NotContains(t, summary, "Fix first:")
	assert.NotContains(t, summary, "real crash path")
}

func TestGenerateReport_FixFirstIsHiddenWhenItWouldNotShortenTheList(t *testing.T) {
	in := stateFixtureInput()
	report := renderLayoutFixture(t, in)
	require.Contains(t, report, `class="fix-first"`, "two picks out of seven findings is a short list")

	everything := in
	everything.Comments = append([]types.LineComment(nil), in.Comments...)
	all := &types.SummaryBlock{Verdict: "request_changes", Upshot: "Everything matters."}
	for _, c := range in.Comments {
		if c.Inactive || c.FilePath == "SUMMARY" {
			continue
		}
		ref := c.ID
		if ref == "" {
			ref = c.FilePath
			if c.LineNumber > 0 {
				ref = fmt.Sprintf("%s:%d", c.FilePath, c.LineNumber)
			}
		}
		all.PriorityIDs = append(all.PriorityIDs, ref)
	}
	everything.Comments[5].Summary = all
	report = renderLayoutFixture(t, everything)
	assert.NotContains(t, report, `class="fix-first"`, "picking every finding is not a short list")
}

func TestGenerateReport_StructuredSummaryVerdictMapping(t *testing.T) {
	cases := map[string]struct{ text, class string }{
		"approve":             {"Verdict: approve.", "approve"},
		"approve_suggestions": {"Verdict: approve with suggestions.", "approve"},
		"request_changes":     {"Verdict: request changes.", "changes"},
		"shrug":               {"Verdict: unavailable", "neutral"},
	}
	for verdict, want := range cases {
		in := stateFixtureInput()
		in.Comments[5].Summary = &types.SummaryBlock{Verdict: verdict}
		report := renderLayoutFixture(t, in)
		assert.Contains(t, report, `<div class="verdict-text">`+want.text+`</div>`, verdict)
		assert.Contains(t, report, `data-verdict="`+want.class+`"`, verdict)
	}
}

func TestGenerateReport_StructuredSummaryWithoutPrioritiesHasNoActionsOrSuggestions(t *testing.T) {
	in := stateFixtureInput()
	in.Comments[5].Summary = &types.SummaryBlock{Verdict: "approve", Notes: "Nothing to add."}
	report := renderLayoutFixture(t, in)
	assert.NotContains(t, report, `class="fix-first"`)
	assert.NotContains(t, report, "<h2>Suggestions</h2>")
	assert.NotContains(t, report, `class="verdict-upshot"`)
	assert.Contains(t, report, "Nothing to add.")
}

func TestMergedRecordLinksByLocationWhenTheSurvivorHasNoID(t *testing.T) {
	in := layoutFixtureInput()
	in.Comments = []types.LineComment{
		{FilePath: "a.go", LineNumber: 13, Importance: "CRITICAL", Provenance: "agent", CommentBody: "survivor without an id", State: "confirmed"},
		{FilePath: "a.go", LineNumber: 14, Importance: "CRITICAL", Provenance: "first-pass", CommentBody: "folded claim", State: "merged", Inactive: true, MergedInto: "a.go:13", MergeBasis: "proximity"},
		{FilePath: "SUMMARY", CommentBody: "Verdict: approve."},
	}
	report := renderLayoutFixture(t, in)
	if !strings.Contains(report, `<a href="#finding-1">a.go:13</a>`) {
		t.Errorf("a merged record must link to its survivor by location when no id is available:\n%s", report[strings.Index(report, "Merged claims"):][:600])
	}
}
