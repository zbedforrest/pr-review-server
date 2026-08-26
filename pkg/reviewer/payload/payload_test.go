package payload

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/types"
)

func TestPayload_ReviewRunJSON(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	p := Payload{ReviewRun: &ReviewRunInfo{
		RunID:       "run-0123456789abcdef0123456789abcdef",
		HTMLPath:    "runs/acme/widgets/7/abcdef0/run-0123456789abcdef0123456789abcdef.html",
		JSONPath:    "runs/acme/widgets/7/abcdef0/run-0123456789abcdef0123456789abcdef.json",
		StartedAt:   now,
		CompletedAt: now.Add(2 * time.Minute),
		DurationMS:  120000,
		Models: []ModelUse{{
			Stage:                "agent",
			Provider:             "openrouter",
			Backend:              "openrouter",
			RequestedModel:       "openai/gpt-5.6-sol",
			ServedModel:          "openai/gpt-5.6-sol",
			ServingModelVerified: false,
			Effort:               "medium",
		}},
		Config: &runconfig.Snapshot{
			Effective: runconfig.Effective{
				SchemaVersion: runconfig.SchemaVersion,
				Agent: runconfig.Agent{
					Enabled: true, Backend: "openrouter", Model: "openai/gpt-5.6-sol",
					Effort: "medium", WallClockSeconds: 900, MaxTurns: 120,
				},
				FirstPass: runconfig.FirstPass{Samples: 3},
			},
			Sources: map[string]string{"agent.model": runconfig.SourceRequest},
			Hash:    "config-hash",
		},
	}}

	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"review_run"`, `"run_id":"run-0123456789abcdef0123456789abcdef"`,
		`"html_path":"runs/acme/widgets/7/abcdef0/run-0123456789abcdef0123456789abcdef.html"`,
		`"provider":"openrouter"`, `"serving_model_verified":false`,
		`"wall_clock_seconds":900`, `"agent.model":"request"`, `"hash":"config-hash"`,
	} {
		if !strings.Contains(string(body), fragment) {
			t.Errorf("review-run JSON missing %s: %s", fragment, body)
		}
	}
}

func TestSourceWindow_BasicSlice(t *testing.T) {
	src := joinLines("L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9", "L10")
	before, after := SourceWindow(src, 5, 2)

	wantBefore := []string{"L3", "L4", "L5"}
	wantAfter := []string{"L6", "L7"}
	assertLines(t, "before", before, wantBefore)
	assertLines(t, "after", after, wantAfter)
}

func TestSourceWindow_StartOfFile(t *testing.T) {
	src := joinLines("A", "B", "C", "D")
	before, after := SourceWindow(src, 1, 5)
	assertLines(t, "before", before, []string{"A"})
	assertLines(t, "after", after, []string{"B", "C", "D"})
}

func TestSourceWindow_EndOfFile(t *testing.T) {
	src := joinLines("A", "B", "C", "D")
	before, after := SourceWindow(src, 4, 5)
	assertLines(t, "before", before, []string{"A", "B", "C", "D"})
	assertLines(t, "after", after, nil)
}

func TestSourceWindow_TrailingNewlineIgnored(t *testing.T) {
	// File ending in \n shouldn't add a phantom empty line.
	src := "A\nB\nC\n"
	before, after := SourceWindow(src, 3, 2)
	assertLines(t, "before", before, []string{"A", "B", "C"})
	assertLines(t, "after", after, nil)
}

func TestSourceWindow_OutOfRange(t *testing.T) {
	src := joinLines("only one line")
	before, after := SourceWindow(src, 50, 5)
	if before != nil || after != nil {
		t.Errorf("expected nils for out-of-range line, got %v / %v", before, after)
	}
}

func TestHunkForLine_FindsContainingHunk(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"--- a/foo.go",
		"+++ b/foo.go",
		"@@ -10,5 +10,7 @@",
		" ctx1",
		" ctx2",
		"-old line",
		"+new line A",
		"+new line B",
		" ctx3",
		" ctx4",
		"@@ -100,3 +102,3 @@",
		" x",
		"-y",
		"+y2",
		"",
	}, "\n")

	// Line 13 (post-image) falls inside the first hunk [10, 10+7).
	got := HunkForLine(diff, "foo.go", 13)
	if !strings.Contains(got, "+new line A") {
		t.Errorf("expected first hunk body, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "@@ -10,5 +10,7 @@") {
		t.Errorf("expected hunk header at start, got:\n%s", got)
	}
	if strings.Contains(got, "y2") {
		t.Errorf("hunk leaked into the next hunk's body:\n%s", got)
	}

	// Line 103 lands in the second hunk.
	got2 := HunkForLine(diff, "foo.go", 103)
	if !strings.HasPrefix(got2, "@@ -100,3 +102,3 @@") || !strings.Contains(got2, "+y2") {
		t.Errorf("expected second hunk, got:\n%s", got2)
	}
}

func TestHunkForLine_NoMatchReturnsEmpty(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"+++ b/foo.go",
		"@@ -1,3 +1,3 @@",
		" a",
		"-b",
		"+B",
		"",
	}, "\n")

	if got := HunkForLine(diff, "foo.go", 999); got != "" {
		t.Errorf("expected empty for line outside all hunks, got %q", got)
	}
	if got := HunkForLine(diff, "other.go", 2); got != "" {
		t.Errorf("expected empty for unknown file, got %q", got)
	}
}

func TestHunkForLine_DefaultCount(t *testing.T) {
	// Unified diff spec: omitted count means 1.
	diff := "diff --git a/x.go b/x.go\n+++ b/x.go\n@@ -5 +5 @@\n-old\n+new\n"
	if got := HunkForLine(diff, "x.go", 5); !strings.Contains(got, "+new") {
		t.Errorf("expected hunk for line 5 with default count=1, got %q", got)
	}
	if got := HunkForLine(diff, "x.go", 6); got != "" {
		t.Errorf("expected empty for line 6 outside default count=1 hunk, got %q", got)
	}
}

func TestBuild_SortsBySeverityThenFileLine(t *testing.T) {
	diff := "" // no diff = no hunk extraction; we're testing sort + counts here.
	comments := []types.LineComment{
		{FilePath: "z.go", LineNumber: 1, CommentBody: "low z", Importance: "LOW"},
		{FilePath: "a.go", LineNumber: 50, CommentBody: "crit a hi", Importance: "CRITICAL"},
		{FilePath: "a.go", LineNumber: 5, CommentBody: "crit a lo", Importance: "CRITICAL"},
		{FilePath: "b.go", LineNumber: 10, CommentBody: "med b", Importance: "MEDIUM"},
		{FilePath: "x.go", LineNumber: 1, CommentBody: "unknown", Importance: ""},
	}

	p := Build("o", "r", 1, "sha", comments, diff, nil)

	if p.Counts.Critical != 2 || p.Counts.Medium != 1 || p.Counts.Low != 1 {
		t.Errorf("counts wrong: %+v", p.Counts)
	}
	if got := []string{
		p.Findings[0].Comment,
		p.Findings[1].Comment,
		p.Findings[2].Comment,
		p.Findings[3].Comment,
		p.Findings[4].Comment,
	}; got[0] != "crit a lo" || got[1] != "crit a hi" || got[2] != "med b" || got[3] != "low z" || got[4] != "unknown" {
		t.Errorf("wrong sort order: %v", got)
	}
}

func TestBuild_AttachesHunkAndSourceWindow(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"+++ b/foo.go",
		"@@ -1,3 +1,4 @@",
		" line1",
		" line2",
		"+inserted",
		" line3",
		"",
	}, "\n")

	contents := joinLines("line1", "line2", "inserted", "line3", "tail1", "tail2")
	comments := []types.LineComment{
		{FilePath: "foo.go", LineNumber: 3, CommentBody: "look at insert", Importance: "MEDIUM"},
	}

	p := Build("o", "r", 1, "sha", comments, diff, map[string]string{"foo.go": contents})

	if len(p.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(p.Findings))
	}
	f := p.Findings[0]
	if !strings.Contains(f.DiffHunk, "+inserted") {
		t.Errorf("expected hunk to include +inserted, got: %s", f.DiffHunk)
	}
	if len(f.SourceBefore) == 0 || f.SourceBefore[len(f.SourceBefore)-1] != "inserted" {
		t.Errorf("expected SourceBefore to end with cited line 'inserted', got: %v", f.SourceBefore)
	}
	if len(f.SourceAfter) == 0 || f.SourceAfter[0] != "line3" {
		t.Errorf("expected SourceAfter to start with 'line3', got: %v", f.SourceAfter)
	}
}

func TestBuild_EmptyComments(t *testing.T) {
	p := Build("o", "r", 1, "sha", nil, "", nil)
	if len(p.Findings) != 0 {
		t.Errorf("expected zero findings, got %d", len(p.Findings))
	}
	if p.Counts.Critical+p.Counts.Medium+p.Counts.Low != 0 {
		t.Errorf("expected zero counts, got %+v", p.Counts)
	}
	if p.SchemaVersion != "1" {
		t.Errorf("expected schema_version=1, got %q", p.SchemaVersion)
	}
}

func TestBuildPublishesOnlyValidatedFindingContracts(t *testing.T) {
	condition := "The candidate returns an error while the control succeeds."
	observable := "Compare exact response statuses."
	valid := &types.FindingContract{
		SchemaVersion:        types.FindingContractSchemaVersion,
		FindingKind:          "production_behavior",
		Materiality:          "current_impact",
		CurrentImpact:        "Affected requests return an error.",
		Falsifiability:       "falsifiable",
		FalsifiableCondition: &condition,
		ExpectedObservable:   &observable,
		Subjects: []types.FindingSubject{{
			Kind: "symbol",
			Path: "handler.go",
			Name: "Handle",
		}},
		Uncertainty:       "One request state is covered.",
		SeverityRationale: "The request cannot complete.",
	}
	invalid := *valid
	invalid.FindingKind = "bug"
	p := Build("o", "r", 1, "sha", []types.LineComment{
		{FilePath: "valid.go", LineNumber: 1, CommentBody: "valid", FindingContract: valid},
		{FilePath: "invalid.go", LineNumber: 2, CommentBody: "invalid", FindingContract: &invalid},
		{FilePath: "missing.go", LineNumber: 3, CommentBody: "missing"},
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "summary"},
	}, "", nil)
	byComment := map[string]Finding{}
	for _, finding := range p.Findings {
		byComment[finding.Comment] = finding
	}
	if byComment["valid"].FindingContractStatus != "valid" || byComment["valid"].FindingContract == nil {
		t.Fatalf("valid contract = %+v", byComment["valid"])
	}
	if byComment["invalid"].FindingContractStatus != "invalid" || byComment["invalid"].FindingContract != nil {
		t.Fatalf("invalid contract = %+v", byComment["invalid"])
	}
	if byComment["missing"].FindingContractStatus != "missing" || byComment["missing"].FindingContract != nil {
		t.Fatalf("missing contract = %+v", byComment["missing"])
	}
	if byComment["summary"].FindingContractStatus != "not_applicable" || byComment["summary"].FindingContract != nil {
		t.Fatalf("summary contract = %+v", byComment["summary"])
	}
	roundTrip := p.ToLineComments()
	for _, comment := range roundTrip {
		if comment.CommentBody == "valid" && comment.FindingContract == nil {
			t.Fatal("valid contract did not round trip")
		}
		if comment.CommentBody != "valid" && comment.FindingContract != nil {
			t.Fatalf("unvalidated contract round tripped: %+v", comment)
		}
	}
}

func TestBuildNormalizesFindingContractsAtPublicationBoundary(t *testing.T) {
	condition := " The candidate returns an error while the control succeeds. "
	observable := " Compare exact response statuses. "
	contract := &types.FindingContract{
		SchemaVersion:        types.FindingContractSchemaVersion,
		FindingKind:          " production_behavior ",
		Materiality:          " current_impact ",
		CurrentImpact:        " Affected requests return an error. ",
		Falsifiability:       " falsifiable ",
		FalsifiableCondition: &condition,
		ExpectedObservable:   &observable,
		Subjects: []types.FindingSubject{{
			Kind: " symbol ",
			Path: " handler.go ",
			Name: " Handle ",
		}},
		Uncertainty:       " One request state is covered. ",
		SeverityRationale: " The request cannot complete. ",
	}

	p := Build("o", "r", 1, "sha", []types.LineComment{{
		FilePath:        "handler.go",
		LineNumber:      1,
		CommentBody:     "valid after normalization",
		FindingContract: contract,
	}}, "", nil)

	finding := p.Findings[0]
	if finding.FindingContractStatus != "valid" || finding.FindingContract == nil {
		t.Fatalf("normalized contract = %+v", finding)
	}
	if finding.FindingContract.FindingKind != "production_behavior" || finding.FindingContract.Subjects[0].Kind != "symbol" || finding.FindingContract.CurrentImpact != "Affected requests return an error." {
		t.Fatalf("contract was not normalized: %+v", finding.FindingContract)
	}
}

func TestLegacyFindingOmitsEmptyContractStatus(t *testing.T) {
	body, err := json.Marshal(Finding{File: "legacy.go", Comment: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "finding_contract_status") {
		t.Fatalf("legacy finding published an empty contract status: %s", body)
	}
}

func TestToLineCommentsRejectsContractWithoutValidStatus(t *testing.T) {
	condition := "The candidate fails."
	observable := "Compare the response status."
	contract := &types.FindingContract{
		SchemaVersion:        types.FindingContractSchemaVersion,
		FindingKind:          "production_behavior",
		Materiality:          "current_impact",
		CurrentImpact:        "Affected requests fail.",
		Falsifiability:       "falsifiable",
		FalsifiableCondition: &condition,
		ExpectedObservable:   &observable,
		Subjects: []types.FindingSubject{{
			Kind: "file",
			Path: "handler.go",
		}},
		Uncertainty:       "One request state is covered.",
		SeverityRationale: "The request cannot complete.",
	}
	p := Payload{Findings: []Finding{{
		Severity:        "critical",
		File:            "handler.go",
		Comment:         "failure",
		FindingContract: contract,
	}}}
	if comments := p.ToLineComments(); comments[0].FindingContract != nil {
		t.Fatal("contract without a valid status crossed the sidecar trust boundary")
	}
}

func TestToCompactMarkdown_FullFinding(t *testing.T) {
	p := Payload{
		SchemaVersion: "1",
		Owner:         "acme",
		Repo:          "example",
		PRNumber:      42,
		CommitSHA:     "abc1234",
		Counts:        Counts{Critical: 1, Medium: 0, Low: 0},
		Findings: []Finding{
			{
				Severity:     "critical",
				File:         "auth/login.go",
				Line:         12,
				Comment:      "Missing nil check.",
				DiffHunk:     "@@ -10,3 +10,4 @@\n+\tx := f()",
				SourceBefore: []string{"line10", "line11", "line12"},
				SourceAfter:  []string{"line13"},
			},
		},
	}
	meta := CompactMeta{
		HeadSHA:           "def5678",
		IsStale:           true,
		IsInFlight:        false,
		GeneratedAt:       "2026-05-29T17:00:00Z",
		ReviewURL:         "https://example.test/reviews/acme_example_42_abc1234.html",
		FindingsAvailable: true,
	}

	out := p.ToCompactMarkdown(meta)

	for _, want := range []string{
		"PR: acme/example#42",
		"Reviewed commit: abc1234",
		"Current HEAD:    def5678",
		"Stale: true",
		"In flight (regenerating): false",
		"Generated at: 2026-05-29T17:00:00Z",
		"Counts: critical=1 medium=0 low=0",
		"Schema: 1",
		"=== FINDINGS (1) ===",
		"--- [CRITICAL] auth/login.go:12 ---",
		"COMMENT:\nMissing nil check.",
		"DIFF HUNK:\n```diff\n@@ -10,3 +10,4 @@\n+\tx := f()\n```",
		"SOURCE CONTEXT",
		"----- cited line above -----",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compact markdown missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestToCompactMarkdown_NoFindingsAvailable(t *testing.T) {
	p := Payload{Owner: "acme", Repo: "example", PRNumber: 7}
	out := p.ToCompactMarkdown(CompactMeta{FindingsAvailable: false})

	if !strings.Contains(out, "=== NO STRUCTURED FINDINGS ===") {
		t.Errorf("expected no-findings notice, got:\n%s", out)
	}
	if strings.Contains(out, "=== FINDINGS") {
		t.Errorf("should not render a findings section when unavailable:\n%s", out)
	}
	if !strings.Contains(out, "Schema: unknown") {
		t.Errorf("expected schema fallback to 'unknown', got:\n%s", out)
	}
}

func TestToCompactMarkdown_OmitsEmptyHunkAndSource(t *testing.T) {
	p := Payload{
		SchemaVersion: "1",
		Findings: []Finding{
			{Severity: "low", File: "x.go", Line: 3, Comment: "nit"},
		},
	}
	out := p.ToCompactMarkdown(CompactMeta{FindingsAvailable: true})

	if strings.Contains(out, "DIFF HUNK") {
		t.Errorf("should omit DIFF HUNK when empty:\n%s", out)
	}
	if strings.Contains(out, "SOURCE CONTEXT") {
		t.Errorf("should omit SOURCE CONTEXT when empty:\n%s", out)
	}
}

// The optional required_checks block must serialize its funnel counters and
// stay absent when the feature issued no checks (sidecar byte-compat with
// checkless builds).
func TestPayload_RequiredChecksJSON(t *testing.T) {
	p := Build("o", "r", 1, "sha", nil, "", nil)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "required_checks") {
		t.Errorf("unset required_checks must be omitted: %s", data)
	}

	p.RequiredChecks = &RequiredChecksInfo{Issued: 3, Answered: 2, Violated: 1, EvidenceOK: 1}
	data, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"checks_issued":3`, `"checks_answered":2`, `"checks_violated":1`, `"checks_evidence_ok":1`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled payload missing %s: %s", want, data)
		}
	}
}

func TestPayload_CarriedFindingsJSON(t *testing.T) {
	p := Build("o", "r", 1, "sha", nil, "", nil)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "carried_findings") {
		t.Errorf("unset carried_findings must be omitted: %s", data)
	}

	p.CarriedFindings = &CarryForwardInfo{FromSHA: "abc1234", CarriedIn: 2, CarriedDropped: 1}
	data, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"from_sha":"abc1234"`, `"carried_in":2`, `"carried_dropped":1`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled payload missing %s: %s", want, data)
		}
	}
}

// ToLineComments must round-trip what Build normalized: severity back to the
// upper-case Importance enum ("unknown" to empty — never invent a severity),
// provenance verbatim, context fields dropped.
func TestToLineComments_RoundTrip(t *testing.T) {
	comments := []types.LineComment{
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "verdict"},
		{FilePath: "a.go", LineNumber: 10, Importance: "CRITICAL", CommentBody: "crash"},
		{FilePath: "b.go", LineNumber: 20, Importance: "medium", CommentBody: "leak"},
		{FilePath: "c.go", LineNumber: 30, Importance: "weird", CommentBody: "??"},
	}
	p := Build("o", "r", 1, "sha", comments, "", nil)
	got := p.ToLineComments()
	if len(got) != len(comments) {
		t.Fatalf("want %d comments, got %d", len(comments), len(got))
	}
	byFile := map[string]types.LineComment{}
	for _, c := range got {
		byFile[c.FilePath] = c
	}
	if c := byFile["a.go"]; c.Importance != "CRITICAL" || c.LineNumber != 10 || c.CommentBody != "crash" {
		t.Errorf("a.go round-trip mangled: %+v", c)
	}
	if c := byFile["b.go"]; c.Importance != "MEDIUM" {
		t.Errorf("b.go: lower-case severity should round-trip to MEDIUM, got %q", c.Importance)
	}
	if c := byFile["c.go"]; c.Importance != "" {
		t.Errorf("c.go: unknown severity must round-trip to empty, got %q", c.Importance)
	}
	// Build stamps provenance (DeriveProvenance); the round-trip keeps it.
	if c := byFile["a.go"]; c.Provenance == "" {
		t.Errorf("a.go: provenance should round-trip, got empty")
	}

	if got := (Payload{}).ToLineComments(); got != nil {
		t.Errorf("empty payload should yield nil, got %+v", got)
	}
}

// --- helpers ---

func joinLines(ls ...string) string {
	return strings.Join(ls, "\n")
}

func assertLines(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len=%d want=%d (got=%v want=%v)", label, len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]=%q want %q", label, i, got[i], want[i])
		}
	}
}
