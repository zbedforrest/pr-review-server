package publisher

import (
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

func TestHeadline_CutsAtClauseBoundaryNotMidWord(t *testing.T) {
	fd := withContract(f("x", "medium", "internal/journey/evaluator.go", 309, "c"), "production_behavior", "current_impact",
		"When a selected component entry fails to render, the request returns 500 with no dispositions, so the entry_payload_error row and any earlier holdout/suppression rows for that request are never recorded.", "")
	h := headline(fd)
	if !strings.HasPrefix(h, "Behavior change · When a selected component entry fails to render, the request returns 500 with no dispositions") {
		t.Fatalf("headline should end at the clause boundary: %q", h)
	}
	if strings.Contains(h, "suppres") || len([]rune(h)) > 140 {
		t.Fatalf("headline must not run past the clause or cut a word: %q", h)
	}
}

func TestRenderInline_FullEffectSentenceAppearsInBodyWhenHeadlineWasCut(t *testing.T) {
	impact := "When a selected component entry fails to render, the request returns 500 with no dispositions, so the entry_payload_error row and any earlier holdout/suppression rows for that request are never recorded."
	fd := withContract(f("x", "medium", "a.go", 3, "reasoning"), "production_behavior", "current_impact", impact, "")
	out := RenderInline(fd, "prism-only", "")
	visible := out[:strings.Index(out, "<details>")]
	if !strings.Contains(visible, impact) {
		t.Fatalf("the full effect sentence must be visible when the headline is a clause of it:\n%s", out)
	}
}

func TestRenderInline_DetailsCarryPlainAgentPrompt(t *testing.T) {
	fd := withContract(f("a.go:0:abc", "medium", "a.go", 3, "reasoning"), "production_behavior", "current_impact", "Users see a 500.", "")
	out := RenderInline(fd, "prism-only", "https://prism.example/go/agent?o=acme&r=example&n=7")
	if !strings.Contains(out, "```text\nPRism finding on a.go:3 in acme/example#7: Users see a 500.") {
		t.Fatalf("details must include a copyable plain prompt:\n%s", out)
	}
	if strings.Index(out, "```text") < strings.Index(out, "<details>") {
		t.Fatalf("the plain prompt belongs inside the folded details")
	}
}

func TestMergeConfidence_AnyMediumCostsAPoint(t *testing.T) {
	if got := MergeConfidence(0, 1, false); got != 4 {
		t.Fatalf("one medium = 4, got %d", got)
	}
	if got := MergeConfidence(0, 3, false); got != 3 {
		t.Fatalf("three mediums = 3, got %d", got)
	}
	if got := MergeConfidence(0, 0, false); got != 5 {
		t.Fatalf("clean = 5, got %d", got)
	}
}

func TestRenderSummary_FooterReportsScopeAndDashboardNotes(t *testing.T) {
	r := Round{Owner: "a", Repo: "b", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		Commentable: map[string]map[int]bool{"a.go": {1: true}, "b.go": {1: true}, "c.go": {}},
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "n"),
			withContract(f("l1", "low", "a.go", 1, "x"), "test_quality", "no_user_impact", "No impact.", ""),
			withContract(f("m1", "medium", "a.go", 1, "x"), "production_behavior", "unknown", "Maybe.", ""),
		}}
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if !strings.Contains(out, "No blocking findings.") || !strings.Contains(out, "3 changed files · 2 notes on the dashboard") {
		t.Fatalf("clean summary must say what it covered:\n%s", out)
	}
}

// Findings dropped by a stricter bar are not "fixed"; only findings that left
// the review entirely are.
func TestDiff_PolicyDropIsNotCountedAsFixed(t *testing.T) {
	r := Round{Owner: "a", Repo: "b", Number: 1, HeadSHA: "abc1234", RoundNumber: 2,
		Findings: []payload.Finding{
			withContract(f("keep", "critical", "a.go", 1, "x"), "production_behavior", "current_impact", "Boom.", ""),
			withContract(f("softened", "low", "a.go", 5, "x"), "production_behavior", "current_impact", "Meh.", ""),
		},
		Previous: []db.PublishedFinding{
			{Kind: db.PublishedKindFinding, Fingerprint: "keep", State: db.PublishedStateOpen},
			{Kind: db.PublishedKindAnnotation, Fingerprint: "softened", State: db.PublishedStateOpen},
			{Kind: db.PublishedKindFinding, Fingerprint: "gone", State: db.PublishedStateOpen},
		}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if !strings.Contains(out, "**Since last review:** 0 new · 1 still open · 1 fixed") {
		t.Fatalf("only 'gone' is fixed; 'softened' is merely below the bar now:\n%s", out)
	}
}

func TestRenderSummary_FullReportLinkIsFirstClassEvenWhenClean(t *testing.T) {
	r := Round{Owner: "a", Repo: "b", Number: 1, HeadSHA: "abc1234", RoundNumber: 1,
		DashboardURL: "https://prism.example/api/review/a/b/1?format=html",
		Findings:     []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "n")}}
	out := RenderSummary(r, Selection{})
	if !strings.Contains(out, "[Full report](https://prism.example/api/review/a/b/1?format=html)") {
		t.Fatalf("summary must carry a first-class Full report link:\n%s", out)
	}
	if strings.Contains(out, ">dashboard</a>") {
		t.Fatalf("the old dashboard sub-link should be gone:\n%s", out)
	}
}
