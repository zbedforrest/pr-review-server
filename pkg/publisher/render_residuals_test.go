package publisher

import (
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/textcut"
)

// The two folded notes of the 2026-10-14 smoke re-audit, anonymised: a
// description-drift note that quotes the PR body first and states the
// problem after "However", and a latent hazard whose problem sits in the
// "but" clause of a sentence too long for the bullet.
const (
	smokeQuoteNote = "The PR description states: 'Room-scoped handling (the session check, with `leave_room` exempt) now applies to `toast_overlay` too.' However, the implementation deliberately does the opposite: the `toast_overlay` case has no `leave_room` exemption, as acknowledged in the in-code comment (lines 494-496). The PR description should be corrected to accurately reflect that `toast_overlay` gets no `leave_room` exemption, unlike `chat_banner` and `push_banner`."
	smokeQuoteWant = "The implementation deliberately does the opposite: the `toast_overlay` case has no `leave_room` exemption, as acknowledged in the in-code comment (lines 494-496)"
	smokeGuardNote = "The current guard `selectSessionDossierRoom(getState()) !== room` only rejects when the session has moved off `room`, but `leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document). This means `toast_overlay` will still fire on `leave_room` when the session hasn't moved yet, contradicting the stated intent."
	smokeGuardWant = "[…] but `leave_room` responses can arrive while the session still reads the departed room (as the existing stale-response test and comment on `chat_banner_notification` document)"
)

func TestFoldedNotes_SmokeAuditNotesStateTheProblem(t *testing.T) {
	first := textcut.FirstSentence(smokeGuardNote)
	if n := len([]rune(first)); n <= bulletMaxRunes {
		t.Fatalf("fixture sentence is %d runes, within the bullet limit", n)
	}
	r := Round{Owner: "acme", Repo: "example", HeadSHA: "abc1234"}
	for name, tc := range map[string]struct {
		finding payload.Finding
		want    string
	}{
		"quoted description then However": {
			finding: lowNote("a", "web/src/store/bridge.ts", 493, "no_user_impact", "No runtime impact; the PR description contradicts the implemented leave_room behavior and could mislead reviewers or QA.", smokeQuoteNote),
			want:    smokeQuoteWant,
		},
		"only rejects when, but": {
			finding: lowNote("b", "web/src/store/bridge.ts", 493, "future_condition_only", "None today: `pto` is off by default and no component renders the overlay, so no overlay can appear over a torn-down player yet.", smokeGuardNote),
			want:    smokeGuardWant,
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

func TestIsContext_ClosingQuoteOfAnyShape(t *testing.T) {
	for _, s := range []string{`He asked "why not."`, "He asked “why not.”", "The body says 'done.'", "The body says ‘done.’"} {
		if !IsContext(s) {
			t.Fatalf("%q is context", s)
		}
	}
	if IsContext("The cache is never invalidated") {
		t.Fatal("a plain statement is not context")
	}
}
