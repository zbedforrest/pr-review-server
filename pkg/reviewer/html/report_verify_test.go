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
	assert.Equal(t, []string{"Verifier: The repro never reaches line 230.", "Quality gate (OK): Accurate but overstated."}, m.VerifyReasons)

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
	markup := report[:strings.Index(report, "const GEMINI_API_KEY")]

	assert.Equal(t, 3, strings.Count(markup, ">REFUTED</span>"), "index row, detailed comment and its reason note")
	assert.Equal(t, 2, strings.Count(markup, ">VERIFIED</span>"))
	assert.Equal(t, 2, strings.Count(markup, ">NOT VERIFIED</span>"))
	assert.Contains(t, markup, "Verifier: Handle returns nothing.")
	for _, count := range []string{">1 verified</span>", ">1 refuted</span>", ">1 not verified</span>"} {
		assert.Contains(t, markup, count)
	}
}

func TestGenerateReport_LeavesWithheldFindingsOffThePage(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n package a\n+func Handle() {}\n+func Other() {}\n"
	refuted := types.LineComment{ID: "F1", FilePath: "a.go", LineNumber: 2, Importance: "CRITICAL", State: stateConfirmed,
		CommentBody: "Handle leaks the session token.", Verification: &types.Verification{Status: types.VerifyDrop, Applied: true}}
	kept := types.LineComment{ID: "F2", FilePath: "a.go", LineNumber: 3, Importance: "LOW", State: stateConfirmed,
		CommentBody: "Other is unused.", Verification: &types.Verification{Status: types.VerifyKeep, Applied: true}}
	summary := types.LineComment{FilePath: "SUMMARY", CommentBody: "s", Summary: &types.SummaryBlock{Verdict: "approve", PriorityIDs: []string{"F1", "F2"}}}
	report, err := GenerateReportFrom(ReportInput{
		Diff: diff, PRNumber: 7, PRTitle: "t", CommitSHA: "abcdef1", GeneratedAt: time.Unix(0, 0),
		Comments: []types.LineComment{summary, refuted, kept},
		Withhold: func(c types.LineComment) bool { return c.Verification.Suppresses() },
	})
	require.NoError(t, err)

	assert.NotContains(t, report, "leaks the session token")
	assert.NotContains(t, report, "REFUTED")
	assert.Contains(t, report, "Other is unused.")
	assert.Contains(t, report, ">1 confirmed</span>")
	assert.Contains(t, report, ">1 withheld</span>")
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
