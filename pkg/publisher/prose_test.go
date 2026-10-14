package publisher

import (
	"strings"
	"testing"
)

func TestEscapeProseHTMLCodesTagsOutsideCode(t *testing.T) {
	in := "The header renders an <img> with an empty src; `<img class=\"light\">` stays.\n" +
		"See <https://example.com/docs> and List<String> in a < b > c.\n" +
		"```suggestion\n<img src=\"{{ url }}\">\n```\n"
	want := "The header renders an `<img>` with an empty src; `<img class=\"light\">` stays.\n" +
		"See <https://example.com/docs> and List`<String>` in a < b > c.\n" +
		"```suggestion\n<img src=\"{{ url }}\">\n```\n"
	if got := EscapeProseHTML(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEscapeProseHTMLHonoursTheHygieneKillSwitch(t *testing.T) {
	t.Setenv(RenderHygieneEnv, "false")
	if got := EscapeProseHTML("an <img> tag"); got != "an <img> tag" {
		t.Fatalf("got %q", got)
	}
}

func TestInlineCommentKeepsTagsInProseAsText(t *testing.T) {
	c := falsifiableTestContract("behavior_change", "current_impact", "A featured logo without a small image renders an <img> with an empty src.", "")
	observable := "The rendered <img class=\"light\"> has an empty src."
	c.ExpectedObservable = &observable
	f := contractFinding("critical", "templates/header.html", 31, "For a featured logo without a small image, the header renders an <img> with an empty src, so no logo appears.", c)
	out := renderInline(f, "", "", "", "", false, false)
	if strings.Contains(strings.ReplaceAll(out, "`<img>`", ""), "an <img>") || strings.Contains(out, " <img class=\"light\"> ") {
		t.Fatalf("raw tags leaked into the comment:\n%s", out)
	}
	if !strings.Contains(out, "`<img>`") {
		t.Fatalf("tag not shown as code:\n%s", out)
	}
}

func TestTitleAfterPraiseStatesTheProblemNotTheContext(t *testing.T) {
	comment := "The PR correctly pairs `use_h2 = true` with `H2_ENABLED=true` so the service accepts h2c. " +
		"However, the deploy workflow also derives both values from the per-environment `DEPLOY_H2` variable. " +
		"If that variable is unset, a workflow deploy will revert the service to HTTP/1.1 and silently end the soak."
	title, ok := FirstFindingSentence(comment)
	if !ok || !strings.HasPrefix(title, "If that variable is unset, a workflow deploy will revert") {
		t.Fatalf("title = %q", title)
	}
}

func TestTitleAfterPraiseKeepsTheFirstSentenceWhenItStatesTheProblem(t *testing.T) {
	comment := "The new parser correctly handles quoted names. However, it drops the trailing tab, so names with spaces lose their lines. Consider a test."
	title, _ := FirstFindingSentence(comment)
	if !strings.HasPrefix(title, "It drops the trailing tab") {
		t.Fatalf("title = %q", title)
	}
}
