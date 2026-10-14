package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
)

// The four cuts the 2026-10-13 audit verification quoted, anonymised: a
// folded note cut inside an open parenthesis, one cut before the problem
// is stated, an inline headline cut mid-clause, and a note that lost its
// "yet" clause to a parenthetical that fit without it.
const (
	auditOpenParenNote     = `In the desktop denied state the component renders two stacked lines that both explain the same problem: the generic heading "We need permission to use your camera" (kept because ` + "`isDesktop`" + ` is true) followed by "Camera access is denied. …". The first line reads as a prompt rather than a blocked state.`
	auditOpenParenNoteWant = `In the desktop denied state the component renders two stacked lines that both explain the same problem: the generic heading "We need permission to use your camera" […]`
	auditFlagNote          = "The desktop denied state hides the retry button whenever `isMicPermissionDenied` is true, but this flag is set from `isMicBlocked()`, whose behavior on browsers that cannot query mic permission (older Firefox, Safari) treats a dismissed prompt as a block. On those browsers a user is told access is denied."
	auditFlagNoteWant      = "[…] but this flag is set from `isMicBlocked()`, whose behavior on browsers that cannot query mic permission (older Firefox, Safari) treats a dismissed prompt as a block"
	auditImpactHeadline    = "Mobile users who dismiss the browser prompt now see the prompt copy with a retry button instead of the blocked copy the desktop-only PR description implies is unchanged."
	auditYetNote           = "Registering CollectionsCarousel (a non-roomlist carousel class) means the featured-carousel path can now select it via ?c=collections, yet only ListBasedCarousel classes have a roomlist consumer contract. The template advertises a dedicated view."
	auditYetNoteWant       = "Registering CollectionsCarousel […] means the featured-carousel path can now select it via ?c=collections, yet only ListBasedCarousel classes have a roomlist consumer contract"
)

func lowNote(id, file string, line int, materiality, impact, comment string) payload.Finding {
	kind := "design_opinion"
	if materiality == "future_condition_only" {
		kind = "latent_hazard"
	}
	fd := withContract(f(id, "low", file, line, comment), kind, materiality, impact, "")
	fd.FindingContract.Falsifiability = "not_falsifiable"
	fd.FindingContract.FalsifiableCondition, fd.FindingContract.ExpectedObservable = nil, nil
	return fd
}

func TestFoldedNotes_AuditedCutsReadAsOneBalancedProblemStatement(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	for name, tc := range map[string]struct {
		finding payload.Finding
		want    string
	}{
		"open parenthesis": {
			finding: lowNote("a", "web/src/Preview.tsx", 12, "no_user_impact", "Users see a redundant heading plus denial message; no functional impact.", auditOpenParenNote),
			want:    auditOpenParenNoteWant,
		},
		"flag set from helper": {
			finding: lowNote("b", "web/src/Preview.tsx", 40, "future_condition_only", "None on browsers that can query mic permission; on browsers where the query throws, the new desktop UI can drop the retry button after a prompt dismissal.", auditFlagNote),
			want:    auditFlagNoteWant,
		},
		"parenthetical dropped keeps the yet clause": {
			finding: lowNote("c", "app/views/discovery.py", 16, "future_condition_only", "Normal users see no change; the page only breaks if a request supplies ?c=collections.", auditYetNote),
			want:    auditYetNoteWant,
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := r.bullet(tc.finding)
			if !strings.Contains(out, tc.want+bulletWhereSep) {
				t.Fatalf("bullet:\n%s\nwant text:\n%s", out, tc.want)
			}
		})
	}
}

func TestFoldedNote_UsesTheImpactFirstSentenceOverTheBody(t *testing.T) {
	impact := "Mobile users who dismiss the browser prompt now see the prompt copy with a retry button. The blocked copy the PR description implies is unchanged no longer renders on mobile."
	fd := withContract(f("x", "low", "web/src/hooks/usePermissions.ts", 20, auditFlagNote), "production_behavior", "current_impact", impact, "")
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	out := r.bullet(fd)
	if !strings.Contains(out, "Mobile users who dismiss the browser prompt now see the prompt copy with a retry button"+bulletWhereSep) {
		t.Fatalf("bullet:\n%s", out)
	}
	if strings.Contains(out, "[…]") {
		t.Fatalf("a whole first sentence is a title, not a cut:\n%s", out)
	}
}

func TestRenderInline_WholeImpactSentenceIsTheHeadline(t *testing.T) {
	if n := len([]rune(auditImpactHeadline)); n <= headlineMaxRunes || n > bulletMaxRunes {
		t.Fatalf("fixture impact is %d runes", n)
	}
	fd := withContract(f("x", "medium", "web/src/hooks/usePermissions.ts", 20, "Body of the comment."), "description_drift", "current_impact", auditImpactHeadline, "")
	out := RenderInline(fd, "prism-only", "", "", "")
	if !strings.Contains(out, "Description drift · "+strings.TrimSuffix(auditImpactHeadline, ".")+"**") {
		t.Fatalf("inline:\n%s", out)
	}
	if strings.Contains(out, "[…]") || strings.Count(out, "desktop-only PR description") != 1 {
		t.Fatalf("the sentence renders whole, once:\n%s", out)
	}
}

func TestClampText_NeverEndsInsideAnOpenPair(t *testing.T) {
	for name, s := range map[string]string{
		"parenthesis":  "The exporter writes the manifest (the one under the build directory, not the cached copy from the previous run) and then uploads it",
		"brackets":     "The exporter writes the manifest [see the build notes for the directory layout and the cache rules] and then uploads it to the bucket",
		"straight":     `The exporter logs "manifest written to the build directory and uploaded to the bucket" before the upload has started at all`,
		"curly":        "The exporter logs “manifest written to the build directory and uploaded to the bucket” before the upload has started at all",
		"code span":    "The exporter calls `upload(manifest, bucket, retries=3, timeout=30, verify=False)` before the manifest is flushed to disk",
		"nested pairs": `The exporter (which logs "manifest written (to the build directory)" first) uploads before the flush completes on slow disks`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, max := range []int{40, 60, 80, 100} {
				got := clampText(s, max, false)
				if len([]rune(got)) > max+1 {
					t.Fatalf("max %d: %d runes: %q", max, len([]rune(got)), got)
				}
				if !balancedPairs(got) {
					t.Fatalf("max %d: unbalanced cut %q", max, got)
				}
				if !strings.Contains(got, "[…]") {
					t.Fatalf("max %d: no marker in %q", max, got)
				}
			}
		})
	}
}

func TestClampText_ClosesThePairWhenNoBalancedCutExists(t *testing.T) {
	s := `"A quoted sentence that opens at the very start and runs on well past any limit a bullet allows without ever closing the quote" is the title`
	got := clampText(s, 60, false)
	if !balancedPairs(got) || !strings.HasSuffix(got, `[…]"`) {
		t.Fatalf("got %q", got)
	}
}

func TestClampText_DropsAParentheticalToKeepTheConsequence(t *testing.T) {
	s := "The retry helper re-enters the backoff loop (the one in the client, not the server-side scheduler) with the original deadline, so a slow upstream is retried past the budget"
	want := "The retry helper re-enters the backoff loop […] with the original deadline, so a slow upstream is retried past the budget"
	if got := clampText(s, 130, false); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := clampText(s, 80, false); got != "The retry helper re-enters the backoff loop […]" {
		t.Fatalf("a consequence that does not fit even without the parenthetical falls back to the clause cut: %q", got)
	}
	code := "The helper calls `retry(fn, (a, b))` with the original deadline (set by the caller), so a slow upstream is retried past the budget"
	if got := clampText(code, 110, false); strings.Contains(got, "`retry(fn […]") || !strings.Contains(got, "`retry(fn, (a, b))`") {
		t.Fatalf("parentheses inside a code span are not a parenthetical: %q", got)
	}
}

func TestFirstSentence_SkipsAPeriodInsideAQuoteOrParenthesis(t *testing.T) {
	for in, want := range map[string]string{
		auditOpenParenNote: `In the desktop denied state the component renders two stacked lines that both explain the same problem: the generic heading "We need permission to use your camera" (kept because ` + "`isDesktop`" + ` is true) followed by "Camera access is denied. …".`,
		"The guard (see the note. It explains why) returns early. The rest is unchanged.": "The guard (see the note. It explains why) returns early.",
		"A stray (parenthesis never closes. The second sentence follows.":                 "A stray (parenthesis never closes.",
		"Plain first sentence. Second one.":                                               "Plain first sentence.",
	} {
		if got := firstSentence(in); got != want {
			t.Fatalf("firstSentence(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func balancedPairs(s string) bool {
	var stack []rune
	inCode := false
	for _, c := range s {
		if c == '`' {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		switch c {
		case '(', '[', '“':
			stack = append(stack, c)
		case ')', ']', '”':
			if len(stack) == 0 {
				return false
			}
			stack = stack[:len(stack)-1]
		case '"':
			if len(stack) > 0 && stack[len(stack)-1] == '"' {
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, c)
			}
		}
	}
	return len(stack) == 0 && !inCode
}
