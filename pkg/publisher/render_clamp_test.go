package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

// The audited cuts of 2026-10-13, anonymised: a folded note cut mid-sentence
// ("curly vs ...") and an inline headline ending in "...".
func TestClampText_CutsAtASentenceBoundaryWithTheMarker(t *testing.T) {
	s := "The folded note explains the curly quote mismatch in full. Then it goes on to describe the straight quote fallback in a second sentence that runs long."
	if got := clampText(s, 90, false); got != "The folded note explains the curly quote mismatch in full. […]" {
		t.Fatalf("got %q", got)
	}
	if got := clampText(s, 400, false); got != s {
		t.Fatalf("a text within the limit is untouched: %q", got)
	}
}

func TestClampText_FallsBackToAClauseThenAWord(t *testing.T) {
	clause := "The retry helper re-enters the backoff loop with the original deadline, so a slow upstream is retried past the caller's budget"
	if got := clampText(clause, 90, false); got != "The retry helper re-enters the backoff loop with the original deadline […]" {
		t.Fatalf("clause cut = %q", got)
	}
	words := strings.Repeat("alpha beta ", 20)
	got := clampText(words, 40, false)
	if !strings.HasSuffix(got, " […]") || len([]rune(got)) > 40 || strings.Contains(got, "alph […]") {
		t.Fatalf("word cut = %q", got)
	}
}

func TestClampText_NeverCutsInsideACodeSpan(t *testing.T) {
	s := "The fallback branch `else { renderLegacy(); return; }` repeats the body of the `if` branch above it instead of sharing one helper"
	got := clampText(s, 45, false)
	if strings.Count(got, "`")%2 != 0 || strings.Contains(got, "`else { renderLegacy") {
		t.Fatalf("got %q", got)
	}
	if got != "The fallback branch […]" {
		t.Fatalf("got %q", got)
	}
}

func TestClampText_LegacyRestoresTheWordCut(t *testing.T) {
	s := strings.Repeat("word ", 50)
	if got := clampText(s, 40, true); !strings.HasSuffix(got, "...") || strings.Contains(got, "[…]") {
		t.Fatalf("got %q", got)
	}
}

func TestMarkedBullet_CutsALongTitleAtAClauseWithTheMarker(t *testing.T) {
	long := "A request to the bare path falls through to the catch-all route and renders the marketing page with a 200 instead of the configured redirect, which the uptime check does not notice because it only reads the status code and never the body."
	if n := len([]rune(long)); n <= bulletMaxRunes {
		t.Fatalf("fixture impact is %d runes", n)
	}
	fd := withContract(f("x", "low", "a.go", 1, "Body."), "production_behavior", "current_impact", long, "")
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	out := r.markedBullet(fd, "")
	if !strings.Contains(out, "which the uptime check does not notice […]"+bulletWhereSep) {
		t.Fatalf("bullet:\n%s", out)
	}
	r.LegacyClamp = true
	if out := r.markedBullet(fd, ""); !strings.Contains(out, "..."+bulletWhereSep) || strings.Contains(out, "[…]") {
		t.Fatalf("legacy bullet:\n%s", out)
	}
}

func TestRenderInline_RendersAWholeHeadlineOver90Runes(t *testing.T) {
	head := "Desktop and mobile both render the denied state with two stacked paragraphs, so the prompt repeats the denial message"
	if n := len([]rune(head)); n <= 90 || n > 140 {
		t.Fatalf("fixture headline is %d runes", n)
	}
	c := &types.FindingContract{Headline: head}
	types.NormalizeFindingContract(c)
	if c.Headline != head {
		t.Fatalf("the contract keeps a headline under %d runes whole: %q", 140, c.Headline)
	}
	fd := withContract(f("x", "medium", "a.tsx", 100, "Body."), "production_behavior", "current_impact", "The prompt repeats the denial message.", "")
	fd.FindingContract.Headline = head
	out := RenderInline(fd, "prism-only", "", "", "")
	if !strings.Contains(out, "Behavior change · "+head+"**") || strings.Contains(out, "...") {
		t.Fatalf("inline:\n%s", out)
	}
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	if bullet := r.markedBullet(fd, ""); !strings.Contains(bullet, head+bulletWhereSep) {
		t.Fatalf("bullet:\n%s", bullet)
	}
}

func TestRenderInline_CutHeadlineEndsWithTheMarkerNotDots(t *testing.T) {
	head := "The pagination cursor is rebuilt from the response body so a page that repeats an id loops forever on the client side, which keeps the spinner on screen and the request queue growing until the tab is closed"
	c := &types.FindingContract{Headline: head}
	types.NormalizeFindingContract(c)
	fd := withContract(f("x", "medium", "a.go", 1, "Body."), "production_behavior", "current_impact", "Clients loop forever.", "")
	fd.FindingContract.Headline = c.Headline
	out := RenderInline(fd, "prism-only", "", "", "")
	if !strings.Contains(out, "[…]**") || strings.Contains(out, "...**") {
		t.Fatalf("inline:\n%s", out)
	}
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	if bullet := r.markedBullet(fd, ""); !strings.Contains(bullet, "[…]"+bulletWhereSep) {
		t.Fatalf("the bullet keeps the contract's cut marker:\n%s", bullet)
	}
}

func TestRenderInline_LegacyClampShowsTheImpactUnderACutTitle(t *testing.T) {
	impact := "Desktop and mobile both render the denied state with two stacked paragraphs, so the permission prompt repeats the denial message twice."
	if n := len([]rune(impact)); n <= 110 || n > 140 {
		t.Fatalf("fixture impact is %d runes", n)
	}
	fd := withContract(f("x", "medium", "a.go", 1, "Body."), "production_behavior", "current_impact", impact, "")
	if out := renderInline(fd, "", "", "", "", false, true); strings.Count(out, "denial message twice") != 1 {
		t.Fatalf("legacy clamp cuts the title, so the sentence must show below once:\n%s", out)
	}
	if out := renderInline(fd, "", "", "", "", false, false); strings.Count(out, "denial message twice") != 1 {
		t.Fatalf("a whole title is not repeated:\n%s", out)
	}
}
