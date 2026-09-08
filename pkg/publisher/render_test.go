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
	visible := out[:strings.Index(out, "<details>")]
	if strings.Count(visible, "Nil deref when cfg is missing.") != 1 {
		t.Errorf("title sentence should be removed from the visible body\n%s", out)
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
	if strings.Count(out, "Single sentence only.") != 1 {
		t.Errorf("a single-sentence comment is the headline and nothing else\n%s", out)
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

func TestRenderSummaryRoundOne(t *testing.T) {
	r := roundOne()
	r.RoundNumber = 1
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	for _, want := range []string{
		SummaryMarker,
		"### PRism review: merge confidence 2/5",
		"- **[CRITICAL]** Critical thing — [`a.go:10`](https://github.com/acme/example/blob/sha-round-1/a.go#L10)",
		"- **[MEDIUM]** Medium thing — [`b.go:20`]",
		"<sub>Reviews (1) · reviewed sha-rou",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Since last review") || strings.Contains(out, "<details>") || strings.Contains(out, "| Sev |") {
		t.Errorf("round 1 must have no diff line and no table:\n%s", out)
	}
}

func TestRenderSummaryRoundTwoShowsDiffAndSkipsGreptileOnly(t *testing.T) {
	r := roundOne()
	r.RoundNumber = 2
	r.Previous = []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "c1", State: db.PublishedStateOpen, CommentID: 77},
		{Kind: db.PublishedKindFinding, Fingerprint: "gone", State: db.PublishedStateOpen},
	}
	r.GreptileOnly = []GreptileOnlyRef{{Title: "Missing await", File: "y.ts", Line: 3, Severity: "medium", CommentID: 555}}
	r.InlineComments = map[string]int64{"c1": 77}
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if !strings.Contains(out, "**Since last review:** 2 new · 1 still open · 1 fixed") {
		t.Errorf("diff line wrong:\n%s", out)
	}
	if !strings.Contains(out, "[`a.go:10`](https://github.com/acme/example/pull/7#discussion_r77)") {
		t.Errorf("previously posted finding must link its inline comment:\n%s", out)
	}
	if strings.Contains(out, "Missing await") || strings.Contains(out, "Greptile") {
		t.Errorf("Greptile-only findings are already on the PR and must not be repeated:\n%s", out)
	}
}

func TestRenderSummaryStaysUnderCap(t *testing.T) {
	r := roundOne()
	r.Findings = r.Findings[:1]
	for i := 0; i < 900; i++ {
		r.Findings = append(r.Findings, f(fmt.Sprintf("c%d", i), "critical", fmt.Sprintf("dir/file%d.go", i), 10, strings.Repeat("word ", 30)))
	}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if len(out) > SummaryMaxChars {
		t.Fatalf("summary %d chars exceeds cap %d", len(out), SummaryMaxChars)
	}
	if !strings.Contains(out, "more on the [dashboard]") || !strings.HasSuffix(strings.TrimSpace(out), "</sub>") {
		t.Fatalf("truncated summary lost structure\n%s", out[len(out)-300:])
	}
}
