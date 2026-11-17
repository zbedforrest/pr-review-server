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
		{"held", &types.Verification{Status: types.VerifyHold, Applied: true}, verifyDisputed, "disputed"},
		{"dropped", &types.Verification{Status: types.VerifyDrop, Applied: true}, verifyRefuted, "refuted"},
		{"unavailable", &types.Verification{Status: types.VerifyUnavailable, Applied: true}, verifyUnavailable, "unavailable"},
		{"kept but graded bad", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true}, verifyGradedBad, "refuted"},
		{"dropped and graded bad", &types.Verification{Status: types.VerifyDrop, Applied: true, Gate: "BAD", GateApplied: true}, verifyRefuted, "refuted"},
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
