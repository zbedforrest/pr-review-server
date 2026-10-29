package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestRenderInlineShowsTheRewriteAndKeepsTheFindingInDetails(t *testing.T) {
	fd := f("a.go:0:abc", "medium", "pkg/a.go", 3, "The guard around `get_all_agents()` is gone, so a Redis error escapes before the stats row is written. Keep the guard.")
	fd.FindingContract = &types.FindingContract{
		CurrentImpact:        "A Redis hiccup aborts the scheduler run before its stats row is written.",
		Uncertainty:          "How often the read fails in production is unknown.",
		Headline:             "A Redis hiccup while reading agent info now aborts the scheduler run before its stats row is written",
		Falsifiability:       "falsifiable",
		FalsifiableCondition: strp("Patch get_all_agents to raise"),
		ExpectedObservable:   strp("no stats row"),
	}
	fd.FindingContractStatus = "valid"
	fd.Display = &types.Display{Title: "Stats row lost on Redis error", Body: "Removing the guard lets a Redis error escape `_handle_content_tasks`. The run's stats row is never written. Keep the guard around `get_all_agents()`."}
	out := RenderInline(fd, "prism-only", "https://prism.example/go/agent?o=acme&r=example&n=7", "", "")

	visible, details, ok := strings.Cut(out, "<details>")
	if !ok {
		t.Fatalf("no details block\n%s", out)
	}
	for _, s := range []string{"**[MEDIUM] Stats row lost on Redis error**", "Keep the guard around `get_all_agents()`."} {
		if !strings.Contains(visible, s) {
			t.Errorf("visible text missing %q\n%s", s, out)
		}
	}
	for _, s := range []string{"How often the read fails", "A Redis hiccup aborts", "Behavior change"} {
		if strings.Contains(visible, s) {
			t.Errorf("visible text should not carry %q\n%s", s, out)
		}
	}
	for _, s := range []string{"A Redis hiccup aborts the scheduler run", "a Redis error escapes before the stats row", "How often the read fails in production is unknown.", "**How to verify:**", "Agent prompt:"} {
		if !strings.Contains(details, s) {
			t.Errorf("details missing %q\n%s", s, out)
		}
	}
	if !strings.Contains(out, "Fix with agent") || !strings.HasPrefix(out, FindingMarker("a.go:0:abc")) {
		t.Errorf("marker and footer must stay\n%s", out)
	}
}

func TestRenderInlineKeepsASuggestionVisibleWithTheRewrite(t *testing.T) {
	fd := f("abc", "medium", "a.go", 3, "Off by one.\n\n```suggestion\nfor i := 0; i < n; i++ {\n```")
	fd.Display = &types.Display{Title: "Loop skips the last item", Body: "The bound is `n-1`. Use `n`."}
	visible, _, _ := strings.Cut(RenderInline(fd, "prism-only", "", "", ""), "<details>")
	if !strings.Contains(visible, "```suggestion\nfor i := 0; i < n; i++ {\n```") {
		t.Errorf("suggestion must stay visible\n%s", visible)
	}
}

func TestRenderInlineIgnoresAnEmptyRewrite(t *testing.T) {
	fd := f("abc", "medium", "a.go", 3, "Nil deref when cfg is missing. The loader returns nil.")
	fd.Display = &types.Display{Title: " ", Body: "Body."}
	if out := RenderInline(fd, "prism-only", "", "", ""); !strings.Contains(out, "Nil deref when cfg is missing.") {
		t.Errorf("an empty rewrite must fall back to the default form\n%s", out)
	}
}

func TestSummaryTextUsesTheRewriteTitle(t *testing.T) {
	fd := f("abc", "medium", "a.go", 3, "Nil deref when cfg is missing. The loader returns nil.")
	fd.Display = &types.Display{Title: "Missing config crashes startup", Body: "Body."}
	if got := summaryText(fd, false); got != "Missing config crashes startup" {
		t.Errorf("summaryText = %q", got)
	}
}
