package publisher

import (
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func withSubjects(x payload.Finding, kind string, names ...string) payload.Finding {
	x.FindingContract.FindingKind = kind
	for _, n := range names {
		x.FindingContract.Subjects = append(x.FindingContract.Subjects, types.FindingSubject{Kind: "symbol", Path: x.File, Name: n})
	}
	return x
}

func unknownChanges(string) (ChangeSet, bool) { return ChangeSet{}, false }

func fid(file string, line int, comment string) string {
	return payload.Fingerprint(file, line, comment)
}

var (
	c1ID = fid("a.go", 10, "Critical thing.")
	m1ID = fid("b.go", 20, "Medium thing.")
	m2ID = fid("a.go", 99, "Medium outside hunk.")
)

// ledgerRoundOne is roundOne with real fingerprints, which the ledger
// matcher reads the cited file out of.
func ledgerRoundOne() Round {
	r := roundOne()
	r.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f(c1ID, "critical", "a.go", 10, "Critical thing."),
		f(m1ID, "medium", "b.go", 20, "Medium thing."),
		f("l1", "low", "a.go", 11, "Low thing."),
		f(m2ID, "medium", "a.go", 99, "Medium outside hunk."),
	}
	r.SourceTags = nil
	return r
}

func TestLedger_AbsentOnSameHeadStaysOpen(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	rerun := ledgerRoundOne()
	rerun.RoundNumber = 0
	rerun.Changes = changed("a.go", "b.go")
	rerun.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	rep := republishSameHead(t, gh, ledger, rerun)
	if rep.Fixed != 0 || rep.StillOpen != 3 {
		t.Fatalf("a re-run on the reviewed head can fix nothing: %+v", rep)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateOpen {
		t.Fatalf("c1 = %+v", row)
	}
}

func TestLedger_AbsentWithoutFileChangeStaysOpen(t *testing.T) {
	for name, changes := range map[string]func(string) (ChangeSet, bool){
		"other file changed": changed("README.md"),
		"changes unknown":    unknownChanges,
		"no lookup":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			publishRound(t, gh, ledger, ledgerRoundOne())
			r2 := ledgerRoundOne()
			r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changes
			r2.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
			rep := publishRound(t, gh, ledger, r2)
			if rep.Fixed != 0 || rep.StillOpen != 3 {
				t.Fatalf("absence alone is not a fix: %+v", rep)
			}
			if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-round-1" {
				t.Fatalf("an untouched absent row keeps its state and last-seen sha: %+v", row)
			}
			if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 0 fixed") {
				t.Fatalf("summary:\n%s", gh.issueEdits[501])
			}
		})
	}
}

func TestLedger_AbsentFromChangedFileIsFixed(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	r2 := ledgerRoundOne()
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	r2.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	rep := publishRound(t, gh, ledger, r2)
	if rep.Fixed != 1 || rep.StillOpen != 2 {
		t.Fatalf("only the finding in the changed file is fixed: %+v", rep)
	}
	if ledger.rows[m1ID].State != db.PublishedStateFixed || ledger.rows[c1ID].State != db.PublishedStateOpen {
		t.Fatalf("m1=%s c1=%s", ledger.rows[m1ID].State, ledger.rows[c1ID].State)
	}
}

const rewordedC1 = "A critical thing happens here: the critical thing."

func rewordedRound(head string) Round {
	r := ledgerRoundOne()
	r.HeadSHA, r.RoundNumber = head, 0
	r.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		f(fid("a.go", 14, rewordedC1), "critical", "a.go", 14, rewordedC1),
		f(m1ID, "medium", "b.go", 20, "Medium thing."),
		f(m2ID, "medium", "a.go", 99, "Medium outside hunk."),
	}
	r.Commentable["a.go"][14] = true
	return r
}

func TestLedger_RewordedFindingAliasesToItsRowByStoredText(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	rep := publishRound(t, gh, ledger, rewordedRound("sha-2"))
	if rep.InlinePosted != 0 || len(gh.reviews) != 1 {
		t.Fatalf("a reworded finding must not become a second root: %+v reviews=%d", rep, len(gh.reviews))
	}
	if rep.StillOpen != 3 || rep.Fixed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if row := ledger.rows[c1ID]; row.LastSeenSHA != "sha-2" || row.State != db.PublishedStateOpen {
		t.Fatalf("the aliased row is refreshed in place: %+v", row)
	}
	if _, fresh := ledger.rows[fid("a.go", 14, rewordedC1)]; fresh {
		t.Fatal("the reworded id must not get its own row")
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 0 fixed") {
		t.Fatalf("summary:\n%s", gh.issueEdits[501])
	}
}

func TestLedger_RewordedFindingAliasesByGitHubBodyWhenTheRowHasNoText(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	ledger.rows[c1ID].CommentText = ""
	r2 := rewordedRound("sha-2")
	r2.PriorComments = map[string]PriorComment{c1ID: {Line: 12, Text: gh.reviews[0].comments[0].Body}}
	rep := publishRound(t, gh, ledger, r2)
	if rep.InlinePosted != 0 {
		t.Fatalf("the posted body is enough to recognise the rewording: %+v", rep)
	}
	if got := ledger.rows[c1ID].CommentText; got == "" || strings.Contains(got, "<!--") || strings.Contains(got, "<img") {
		t.Fatalf("the row must be backfilled with stripped prose, got %q", got)
	}
}

func TestLedger_AnnotationRowsWithoutCommentsBackfillFromTheFinding(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	ledger.rows[m2ID].CommentText = ""
	r2 := ledgerRoundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	publishRound(t, gh, ledger, r2)
	if ledger.rows[m2ID].CommentText != "Medium outside hunk." {
		t.Fatalf("m2 = %+v", ledger.rows[m2ID])
	}
}

func TestLedger_SubjectKeyAliasesAcrossLinesAndFiles(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r1 := roundOne()
	id := fid("a.go", 10, "fetchUser retries without backoff.")
	r1.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		withSubjects(f(id, "critical", "a.go", 10, "fetchUser retries without backoff."), "production_behavior", "fetchUser", "retry"),
	}
	publishRound(t, gh, ledger, r1)
	if ledger.rows[id].Subjects != "fetchuser,retry" {
		t.Fatalf("subjects column = %q", ledger.rows[id].Subjects)
	}

	moved := roundOne()
	moved.HeadSHA, moved.RoundNumber = "sha-2", 0
	moved.Commentable = map[string]map[int]bool{"a.go": {80: true}, "lib/a.go": {80: true}}
	for name, file := range map[string]string{"same file, far line": "a.go", "re-anchored to another file": "lib/a.go"} {
		t.Run(name, func(t *testing.T) {
			r := moved
			r.Findings = []payload.Finding{
				f("sum", "unknown", "SUMMARY", 0, "Narrative."),
				withSubjects(f("new-id", "critical", file, 80, "No delay between attempts after a 429 response."), "production_behavior", "fetchUser", "retry"),
			}
			rep := republishSameHead(t, gh, ledger, r)
			if rep.InlinePosted != 0 || rep.StillOpen != 1 {
				t.Fatalf("the subject key must alias the moved finding: %+v", rep)
			}
		})
	}
}

func TestLedger_TerminalRowsSuppressEveryLaterWording(t *testing.T) {
	for _, state := range []string{db.PublishedStateDismissed, db.PublishedStateContested, db.PublishedStateExternal} {
		t.Run(state, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			publishRound(t, gh, ledger, ledgerRoundOne())
			ledger.rows[c1ID].State = state
			rep := publishRound(t, gh, ledger, rewordedRound("sha-2"))
			if rep.InlinePosted != 0 || len(gh.reviews) != 1 {
				t.Fatalf("a settled point must not come back under new words: %+v", rep)
			}
			if rep.StillOpen != 2 || strings.Contains(gh.issueEdits[501], "critical thing") {
				t.Fatalf("settled rows are neither counted nor shown: %+v\n%s", rep, gh.issueEdits[501])
			}
			if row := ledger.rows[c1ID]; row.State != state || row.LastSeenSHA != "sha-round-1" {
				t.Fatalf("terminal row must be left alone: %+v", row)
			}
		})
	}
}

func securityRound(head string, line int, comment string) Round {
	r := roundOne()
	r.HeadSHA, r.RoundNumber = head, 0
	r.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		withSubjects(f(fid("a.go", line, comment), "critical", "a.go", line, comment), "security_risk", "token"),
	}
	r.Commentable = map[string]map[int]bool{"a.go": {10: true, 12: true}}
	return r
}

func TestLedger_CriticalSecurityRiskReturnsOnceWhenItsAnchorChanged(t *testing.T) {
	const first, second, third = "The token is logged in clear text.", "The token still reaches the log in clear text.", "Clear-text token in the log output."
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, securityRound("sha-1", 10, first))
	ledger.rows[fid("a.go", 10, first)].State = db.PublishedStateDismissed

	untouched := securityRound("sha-2", 12, second)
	untouched.Changes = changed("b.go")
	if rep := publishRound(t, gh, ledger, untouched); rep.InlinePosted != 0 {
		t.Fatalf("an unchanged anchor keeps the dismissal: %+v", rep)
	}

	moved := securityRound("sha-3", 12, second)
	moved.Changes = func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{"a.go": true}, Lines: map[string]map[int]bool{"a.go": {11: true}}}, true
	}
	rep := publishRound(t, gh, ledger, moved)
	if rep.InlinePosted != 1 || len(gh.reviews) != 2 {
		t.Fatalf("a changed anchor lets a critical security risk return once: %+v", rep)
	}
	if body := gh.reviews[1].comments[0].Body; !strings.Contains(body, "changed at sha-3") || !strings.Contains(body, FindingMarker(fid("a.go", 12, second))) {
		t.Fatalf("the fresh root must name the change under its own marker:\n%s", body)
	}
	if ledger.rows[fid("a.go", 10, first)].State != db.PublishedStateDismissed || ledger.rows[fid("a.go", 12, second)].State != db.PublishedStateOpen {
		t.Fatalf("old row stays dismissed, the new one opens: %+v", ledger.rows)
	}

	ledger.rows[fid("a.go", 12, second)].State = db.PublishedStateDismissed
	again := securityRound("sha-4", 12, third)
	again.Changes = moved.Changes
	if rep := publishRound(t, gh, ledger, again); rep.InlinePosted != 0 || len(gh.reviews) != 2 {
		t.Fatalf("two settled rows on the point end the exception: %+v", rep)
	}
}

func TestLedger_SecurityExceptionIsOneShotWhileTheFreshRootLives(t *testing.T) {
	const first, second, third = "The token is logged in clear text.", "The token still reaches the log in clear text.", "Clear-text token in the log output."
	anchorMoved := func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{"a.go": true}, Lines: map[string]map[int]bool{"a.go": {11: true, 12: true}}}, true
	}
	for name, freshState := range map[string]string{"open": db.PublishedStateOpen, "fixed": db.PublishedStateFixed} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			publishRound(t, gh, ledger, securityRound("sha-1", 10, first))
			ledger.rows[fid("a.go", 10, first)].State = db.PublishedStateDismissed
			returned := securityRound("sha-3", 12, second)
			returned.Changes = anchorMoved
			if rep := publishRound(t, gh, ledger, returned); rep.InlinePosted != 1 {
				t.Fatalf("setup: %+v", rep)
			}
			freshID := fid("a.go", 12, second)
			ledger.rows[freshID].State = freshState
			freshComment := ledger.rows[freshID].CommentID

			again := securityRound("sha-4", 12, third)
			again.Changes = anchorMoved
			rep := publishRound(t, gh, ledger, again)
			if rep.InlinePosted != 0 || len(gh.reviews) != 2 {
				t.Fatalf("a third wording must not open a third root: %+v reviews=%d", rep, len(gh.reviews))
			}
			if _, posted := ledger.rows[fid("a.go", 12, third)]; posted {
				t.Fatal("the third wording must not get its own row")
			}
			fresh := ledger.rows[freshID]
			if fresh.State != db.PublishedStateOpen || fresh.LastSeenSHA != "sha-4" || rep.StillOpen != 1 {
				t.Fatalf("the third wording refreshes the fresh row instead: %+v report=%+v", fresh, rep)
			}
			if ledger.rows[fid("a.go", 10, first)].State != db.PublishedStateDismissed {
				t.Fatal("the dismissed row stays dismissed")
			}
			replies := gh.repliesTo(freshComment)
			if freshState == db.PublishedStateFixed && (rep.Reopened != 1 || len(replies) != 1 || !strings.Contains(replies[0], "Back at sha-4")) {
				t.Fatalf("a fixed fresh row reopens in its thread: %+v replies=%q", rep, replies)
			}
			if freshState == db.PublishedStateOpen && len(replies) != 0 {
				t.Fatalf("an open fresh row needs no note: %q", replies)
			}
		})
	}
}

func TestLedger_RowsRememberTheFindingKind(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, securityRound("sha-1", 10, "The token is logged in clear text."))
	if row := ledger.rows[fid("a.go", 10, "The token is logged in clear text.")]; row.FindingKind != "security_risk" || row.Subjects != "token" {
		t.Fatalf("row = %+v", row)
	}
}

func TestLedger_SeverityIsClampedToTheRowUnlessTheAnchorChanged(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	escalated := roundOne()
	escalated.HeadSHA, escalated.RoundNumber = "sha-2", 0
	escalated.Findings[2].Severity = "critical" // m1 comes back as critical with its lines untouched
	escalated.Changes = changed("a.go")
	rep := publishRound(t, gh, ledger, escalated)
	if rep.ThreadReplies != 0 || ledger.rows["m1"].Severity != "medium" {
		t.Fatalf("an unchanged anchor keeps the ledger severity: %+v row=%+v", rep, ledger.rows["m1"])
	}
	if rep.Confidence != 2 || strings.Contains(gh.issueEdits[501], "CRITICAL\" src=\"\"") {
		t.Fatalf("the clamped severity is what the summary scores: %+v", rep)
	}

	changedLines := roundOne()
	changedLines.HeadSHA, changedLines.RoundNumber = "sha-3", 0
	changedLines.Findings[2].Severity = "critical"
	changedLines.Changes = func(string) (ChangeSet, bool) {
		return ChangeSet{Files: map[string]bool{"b.go": true}, Lines: map[string]map[int]bool{"b.go": {21: true}}}, true
	}
	rep = publishRound(t, gh, ledger, changedLines)
	if ledger.rows["m1"].Severity != "critical" || rep.ThreadReplies != 1 {
		t.Fatalf("a changed anchor accepts the new severity once, in the thread: %+v row=%+v", rep, ledger.rows["m1"])
	}
	if got := gh.repliesTo(1002); len(got) != 1 || !strings.Contains(got[0], "medium to critical at sha-3") {
		t.Fatalf("m1 thread replies = %q", got)
	}
	if len(gh.reviews) != 1 {
		t.Fatal("a severity change is never a new root")
	}
}

func TestLedger_LegacyResolvedRowReopensUnderNewWording(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	ledger.rows[c1ID].State = db.PublishedStateResolved
	rep := publishRound(t, gh, ledger, rewordedRound("sha-2"))
	if rep.InlinePosted != 0 || rep.Reopened != 1 || len(gh.repliesTo(1001)) != 1 {
		t.Fatalf("a legacy resolved row reopens in its thread: %+v", rep)
	}
}

func TestLegacyPolicy_RestoresRepostOnReturn(t *testing.T) {
	legacy := testPolicy()
	legacy.LegacyLedger = true
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishWith(t, gh, ledger, roundOne(), legacy)

	gone := roundOne()
	gone.HeadSHA, gone.RoundNumber = "sha-2", 0
	gone.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	rep := publishWith(t, gh, ledger, gone, legacy)
	if rep.Fixed != 3 || ledger.rows["c1"].State != db.PublishedStateResolved {
		t.Fatalf("legacy resolves on absence alone: %+v", rep)
	}

	back := roundOne()
	back.HeadSHA, back.RoundNumber = "sha-3", 0
	rep = publishWith(t, gh, ledger, back, legacy)
	if rep.InlinePosted != 2 || len(gh.reviews) != 2 || len(gh.replies) != 0 {
		t.Fatalf("legacy reposts what comes back: %+v reviews=%d", rep, len(gh.reviews))
	}
}

func TestLegacyLedgerFromEnv(t *testing.T) {
	for value, want := range map[string]bool{"": false, "true": false, "1": false, "false": true, "0": true, " FALSE ": true} {
		t.Setenv(PolicyV2Env, value)
		if got := LegacyLedgerFromEnv(); got != want {
			t.Errorf("%s=%q: legacy=%v want %v", PolicyV2Env, value, got, want)
		}
	}
}

func TestStripRendered(t *testing.T) {
	x := f("a.go:1:abc", "medium", "a.go", 12, "The retry loop never backs off after a 429.")
	body := RenderInline(x, SourceTagBoth, "https://prism.example/go/agent?o=acme&r=example&n=7", "https://prism.example/badge", "")
	got := StripRendered(body)
	for _, banned := range []string{"<!--", "<img", "Agent prompt", "Fix with agent", "Source:", "Reasoning and how to verify", "https://"} {
		if strings.Contains(got, banned) {
			t.Errorf("stripped body still carries %q: %s", banned, got)
		}
	}
	if !strings.Contains(got, "retry loop never backs off") {
		t.Errorf("prose lost: %s", got)
	}
}

func TestChangedLines(t *testing.T) {
	patch := "@@ -10,4 +10,5 @@\n a\n-b\n+B\n+C\n d\n@@ -40,2 +41,2 @@\n x\n+y\n"
	got := ChangedLines(patch)
	for _, l := range []int{11, 12, 42} {
		if !got[l] {
			t.Errorf("line %d should be changed: %v", l, got)
		}
	}
	for _, l := range []int{10, 13, 41} {
		if got[l] {
			t.Errorf("line %d is context: %v", l, got)
		}
	}
}
