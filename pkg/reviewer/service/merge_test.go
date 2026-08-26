package service

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func lc(file string, line int, imp, body string) types.LineComment {
	return types.LineComment{FilePath: file, LineNumber: line, Importance: imp, CommentBody: body}
}

// The core regression this exists to prevent: a first-pass bug finding the
// agent dropped must survive the merge, provenance-tagged.
func TestMergeFindings_DroppedFirstPassFindingSurvives(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		lc("a/b.ts", 10, "LOW", "nit"),
	}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("src/routing.ts", 85, "CRITICAL", "route classification breaks"),
	}}
	got := MergeFindings(agent, gemini)
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %d: %+v", len(got), got)
	}
	readmitted := got[2]
	// Re-admissions cap at MEDIUM: unconfirmed first-pass severity must not
	// create blockers (the first pass emits CRITICALs on half of clean PRs).
	if readmitted.Importance != "MEDIUM" {
		t.Errorf("re-admitted severity should cap at MEDIUM, got %q", readmitted.Importance)
	}
	if !strings.Contains(readmitted.CommentBody, "gemini finding") ||
		!strings.Contains(readmitted.CommentBody, "route classification breaks") {
		t.Errorf("provenance tag or body missing: %q", readmitted.CommentBody)
	}
}

func TestMergeFindings_DuplicateKeepsAgentPhrasingUpgradesSeverity(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("x.py", 100, "LOW", "agent phrasing"),
	}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("deep/path/x.py", 105, "CRITICAL", "gemini phrasing"), // same file (suffix), within tolerance
	}}
	got := MergeFindings(agent, gemini)
	if len(got) != 1 {
		t.Fatalf("want dedupe to 1, got %d", len(got))
	}
	if got[0].CommentBody != "agent phrasing" {
		t.Errorf("higher-priority phrasing should win: %q", got[0].CommentBody)
	}
	// Upgrades sourced from a lower-priority set also cap at MEDIUM.
	if got[0].Importance != "MEDIUM" {
		t.Errorf("severity should upgrade but cap at MEDIUM, got %q", got[0].Importance)
	}
}

func TestMergeFindings_SeverityNeverDowngrades(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("x.py", 100, "CRITICAL", "agent"),
	}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("x.py", 100, "LOW", "gemini"),
	}}
	got := MergeFindings(agent, gemini)
	if len(got) != 1 || got[0].Importance != "CRITICAL" {
		t.Fatalf("severity downgraded: %+v", got)
	}
}

func TestMergeFindings_BeyondToleranceIsDistinct(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("x.py", 100, "LOW", "a")}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("x.py", 100+mergeLineTolerance+1, "MEDIUM", "b")}}
	if got := MergeFindings(agent, gemini); len(got) != 2 {
		t.Fatalf("want 2 distinct findings, got %d", len(got))
	}
}

func TestMergeFindings_OnlyPrimarySummaryKept(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "MEDIUM", "agent verdict")}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "gemini verdict")}}
	got := MergeFindings(agent, gemini)
	if len(got) != 1 || got[0].CommentBody != "agent verdict" {
		t.Fatalf("want only the primary SUMMARY, got %+v", got)
	}
}

func TestMergeFindings_WholeFileMatchesOnlyWholeFile(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("x.py", 0, "LOW", "whole-file note")}}
	gemini := FindingSet{Provenance: "gemini", Comments: []types.LineComment{
		lc("x.py", 5, "MEDIUM", "line 5 bug")}}
	if got := MergeFindings(agent, gemini); len(got) != 2 {
		t.Fatalf("line-0 should not swallow a line-anchored finding: %+v", got)
	}
}

// Whole-file findings carry no line signal to corroborate a basename match,
// so they dedup only on a strict path match: two directories' models.py are
// distinct findings, not duplicates.
func TestMergeFindings_WholeFileRequiresStrictPathMatch(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("payments/models.py", 0, "LOW", "reviewed, looks fine")}}
	checks := FindingSet{Provenance: "required-check", Comments: []types.LineComment{
		lc("tipping/models.py", 0, "MEDIUM", "unresolved memory check")}}
	got := MergeFindings(agent, checks)
	if len(got) != 2 {
		t.Fatalf("basename-only match must not dedup whole-file findings: %+v", got)
	}

	// Exact (and whole-path-suffix) matches still dedup, and the earlier
	// set's phrasing wins: a gate alert collapses into the required-check
	// synthesis that precedes it, not the other way around.
	synth := FindingSet{Provenance: "required-check", Comments: []types.LineComment{
		lc("app/Tooltip.tsx", 0, "MEDIUM", "escalated VIOLATED answer")}}
	mech := FindingSet{Provenance: "mechanical", Comments: []types.LineComment{
		lc("app/Tooltip.tsx", 0, "MEDIUM", "generic advisory")}}
	got = MergeFindings(synth, mech)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "escalated VIOLATED answer") {
		t.Fatalf("gate alert should collapse into the earlier synthesis: %+v", got)
	}
}

func TestMergeFindings_EmptyAndSingleSet(t *testing.T) {
	if got := MergeFindings(); got != nil {
		t.Errorf("no sets -> nil, got %+v", got)
	}
	one := FindingSet{Provenance: "agent", Comments: []types.LineComment{lc("a.go", 1, "LOW", "x")}}
	got := MergeFindings(one)
	if len(got) != 1 || strings.Contains(got[0].CommentBody, "reconciliation") {
		t.Errorf("single set should pass through untagged: %+v", got)
	}
}

// TestMergeFindings_SummaryReconciliationNote — when findings are re-admitted,
// the primary SUMMARY must disclose the potential contradiction; when nothing
// is re-admitted the SUMMARY stays untouched.
func TestMergeFindings_SummaryReconciliationNote(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		lc("a.ts", 5, "LOW", "nit"),
	}}
	gemini := FindingSet{Provenance: "first-pass", Comments: []types.LineComment{
		lc("b.ts", 40, "CRITICAL", "crash"),
	}}
	got := MergeFindings(agent, gemini)
	if !strings.Contains(got[0].CommentBody, "Reconciliation: 1 earlier-pass finding") {
		t.Errorf("SUMMARY missing reconciliation note: %q", got[0].CommentBody)
	}
	// Re-admissions from non-carried sets must NOT trigger the carried
	// mention — with carry-forward off the merge output is unchanged.
	if strings.Contains(got[0].CommentBody, "carried forward") {
		t.Errorf("SUMMARY should not mention carry-forward without a carried set: %q", got[0].CommentBody)
	}
	// No re-admissions -> untouched SUMMARY.
	got = MergeFindings(agent, FindingSet{Provenance: "first-pass", Comments: nil})
	if strings.Contains(got[0].CommentBody, "Reconciliation") {
		t.Errorf("SUMMARY should be untouched with no re-admissions: %q", got[0].CommentBody)
	}
}

// ---- Cross-review carry-forward -------------------------------------------

// A prior finding on a file the new push did NOT touch survives the staleness
// filter; SUMMARY entries never carry (each review writes its own summary).
func TestCarryForwardFindings_UntouchedFileCarries(t *testing.T) {
	prior := []types.LineComment{
		lc("SUMMARY", 0, "LOW", "old verdict"),
		lc("src/api/handler.py", 42, "CRITICAL", "unhandled nil deref"),
	}
	carried, dropped := CarryForwardFindings(prior, []string{"src/other/widget.py"})
	if dropped != 0 || len(carried) != 1 {
		t.Fatalf("want 1 carried / 0 dropped, got %d / %d: %+v", len(carried), dropped, carried)
	}
	if carried[0].FilePath != "src/api/handler.py" || carried[0].CommentBody != "unhandled nil deref" {
		t.Errorf("carried finding mangled: %+v", carried[0])
	}
	// Re-attributed: the structured field must agree with what the merge's
	// note will say, or payload.DeriveProvenance reports the stale label.
	if carried[0].Provenance != "carried" {
		t.Errorf("carried finding provenance = %q, want %q", carried[0].Provenance, "carried")
	}
}

// A prior finding whose cited file the new push modified is assumed addressed
// and dropped — including when the paths match only by whole-path suffix.
func TestCarryForwardFindings_TouchedFileDrops(t *testing.T) {
	prior := []types.LineComment{
		lc("src/api/handler.py", 42, "CRITICAL", "exact-path match"),
		lc("api/handler.py", 10, "MEDIUM", "suffix-path match"),
	}
	carried, dropped := CarryForwardFindings(prior, []string{"src/api/handler.py"})
	if len(carried) != 0 || dropped != 2 {
		t.Fatalf("want 0 carried / 2 dropped, got %d / %d: %+v", len(carried), dropped, carried)
	}
	// Bare basename must NOT drop: two directories' handler.py are different
	// files (mirrors whole-file dedup's strict-path rule).
	carried, dropped = CarryForwardFindings(
		[]types.LineComment{lc("other/dir/handler.py", 5, "LOW", "distinct file")},
		[]string{"src/api/handler.py"})
	if len(carried) != 1 || dropped != 0 {
		t.Fatalf("basename-only match must not drop: got %d carried / %d dropped", len(carried), dropped)
	}
}

// A finding loaded from a prior sidecar may already start with a
// reconciliation marker (it was re-admitted or carried last time); carrying
// it again must strip the stale marker so notes don't stack.
func TestCarryForwardFindings_StripsStaleMarker(t *testing.T) {
	body := provenanceNote("mechanical") + "shared module edited"
	carried, _ := CarryForwardFindings([]types.LineComment{lc("common/base.py", 0, "MEDIUM", body)}, nil)
	if len(carried) != 1 {
		t.Fatalf("want 1 carried, got %d", len(carried))
	}
	if carried[0].CommentBody != "shared module edited" {
		t.Errorf("stale marker not stripped: %q", carried[0].CommentBody)
	}
	// A body without a marker passes through untouched.
	carried, _ = CarryForwardFindings([]types.LineComment{lc("a.go", 1, "LOW", "plain body")}, nil)
	if carried[0].CommentBody != "plain body" {
		t.Errorf("plain body mangled: %q", carried[0].CommentBody)
	}
}

// A carried finding duplicating one of this run's findings collapses into the
// current phrasing (the carried copy adds nothing the reviewer didn't re-find).
func TestMergeFindings_CarriedDedupsIntoAgentFinding(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("x.py", 100, "LOW", "agent phrasing"),
	}}
	carried := FindingSet{Provenance: CarriedProvenance("abcdef1234567890"), Comments: []types.LineComment{
		lc("x.py", 103, "CRITICAL", "carried phrasing"),
	}}
	got := MergeFindings(agent, carried)
	if len(got) != 1 || got[0].CommentBody != "agent phrasing" {
		t.Fatalf("carried duplicate should collapse into agent phrasing: %+v", got)
	}
	// The duplicate's severity upgrade is capped at MEDIUM like any
	// lower-priority source.
	if got[0].Importance != "MEDIUM" {
		t.Errorf("upgrade from carried duplicate should cap at MEDIUM, got %q", got[0].Importance)
	}
}

func TestCarryForwardFindingsDropsStaleFindingContract(t *testing.T) {
	contract := &types.FindingContract{SchemaVersion: types.FindingContractSchemaVersion}
	prior := lc("x.py", 100, "LOW", "prior phrasing")
	prior.FindingContract = contract

	carried, dropped := CarryForwardFindings([]types.LineComment{prior}, nil)
	if dropped != 0 || len(carried) != 1 {
		t.Fatalf("carried = %+v, dropped = %d", carried, dropped)
	}
	if carried[0].FindingContract != nil {
		t.Fatal("carried finding retained a contract bound to an earlier head")
	}
}

// A unique carried finding survives the merge capped at MEDIUM, with a note
// deterministically naming the SHA it came from, and the SUMMARY
// reconciliation note mentions the carried count.
func TestMergeFindings_CarriedUniqueCappedMediumWithSHANote(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		lc("a.ts", 5, "LOW", "nit"),
	}}
	carried := FindingSet{Provenance: CarriedProvenance("0123456789abcdef0123"), Comments: []types.LineComment{
		lc("b.ts", 40, "CRITICAL", "crash on empty payload"),
	}}
	got := MergeFindings(agent, carried)
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %d: %+v", len(got), got)
	}
	readmitted := got[2]
	if readmitted.Importance != "MEDIUM" {
		t.Errorf("carried finding should cap at MEDIUM, got %q", readmitted.Importance)
	}
	if !strings.Contains(readmitted.CommentBody, "carried from review of 0123456") ||
		!strings.Contains(readmitted.CommentBody, "crash on empty payload") {
		t.Errorf("carried note or body missing: %q", readmitted.CommentBody)
	}
	if sha, ok := CarriedFromSHA(readmitted.CommentBody); !ok || sha != "0123456" {
		t.Errorf("CarriedFromSHA = (%q, %t), want (%q, true)", sha, ok, "0123456")
	}
	if !strings.Contains(got[0].CommentBody, "1 of the retained finding(s) were carried forward") {
		t.Errorf("SUMMARY missing carried mention: %q", got[0].CommentBody)
	}
}

// CarriedFromSHA must not misidentify other provenance notes or plain bodies.
func TestCarriedFromSHA_NonCarriedBodies(t *testing.T) {
	for _, body := range []string{
		"plain finding body",
		provenanceNote("mechanical") + "gate alert",
		provenanceNote("first-pass") + "readmitted",
		"",
	} {
		if sha, ok := CarriedFromSHA(body); ok {
			t.Errorf("CarriedFromSHA(%q) = (%q, true), want ok=false", body, sha)
		}
	}
}
