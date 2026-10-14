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

func TestEscapeProseHTMLLeavesTildeFencesAlone(t *testing.T) {
	in := "Before <img>.\n~~~html\n<img src=\"x\">\n~~~\nAfter <br>.\n"
	want := "Before `<img>`.\n~~~html\n<img src=\"x\">\n~~~\nAfter `<br>`.\n"
	if got := EscapeProseHTML(in); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestFoldAfterAProblemTitleKeepsTheContextOnce(t *testing.T) {
	comment := "The PR correctly pairs `use_h2 = true` with `H2_ENABLED=true`. " +
		"However, the deploy workflow also derives both values from `DEPLOY_H2`. " +
		"If that variable is unset, a workflow deploy will revert the service. Confirm it is set."
	got := commentAfterTitle(comment)
	want := "The deploy workflow also derives both values from `DEPLOY_H2`. Confirm it is set."
	if got != want {
		t.Fatalf("fold = %q, want %q", got, want)
	}
}

func TestStatesProblemNeedsAWholeWordIf(t *testing.T) {
	if StatesProblem("The modified deploy path will also re-derive both values.") {
		t.Fatal("`modified` is not a conditional")
	}
}

func TestEscapeProseHTMLRespectsCodeSpansOnTrickyLines(t *testing.T) {
	cases := map[string]string{
		"emits `<img>` and a stray ` tick near <br>": "emits `<img>` and a stray &#96; tick near `<br>`",
		"wrap ``a <b> c`` and <i>":                   "wrap ``a <b> c`` and `<i>`",
		"`<img>` then <img> then `x`":                "`<img>` then `<img>` then `x`",
	}
	for in, want := range cases {
		if got := EscapeProseHTML(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestContextWithADiscourseWordDoesNotTitleThePraiseLedFinding(t *testing.T) {
	comment := "The PR correctly pairs A with B. However, the deploy workflow also only derives both values from DEPLOY_H2. " +
		"If it is unset, a deploy will revert the service."
	title, _ := FirstFindingSentence(comment)
	if !strings.HasPrefix(title, "If it is unset, a deploy will revert") {
		t.Fatalf("title = %q", title)
	}
}

func TestEscapeProseHTMLOnlyTreatsRealFencesAsCode(t *testing.T) {
	in := "```js``` is inline, then <img>.\n````\n```\n<b>\n````\nafter <i>\n"
	want := "```js``` is inline, then `<img>`.\n````\n```\n<b>\n````\nafter `<i>`\n"
	if got := EscapeProseHTML(in); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestEscapeProseHTMLCodesCommentsAndDeclarations(t *testing.T) {
	got := EscapeProseHTML("the template keeps an <!-- slot --> placeholder before <!DOCTYPE html>")
	if got != "the template keeps an `<!-- slot -->` placeholder before `<!DOCTYPE html>`" {
		t.Fatalf("got %q", got)
	}
}

func TestStatesProblemIgnoresNeutralDescriptions(t *testing.T) {
	for _, sentence := range []string{
		"The cache reuses the stale entries between runs.",
		"If the caller passes a name, the helper can look it up.",
		"The list may be empty on first load.",
	} {
		if StatesProblem(sentence) {
			t.Errorf("%q is description, not a problem", sentence)
		}
	}
}

func TestCommentTextStaysRawForClaimAnalysis(t *testing.T) {
	f := contractFinding("medium", "templates/header.html", 3, "The header no longer renders <img> for the small logo.", nil)
	if got := commentText(f); !strings.Contains(got, " <img> ") {
		t.Fatalf("analysis text was escaped: %q", got)
	}
}

func TestStrayBacktickInAnotherSegmentCannotReopenAnEscapedTag(t *testing.T) {
	got := EscapeProseHTML("a stray ` then `code` then <img> tag")
	if got != "a stray ` then `code&#96; then `<img>` tag" {
		t.Fatalf("got %q", got)
	}
}
