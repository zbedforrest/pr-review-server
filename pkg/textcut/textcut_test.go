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
	smokeGuardWant   = "`leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document)"
	smokeGuardLegacy = "[…] but `leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document)"
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
	if got := ClampWith(smokeGuardNote, smokeNoteMaxRune, Options{KeepProblemClause: true, MarkDroppedLead: true}); got != smokeGuardLegacy {
		t.Fatalf("legacy: got  %q\nwant %q", got, smokeGuardLegacy)
	}
	s := "The helper returns the cached value whenever the key is present, but the cache is never invalidated when the row is deleted, so a deleted row is served until the process restarts"
	got := ClampWith(s, 120, Options{KeepProblemClause: true})
	if !strings.HasPrefix(got, "The cache is never invalidated") || len([]rune(got)) > 120 {
		t.Fatalf("got %q", got)
	}
	got = ClampWith(s, 120, Options{KeepProblemClause: true, MarkDroppedLead: true})
	if !strings.HasPrefix(got, "[…] but the cache is never invalidated") || len([]rune(got)) > 120 {
		t.Fatalf("legacy: got %q", got)
	}
	if got := Clamp(smokeGuardNote, smokeNoteMaxRune); strings.HasPrefix(got, "[…]") {
		t.Fatalf("without the option the cut stays on the right: %q", got)
	}
	for _, opts := range []Options{{KeepProblemClause: true}, {KeepProblemClause: true, MarkDroppedLead: true}} {
		if got := ClampWith(smokeGuardNote, 30, opts); strings.HasPrefix(got, "[…]") || len([]rune(got)) > 30 {
			t.Fatalf("got %q", got)
		}
	}
}

func TestClamp_NeverEndsOnAConnective(t *testing.T) {
	connectives := map[string]bool{"however": true, "but": true, "only": true, "when": true, "unless": true, "so": true, "that": true, "which": true, "because": true, "and": true, "or": true}
	for _, s := range []string{smokeQuoteNote, smokeGuardNote, auditLeadNote,
		"The retry helper honours the deadline only when the first attempt fails fast, however, every later attempt re-enters the backoff loop with a fresh deadline"} {
		for _, opts := range []Options{{KeepProblemClause: true}, {KeepProblemClause: true, MarkDroppedLead: true}} {
			for max := 30; max < len([]rune(s)); max++ {
				got := ClampWith(s, max, opts)
				if n := len([]rune(got)); n > max {
					t.Fatalf("max %d: %d runes: %q", max, n, got)
				}
				if !opts.MarkDroppedLead && OpensWithMarkerOrConnective(got) && got != strings.TrimSpace(Marker) {
					t.Fatalf("max %d: opens with a marker or connective: %q", max, got)
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
}

// The inline title of the 2026-10-14 annoyance audit, anonymised: the
// contrast sits inside the "Where" clause, so the problem is the main
// clause after it, and the title opened with "[…] but differ by".
const (
	auditLeadNote = "Where two enabled campaigns share a trigger and auth×device unit but differ by target_audience or assign_countries, the higher-priority one now holds the slot and the other's audience gets matched=false, whereas main fell through and served it"
	auditLeadWant = "The higher-priority one now holds the slot and the other's audience gets matched=false, whereas main fell through and served it"
)

func TestClamp_DroppedLeadStartsAtTheProblemClauseCapitalised(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		max  int
		want string
	}{
		"contrast inside an unclosed subordinate clause": {
			in:   auditLeadNote,
			max:  200,
			want: auditLeadWant,
		},
		"contrast after the context clause": {
			in:   "The helper returns the cached value whenever the key is present, but the cache is never invalidated when the row is deleted, so a deleted row is served until the process restarts",
			max:  130,
			want: "The cache is never invalidated when the row is deleted, so a deleted row is served until the process restarts",
		},
		"however clause": {
			in:   "The retry helper works for every caller in the service, but only when the budget is set; however, the second attempt ignores the deadline and re-enters the backoff loop with a fresh budget every time",
			max:  110,
			want: "The second attempt ignores the deadline and re-enters the backoff loop with a fresh budget every time",
		},
		"code span opens the clause": {
			in:   smokeGuardNote,
			max:  smokeNoteMaxRune,
			want: smokeGuardWant,
		},
		"identifier is not capitalised": {
			in:   "The fanout skip reads the flag from the request context on every call of the handler, but purchaser_id is read unconditionally before the guard runs and the row may be missing",
			max:  110,
			want: "purchaser_id is read unconditionally before the guard runs and the row may be missing",
		},
		"list comma inside the unclosed subordinate clause": {
			in:   "Where a, b and c share a trigger but differ by target_audience or assign_countries, the higher-priority one now holds the slot and the other is dropped",
			max:  120,
			want: "The higher-priority one now holds the slot and the other is dropped",
		},
		"participle after the contrast is not a main clause": {
			in:   "When the flag is off, the handler returns early but the cache entry is never invalidated by the sweeper, leaving the next caller with a deleted row until the restart",
			max:  120,
			want: "The cache entry is never invalidated by the sweeper, leaving the next caller with a deleted row until the restart",
		},
		"lowercase tool name is not capitalised": {
			in:   "The script runs on every CI job for the whole matrix with the lockfile checked in, but npm install fails on the pinned registry and the job retries forever",
			max:  110,
			want: "npm install fails on the pinned registry and the job retries forever",
		},
		"subordinate clause closed before the contrast": {
			in:   "When the flag is off, the handler returns early but the cache entry written just before the return is never invalidated by the sweeper and is served to the next caller",
			max:  120,
			want: "The cache entry written just before the return is never invalidated by the sweeper and is served to the next caller",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ClampWith(tc.in, tc.max, Options{KeepProblemClause: true})
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			if OpensWithMarkerOrConnective(got) {
				t.Fatalf("opens with a marker or connective: %q", got)
			}
		})
	}
	legacy := ClampWith(auditLeadNote, 200, Options{KeepProblemClause: true, MarkDroppedLead: true})
	if !strings.HasPrefix(legacy, "[…] but differ by target_audience") {
		t.Fatalf("legacy form: %q", legacy)
	}
}

func TestClamp_SkipsAClauseThatOpensWithAConnective(t *testing.T) {
	s := "The migration runs every night on the replica with the full table locked for the whole batch, but because the lock is taken before the batch size is read the job holds it for the entire run"
	got := ClampWith(s, 120, Options{KeepProblemClause: true})
	if !strings.HasPrefix(got, "The migration runs") || OpensWithMarkerOrConnective(got) {
		t.Fatalf("got %q", got)
	}
}

func TestStandAlone(t *testing.T) {
	for in, want := range map[string]string{
		"[…] but the cache is stale":           "The cache is stale",
		"[…] but don't implement it":           "Don't implement it",
		"[…] but free-approved gets x":         "Free-approved gets x",
		"[…] but x-ray_id is read":             "x-ray_id is read",
		"[…] however, the job re-runs":         "The job re-runs",
		"but `foo` is nil":                     "`foo` is nil",
		"the handler drops the row":            "The handler drops the row",
		"npm install fails on CI":              "npm install fails on CI",
		"isMicBlocked treats a prompt":         "isMicBlocked treats a prompt",
		"when the flag is off, X":              "When the flag is off, X",
		"[…]":                                  "[…]",
		"":                                     "",
		"The row is served":                    "The row is served",
		"So far the handler reads the flag":    "So far the handler reads the flag",
		"Nor does the sweeper clear it":        "Nor does the sweeper clear it",
		"However, the claim holds":             "However, the claim holds",
		"Yet another reader ignores it":        "Yet another reader ignores it",
		"rather than failing closed, it opens": "Rather than failing closed, it opens",
		"after the restart the row is served":  "After the restart the row is served",
		"[…] But the cache is stale":           "The cache is stale",
		"main fell through and served it":      "main fell through and served it",
	} {
		if got := StandAlone(in); got != want {
			t.Errorf("StandAlone(%q) = %q, want %q", in, got, want)
		}
	}
	for s, want := range map[string]bool{
		"[…] but x": true, "but x": true, "however, x": true, "when x": true, "only x": true,
		"But x": false, "The x": false, "`x` y": false, "": false, "x […] y": false,
	} {
		if got := OpensWithMarkerOrConnective(s); got != want {
			t.Errorf("OpensWithMarkerOrConnective(%q) = %v", s, got)
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
	for _, opts := range []Options{{KeepProblemClause: true}, {KeepProblemClause: true, MarkDroppedLead: true}} {
		got := ClampWith(s, 90, opts)
		if strings.HasPrefix(got, "[…]") && strings.HasSuffix(got, Marker) && len([]rune(got)) < 60 {
			t.Fatalf("cut at both ends: %q", got)
		}
		if len([]rune(got)) > 90 || len([]rune(got)) < 70 {
			t.Fatalf("got %d runes: %q", len([]rune(got)), got)
		}
	}
}

func TestClamp_ProblemClauseSurvivesNonASCIICase(t *testing.T) {
	s := strings.Repeat("İ", 30) + " x, but a b c d e f g h i j k l m n o p q r s t u v w x y z"
	if got := ClampWith(s, 60, Options{KeepProblemClause: true}); !strings.HasPrefix(got, "A b c") {
		t.Fatalf("got %q", got)
	}
	if got := ClampWith(s, 60, Options{KeepProblemClause: true, MarkDroppedLead: true}); !strings.HasPrefix(got, "[…] but a b c") {
		t.Fatalf("legacy: got %q", got)
	}
}

func TestClamp_WalksPastAJointWhoseTailCannotStandAlone(t *testing.T) {
	s := "The retry helper works for every caller in the service, but only when the budget is set; however, the second attempt ignores the deadline and re-enters the backoff loop with a fresh budget every time"
	if got := ClampWith(s, 110, Options{KeepProblemClause: true}); !strings.HasPrefix(got, "The second attempt ignores") || len([]rune(got)) > 110 {
		t.Fatalf("got %q", got)
	}
	if got := ClampWith(s, 110, Options{KeepProblemClause: true, MarkDroppedLead: true}); !strings.HasPrefix(got, "[…] however, the second attempt ignores") || len([]rune(got)) > 110 {
		t.Fatalf("legacy: got %q", got)
	}
}

func TestClamp_LastResortCutAvoidsAConnective(t *testing.T) {
	got := ClampWith("Writes to the "+strings.Repeat("x", 300), 40, Options{KeepProblemClause: true})
	if len([]rune(got)) > 40 || strings.HasPrefix(got, "Writes to the […]") || strings.HasPrefix(got, "Writes to […]") {
		t.Fatalf("got %q", got)
	}
}

func TestFirstSentence_ABracketIsNotASentenceEnd(t *testing.T) {
	if got := FirstSentence("The helper (e.g.) drops the row. Then nothing."); got != "The helper (e.g.) drops the row." {
		t.Fatalf("got %q", got)
	}
}
