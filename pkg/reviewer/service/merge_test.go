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
	if readmitted.Provenance == "" || readmitted.Provenance == "agent" ||
		readmitted.CommentBody != "route classification breaks" {
		t.Errorf("re-admitted finding must carry its set's provenance with an untouched body: %+v", readmitted)
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

// Re-admitted findings are attributed structurally: the SUMMARY prose stays
// exactly as the agent wrote it, and the finding carries its provenance in
// the field the payload and renderers read, with no prose preface.
func TestMergeFindings_ReadmissionIsStructuredNotProse(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		lc("a.ts", 5, "LOW", "nit"),
	}}
	firstPass := FindingSet{Provenance: "first-pass", Comments: []types.LineComment{
		lc("b.ts", 40, "CRITICAL", "crash"),
	}}
	got := MergeFindings(agent, firstPass)
	if got[0].CommentBody != "Verdict: approve" {
		t.Errorf("SUMMARY must not be rewritten by the merge: %q", got[0].CommentBody)
	}
	if got[1].Provenance != "agent" {
		t.Errorf("agent finding provenance = %q", got[1].Provenance)
	}
	if got[2].Provenance != "first-pass" || got[2].CommentBody != "crash" || got[2].Importance != "MEDIUM" {
		t.Errorf("re-admitted finding = %+v, want provenance first-pass, untouched body, MEDIUM cap", got[2])
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
	if readmitted.CommentBody != "crash on empty payload" {
		t.Errorf("carried body must stay untouched: %q", readmitted.CommentBody)
	}
	if readmitted.Provenance != CarriedProvenance("0123456789abcdef0123") {
		t.Errorf("carried provenance = %q, want the source-sha label", readmitted.Provenance)
	}
	if sha, ok := CarriedFromSHA(readmitted.Provenance); !ok || sha != "0123456" {
		t.Errorf("CarriedFromSHA = (%q, %t), want (%q, true)", sha, ok, "0123456")
	}
	if got[0].CommentBody != "Verdict: approve" {
		t.Errorf("SUMMARY must not be rewritten by the merge: %q", got[0].CommentBody)
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

func TestMergeFindings_SetLabelOverridesTheCommentsOwnProvenance(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{lc("SUMMARY", 0, "LOW", "Verdict: approve")}}
	carriedIn := lc("b.ts", 40, "MEDIUM", "stale")
	carriedIn.Provenance = "carried"
	carried := FindingSet{Provenance: CarriedProvenance("0123456789abcdef0123"), Comments: []types.LineComment{carriedIn}}
	got := MergeFindings(agent, carried)
	if sha, ok := CarriedFromSHA(got[1].Provenance); !ok || sha != "0123456" {
		t.Fatalf("the set label names the source review and must win over the bare stamp: %q", got[1].Provenance)
	}
}

func TestMergeFindings_BlankLowerPriorityLabelDefaultsToFirstPass(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{lc("SUMMARY", 0, "LOW", "Verdict: approve")}}
	unlabeled := FindingSet{Comments: []types.LineComment{lc("b.ts", 40, "CRITICAL", "crash")}}
	got := MergeFindings(agent, unlabeled)
	if got[1].Provenance != "first-pass" {
		t.Fatalf("a re-admitted finding must never read as the agent's own: %q", got[1].Provenance)
	}
}

func TestMergeFindingsWithRecords_DuplicateBecomesAMergedRecord(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		{ID: "A-1", FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "agent phrasing"},
	}}
	fp := lc("a.go", 12, "CRITICAL", "first-pass phrasing")
	fp.Original = &types.OriginalClaim{SourceID: "FP-1"}
	firstPass := FindingSet{Provenance: "first-pass", Comments: []types.LineComment{fp}}
	merged, records := MergeFindingsWithRecords(agent, firstPass)
	if len(merged) != 2 || merged[1].Importance != "MEDIUM" {
		t.Fatalf("merged = %+v", merged)
	}
	if len(records) != 1 || records[0].State != StateMerged || !records[0].Inactive || records[0].MergedInto != "A-1" || records[0].CommentBody != "first-pass phrasing" {
		t.Fatalf("the dropped duplicate must survive as a merged record: %+v", records)
	}
}

func TestCarryForwardFindings_SkipsInactiveRecords(t *testing.T) {
	rejected := lc("a.go", 1, "MEDIUM", "rejected last time")
	rejected.State, rejected.Inactive = StateRejected, true
	carried, dropped := CarryForwardFindings([]types.LineComment{rejected, lc("b.go", 2, "LOW", "still valid")}, nil)
	if len(carried) != 1 || carried[0].FilePath != "b.go" || dropped != 0 {
		t.Fatalf("an inactive record must never be carried forward as a claim: carried=%+v dropped=%d", carried, dropped)
	}
	if carried[0].State != StateUnverified {
		t.Errorf("a carried finding is a re-admitted claim and reads as unverified: %+v", carried[0])
	}
}

func TestMergeFindingsWithRecords_DisputedClaimsAreNotFoldedByProximity(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		lc("SUMMARY", 0, "LOW", "Verdict: approve"),
		{ID: "A-1", FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "unrelated nearby finding"},
	}}
	disputed := lc("a.go", 12, "CRITICAL", "token leaks")
	disputed.State = StateUnverified
	disputed.Assessment = &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "redacted by the logger"}
	firstPass := FindingSet{Provenance: "first-pass", Comments: []types.LineComment{disputed}}
	merged, records := MergeFindingsWithRecords(agent, firstPass)
	if len(records) != 0 || len(merged) != 3 || merged[2].Assessment == nil || merged[2].State != StateUnverified {
		t.Fatalf("a claim the agent explicitly rejected cannot be the same defect as its nearby positive finding; it stays active and disputed: merged=%+v records=%+v", merged, records)
	}
}

func TestMergeFindingsWithRecords_ProximityFoldsRecordTheirBasis(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{{ID: "A-1", FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "agent"}}}
	fp := lc("a.go", 12, "CRITICAL", "first-pass")
	fp.State = StateUnverified
	_, records := MergeFindingsWithRecords(agent, FindingSet{Provenance: "first-pass", Comments: []types.LineComment{fp}})
	if len(records) != 1 || records[0].MergeBasis != "proximity" {
		t.Fatalf("a heuristic fold must say so: %+v", records)
	}
}

func TestMergeFindingsWithRecords_IntraAgentDuplicatesLeaveARecordAndRemapReferences(t *testing.T) {
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{
		{FilePath: "SUMMARY", Summary: &types.SummaryBlock{Verdict: "approve", PriorityIDs: []string{"A-2"}}},
		{ID: "A-1", FilePath: "a.go", LineNumber: 10, Importance: "MEDIUM", CommentBody: "first phrasing"},
		{ID: "A-2", FilePath: "a.go", LineNumber: 12, Importance: "CRITICAL", CommentBody: "second phrasing of the same defect"},
	}}
	claim := lc("a.go", 12, "CRITICAL", "first-pass claim")
	claim.State, claim.Inactive, claim.MergedInto, claim.MergeBasis = StateMerged, true, "A-2", "sources"
	merged, records := MergeFindingsWithRecords(agent)
	records = append(records, claim)
	RemapMergeTargets(merged, records)

	if len(merged) != 2 || merged[1].ID != "A-1" || merged[1].Importance != "CRITICAL" {
		t.Fatalf("survivor keeps the higher severity: %+v", merged)
	}
	var dropped types.LineComment
	for _, r := range records {
		if r.CommentBody == "second phrasing of the same defect" {
			dropped = r
		}
	}
	if dropped.State != StateMerged || !dropped.Inactive || dropped.MergedInto != "A-1" || dropped.MergeBasis != "proximity" {
		t.Fatalf("the dropped agent finding must survive as a merged record: %+v", records)
	}
	if merged[0].Summary.PriorityIDs[0] != "A-1" {
		t.Errorf("a priority id naming the dropped finding must follow it to the survivor: %+v", merged[0].Summary.PriorityIDs)
	}
	if records[len(records)-1].MergedInto != "A-1" {
		t.Errorf("a claim merged into the dropped finding must follow it too: %+v", records[len(records)-1])
	}
}
