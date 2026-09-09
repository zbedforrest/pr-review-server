package html

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pr-review-server/pkg/reviewer/types"
)

// mergeNote mirrors the legacy service/merge.go provenanceNote so fixtures
// track the production phrasing the renderer classifies on.
func mergeNote(provenance string) string {
	return "_[" + provenance + " finding — retained by reconciliation, not independently confirmed by the review agent]_\n\n"
}

// alertFixtureComments is a merged finding set with one agent finding, one
// mechanical gate alert, and one required-check VIOLATED synthesis.
func alertFixtureComments() []types.LineComment {
	return []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  13,
			CommentBody: "Add error handling here",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "web/components/OverlayHost.tsx",
			LineNumber:  0,
			Importance:  "MEDIUM",
			CommentBody: mergeNote("mechanical") + "**Mechanical alert — portal overlay without an explicit layer.** This change renders portal-based UI without a `layer` prop.",
		},
		{
			FilePath:    "app/config/handlers.py",
			LineNumber:  0,
			Importance:  "MEDIUM",
			CommentBody: mergeNote("required-check") + "**Required check CHK-settings-ref-1 answered VIOLATED without an accompanying finding — escalated automatically.** CHK-settings-ref-1 | VIOLATED | EVIDENCE: handlers.py:42",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "Verdict: request changes.\n\nOverall summary.",
		},
	}
}

func alertFixtureChecks() []CheckRecord {
	return []CheckRecord{
		{ID: "CHK-portal-layer-1", Source: "gate", Verdict: "SAFE", EvidencePath: "web/components/OverlayHost.tsx", EvidenceResolved: true},
		{ID: "CHK-settings-ref-1", Source: "gate", Verdict: "VIOLATED", EvidencePath: "app/config/handlers.py", EvidenceResolved: true},
	}
}

const alertFixtureDiff = `diff --git a/app.go b/app.go
index 123..456 100644
--- a/app.go
+++ b/app.go
@@ -12,6 +12,7 @@ func main() {
 	fmt.Println("Hello")
+	// New line
 }`

func renderAlertFixture(t *testing.T) string {
	t.Helper()
	report, err := GenerateReportFrom(ReportInput{
		Comments: alertFixtureComments(), Diff: alertFixtureDiff, PRNumber: 42,
		PRURL: "https://github.com/acme/example/pull/42", PRBody: "Test PR body", CommitSHA: "abc1234",
		ModelName: "model-x", GeneratedAt: time.Date(2026, 1, 15, 14, 30, 0, 0, time.UTC),
		Checks: alertFixtureChecks(),
	})
	assert.NoError(t, err)
	return report
}

func TestGenerateReport_DeterministicAlerts_DefaultInsideReviewDetails(t *testing.T) {
	t.Setenv("SURFACE_ALERTS", "")

	report := renderAlertFixture(t)

	assert.NotContains(t, report, `class="deterministic-alerts pinned"`)
	detailsIdx := strings.Index(report, "<summary>Review details</summary>")
	assert.Greater(t, detailsIdx, -1)
	assert.NotContains(t, report[:detailsIdx], `class="deterministic-alert"`)
	assert.Equal(t, 2, strings.Count(report[detailsIdx:], `class="deterministic-alert"`))
	assert.Contains(t, report, "Add error handling here")
}

func TestGenerateReport_DeterministicAlerts_SurfaceAlertsAlsoPinsAtTop(t *testing.T) {
	t.Setenv("SURFACE_ALERTS", "true")

	report := renderAlertFixture(t)

	pinnedIdx := strings.Index(report, `class="deterministic-alerts pinned"`)
	descIdx := strings.Index(report, "<h2>PR Description</h2>")
	assert.Greater(t, pinnedIdx, -1)
	assert.Greater(t, descIdx, pinnedIdx)
	assert.Equal(t, 4, strings.Count(report, `class="deterministic-alert"`), "two rows pinned plus two in review details")

	assert.Contains(t, report, "CHK-portal-layer-1")
	assert.Contains(t, report, "alert-verdict-safe")
	assert.Contains(t, report, "CHK-settings-ref-1")
	assert.Contains(t, report, "alert-verdict-violated")

	assert.Contains(t, report, `class="alert-badge alert-provenance"`)
	assert.NotContains(t, report[:descIdx], "Add error handling here")
}

func TestBuildDeterministicAlerts_VerdictResolution(t *testing.T) {
	alerts := buildDeterministicAlerts(alertFixtureComments(), alertFixtureChecks())
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(alerts), alerts)
	}

	gate := alerts[0]
	assert.Equal(t, "mechanical", gate.Provenance)
	assert.Equal(t, "web/components/OverlayHost.tsx", gate.FilePath)
	assert.Equal(t, "CHK-portal-layer-1", gate.CheckID)
	assert.Equal(t, "SAFE", gate.Verdict)
	assert.Equal(t, "reference resolved", gate.Evidence)
	assert.NotContains(t, gate.Body, "retained by reconciliation")

	synth := alerts[1]
	assert.Equal(t, "required-check", synth.Provenance)
	assert.Equal(t, "CHK-settings-ref-1", synth.CheckID)
	assert.Equal(t, "VIOLATED", synth.Verdict)
	assert.Equal(t, "reference resolved", synth.Evidence)
}

func TestBuildDeterministicAlerts_UnansweredNoteAndPositionalNumbering(t *testing.T) {
	comments := []types.LineComment{
		{
			FilePath:   "web/components/PanelA.tsx",
			LineNumber: 0,
			Importance: "MEDIUM",
			Provenance: "mechanical",
			CommentBody: "**Mechanical alert: portal overlay without an explicit layer.** Body A." +
				"\n\n_Required check CHK-portal-layer-1 was not answered with evidence, treat as unresolved risk, not a cleared concern._",
		},
		{
			FilePath:    "web/components/PanelB.tsx",
			LineNumber:  0,
			Importance:  "MEDIUM",
			Provenance:  "mechanical",
			CommentBody: "**Mechanical alert: portal overlay without an explicit layer.** Body B.",
		},
		{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: "Verdict: approve.",
		},
	}
	checks := []CheckRecord{
		{ID: "CHK-portal-layer-1", Source: "gate", Verdict: "UNANSWERED", Unresolved: true},
		{ID: "CHK-portal-layer-2", Source: "gate", Verdict: "SAFE", EvidencePath: "web/components/PanelB.tsx", EvidenceResolved: true},
	}

	alerts := buildDeterministicAlerts(comments, checks)
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(alerts), alerts)
	}
	assert.Equal(t, "CHK-portal-layer-1", alerts[0].CheckID)
	assert.Equal(t, "UNANSWERED", alerts[0].Verdict)
	assert.Equal(t, "", alerts[0].Evidence)
	assert.Equal(t, "CHK-portal-layer-2", alerts[1].CheckID)
	assert.Equal(t, "SAFE", alerts[1].Verdict)
	assert.Equal(t, "reference resolved", alerts[1].Evidence)
}

func TestBuildDeterministicAlerts_LegacySummaryLedgerIsIgnored(t *testing.T) {
	comments := []types.LineComment{
		{
			FilePath:    "web/components/PanelA.tsx",
			LineNumber:  0,
			Importance:  "MEDIUM",
			Provenance:  "mechanical",
			CommentBody: "**Mechanical alert: portal overlay without an explicit layer.** Body A.",
		},
		{
			FilePath:   "SUMMARY",
			LineNumber: 0,
			CommentBody: "Overall.\n\n---\n_Required checks (id | verdict | evidence)_\n" +
				"- CHK-portal-layer-1 | SAFE | evidence-ok\n",
		},
	}
	alerts := buildDeterministicAlerts(comments, nil)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	assert.Equal(t, "", alerts[0].CheckID)
	assert.Equal(t, "", alerts[0].Verdict)
}

func TestBuildDeterministicAlerts_NoChecksNoVerdict(t *testing.T) {
	comments := []types.LineComment{
		{
			FilePath:    "app/models.py",
			LineNumber:  0,
			Importance:  "MEDIUM",
			Provenance:  "mechanical",
			CommentBody: "**Mechanical alert: new model property.** Body.",
		},
	}
	alerts := buildDeterministicAlerts(comments, nil)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	assert.Equal(t, "", alerts[0].CheckID)
	assert.Equal(t, "", alerts[0].Verdict)
}

func TestBuildDeterministicAlerts_UnresolvedClearanceStaysMarked(t *testing.T) {
	comments := []types.LineComment{{
		FilePath: "app/Tooltip.tsx", Provenance: "mechanical",
		CommentBody: "**Mechanical alert: portal overlay without an explicit layer.** body\n\n_Required check CHK-portal-layer-1 was not answered with evidence, treat as unresolved risk._",
	}}
	checks := []CheckRecord{{ID: "CHK-portal-layer-1", Source: "gate", Verdict: "SAFE", Unresolved: true}}
	alerts := buildDeterministicAlerts(comments, checks)
	if len(alerts) != 1 || alerts[0].Verdict != "SAFE" || alerts[0].Evidence != "unresolved" {
		t.Fatalf("a SAFE answer without resolvable evidence must show as unresolved: %+v", alerts)
	}
}

func TestAlertView_UnresolvedClearanceIsNotStyledSafe(t *testing.T) {
	a := AlertView{Verdict: "SAFE", Evidence: "unresolved"}
	if a.VerdictClass() != "unresolved" {
		t.Errorf("class = %q, want unresolved", a.VerdictClass())
	}
	if b := (AlertView{Verdict: "SAFE", Evidence: evidenceResolvedLabel}); b.VerdictClass() != "safe" {
		t.Errorf("resolved SAFE class = %q", b.VerdictClass())
	}
}
