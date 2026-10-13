package textcut

import (
	"strings"
	"testing"
)

func TestClamp_WordCutDoesNotEndOnAFunctionWord(t *testing.T) {
	s := "Registering the collections carousel means the featured-carousel path can now select it via ?c=collections, yet only list-based carousel classes carry a roomlist contract"
	got := Clamp(s, 110)
	if !strings.HasSuffix(got, "can now select it […]") {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_DroppedTrailingParentheticalLeavesNoStrayPeriod(t *testing.T) {
	s := "The helper returns true whenever the permission cannot be queried (missing the permissions API, or the query throwing on an older browser)."
	if got := Clamp(s, 100); got != "The helper returns true whenever the permission cannot be queried […]" {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_ShortTextAndTinyLimit(t *testing.T) {
	if got := Clamp("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := Clamp("a text longer than the limit", 3); got != "[…]" {
		t.Fatalf("got %q", got)
	}
}

func TestFirstSentence_FallsBackToTheFirstEndWhenNoBalancedOneExists(t *testing.T) {
	if got := FirstSentence("A stray (parenthesis never closes. Second sentence."); got != "A stray (parenthesis never closes." {
		t.Fatalf("got %q", got)
	}
	if got := FirstSentence("No sentence end at all"); got != "No sentence end at all" {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_SentenceEndBeatsALaterClauseBoundary(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		max  int
		want string
	}{
		"clause after the sentence": {
			in:   "The fix works as described. It also changes behavior on the retry path, so the tests may need updates.",
			max:  70,
			want: "The fix works as described. […]",
		},
		"clause before and after the sentence": {
			in:   "The cache is rebuilt, so the first load is slow. The health check then times out, which marks the pod unready.",
			max:  80,
			want: "The cache is rebuilt, so the first load is slow. […]",
		},
		"sentence too early for the window": {
			in:   "Fixed. The retry helper re-enters the backoff loop with the original deadline, so a slow upstream is retried past the budget",
			max:  90,
			want: "Fixed. The retry helper re-enters the backoff loop with the original deadline […]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Clamp(tc.in, tc.max); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestClamp_ClosedPairNeverExceedsTheLimit(t *testing.T) {
	for _, s := range []string{
		`"` + strings.Repeat("quoted words run on ", 12),
		"(" + strings.Repeat("parenthetical words run on ", 12),
		`He said "the build (which ` + strings.Repeat("never ", 30) + "ends",
		"`" + strings.Repeat("x", 300),
	} {
		for _, max := range []int{20, 60, 140} {
			got := Clamp(s, max)
			if n := len([]rune(got)); n > max {
				t.Fatalf("max %d: %d runes: %q", max, n, got)
			}
			if !strings.Contains(got, "[…]") {
				t.Fatalf("max %d: no marker in %q", max, got)
			}
		}
	}
}
