package publisher

import (
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// The audited cases of 2026-10-13, anonymised: a "Not seen" posted when the
// file changed outside the cited lines, two status notes one second apart on
// one thread about a point that was not the root's, and a return reopened
// for a finding about another subject.
const (
	previewFile = "web/src/components/drawer/PreviewPlayer/PreviewPlayer.tsx"
	previewText = "Desktop and mobile both render the denied state with two stacked paragraphs, so the permission prompt repeats the denial message."
	headerFile  = "mobile/templates/header.html"
	headerText  = "Mobile header renders the full-size logo instead of the small logo when the user has no featured logo."
	otherText   = "Mobile header passes the current_logo tag as a template variable, so the small logo src renders empty for every user."
)

func singleRound(head, file string, line int, sev, text string, subjects ...string) Round {
	fd := f(fid(file, line, text), sev, file, line, text)
	if len(subjects) > 0 {
		fd = withSubjects(fd, "production_behavior", subjects...)
	}
	return Round{
		Owner: "acme", Repo: "example", Number: 31, HeadSHA: head,
		Findings:    []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), fd},
		Commentable: map[string]map[int]bool{file: {line: true}},
	}
}

func absentRound(head, file string, line int) Round {
	r := singleRound(head, file, line, "medium", "unused")
	r.Findings = r.Findings[:1]
	return r
}

// hunked is a Round.Changes whose compare carries the file's hunks.
func hunked(file string, hunks ...Hunk) func(string) (ChangeSet, bool) {
	return func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{file: true}, Hunks: map[string][]Hunk{file: hunks}}, true
	}
}

func TestLedger_AbsentFindingOutsideTheChangedHunksStaysOpen(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))

	gone := absentRound("sha-2", previewFile, 100)
	gone.Changes = hunked(previewFile, Hunk{3, 7, 3, 6}, Hunk{113, 9, 112, 7})
	rep := publishThreads(t, gh, ledger, gone)
	if rep.Fixed != 0 || rep.StillOpen != 1 || rep.ThreadsResolved != 0 || rep.ThreadReplies != 0 {
		t.Fatalf("a change elsewhere in the file is not a fix: %+v", rep)
	}
	if got := gh.repliesTo(1001); len(got) != 0 {
		t.Fatalf("no note on an untouched point, got %q", got)
	}
	if row := ledger.rows[fid(previewFile, 100, previewText)]; row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-1" {
		t.Fatalf("row = %+v", row)
	}
	if !strings.Contains(gh.issueEdits[501], "0 new · 1 still open · 0 fixed") {
		t.Fatalf("summary:\n%s", gh.issueEdits[501])
	}
}

func TestLedger_AbsentFindingInsideAChangedHunkIsFixed(t *testing.T) {
	for name, hunk := range map[string]Hunk{"covering": {97, 9, 97, 7}, "margin above": {102, 4, 102, 4}, "margin below": {94, 4, 94, 2}} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeThreadGitHub(), newFakeLedger()
			publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
			gone := absentRound("sha-2", previewFile, 100)
			gone.Changes = hunked(previewFile, hunk)
			rep := publishThreads(t, gh, ledger, gone)
			if rep.Fixed != 1 || rep.ThreadsResolved != 1 {
				t.Fatalf("report = %+v", rep)
			}
			if got := gh.repliesTo(1001); len(got) != 1 || got[0] != "Not seen at sha-2." {
				t.Fatalf("replies = %q", got)
			}
		})
	}
}

func TestLedger_AbsentFindingWhoseFilePatchIsMissingStaysOpen(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
	gone := absentRound("sha-2", previewFile, 100)
	gone.Changes = func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{previewFile: true}, Hunks: map[string][]Hunk{"other.go": {{1, 2, 1, 2}}}}, true
	}
	if rep := publishThreads(t, gh, ledger, gone); rep.Fixed != 0 || rep.StillOpen != 1 {
		t.Fatalf("a file whose patch the compare did not carry cannot prove a fix: %+v", rep)
	}
}

func TestLedger_FixedByFileRestoresTheFileRule(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
	gone := absentRound("sha-2", previewFile, 100)
	gone.Changes = hunked(previewFile, Hunk{3, 7, 3, 6})
	pol := testPolicy()
	pol.FixedByFile = true
	if rep := publishThreadsWith(t, gh, ledger, gone, pol); rep.Fixed != 1 {
		t.Fatalf("the kill switch fixes on any change to the file: %+v", rep)
	}
}

func TestLedger_RowRemembersTheCitedLine(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
	if row := ledger.rows[fid(previewFile, 100, previewText)]; row.Line != 100 {
		t.Fatalf("row = %+v", row)
	}
	moved := singleRound("sha-2", previewFile, 104, "medium", previewText)
	moved.Changes = hunked(previewFile, Hunk{3, 7, 3, 11})
	publishThreads(t, gh, ledger, moved)
	if row := ledger.rows[fid(previewFile, 100, previewText)]; row.Line != 104 || row.LastSeenSHA != "sha-2" {
		t.Fatalf("the line follows the finding: %+v", row)
	}
}

// A row written before the Line column is anchored by the comment GitHub
// shows, else by its fingerprint bucket.
func TestLedger_LegacyRowIsAnchoredByItsCommentOrBucket(t *testing.T) {
	for name, tc := range map[string]struct {
		prior int
		hunk  Hunk
		fixed bool
	}{
		"comment line inside hunk":  {prior: 104, hunk: Hunk{90, 4, 101, 6}, fixed: true},
		"comment line outside hunk": {prior: 104, hunk: Hunk{3, 7, 3, 6}, fixed: false},
		"bucket inside hunk":        {prior: 0, hunk: Hunk{105, 4, 105, 4}, fixed: true},
		"bucket outside hunk":       {prior: 0, hunk: Hunk{120, 4, 120, 4}, fixed: false},
	} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeThreadGitHub(), newFakeLedger()
			publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
			id := fid(previewFile, 100, previewText)
			ledger.rows[id].Line = 0
			gone := absentRound("sha-2", previewFile, 100)
			gone.Changes = hunked(previewFile, tc.hunk)
			if tc.prior > 0 {
				gone.PriorComments = map[string]PriorComment{id: {Line: tc.prior, Text: previewText}}
			}
			rep := publishThreads(t, gh, ledger, gone)
			if (rep.Fixed == 1) != tc.fixed {
				t.Fatalf("fixed = %d, want %v: %+v", rep.Fixed, tc.fixed, rep)
			}
		})
	}
}

func TestThreads_ReturnWithSeverityChangeIsOneNote(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", headerFile, 25, "critical", headerText, "featured_mode_logos"))
	touched := hunked(headerFile, Hunk{22, 8, 22, 9})

	lowered := singleRound("sha-2", headerFile, 25, "low", headerText, "featured_mode_logos")
	lowered.Changes = touched
	publishThreads(t, gh, ledger, lowered)
	gone := absentRound("sha-3", headerFile, 25)
	gone.Changes = touched
	publishThreads(t, gh, ledger, gone)
	if got := gh.repliesTo(1001); len(got) != 2 || got[0] != "Severity critical to low at sha-2: the cited lines changed." || got[1] != "Not seen at sha-3." {
		t.Fatalf("precondition: replies = %q", got)
	}

	back := singleRound("sha-4", headerFile, 25, "medium", headerText, "featured_mode_logos")
	back.Changes = touched
	rep := publishThreads(t, gh, ledger, back)
	if rep.Reopened != 1 || rep.ThreadReplies != 1 || rep.ThreadsUnresolved != 1 {
		t.Fatalf("report = %+v", rep)
	}
	got := gh.repliesTo(1001)
	if len(got) != 3 || got[2] != "Back at sha-4; severity low to medium: the cited lines changed." {
		t.Fatalf("one note per thread per round, got %q", got)
	}
	if row := ledger.rows[fid(headerFile, 25, headerText)]; row.State != db.PublishedStateOpen || row.Severity != "medium" {
		t.Fatalf("row = %+v", row)
	}
}

func TestThreads_LegacyStatusNotesPostTwo(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	pol := testPolicy()
	pol.LegacyStatusNotes = true
	touched := hunked(headerFile, Hunk{22, 8, 22, 9})
	publishThreadsWith(t, gh, ledger, singleRound("sha-1", headerFile, 25, "critical", headerText, "featured_mode_logos"), pol)
	lowered := singleRound("sha-2", headerFile, 25, "low", headerText, "featured_mode_logos")
	lowered.Changes = touched
	publishThreadsWith(t, gh, ledger, lowered, pol)
	gone := absentRound("sha-3", headerFile, 25)
	gone.Changes = touched
	publishThreadsWith(t, gh, ledger, gone, pol)
	back := singleRound("sha-4", headerFile, 25, "medium", headerText, "featured_mode_logos")
	back.Changes = touched
	rep := publishThreadsWith(t, gh, ledger, back, pol)
	if got := gh.repliesTo(1001); rep.ThreadReplies != 2 || len(got) != 4 || got[2] != "Severity low to medium at sha-4: the cited lines changed." || got[3] != "Back at sha-4." {
		t.Fatalf("the kill switch restores two notes: %+v replies=%q", rep, got)
	}
}

func TestLedger_ReturnOnAnotherSubjectIsNotAReopen(t *testing.T) {
	for name, legacy := range map[string]bool{"status notes v2": false, "legacy": true} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeThreadGitHub(), newFakeLedger()
			pol := testPolicy()
			pol.LegacyStatusNotes = legacy
			publishThreadsWith(t, gh, ledger, singleRound("sha-1", headerFile, 25, "critical", headerText, "featured_mode_logos"), pol)
			rootID := fid(headerFile, 25, headerText)
			gone := absentRound("sha-2", headerFile, 25)
			gone.Changes = hunked(headerFile, Hunk{22, 8, 22, 9})
			publishThreadsWith(t, gh, ledger, gone, pol)
			if ledger.rows[rootID].State != db.PublishedStateFixed {
				t.Fatalf("precondition: %+v", ledger.rows[rootID])
			}

			other := singleRound("sha-3", headerFile, 27, "medium", otherText, "current_logo")
			other.Changes = hunked(headerFile, Hunk{22, 8, 22, 9})
			rep := publishThreadsWith(t, gh, ledger, other, pol)
			if legacy {
				if rep.Reopened != 1 || rep.InlinePosted != 0 {
					t.Fatalf("legacy reopens on the text match: %+v", rep)
				}
				return
			}
			if rep.Reopened != 0 || len(gh.repliesTo(1001)) != 1 {
				t.Fatalf("a finding about another subject is not a return: %+v replies=%q", rep, gh.repliesTo(1001))
			}
			if ledger.rows[rootID].State != db.PublishedStateFixed {
				t.Fatalf("the fixed row stays fixed: %+v", ledger.rows[rootID])
			}
			if rep.InlinePosted != 1 || len(gh.reviews) != 2 {
				t.Fatalf("the other point gets its own root: %+v reviews=%d", rep, len(gh.reviews))
			}
			if _, own := ledger.rows[fid(headerFile, 27, otherText)]; !own {
				t.Fatal("the other point gets its own row")
			}
		})
	}
}

func TestLedger_InheritedPathMakesNoTransition(t *testing.T) {
	file := "lib/logo.ts"
	text := "The compact logo helper ignores the white-label precedence and always returns the default asset."
	inherited := &payload.ReviewScope{MergeBase: "mb-1", InheritedPaths: []string{file}}
	touched := hunked(file, Hunk{38, 6, 38, 6})

	t.Run("no fix", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes, gone.Scope = touched, inherited
		if rep := publishThreads(t, gh, ledger, gone); rep.Fixed != 0 || rep.StillOpen != 0 || len(gh.repliesTo(1001)) != 0 {
			t.Fatalf("report = %+v replies=%q", rep, gh.repliesTo(1001))
		}
		if ledger.rows[fid(file, 40, text)].State != db.PublishedStateOpen {
			t.Fatalf("row = %+v", ledger.rows[fid(file, 40, text)])
		}
	})
	t.Run("kill switch", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes, gone.Scope = touched, inherited
		pol := testPolicy()
		pol.NoScopeGuard = true
		if rep := publishThreadsWith(t, gh, ledger, gone, pol); rep.Fixed != 1 {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("no return", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes = touched
		publishThreads(t, gh, ledger, gone)
		back := singleRound("sha-3", file, 40, "critical", text)
		back.Changes, back.Scope = touched, inherited
		rep := publishThreads(t, gh, ledger, back)
		if rep.Reopened != 0 || rep.InlinePosted != 0 || rep.StillOpen != 0 || len(gh.repliesTo(1001)) != 1 {
			t.Fatalf("report = %+v replies=%q", rep, gh.repliesTo(1001))
		}
		if ledger.rows[fid(file, 40, text)].State != db.PublishedStateFixed {
			t.Fatalf("row = %+v", ledger.rows[fid(file, 40, text)])
		}
	})
	t.Run("no severity change", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		lowered := singleRound("sha-2", file, 40, "low", text)
		lowered.Changes, lowered.Scope = touched, inherited
		publishThreads(t, gh, ledger, lowered)
		if row := ledger.rows[fid(file, 40, text)]; row.Severity != "critical" || len(gh.repliesTo(1001)) != 0 {
			t.Fatalf("row = %+v replies=%q", row, gh.repliesTo(1001))
		}
	})
}

func TestLedger_PathOutsideThePRDiffMakesNoReturn(t *testing.T) {
	file := "lib/logo.ts"
	text := "The compact logo helper ignores the white-label precedence and always returns the default asset."
	scoped := &payload.ReviewScope{MergeBase: "mb-1"}
	touched := hunked(file, Hunk{38, 6, 38, 6})

	t.Run("a hunk at the cited lines still fixes", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes, gone.Scope, gone.DiffFiles = touched, scoped, map[string]bool{"other.ts": true}
		if rep := publishThreads(t, gh, ledger, gone); rep.Fixed != 1 {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("no return or severity change", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes, gone.Scope = touched, scoped
		publishThreads(t, gh, ledger, gone)
		back := singleRound("sha-3", file, 40, "low", text)
		back.Changes, back.Scope, back.DiffFiles = touched, scoped, map[string]bool{"other.ts": true}
		if rep := publishThreads(t, gh, ledger, back); rep.Reopened != 0 || rep.InlinePosted != 0 || len(gh.repliesTo(1001)) != 1 {
			t.Fatalf("report = %+v replies=%q", rep, gh.repliesTo(1001))
		}
		if row := ledger.rows[fid(file, 40, text)]; row.State != db.PublishedStateFixed || row.Severity != "critical" {
			t.Fatalf("row = %+v", row)
		}
	})
	t.Run("the diff listing alone guards nothing", func(t *testing.T) {
		gh, ledger := newFakeThreadGitHub(), newFakeLedger()
		publishThreads(t, gh, ledger, singleRound("sha-1", file, 40, "critical", text))
		gone := absentRound("sha-2", file, 40)
		gone.Changes, gone.Scope = touched, scoped
		publishThreads(t, gh, ledger, gone)
		back := singleRound("sha-3", file, 40, "critical", text)
		back.Changes, back.DiffFiles = touched, map[string]bool{"other.ts": true}
		if rep := publishThreads(t, gh, ledger, back); rep.Reopened != 1 {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestLedger_MergeBaseDriftMakesNoTransition(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	first := singleRound("sha-1", previewFile, 100, "medium", previewText)
	first.Scope = &payload.ReviewScope{MergeBase: "mb-1"}
	publishThreads(t, gh, ledger, first)
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum.MergeBase != "mb-1" {
		t.Fatalf("summary row = %+v", sum)
	}

	drifted := absentRound("sha-2", previewFile, 100)
	drifted.Changes, drifted.Scope = hunked(previewFile, Hunk{97, 9, 97, 7}), &payload.ReviewScope{MergeBase: "mb-2"}
	if rep := publishThreads(t, gh, ledger, drifted); rep.Fixed != 0 || rep.StillOpen != 0 || len(gh.repliesTo(1001)) != 0 {
		t.Fatalf("a review against another base proves nothing: %+v", rep)
	}
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum.MergeBase != "mb-2" {
		t.Fatalf("summary row = %+v", sum)
	}

	settled := absentRound("sha-3", previewFile, 100)
	settled.Changes, settled.Scope = hunked(previewFile, Hunk{97, 9, 97, 7}), &payload.ReviewScope{MergeBase: "mb-2"}
	if rep := publishThreads(t, gh, ledger, settled); rep.Fixed != 1 {
		t.Fatalf("the next review on the same base fixes: %+v", rep)
	}

	unscoped := singleRound("sha-4", previewFile, 100, "medium", previewText)
	unscoped.Changes = hunked(previewFile, Hunk{97, 9, 97, 7})
	publishThreads(t, gh, ledger, unscoped)
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum.MergeBase != "" {
		t.Fatalf("a round without a scope leaves no merge-base: %+v", sum)
	}
	rebased := absentRound("sha-5", previewFile, 100)
	rebased.Changes, rebased.Scope = hunked(previewFile, Hunk{97, 9, 97, 7}), &payload.ReviewScope{MergeBase: "mb-3"}
	if rep := publishThreads(t, gh, ledger, rebased); rep.Fixed != 1 {
		t.Fatalf("no drift against a round that recorded no base: %+v", rep)
	}
}

func TestLedger_UnknownBaseMakesNoTransition(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	first := singleRound("sha-1", previewFile, 100, "medium", previewText)
	first.Scope = &payload.ReviewScope{MergeBase: "mb-1"}
	publishThreads(t, gh, ledger, first)

	unknown := absentRound("sha-2", previewFile, 100)
	unknown.Changes, unknown.Scope = hunked(previewFile, Hunk{97, 9, 97, 7}), &payload.ReviewScope{BaseFetchFailed: true}
	if rep := publishThreads(t, gh, ledger, unknown); rep.Fixed != 0 || len(gh.repliesTo(1001)) != 0 {
		t.Fatalf("a review whose base could not be fetched proves nothing: %+v", rep)
	}
	if sum := ledger.get(db.PublishedKindSummary, "summary"); sum.MergeBase != "mb-1" {
		t.Fatalf("an unknown base must not overwrite the recorded one: %+v", sum)
	}

	settled := absentRound("sha-3", previewFile, 100)
	settled.Changes, settled.Scope = hunked(previewFile, Hunk{97, 9, 97, 7}), &payload.ReviewScope{MergeBase: "mb-1"}
	if rep := publishThreads(t, gh, ledger, settled); rep.Fixed != 1 {
		t.Fatalf("the next review on the recorded base fixes: %+v", rep)
	}
}

// The audited "Not seen" on an unchanged workflow: a compare that carries
// no hunks at all cannot place the cited lines, so the row stays open.
func TestLedger_AbsentFindingWithoutAnyHunksStaysOpen(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, singleRound("sha-1", previewFile, 100, "medium", previewText))
	gone := absentRound("sha-2", previewFile, 100)
	gone.Changes = func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{previewFile: true}}, true
	}
	rep := publishThreads(t, gh, ledger, gone)
	if rep.Fixed != 0 || rep.StillOpen != 1 || rep.HunksUnknown != 1 || rep.ThreadsResolved != 0 {
		t.Fatalf("a file change without hunks proves nothing: %+v", rep)
	}
	if got := gh.repliesTo(1001); len(got) != 0 {
		t.Fatalf("replies = %q", got)
	}
	if row := ledger.rows[fid(previewFile, 100, previewText)]; row.State != db.PublishedStateOpen {
		t.Fatalf("row = %+v", row)
	}
	pol := testPolicy()
	pol.FixedByFile = true
	byFile := absentRound("sha-3", previewFile, 100)
	byFile.Changes = gone.Changes
	if rep := publishThreadsWith(t, gh, ledger, byFile, pol); rep.Fixed != 1 || rep.HunksUnknown != 0 {
		t.Fatalf("the kill switch restores the file rule: %+v", rep)
	}
}
