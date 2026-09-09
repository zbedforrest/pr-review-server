package service

import (
	"os"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestApplyDispositions_ReproducesTodaysActiveSetAndKeepsEverythingElseAsRecords(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{
		lc("a.go", 3, "CRITICAL", "Nil deref when cfg is missing."), // FP-1: confirmed by A-1
		lc("b.go", 9, "CRITICAL", "Leaks the token in logs."),       // FP-2: rejected, critical -> disputed, active
		lc("c.go", 12, "CRITICAL", "Retry loop never backs off."),   // FP-3: unaddressed critical -> unverified, active
		lc("d.go", 20, "MEDIUM", "Missing null check on response."), // FP-4: rejected, non-critical -> rejected record
		lc("e.go", 30, "LOW", "Typo in the log message."),           // FP-5: unaddressed low -> not examined record
	})
	agentOut := []types.LineComment{
		{ID: "A-1", FilePath: "a.go", LineNumber: 3, Importance: "CRITICAL", CommentBody: "cfg is nil on the first request.", Sources: []string{"FP-1"}},
		{FilePath: "b.go", LineNumber: 9, Disposition: &types.Disposition{SourceID: "FP-2", State: "rejected", Reason: "The logger redacts tokens.", Evidence: []types.EvidenceRef{{File: "log/redact.go", Line: 40}}}},
		{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-4", State: "rejected", Reason: "The client never returns nil here.", Evidence: []types.EvidenceRef{{File: "client/http.go", Line: 88}}}},
		{FilePath: "SUMMARY", CommentBody: "Verdict: approve."},
	}

	findings, active, records := ApplyDispositions(agentOut, claims)

	if len(findings) != 2 || findings[0].ID != "A-1" || findings[1].FilePath != "SUMMARY" {
		t.Fatalf("disposition entries must be removed from the findings: %+v", findings)
	}
	if len(active) != 2 {
		t.Fatalf("active first-pass set must be the two criticals (disputed and unaddressed): %+v", active)
	}
	disputed, unaddressed := active[0], active[1]
	if disputed.FilePath != "b.go" || disputed.State != "unverified" || disputed.Assessment == nil || disputed.Assessment.Reason != "The logger redacts tokens." || disputed.Original == nil || disputed.Original.SourceID != "FP-2" {
		t.Errorf("rejected critical stays active as disputed: %+v", disputed)
	}
	if unaddressed.FilePath != "c.go" || unaddressed.State != "unverified" || unaddressed.Assessment != nil {
		t.Errorf("unaddressed critical stays active as unverified: %+v", unaddressed)
	}
	if len(records) != 3 {
		t.Fatalf("records = %+v, want merged FP-1, rejected FP-4, not-examined FP-5", records)
	}
	merged, rejected, unexamined := records[0], records[1], records[2]
	if merged.State != "merged" || !merged.Inactive || merged.MergedInto != "A-1" || merged.Original.SourceID != "FP-1" {
		t.Errorf("confirmed claim becomes a merged record: %+v", merged)
	}
	if rejected.State != "rejected" || !rejected.Inactive || rejected.Assessment == nil || rejected.Importance != "MEDIUM" || rejected.CommentBody != "Missing null check on response." {
		t.Errorf("rejected medium becomes an inactive record with its reason: %+v", rejected)
	}
	if unexamined.State != "unverified" || !unexamined.Inactive || unexamined.Provenance != "first-pass" {
		t.Errorf("unaddressed low becomes an inactive unverified record: %+v", unexamined)
	}
}

func TestApplyDispositions_UnknownSourceIdsAreIgnored(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("a.go", 3, "LOW", "nit")})
	agentOut := []types.LineComment{
		{FilePath: "x.go", Disposition: &types.Disposition{SourceID: "FP-9", State: "rejected", Reason: "n/a"}},
		{FilePath: "y.go", LineNumber: 1, Importance: "LOW", CommentBody: "own finding", Sources: []string{"FP-7"}},
	}
	findings, active, records := ApplyDispositions(agentOut, claims)
	if len(findings) != 1 || len(active) != 0 || len(records) != 1 || records[0].State != "unverified" {
		t.Fatalf("findings=%d active=%d records=%+v", len(findings), len(active), records)
	}
}

func TestFirstPassClaims_SkipsTheFirstPassSummary(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{
		lc("SUMMARY", 0, "", "Overall the change looks fine."),
		lc("a.go", 3, "LOW", "nit"),
	})
	if len(claims) != 1 || claims[0].SourceID != "FP-1" || claims[0].FilePath != "a.go" {
		t.Fatalf("the first pass's narrative is not a claim: %+v", claims)
	}
}

func TestApplyDispositions_ConfirmingFindingWithoutIDStillNamesTheMergeTarget(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("a.go", 3, "CRITICAL", "Nil deref.")})
	agentOut := []types.LineComment{{FilePath: "a.go", LineNumber: 3, Importance: "CRITICAL", CommentBody: "cfg is nil.", Sources: []string{"FP-1"}}}
	_, active, records := ApplyDispositions(agentOut, claims)
	if len(active) != 0 || len(records) != 1 || records[0].MergedInto != "a.go:3" {
		t.Fatalf("a merged record must point somewhere: active=%+v records=%+v", active, records)
	}
}

func TestNormalizeAgentLifecycleFields_OnlyThePolicyMayCreateRecords(t *testing.T) {
	out := []types.LineComment{
		{FilePath: "a.go", LineNumber: 1, Importance: "CRITICAL", CommentBody: "real", State: "rejected", Inactive: true, MergedInto: "x", Assessment: &types.Disposition{State: "rejected"}, Original: &types.OriginalClaim{SourceID: "FP-9"}},
		{FilePath: "b.go", LineNumber: 2, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "r"}},
	}
	NormalizeAgentLifecycleFields(out)
	if out[0].State != "" || out[0].Inactive || out[0].MergedInto != "" || out[0].Assessment != nil || out[0].Original != nil {
		t.Errorf("agent-authored lifecycle fields must be cleared: %+v", out[0])
	}
	if out[1].Disposition == nil {
		t.Errorf("disposition entries are the agent's to make and must survive")
	}
}

func TestApplyDispositions_RejectionWithoutAReasonIsNotARejection(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("d.go", 20, "MEDIUM", "Missing null check.")})
	agentOut := []types.LineComment{{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected"}}}
	_, active, records := ApplyDispositions(agentOut, claims)
	if len(active) != 0 || len(records) != 1 || records[0].State != StateUnverified || records[0].Assessment != nil {
		t.Fatalf("a reasonless rejection must fall through to unaccounted (unverified): active=%+v records=%+v", active, records)
	}
}

func TestApplyDispositions_SourcesOnSummaryOrCheckDoNotConfirmClaims(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("a.go", 3, "CRITICAL", "Nil deref.")})
	agentOut := []types.LineComment{{FilePath: "SUMMARY", CommentBody: "Verdict: approve.", Sources: []string{"FP-1"}}}
	_, active, records := ApplyDispositions(agentOut, claims)
	if len(active) != 1 || len(records) != 0 || active[0].State != StateUnverified {
		t.Fatalf("only an ordinary finding can cover a claim: active=%+v records=%+v", active, records)
	}
}

func TestNormalizeAgentLifecycleFields_DuplicateIdsBecomeUnlabelled(t *testing.T) {
	out := []types.LineComment{
		{ID: "A-1", FilePath: "a.go", LineNumber: 1, CommentBody: "first"},
		{ID: "A-1", FilePath: "b.go", LineNumber: 2, CommentBody: "second"},
		{ID: "A-2", FilePath: "c.go", LineNumber: 3, CommentBody: "third"},
	}
	NormalizeAgentLifecycleFields(out)
	if out[0].ID != "" || out[1].ID != "" || out[2].ID != "A-2" {
		t.Fatalf("an id the agent used twice identifies nothing: %q %q %q", out[0].ID, out[1].ID, out[2].ID)
	}
}

func TestApplyDispositions_RejectionNeedsEvidence(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("d.go", 20, "MEDIUM", "Missing null check.")})
	agentOut := []types.LineComment{{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "Never nil here."}}}
	_, _, records := ApplyDispositions(agentOut, claims)
	if records[0].State != StateUnverified {
		t.Fatalf("a rejection without evidence is unsupported prose and falls through to unverified: %+v", records[0])
	}
	agentOut[0].Disposition.Evidence = []types.EvidenceRef{{File: "d.go", Line: 18}}
	_, _, records = ApplyDispositions(agentOut, claims)
	if records[0].State != StateRejected {
		t.Fatalf("with a reason and evidence the rejection stands: %+v", records[0])
	}
}

func TestApplyDispositions_RejectionEvidenceMustResolve(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("d.go", 20, "MEDIUM", "Missing null check.")})
	reject := func(file string) []types.LineComment {
		return []types.LineComment{{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "Never nil here.", Evidence: []types.EvidenceRef{{File: file, Line: 1}}}}}
	}
	exists := func(path string) bool { return path == "client/http.go" }
	_, _, records := ApplyDispositionsWithEvidence(reject("does/not/exist.go"), claims, exists)
	if records[0].State != StateUnverified {
		t.Fatalf("evidence that does not resolve is no evidence: %+v", records[0])
	}
	_, _, records = ApplyDispositionsWithEvidence(reject("client/http.go"), claims, exists)
	if records[0].State != StateRejected {
		t.Fatalf("resolving evidence supports the rejection: %+v", records[0])
	}
}

func TestApplyDispositions_KeepsOnlyResolvedEvidenceAndRecordsTheMergeBasis(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("d.go", 20, "MEDIUM", "Missing null check."), lc("a.go", 3, "CRITICAL", "Nil deref.")})
	agentOut := []types.LineComment{
		{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "Never nil.", Evidence: []types.EvidenceRef{{File: "ghost.go", Line: 1}, {File: "client/http.go", Line: 88}}}},
		{ID: "A-1", FilePath: "a.go", LineNumber: 3, Importance: "CRITICAL", CommentBody: "cfg nil", Sources: []string{"FP-2"}},
	}
	_, _, records := ApplyDispositionsWithEvidence(agentOut, claims, func(p string) bool { return p == "client/http.go" })
	var rejected, merged types.LineComment
	for _, r := range records {
		switch r.State {
		case StateRejected:
			rejected = r
		case StateMerged:
			merged = r
		}
	}
	if len(rejected.Assessment.Evidence) != 1 || rejected.Assessment.Evidence[0].File != "client/http.go" {
		t.Errorf("unresolved evidence must not be stored as grounding: %+v", rejected.Assessment.Evidence)
	}
	if merged.MergeBasis != "sources" {
		t.Errorf("a claim the agent linked is merged by sources: %+v", merged)
	}
}

func TestEvidenceFileExists_RequiresARegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/pkg/x", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/pkg/x/f.go", []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := evidenceFileExists([]string{"pkg/x/f.go"}, dir)
	if !check("pkg/x/f.go") {
		t.Error("a regular file in the worktree resolves")
	}
	if check("pkg") || check("pkg/x") {
		t.Error("a directory is not evidence")
	}
	if check("f.go") {
		t.Error("a bare basename is not evidence")
	}
}

func TestNormalizeAgentLifecycleFields_ClearsMergeBasis(t *testing.T) {
	out := []types.LineComment{{FilePath: "a.go", LineNumber: 1, CommentBody: "x", MergeBasis: "sources"}}
	NormalizeAgentLifecycleFields(out)
	if out[0].MergeBasis != "" {
		t.Fatal("merge basis is policy metadata, not the agent's to set")
	}
}

func TestApplyDispositions_RejectionEvidenceNeedsALine(t *testing.T) {
	claims := firstPassClaims([]types.LineComment{lc("d.go", 20, "MEDIUM", "Missing null check.")})
	agentOut := []types.LineComment{{FilePath: "d.go", LineNumber: 20, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "Never nil.", Evidence: []types.EvidenceRef{{File: "d.go"}}}}}
	_, _, records := ApplyDispositionsWithEvidence(agentOut, claims, func(string) bool { return true })
	if records[0].State != StateUnverified {
		t.Fatalf("a file without a line is the claim's own location, not evidence: %+v", records[0])
	}
}

func TestNormalizeAgentLifecycleFields_DuplicateCensusIgnoresNonFindings(t *testing.T) {
	out := []types.LineComment{
		{ID: "A-1", FilePath: "a.go", LineNumber: 1, CommentBody: "real"},
		{ID: "A-1", FilePath: "b.go", LineNumber: 2, Disposition: &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: "r"}},
		{ID: "A-1", FilePath: "SUMMARY", Summary: &types.SummaryBlock{Verdict: "approve"}},
	}
	NormalizeAgentLifecycleFields(out)
	if out[0].ID != "A-1" || out[1].ID != "" || out[2].ID != "" {
		t.Fatalf("a mislabelled disposition or summary must not cost the real finding its id: %q %q %q", out[0].ID, out[1].ID, out[2].ID)
	}
}

func TestEvidenceFileExists_LineMustBeWithinTheFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/f.go", []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := evidenceRefResolves(nil, dir)
	if !check(types.EvidenceRef{File: "f.go", Line: 3}) {
		t.Error("a line inside the file resolves")
	}
	if check(types.EvidenceRef{File: "f.go", Line: 99999}) {
		t.Error("a line past the end of the file is not evidence")
	}
}
