package publisher

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func strp(s string) *string { return &s }

func baseRound() Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 7, HeadSHA: "abc1234def5678", RoundNumber: 1,
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "The narrative."),
			f("c1", "critical", "path/file.go", 12, "Nil deref when cfg is missing.\nMore detail here."),
			f("m1", "medium", "other.py", 40, "Unbounded retry loop."),
			f("l1", "low", "x.ts", 8, "Typo in log message."),
		},
		SourceTags:   map[string]string{"m1": "both"},
		DashboardURL: "https://prism.example/pr/acme/example/7",
	}
}

func TestRenderSummaryRoundOne(t *testing.T) {
	r := baseRound()
	sel := Select(r.Findings, nil, map[string]map[int]bool{"path/file.go": {12: true}}, DefaultPolicy())
	out := RenderSummary(r, sel)

	mustContain := []string{
		SummaryMarker,
		"### PRism review: merge confidence 3/5",
		"Findings that should be addressed before merge.",
		"<details><summary>Findings (3)</summary>",
		"| critical | `path/file.go:12` | Nil deref when cfg is missing. | PRism |",
		"| medium | `other.py:40` | Unbounded retry loop. | Both |",
		"| low | `x.ts:8` | Typo in log message. | PRism |",
		"reviewed abc1234 ",
		`<a href="https://prism.example/pr/acme/example/7">dashboard</a>`,
		"Reviews (1)",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("summary missing %q\n%s", s, out)
		}
	}
	if strings.Contains(out, "Since last review") {
		t.Errorf("round 1 must not show since-last-review line\n%s", out)
	}
	if strings.Contains(out, "The narrative.") {
		t.Errorf("SUMMARY finding must not appear as a table row\n%s", out)
	}
}

func TestRenderSummaryRoundTwoCountsAndGreptile(t *testing.T) {
	r := baseRound()
	r.RoundNumber = 2
	r.Previous = []db.PublishedFinding{
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "c1", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "gone1", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "gone2", State: db.PublishedStateOpen},
		{Kind: db.PublishedKindFinding, Fingerprint: "gone3", State: db.PublishedStateResolved},
	}
	r.GreptileOnly = []GreptileOnlyRef{
		{Title: "Missing await", File: "y.ts", Line: 3, Severity: "medium", CommentID: 555},
		{Title: "Unlinked note", File: "z.ts", Line: 9, Severity: "low"},
	}
	sel := Select(r.Findings, nil, nil, DefaultPolicy())
	out := RenderSummary(r, sel)

	mustContain := []string{
		"**Since last review:** 2 new · 1 still open · 2 fixed",
		"<details><summary>Findings (5)</summary>",
		"| medium | `y.ts:3` | [Missing await](https://github.com/acme/example/pull/7#discussion_r555) | Greptile |",
		"| low | `z.ts:9` | Unlinked note | Greptile |",
		"Reviews (2)",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("summary missing %q\n%s", s, out)
		}
	}
	critIdx := strings.Index(out, "| critical |")
	medIdx := strings.Index(out, "| medium |")
	lowIdx := strings.Index(out, "| low |")
	if !(critIdx < medIdx && medIdx < lowIdx) {
		t.Errorf("rows not sorted by severity\n%s", out)
	}
}

func TestRenderSummaryRecommendationLines(t *testing.T) {
	cases := map[int]string{
		5: "No blocking findings.",
		4: "Minor findings worth a look before merge.",
		3: "Findings that should be addressed before merge.",
		2: "Significant findings; please address before merge.",
		0: "Significant findings; please address before merge.",
	}
	for score, want := range cases {
		if got := recommendation(score); got != want {
			t.Errorf("recommendation(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestRenderSummaryTruncates(t *testing.T) {
	r := baseRound()
	r.Findings = nil
	for i := 0; i < 2000; i++ {
		r.Findings = append(r.Findings, f(fmt.Sprintf("id%d", i), "medium", fmt.Sprintf("dir/very/long/path/to/file%04d.go", i), i+1, strings.Repeat("x", 120)))
	}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if len(out) > SummaryMaxChars {
		t.Fatalf("summary length %d exceeds cap %d", len(out), SummaryMaxChars)
	}
	if !strings.Contains(out, SummaryMarker) || !strings.Contains(out, "</details>") {
		t.Fatalf("truncated summary lost structure\n%s", out[len(out)-400:])
	}
	if !strings.Contains(out, "see dashboard") {
		t.Fatalf("truncated summary must point to the dashboard")
	}
}

func TestRenderInlineFull(t *testing.T) {
	fd := f("a.go:0:abc", "critical", "pkg/a.go", 3, "Nil deref when cfg is missing. The config loader returns nil on a missing file and the caller dereferences it.")
	fd.FindingContract = &types.FindingContract{
		Falsifiability:       "falsifiable",
		FalsifiableCondition: strp("Run the service with no config file"),
		ExpectedObservable:   strp("a panic in loadConfig"),
	}
	out := RenderInline(fd, "both", "https://prism.example/go/agent?o=acme&r=example&n=7")

	mustContain := []string{
		FindingMarker("a.go:0:abc"),
		"**[CRITICAL] Nil deref when cfg is missing.**",
		"The config loader returns nil on a missing file and the caller dereferences it.",
		"**How to verify:** Run the service with no config file. Expected: a panic in loadConfig.",
		"<sub>Source: PRism · Both</sub>",
		`<sub><a href="https://prism.example/go/agent?o=acme&r=example&n=7&f=a.go%3A0%3Aabc&p=pkg%2Fa.go&l=3">Fix with agent</a></sub>`,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("inline missing %q\n%s", s, out)
		}
	}
	if strings.Count(out, "Nil deref when cfg is missing.") != 1 {
		t.Errorf("title sentence should be removed from the body\n%s", out)
	}
}

func TestRenderInlineMinimal(t *testing.T) {
	fd := f("abc", "medium", "a.go", 3, "Single sentence only.")
	fd.FindingContract = &types.FindingContract{Falsifiability: "not_falsifiable"}
	out := RenderInline(fd, "prism-only", "")
	if strings.Contains(out, "How to verify") {
		t.Errorf("non-falsifiable contract must not render verify line\n%s", out)
	}
	if strings.Contains(out, "Source:") {
		t.Errorf("prism-only must omit the source line\n%s", out)
	}
	if strings.Contains(out, "Fix with agent") {
		t.Errorf("empty agent link base must omit the agent link\n%s", out)
	}
	if strings.Count(out, "Single sentence only.") != 2 {
		t.Errorf("single-sentence comment should appear as both title and body\n%s", out)
	}
}

func TestRenderInlineTitleCapAndSuggestion(t *testing.T) {
	long := strings.Repeat("word ", 40)
	body := long + "\n```suggestion\nfixed := true\n```"
	out := RenderInline(f("abc", "low", "a.go", 3, body), "", "")
	if !strings.Contains(out, "```suggestion\nfixed := true\n```") {
		t.Errorf("suggestion fence not preserved\n%s", out)
	}
	titleLine := strings.Split(out, "\n")[1]
	if len(titleLine) > len("**[LOW] **")+100 {
		t.Errorf("title too long: %d chars\n%s", len(titleLine), titleLine)
	}
	if !strings.HasSuffix(titleLine, "...**") {
		t.Errorf("truncated title should end with ellipsis\n%s", titleLine)
	}
}

const provenanceNote = "_[first-pass finding — retained by reconciliation, not independently confirmed by the review agent]_\n\n"

func TestRenderInline_StripsProvenanceNoteFromTitleAndBody(t *testing.T) {
	fd := f("x", "medium", "a.go", 3, provenanceNote+"Treating raw as context is wrong. It marks the next line commentable.")
	out := RenderInline(fd, "prism-only", "")

	if strings.Contains(out, "retained by reconciliation") {
		t.Fatalf("provenance note must not be rendered:\n%s", out)
	}
	if !strings.Contains(out, "**[MEDIUM] Treating raw as context is wrong.**") {
		t.Errorf("title must come from the real comment:\n%s", out)
	}
}

func TestRenderSummary_TableUsesRealFirstLineNotProvenanceNote(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Findings: []payload.Finding{f("x", "medium", "a.go", 3, provenanceNote+"Treating raw as context is wrong.")}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if strings.Contains(out, "retained by reconciliation") || !strings.Contains(out, "Treating raw as context is wrong.") {
		t.Fatalf("summary table row wrong:\n%s", out)
	}
}

func TestRenderSummary_RequestChangesVerdictCapsConfidence(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "**Verdict: request changes.** Two mediums need attention."),
			f("x", "medium", "a.go", 3, "Something."),
		}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if !strings.Contains(out, "merge confidence 3/5") {
		t.Fatalf("a request-changes verdict must cap confidence at 3:\n%s", out)
	}
}

func TestCommentableLines_TrailingNewlineDoesNotExtendHunk(t *testing.T) {
	patch := "@@ -1,2 +1,3 @@\n a\n+b\n c\n"
	got := CommentableLines(patch)
	if !got[1] || !got[2] || !got[3] {
		t.Fatalf("hunk lines 1-3 must be commentable: %v", got)
	}
	if got[4] {
		t.Fatalf("the line after the hunk must not be commentable when the patch ends in a newline: %v", got)
	}
}

func TestRenderInline_TitleFromAlreadyBoldSentenceIsNotDoubleBold(t *testing.T) {
	fd := f("x", "critical", "a.go", 3, "**The refresh loop upserts a stale snapshot.**\n\nDetails follow here.")
	out := RenderInline(fd, "prism-only", "")
	if !strings.Contains(out, "**[CRITICAL] The refresh loop upserts a stale snapshot.**") || strings.Contains(out, "****") {
		t.Fatalf("title must not nest bold markers:\n%s", out)
	}
	if strings.Count(out, "The refresh loop upserts a stale snapshot.") != 1 {
		t.Fatalf("title sentence must not be repeated in the body:\n%s", out)
	}
}

func TestRenderInline_HowToVerifyReadsWellWithConditionalObservable(t *testing.T) {
	fd := f("x", "medium", "a.go", 3, "Legacy path now verifies TLS.")
	fd.FindingContract = &types.FindingContract{
		Falsifiability:       "falsifiable",
		FalsifiableCondition: strp("Issue a POST to the arbiter endpoint with verify enabled."),
		ExpectedObservable:   strp("If the finding is wrong the request completes with a 2xx; if it is right httpx raises ConnectError."),
	}
	out := RenderInline(fd, "prism-only", "")
	want := "**How to verify:** Issue a POST to the arbiter endpoint with verify enabled. Expected: If the finding is wrong the request completes with a 2xx; if it is right httpx raises ConnectError."
	if !strings.Contains(out, want) {
		t.Fatalf("how-to-verify line:\n%s", out)
	}
	if strings.Contains(out, "expect If") {
		t.Fatalf("must not glue 'expect' onto a conditional sentence:\n%s", out)
	}
}
