package html

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/pkg/reviewer/types"
)

func TestVerifyMarkForLabelsEachAppliedOutcome(t *testing.T) {
	cases := []struct {
		name  string
		v     *types.Verification
		pill  string
		class string
	}{
		{"no verification", nil, verifyNone, "none"},
		{"recorded but not applied", &types.Verification{Status: types.VerifyDrop}, verifyNone, "none"},
		{"skipped", &types.Verification{Status: types.VerifySkipped, Applied: true}, verifyNone, "none"},
		{"kept", &types.Verification{Status: types.VerifyKeep, Applied: true}, verifyVerified, "verified"},
		{"revised", &types.Verification{Status: types.VerifyRevise, Applied: true}, verifyRevised, "verified"},
		{"held", &types.Verification{Status: types.VerifyHold, Applied: true}, verifyHeld, "disputed"},
		{"dropped", &types.Verification{Status: types.VerifyDrop, Applied: true}, verifyRefuted, "refuted"},
		{"unavailable", &types.Verification{Status: types.VerifyUnavailable, Applied: true}, verifyUnavailable, "unavailable"},
		{"kept but graded bad", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true}, verifyGradedBad, "refuted"},
		{"dropped and graded bad", &types.Verification{Status: types.VerifyDrop, Applied: true, Gate: "BAD", GateApplied: true}, verifyRefuted, "refuted"},
		{"low value", &types.Verification{Status: types.VerifyKeep, Applied: true, LowValue: true}, verifyLowValue, "disputed"},
		{"critical graded bad and demoted", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true, GateDemoted: true}, verifyDemoted, "disputed"},
		{"gate only, good", &types.Verification{Status: types.VerifySkipped, Gate: "GOOD", GateApplied: true}, verifyGradedGood, "verified"},
		{"gate only, ok", &types.Verification{Status: types.VerifySkipped, Gate: "OK", GateApplied: true}, verifyGradedOK, "unavailable"},
		{"kept and graded good", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "GOOD", GateApplied: true}, verifyVerified, "verified"},
		{"bad grade recorded in shadow", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD"}, verifyVerified, "verified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := verifyMarkFor(tc.v)
			assert.Equal(t, tc.pill, m.VerifyPill)
			assert.Equal(t, tc.class, m.VerifyClass)
			assert.NotEmpty(t, m.VerifyNote)
		})
	}
}

func TestVerifyMarkForCarriesAppliedReasonsOnly(t *testing.T) {
	m := verifyMarkFor(&types.Verification{
		Status: types.VerifyHold, Applied: true, Reason: "The repro never reaches line 230.",
		Gate: "OK", GateApplied: true, GateReason: "Accurate but overstated.",
	})
	assert.Equal(t, []string{"Verifier: The repro never reaches line 230."}, m.VerifyReasons,
		"an OK grade changes nothing on the page, so its free text is not shown")

	bad := verifyMarkFor(&types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true, GateReason: "Restates the PR."})
	assert.Equal(t, []string{"Quality gate (BAD): Restates the PR."}, bad.VerifyReasons)

	revised := verifyMarkFor(&types.Verification{Status: types.VerifyRevise, Applied: true, Reason: "The claim that this contradicts the PR is wrong."})
	assert.Equal(t, []string{"Verifier, on the original wording: The claim that this contradicts the PR is wrong."}, revised.VerifyReasons)

	shadow := verifyMarkFor(&types.Verification{Status: types.VerifyDrop, Reason: "recorded only", Gate: "BAD", GateReason: "recorded only"})
	assert.Empty(t, shadow.VerifyReasons)
}

func TestGenerateReport_LabelsFindingsByVerification(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{
			{FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", CommentBody: "Handle drops the error.",
				Verification: &types.Verification{Status: types.VerifyDrop, Applied: true, Reason: "Handle returns nothing."}},
			{FilePath: "a.go", LineNumber: 3, Importance: "LOW", CommentBody: "Other is unused.",
				Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}},
			{FilePath: "b.go", LineNumber: 9, Importance: "LOW", CommentBody: "Nobody checked this one."},
		},
	})
	require.NoError(t, err)
	markup := report[:strings.Index(report, "<script>")]

	assert.Equal(t, 3, strings.Count(markup, ">REFUTED</span>"), "index row, detailed comment and its reason note")
	assert.Equal(t, 2, strings.Count(markup, ">VERIFIED</span>"))
	assert.Equal(t, 2, strings.Count(markup, ">NOT VERIFIED</span>"))
	assert.Contains(t, markup, "Verifier: Handle returns nothing.")
	for _, count := range []string{">1 verified</span>", ">1 refuted</span>", ">1 not verified</span>"} {
		assert.Contains(t, markup, count)
	}
}

func TestGenerateReport_MovesHeldFindingsOutOfTheDiffWhenTheRunPosts(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	refuted := types.LineComment{ID: "F1", FilePath: "a.go", LineNumber: 2, Importance: "CRITICAL", State: stateConfirmed,
		CommentBody: "Handle leaks the session token.", Verification: &types.Verification{Status: types.VerifyDrop, Applied: true}}
	kept := types.LineComment{ID: "F2", FilePath: "a.go", LineNumber: 3, Importance: "LOW", State: stateConfirmed,
		CommentBody: "Other is unused. Unlike F1, this is cosmetic.", Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}}
	summary := types.LineComment{FilePath: "SUMMARY", CommentBody: "s", Summary: &types.SummaryBlock{Verdict: "request_changes",
		Upshot: "F1 is a CRITICAL token leak.", PriorityIDs: []string{"F1", "F2"}}}
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{summary, refuted, kept},
		Publish:  PublishView{Held: heldWhenSuppressed, Confidence: 4},
	})
	require.NoError(t, err)

	page, held := report[:strings.Index(report, `id="held-back"`)], report[strings.Index(report, `id="held-back"`):]
	for _, leak := range []string{"leaks the session token", "REFUTED", "F1", "token leak", "sev-critical\"", "request changes"} {
		assert.NotContains(t, page, leak)
	}
	assert.Contains(t, held, "Handle leaks the session token.")
	assert.Contains(t, held, "PRism held it back because the verifier refuted it.")
	assert.Contains(t, report, "Unlike another finding, this is cosmetic.")
	assert.Contains(t, report, `<div class="verdict-upshot">Posted 1 comment. 1 finding held back, listed at the end of the page.</div>`)
	assert.Contains(t, report, "Other is unused.")
	assert.Contains(t, report, ">1 confirmed</span>")
	assert.Contains(t, report, ">1 held back</a>")
	assert.Contains(t, report, `<div class="verdict-text">Merge confidence 4/5</div>`)
	assert.NotContains(t, report, `class="fix-first"`)
}

func TestGenerateReport_ShowsNoVerificationPillsWhenNothingWasVerified(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{
			{FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", CommentBody: "Handle drops the error."},
			{FilePath: "b.go", LineNumber: 9, Importance: "LOW", CommentBody: "Other is unused.", Verification: &types.Verification{Status: types.VerifySkipped, Applied: true}},
		},
	})
	require.NoError(t, err)
	assert.NotContains(t, report, "NOT VERIFIED")
	assert.NotContains(t, report, "not verified</span>")
}

func TestGenerateReport_RendersAFindingOnADeletedLineNumber(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,6 +1,2 @@\n package a\n-func One() {}\n-func Two() {}\n-func Three() {}\n-func Four() {}\n+func Five() {}\n"
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{{FilePath: "a.go", LineNumber: 4, Importance: "MEDIUM", CommentBody: "Removing Three breaks the caller in b.go."}},
	})
	require.NoError(t, err)
	assert.Contains(t, report, `id="finding-1"`)
	assert.Contains(t, report, "Removing Three breaks the caller in b.go.")
}

func TestGenerateReport_LeadsWithTheRewriteGitHubShows(t *testing.T) {
	cond, expected := "Call Save with a full disk.", "Handle returns nil."
	contract := &types.FindingContract{SchemaVersion: 1, FindingKind: "production_behavior", Materiality: "current_impact",
		CurrentImpact: "Failed writes report success today.", Falsifiability: "falsifiable",
		FalsifiableCondition: &cond, ExpectedObservable: &expected, Uncertainty: "Only checked the disk path.",
		SeverityRationale: "Data loss.", Subjects: []types.FindingSubject{{Kind: "symbol", Path: "a.go", Name: "Handle"}}}
	report, err := GenerateReportFrom(ReportInput{
		PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{{FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", CommentBody: "The original long-winded claim.",
			FindingContract: contract, Display: &types.Display{Title: "Save errors are swallowed", Body: "A failed Save returns nil to the caller."}}},
	})
	require.NoError(t, err)
	title := strings.Index(report, `<div class="finding-display-title">Save errors are swallowed</div>`)
	original := strings.Index(report, "The original long-winded claim.")
	require.Positive(t, title)
	assert.Greater(t, original, title)
	assert.Contains(t, report, "A failed Save returns nil to the caller.")
	assert.Contains(t, report, "Failed writes report success today.")
	assert.Contains(t, report, "Call Save with a full disk. Expected: Handle returns nil.")
}

func TestGenerateReport_EmbedsNoChatClientOrDuplicateDiff(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		Diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+var distinctiveDiffLine = 1\n", PRNumber: 7, PRTitle: "t",
		CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
	})
	require.NoError(t, err)
	assert.NotContains(t, report, "generativelanguage.googleapis.com")
	assert.NotContains(t, report, "API_KEY")
	assert.Equal(t, 1, strings.Count(report, "distinctiveDiffLine"))
	assert.NotContains(t, report, "chat-sidebar")
	assert.NotContains(t, report, "prompt-handle")
	assert.NotContains(t, report, "prompt-body")
	assert.NotContains(t, report, "Prompt handle")
}

func TestGenerateReport_HighlightsOnlyWhenTheLibraryLoadedAndAfterNavigationIsWired(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0)})
	require.NoError(t, err)
	guard := strings.Index(report, "if (window.hljs) {")
	require.Positive(t, guard)
	assert.Equal(t, 1, strings.Count(report, "hljs.highlightElement"))
	assert.Greater(t, strings.Index(report, "window.hljs.highlightElement"), guard)
	assert.Greater(t, guard, strings.Index(report, "document.addEventListener('keydown'"))
	assert.Greater(t, guard, strings.Index(report, "comment.id = `comment-${number}`"))
}

func TestGenerateReport_ShowsTheCostOfEachStage(t *testing.T) {
	report, err := GenerateReportFrom(ReportInput{
		PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Stages: []StageRecord{{Name: "Review runs", Model: "flash", Detail: "5 valid", CostUSD: 0.30, DurationMS: 90_000}, {Name: "Quality gate", CostUSD: 0.58}},
	})
	require.NoError(t, err)
	assert.Contains(t, report, "<td>Review runs</td><td>flash</td><td>5 valid</td><td>1m30s</td><td>$0.30</td>")
	assert.Contains(t, report, "<td>$0.88</td>")
	assert.NotContains(t, report, "Candidate tokens")
}

func TestGenerateReport_GroupsTheIndexByHowFindingsPost(t *testing.T) {
	finding := func(id, sev, body string) types.LineComment {
		return types.LineComment{ID: id, FilePath: "a.go", LineNumber: 0, Importance: sev, State: stateConfirmed, CommentBody: body}
	}
	report, err := GenerateReportFrom(ReportInput{
		PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{
			finding("F1", "LOW", "A low note."),
			finding("F2", "MEDIUM", "A medium inline."),
			finding("F3", "CRITICAL", "A critical inline."),
			{FilePath: "b.go", Importance: "MEDIUM", State: stateUnverified, CommentBody: "An unverified claim."},
		},
		Publish: PublishView{Inline: func(c types.LineComment) bool { return c.Importance != "LOW" }},
	})
	require.NoError(t, err)
	index := report[strings.Index(report, `<div class="findings-index">`):]
	inline, notes, unverified := strings.Index(index, "<h3>Inline comments on the PR</h3>"), strings.Index(index, "<h3>Summary notes</h3>"), strings.Index(index, "<h3>Needs verification</h3>")
	require.True(t, inline >= 0 && notes > inline && unverified > notes, "inline, then notes, then unverified")
	critical, medium, low := strings.Index(index, "A critical inline."), strings.Index(index, "A medium inline."), strings.Index(index, "A low note.")
	assert.True(t, inline < medium && medium < critical && critical < notes && notes < low, "page order inside each group")
	assert.NotContains(t, index, "<h3>Confirmed</h3>")
}

func TestGenerateReport_DoesNotCountUnsettledFindingsAsConfirmed(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{
			{FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", State: stateConfirmed, CommentBody: "Handle drops the error.",
				Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}},
			{FilePath: "a.go", LineNumber: 3, Importance: "LOW", State: stateConfirmed, CommentBody: "Other is unused.",
				Verification: &types.Verification{Status: types.VerifyUnavailable, Applied: true}},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, report, ">1 confirmed</span>")
	assert.Contains(t, report, ">1 could not verify</span>")
	assert.Equal(t, 2, strings.Count(report, ">1 could not verify</span>"), "the header pill and the verification row")
}

func TestVerifyMarkFor_SaysWhenTheGateLoweredTheSeverity(t *testing.T) {
	lowered := verifyMarkFor(&types.Verification{Status: types.VerifySkipped, Gate: "OK", GateApplied: true, GateLoweredFrom: "medium", GateReason: "Real but minor."})
	assert.Equal(t, verifyGradedOK, lowered.VerifyPill)
	assert.Equal(t, []string{
		"Lowered from MEDIUM by the quality gate: the finding holds, the severity was overstated.",
	}, lowered.VerifyReasons)

	shadow := verifyMarkFor(&types.Verification{Status: types.VerifySkipped, Gate: "OK", GateLoweredFrom: "MEDIUM"})
	assert.Empty(t, shadow.VerifyReasons, "a grade that was only recorded lowered nothing")
	assert.Empty(t, verifyMarkFor(&types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "GOOD", GateApplied: true}).VerifyReasons)
}

func heldWhenSuppressed(c types.LineComment) string {
	if c.Verification.Suppresses() {
		return "the verifier refuted it"
	}
	return ""
}

func TestGenerateReport_KeepsHeldFindingsInTheDiffWhenNothingPosts(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	lowValue := types.LineComment{ID: "F1", FilePath: "a.go", LineNumber: 2, Importance: "LOW", State: stateConfirmed,
		CommentBody:  "Handle ignores the 6.x claim attribute.",
		Verification: &types.Verification{Status: types.VerifyKeep, Applied: true, LowValue: true, ValueReason: "Only matters after the upgrade."}}
	summary := types.LineComment{FilePath: "SUMMARY", CommentBody: "s", Summary: &types.SummaryBlock{Verdict: "approve",
		Upshot: "Correct for 5.2.0; the 6.x detection is unproven.", PriorityIDs: []string{"F1"}}}
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{summary, lowValue},
		Publish: PublishView{Held: func(c types.LineComment) string {
			if c.ID == "F1" {
				return "the verifier held it back"
			}
			return ""
		}, Confidence: 5, NotPublished: "PRism does not post on this author's pull requests"},
	})
	require.NoError(t, err)

	assert.Contains(t, report, "Not posted to the PR: PRism does not post on this author&#39;s pull requests. Findings marked HELD BACK would not have posted either; each says why.")
	assert.Contains(t, report, `<div class="verdict-upshot">Correct for 5.2.0; the 6.x detection is unproven.</div>`)
	assert.Contains(t, report, ">0 confirmed</span>")
	assert.Contains(t, report, ">1 held back</a>")
	assert.Contains(t, report, `<h3 id="held-back">Held back</h3>`)
	assert.Contains(t, report, `<div class="comment-counter" id="finding-1">`)
	assert.Contains(t, report, `<span class="status-pill status-held">HELD BACK</span>`)
	assert.Contains(t, report, "PRism held this back because the verifier held it back.")
	assert.Contains(t, report, "Not worth posting: Only matters after the upgrade.")
	assert.Contains(t, report, "Handle ignores the 6.x claim attribute.")
	assert.NotContains(t, report, `<details class="review-details held-back"`)
}

func TestGenerateReport_LeavesHeldFindingsOutOfFixFirst(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,4 @@\n package a\n+func Handle() {}\n+func Other() {}\n+func Third() {}\n"
	refuted := types.LineComment{ID: "F1", FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", State: stateConfirmed,
		CommentBody: "Handle leaks the session token.", Verification: &types.Verification{Status: types.VerifyDrop, Applied: true}}
	kept := types.LineComment{ID: "F2", FilePath: "a.go", LineNumber: 3, Importance: "LOW", State: stateConfirmed,
		CommentBody: "Other drops the error.", Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}}
	third := types.LineComment{ID: "F3", FilePath: "a.go", LineNumber: 4, Importance: "LOW", State: stateConfirmed,
		CommentBody: "Third is unused.", Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}}
	summary := types.LineComment{FilePath: "SUMMARY", CommentBody: "s", Summary: &types.SummaryBlock{Verdict: "approve",
		Upshot: "Fine to merge.", PriorityIDs: []string{"F1", "F2"}}}
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{summary, refuted, kept, third},
		Publish:  PublishView{Held: heldWhenSuppressed, Confidence: 5, NotPublished: "the pull request is a draft"},
	})
	require.NoError(t, err)

	fixFirst := report[strings.Index(report, `class="fix-first"`):]
	fixFirst = fixFirst[:strings.Index(fixFirst, "</ol>")]
	assert.Contains(t, fixFirst, "Other drops the error")
	assert.NotContains(t, fixFirst, "session token")
}

func TestGenerateReport_KeepsHeldFindingsInTheDiffWhenTheRunPostsNothingElse(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	refuted := types.LineComment{ID: "F1", FilePath: "a.go", LineNumber: 2, Importance: "MEDIUM", State: stateConfirmed,
		CommentBody: "Handle leaks the session token.", Verification: &types.Verification{Status: types.VerifyDrop, Applied: true}}
	summary := types.LineComment{FilePath: "SUMMARY", CommentBody: "s", Summary: &types.SummaryBlock{Verdict: "request_changes",
		Upshot: "F1 blocks the merge.", PriorityIDs: []string{"F1"}}}
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{summary, refuted},
		Publish:  PublishView{Held: heldWhenSuppressed, Inline: func(types.LineComment) bool { return false }, Confidence: 5},
	})
	require.NoError(t, err)

	assert.Contains(t, report, `<div class="verdict-upshot">No comments posted on abcdef1. 1 finding held back, marked HELD BACK in the diff; each says why.</div>`)
	assert.Contains(t, report, `<span class="status-pill status-held">HELD BACK</span>`)
	assert.Contains(t, report, "PRism held this back because the verifier refuted it.")
	assert.NotContains(t, report, "F1 blocks the merge.")
	assert.NotContains(t, report, `class="fix-first"`)
	assert.NotContains(t, report, `<details class="review-details held-back"`)
}
