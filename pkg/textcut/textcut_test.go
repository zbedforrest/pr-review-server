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

// The two folded notes of the 2026-10-14 smoke re-audit, anonymised: one
// cut after "However" with the problem left out, one cut before the "but"
// clause that states the problem.
const (
	smokeQuoteNote   = "The PR description states: 'Room-scoped handling (the session check, with `leave_room` exempt) now applies to `toast_overlay` too.' However, the implementation deliberately does the opposite: the `toast_overlay` case has no `leave_room` exemption, as acknowledged in the in-code comment (lines 494-496)."
	smokeGuardNote   = "The current guard `selectSessionDossierRoom(getState()) !== room` only rejects when the session has moved off `room`, but `leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document)"
	smokeGuardWant   = "[…] but `leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document)"
	smokeQuoteWant   = "The PR description states: 'Room-scoped handling (the session check, with `leave_room` exempt) now applies to `toast_overlay` too.'"
	smokeNoteMaxRune = 200
)

func TestFirstSentence_EndsAfterAClosingQuote(t *testing.T) {
	if got := FirstSentence(smokeQuoteNote); got != smokeQuoteWant {
		t.Fatalf("got %q", got)
	}
	if got := FirstSentence(`He wrote "stop." Then nothing.`); got != `He wrote "stop."` {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_KeepsTheProblemClauseOverTheLeadingContext(t *testing.T) {
	if got := ClampWith(smokeGuardNote, smokeNoteMaxRune, Options{KeepProblemClause: true}); got != smokeGuardWant {
		t.Fatalf("got  %q\nwant %q", got, smokeGuardWant)
	}
	s := "The helper returns the cached value whenever the key is present, but the cache is never invalidated when the row is deleted, so a deleted row is served until the process restarts"
	got := ClampWith(s, 120, Options{KeepProblemClause: true})
	if !strings.HasPrefix(got, "[…] but the cache is never invalidated") || len([]rune(got)) > 120 {
		t.Fatalf("got %q", got)
	}
	if got := Clamp(smokeGuardNote, smokeNoteMaxRune); strings.HasPrefix(got, "[…]") {
		t.Fatalf("without the option the cut stays on the right: %q", got)
	}
	if got := ClampWith(smokeGuardNote, 30, Options{KeepProblemClause: true}); strings.HasPrefix(got, "[…]") || len([]rune(got)) > 30 {
		t.Fatalf("got %q", got)
	}
}

func TestClamp_NeverEndsOnAConnective(t *testing.T) {
	connectives := map[string]bool{"however": true, "but": true, "only": true, "when": true, "unless": true, "so": true, "that": true, "which": true, "because": true, "and": true, "or": true}
	for _, s := range []string{smokeQuoteNote, smokeGuardNote,
		"The retry helper honours the deadline only when the first attempt fails fast, however, every later attempt re-enters the backoff loop with a fresh deadline"} {
		for max := 30; max < len([]rune(s)); max++ {
			got := ClampWith(s, max, Options{KeepProblemClause: true})
			if n := len([]rune(got)); n > max {
				t.Fatalf("max %d: %d runes: %q", max, n, got)
			}
			text := strings.TrimSuffix(strings.TrimSpace(got), Marker)
			text = strings.TrimRight(text, " ,;:`)]\"'”")
			words := strings.Fields(text)
			if len(words) == 0 {
				continue
			}
			if last := strings.ToLower(strings.Trim(words[len(words)-1], "`,;:.")); connectives[last] {
				t.Fatalf("max %d: cut ends on %q: %q", max, last, got)
			}
		}
	}
}

func TestClamp_KeepsTheLeadingClauseWhenItStatesTheProblem(t *testing.T) {
	statesProblem := func(s string) bool { return strings.Contains(s, "drops") }
	for name, tc := range map[string]struct {
		text, wantPrefix string
		max              int
	}{
		"tail opens with a qualifier": {
			text:       "Every request takes a connection from the pool and never returns it to the pool, so the pool is exhausted after the first hundred requests, but only when `KEEPALIVE` is off, which is the default in the deployed config",
			wantPrefix: "Every request takes a connection",
			max:        140,
		},
		"tail opens with an anaphor": {
			text:       "The desktop denied state hides the retry button whenever `isMicPermissionDenied` is true, but this flag is set from `isMicBlocked()`, whose behavior on browsers that cannot query mic permission treats a dismissed prompt as a block",
			wantPrefix: "The desktop denied state hides",
			max:        140,
		},
		"head states the defect": {
			text:       "The migration drops `legacy_id` before the backfill has copied the last batch of rows into the new column, but the job is retried nightly and reads the same column",
			wantPrefix: "The migration drops",
			max:        120,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ClampWith(tc.text, tc.max, Options{KeepProblemClause: true, HeadStatesProblem: statesProblem})
			if !strings.HasPrefix(got, tc.wantPrefix) || len([]rune(got)) > tc.max {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestClamp_PrefersTheRightCutWhenTheTailMustBeCutShorter(t *testing.T) {
	s := "The formatter caches the first locale it sees for the lifetime of the process, but the result is rendered per locale by the template layer, which re-reads the cache on every request from a different region"
	got := ClampWith(s, 90, Options{KeepProblemClause: true})
	if strings.HasPrefix(got, "[…]") && strings.HasSuffix(got, Marker) && len([]rune(got)) < 60 {
		t.Fatalf("cut at both ends: %q", got)
	}
	if len([]rune(got)) > 90 || len([]rune(got)) < 70 {
		t.Fatalf("got %d runes: %q", len([]rune(got)), got)
	}
}

func TestClamp_ProblemClauseSurvivesNonASCIICase(t *testing.T) {
	s := strings.Repeat("İ", 30) + " x, but a b c d e f g h i j k l m n o p q r s t u v w x y z"
	got := ClampWith(s, 60, Options{KeepProblemClause: true})
	if !strings.HasPrefix(got, "[…] but a b c") {
		t.Fatalf("got %q", got)
	}
}
