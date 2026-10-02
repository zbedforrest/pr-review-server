package publisher

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func TestTitleSource_HeadlineThenAssertedImpactThenFirstSentence(t *testing.T) {
	impactOnly := withContract(f("a", "medium", "a.go", 1, "Retries never back off. More detail."), "production_behavior", "current_impact", "Every retry is rejected.", "")
	headlined := withContract(f("b", "medium", "a.go", 1, "Retries never back off."), "production_behavior", "current_impact", "Every retry is rejected.", "")
	headlined.FindingContract.Headline = "Retry loop never backs off"
	immaterial := withContract(f("c", "low", "a.go", 1, "Ordering within a group is unstable. Values are unchanged."), "production_behavior", "no_user_impact", "No user impact; output ordering is unstable.", "")
	unknown := withContract(f("d", "medium", "a.go", 1, "The callback binding is fragile."), "production_behavior", "unknown", "If the contract matches there is no impact.", "")
	plain := noContract("e", "medium", "a.go", 1, "**Nil deref** when cfg is missing. Details.")

	cases := []struct {
		name string
		f    payload.Finding
		want string
	}{
		{"asserted impact without headline is the impact", impactOnly, "Every retry is rejected"},
		{"headline wins over impact", headlined, "Retry loop never backs off"},
		{"nil impact falls back to the first sentence", immaterial, "Ordering within a group is unstable."},
		{"unknown materiality falls back to the first sentence", unknown, "The callback binding is fragile."},
		{"no contract is the first sentence", plain, "Nil deref when cfg is missing."},
	}
	for _, tc := range cases {
		if got := titleSource(tc.f); got != tc.want {
			t.Errorf("%s: titleSource = %q, want %q", tc.name, got, tc.want)
		}
		if got := summaryText(tc.f, false); got != tc.want {
			t.Errorf("%s: summaryText = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := summaryText(immaterial, true); got != "No user impact; output ordering is unstable." {
		t.Errorf("legacy summaryText keeps the impact sentence, got %q", got)
	}
}

func TestMarkedBullet_KeepsTheEllipsisOfAShortenedHeadline(t *testing.T) {
	fd := withContract(f("a", "medium", "a.go", 1, "Retries never back off."), "production_behavior", "current_impact", "Every retry is rejected.", "")
	fd.FindingContract.Headline = "Retry loop never backs off and keeps hammering the upstream until the..."
	r := Round{Owner: "a", Repo: "b", HeadSHA: "abc1234"}
	out := r.markedBullet(fd, "")
	if !strings.Contains(out, "until the... \u2014") || strings.Contains(out, "the.. ") {
		t.Fatalf("the ellipsis must survive the period trim:\n%s", out)
	}
}

func TestRenderInline_LongFirstSentenceTitleKeepsItsTailInTheFold(t *testing.T) {
	long := "The retry helper re-enters the backoff loop with the original deadline, so a slow upstream is retried past the caller's budget and the request outlives its context by several seconds. Then more."
	out := RenderInline(noContract("x", "medium", "a.go", 1, long), "prism-only", "", "")
	if !strings.Contains(out, "outlives its context by several seconds.") {
		t.Fatalf("a cut title must leave the whole sentence readable in the fold:\n%s", out)
	}
}

func TestRenderInline_HeadlineWithoutImpactKeepsTheWholeComment(t *testing.T) {
	fd := withContract(f("h", "medium", "a.go", 1, "The cache key omits the tenant. Two tenants share entries."), "latent_hazard", "unknown", "", "")
	fd.FindingContract.Headline = "Cache key omits the tenant"
	out := RenderInline(fd, "prism-only", "", "")
	if !strings.Contains(out, "**[MEDIUM] Cache key omits the tenant**") || !strings.Contains(out, "The cache key omits the tenant. Two tenants share entries.") {
		t.Fatalf("a contract headline must not swallow the comment's first sentence:\n%s", out)
	}
}

func TestRenderInline_ImmaterialFindingIsTitledByItsCommentAndShowsTheImpactBelow(t *testing.T) {
	fd := withContract(f("x", "low", "a.go", 1, "Ordering within a group is unstable. The grain column was not added to ORDER BY."), "production_behavior", "no_user_impact", "No user impact; values are unchanged.", "")
	out := RenderInline(fd, "prism-only", "", "")
	visible := out[:strings.Index(out, "<details>")]
	if !strings.Contains(visible, "**[LOW] Behavior change · Ordering within a group is unstable.**\n\nNo user impact; values are unchanged.") {
		t.Fatalf("title from the comment, impact sentence as the calibration line:\n%s", out)
	}
	if strings.Count(out, "Ordering within a group is unstable.") != 1 {
		t.Fatalf("the title sentence must not repeat in the folded reasoning:\n%s", out)
	}
	if !strings.Contains(out, "The grain column was not added to ORDER BY.") {
		t.Fatalf("the rest of the reasoning stays in the fold:\n%s", out)
	}
}

var (
	nilImpactBulletRe = regexp.MustCompile(`(?m)^- (?:\*\*\[[A-Z]+\]\*\*|<img[^>]*>) (?:\*\*[^*]+\*\* )?(None\b|No )`)
	inlineTitleLineRe = regexp.MustCompile(`(?m)^\*\*\[[A-Z]+\] (.+?)\*\*$`)
)

func storedRound(t *testing.T) Round {
	t.Helper()
	raw, err := os.ReadFile("testdata/stored_payload.json")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := payload.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	commentable := map[string]map[int]bool{}
	for _, x := range pl.Findings {
		if x.Line > 0 {
			if commentable[x.File] == nil {
				commentable[x.File] = map[int]bool{}
			}
			commentable[x.File][x.Line] = true
		}
	}
	return Round{Owner: pl.Owner, Repo: pl.Repo, Number: pl.PRNumber, HeadSHA: pl.CommitSHA, RoundNumber: 1,
		Findings: pl.Findings, Commentable: commentable, ShowUnverified: true}
}

func kindLabelSet() map[string]bool {
	out := map[string]bool{}
	for _, label := range kindLabels {
		out[label] = true
	}
	return out
}

// Re-rendering the stored payload fixture (anonymised copies of the shapes
// the audit flagged) produces no bullet that opens with a nil-impact phrase
// and no inline comment titled by its bare kind label; the legacy switch
// restores both defects, which is what proves the fixture covers them.
func TestRender_StoredPayloadHasNoNilImpactBulletsOrBareLabelTitles(t *testing.T) {
	r := storedRound(t)
	summary := RenderSummary(r, Selection{})
	if m := nilImpactBulletRe.FindAllString(summary, -1); len(m) != 0 {
		t.Errorf("summary still carries nil-impact bullets %q:\n%s", m, summary)
	}
	for _, want := range []string{
		"Session cookie issued before the CSRF token is bound",
		"When a selected component entry fails to render, the request returns 500 with no dispositions",
		"Verify progress callback contract before trusting results",
		"Relink changes the link without running the payee-side sync",
		"`region` is normalised to '' for non-domestic traffic while the upstream model keeps NULL",
		"The `ORDER BY 1 DESC, 2, 3, 4` was not extended to the new grain column",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing bullet %q:\n%s", want, summary)
		}
	}

	labels := kindLabelSet()
	for _, x := range r.Findings {
		if isNarrative(x) {
			continue
		}
		body := RenderInline(x, "prism-only", "", "")
		m := inlineTitleLineRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no title line in:\n%s", body)
		}
		if labels[m[1]] {
			t.Errorf("%s: bare kind label as title:\n%s", x.ID, body)
		}
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(m[1], strings.SplitN(m[1], " · ", 2)[0]+" · ")), "No") && x.FindingContract.Materiality != "current_impact" {
			t.Errorf("%s: nil-impact sentence as title:\n%s", x.ID, body)
		}
	}

	r.LegacyTitles = true
	legacy := RenderSummary(r, Selection{})
	if m := nilImpactBulletRe.FindAllString(legacy, -1); len(m) != 3 {
		t.Errorf("legacy titles must reproduce the three nil-impact bullets, got %d:\n%s", len(m), legacy)
	}
	long := r.Findings[4]
	if m := inlineTitleLineRe.FindStringSubmatch(renderInline(long, "prism-only", "", "", true)); m == nil || m[1] != "Behavior change" {
		t.Errorf("legacy titles must reproduce the bare kind label, got %v", m)
	}
}

func TestNormalizeFindingContract_ShortensALongHeadlineForTheRenderer(t *testing.T) {
	c := &types.FindingContract{Headline: "The pagination cursor is rebuilt from the response body so a page that repeats an id loops forever on the client side"}
	types.NormalizeFindingContract(c)
	if c.Headline == "" || !strings.HasSuffix(c.Headline, "...") || len([]rune(c.Headline)) > 90 {
		t.Fatalf("a long headline is shortened, not blanked: %q", c.Headline)
	}
	fd := withContract(f("x", "medium", "a.go", 1, "Body."), "production_behavior", "current_impact", "Clients loop forever.", "")
	fd.FindingContract.Headline = c.Headline
	if title := headline(fd, false); title != "Behavior change · "+c.Headline {
		t.Fatalf("title = %q", title)
	}
}
