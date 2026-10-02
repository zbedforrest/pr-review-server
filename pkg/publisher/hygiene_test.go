package publisher

import (
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

func summaryOnly(head string) Round {
	r := roundOne()
	r.HeadSHA, r.RoundNumber = head, 0
	r.Findings = []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")}
	return r
}

func fingerprints(notes []HygieneNote) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Fingerprint)
	}
	return out
}

func TestHygiene_CleanRoundsReportNothing(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	rep := publishRound(t, gh, ledger, roundOne())
	if h := rep.Hygiene; len(h.RepeatedPosts)+len(h.RepeatAfterDismiss)+len(h.FixedWithoutFileChange)+len(h.SameCommitResolves)+len(h.SeverityEscalations) != 0 {
		t.Fatalf("round one hygiene = %+v, want empty", h)
	}
	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	r2.ChangedFiles = map[string]bool{"a.go": true, "b.go": true}
	rep = publishRound(t, gh, ledger, r2)
	if h := rep.Hygiene; len(h.RepeatedPosts)+len(h.RepeatAfterDismiss)+len(h.FixedWithoutFileChange)+len(h.SameCommitResolves)+len(h.SeverityEscalations) != 0 {
		t.Fatalf("unchanged findings on a new head hygiene = %+v, want empty", h)
	}
}

func TestHygiene_RepostOfAResolvedRootIsARepeatedPost(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	publishRound(t, gh, ledger, summaryOnly("sha-2"))

	rep := publishRound(t, gh, ledger, func() Round { r := roundOne(); r.HeadSHA, r.RoundNumber = "sha-3", 0; return r }())
	if got := fingerprints(rep.Hygiene.RepeatedPosts); len(got) != 2 || !strings.Contains(strings.Join(got, ","), "c1") || !strings.Contains(strings.Join(got, ","), "m1") {
		t.Fatalf("repeated posts = %v, want c1 and m1", got)
	}
	if !strings.Contains(rep.Hygiene.RepeatedPosts[0].Detail, "prior_state=resolved") {
		t.Fatalf("detail = %q", rep.Hygiene.RepeatedPosts[0].Detail)
	}
	if len(rep.Hygiene.RepeatAfterDismiss) != 0 || len(rep.Hygiene.SeverityEscalations) != 0 {
		t.Fatalf("hygiene = %+v, want only repeated posts", rep.Hygiene)
	}
}

func TestHygiene_DismissedRowStaysSilentAndIsNotARepeat(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	ledger.rows["c1"].State = db.PublishedStateDismissed

	rep := publishRound(t, gh, ledger, func() Round { r := roundOne(); r.HeadSHA, r.RoundNumber = "sha-2", 0; return r }())
	if rep.InlinePosted != 0 || len(rep.Hygiene.RepeatedPosts) != 0 || len(rep.Hygiene.RepeatAfterDismiss) != 0 {
		t.Fatalf("a dismissed finding must be suppressed, not reposted: %+v", rep)
	}
}

func TestHygiene_RepostOverADismissedRowIsARepeatAfterDismiss(t *testing.T) {
	var h Hygiene
	prior := &db.PublishedFinding{Fingerprint: "c1", Kind: db.PublishedKindFinding, CommentID: 1001, State: db.PublishedStateDismissed, Severity: "critical"}
	h.notePosted(f("c1", "critical", "a.go", 10, "Critical thing."), prior)
	if len(h.RepeatedPosts) != 1 || len(h.RepeatAfterDismiss) != 1 || h.RepeatAfterDismiss[0].Fingerprint != "c1" {
		t.Fatalf("hygiene = %+v", h)
	}
	h = Hygiene{}
	h.notePosted(f("c1", "critical", "a.go", 10, "Critical thing."), &db.PublishedFinding{Fingerprint: "c1", State: db.PublishedStateResolved})
	if len(h.RepeatedPosts) != 0 {
		t.Fatalf("a prior row without a comment is not a repeated root: %+v", h)
	}
}

func TestHygiene_ResolveOnTheSameHeadIsASameCommitResolve(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	rerun := summaryOnly("sha-round-1")
	rerun.ChangedFiles = map[string]bool{}
	rep := publishRound(t, gh, ledger, rerun)
	if rep.Fixed != 3 {
		t.Fatalf("precondition: every tracked finding resolves on a re-run, got %+v", rep)
	}
	if got := fingerprints(rep.Hygiene.SameCommitResolves); len(got) != 3 {
		t.Fatalf("same-commit resolves = %v, want c1, m1 and m2", got)
	}
	if len(rep.Hygiene.FixedWithoutFileChange) != 0 {
		t.Fatalf("a same-commit resolve is counted once, not also as fixed without a file change: %+v", rep.Hygiene)
	}
}

// filedRound is roundOne with ids in the real fingerprint shape, so the
// cited file can be read back from a ledger row.
func filedRound() Round {
	r := roundOne()
	for i := range r.Findings {
		if fd := &r.Findings[i]; fd.File != "SUMMARY" {
			fd.ID = payload.Fingerprint(fd.File, fd.Line, fd.Comment)
		}
	}
	r.SourceTags = nil
	return r
}

func TestHygiene_ResolveWithoutTheCitedFileChangingIsCounted(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, filedRound())

	gone := summaryOnly("sha-2")
	gone.ChangedFiles = map[string]bool{"b.go": true}
	rep := publishRound(t, gh, ledger, gone)
	got := fingerprints(rep.Hygiene.FixedWithoutFileChange)
	if len(got) != 2 || !strings.HasPrefix(got[0], "a.go:") || !strings.HasPrefix(got[1], "a.go:") {
		t.Fatalf("fixed without file change = %v, want the two a.go rows and not the b.go one", got)
	}
	if !strings.Contains(rep.Hygiene.FixedWithoutFileChange[0].Detail, "file=a.go") {
		t.Fatalf("detail = %q", rep.Hygiene.FixedWithoutFileChange[0].Detail)
	}
	if len(rep.Hygiene.SameCommitResolves) != 0 {
		t.Fatalf("a new head is not a same-commit resolve: %+v", rep.Hygiene)
	}
}

func TestHygiene_UnknownChangedFilesCannotJudgeAResolve(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	rep := publishRound(t, gh, ledger, summaryOnly("sha-2"))
	if rep.Fixed != 3 || len(rep.Hygiene.FixedWithoutFileChange) != 0 || len(rep.Hygiene.SameCommitResolves) != 0 {
		t.Fatalf("without a changed-file set nothing is counted: %+v", rep)
	}
}

func TestHygiene_HigherSeverityOnAnExistingRowIsAnEscalation(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	publishRound(t, gh, ledger, summaryOnly("sha-2"))

	back := roundOne()
	back.HeadSHA, back.RoundNumber = "sha-3", 0
	for i := range back.Findings {
		switch back.Findings[i].ID {
		case "m1", "m2":
			back.Findings[i].Severity = "critical"
		}
	}
	rep := publishRound(t, gh, ledger, back)
	got := strings.Join(fingerprints(rep.Hygiene.SeverityEscalations), ",")
	if len(rep.Hygiene.SeverityEscalations) != 2 || !strings.Contains(got, "m1") || !strings.Contains(got, "m2") {
		t.Fatalf("escalations = %v, want the reposted m1 and the annotation m2, not the unchanged c1", got)
	}
	if d := rep.Hygiene.SeverityEscalations[0].Detail; !strings.Contains(d, "from=medium to=critical") {
		t.Fatalf("detail = %q", d)
	}
}

func TestHygiene_LowerOrEqualSeverityIsNotAnEscalation(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	for i := range r2.Findings {
		if r2.Findings[i].ID == "m2" {
			r2.Findings[i].Severity = "low"
		}
	}
	rep := publishRound(t, gh, ledger, r2)
	if len(rep.Hygiene.SeverityEscalations) != 0 {
		t.Fatalf("escalations = %+v, want none", rep.Hygiene.SeverityEscalations)
	}
}

func TestHygiene_OpenInlineRowRenderedAtHigherSeverityIsAnEscalation(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	for i := range r2.Findings {
		if r2.Findings[i].ID == "c1" {
			r2.Findings[i].Severity = "critical"
			r2.Findings[i].Comment = "Critical thing."
		}
	}
	ledger.rows["c1"].Severity = "medium"
	rep := publishRound(t, gh, ledger, r2)
	if rep.InlinePosted != 0 {
		t.Fatalf("precondition: an open inline row is not reposted, got %+v", rep)
	}
	if got := fingerprints(rep.Hygiene.SeverityEscalations); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("escalations = %v, want the open row c1", got)
	}
	if d := rep.Hygiene.SeverityEscalations[0].Detail; !strings.Contains(d, "from=medium to=critical prior_state=open") {
		t.Fatalf("detail = %q", d)
	}
	rep = publishRound(t, gh, ledger, r2)
	if len(rep.Hygiene.SeverityEscalations) != 0 {
		t.Fatalf("a re-run on the same head recounts nothing: %+v", rep.Hygiene.SeverityEscalations)
	}
}

func TestHygiene_OpenRowEscalationIsCountedOnceAcrossPushes(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())
	ledger.rows["c1"].Severity = "medium"

	escalated := func(head string) Round {
		r := roundOne()
		r.HeadSHA, r.RoundNumber = head, 0
		for i := range r.Findings {
			if r.Findings[i].ID == "c1" {
				r.Findings[i].Severity = "critical"
			}
		}
		return r
	}
	rep := publishRound(t, gh, ledger, escalated("sha-2"))
	if got := fingerprints(rep.Hygiene.SeverityEscalations); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("first push escalations = %v, want c1", got)
	}
	if ledger.rows["c1"].Severity != "critical" || ledger.rows["c1"].LastSeenSHA != "sha-2" {
		t.Fatalf("the refreshed row must carry the asserted severity: %+v", ledger.rows["c1"])
	}
	rep = publishRound(t, gh, ledger, escalated("sha-3"))
	if len(rep.Hygiene.SeverityEscalations) != 0 {
		t.Fatalf("the same severity on the next push is not a second escalation: %+v", rep.Hygiene.SeverityEscalations)
	}
}

func TestHygiene_NoteWrittenCountsOnlyAHigherSeverity(t *testing.T) {
	prior := &db.PublishedFinding{Fingerprint: "m1", Severity: "medium", State: db.PublishedStateOpen}
	for _, tc := range []struct {
		severity string
		want     int
	}{{"low", 0}, {"medium", 0}, {"critical", 1}} {
		var h Hygiene
		h.noteWritten(f("m1", tc.severity, "b.go", 20, "Medium thing."), prior)
		if len(h.SeverityEscalations) != tc.want {
			t.Fatalf("%s over medium: escalations = %+v, want %d", tc.severity, h.SeverityEscalations, tc.want)
		}
	}
	var h Hygiene
	h.noteWritten(f("m1", "critical", "b.go", 20, "Medium thing."), nil)
	if len(h.SeverityEscalations) != 0 {
		t.Fatalf("no prior row, no escalation: %+v", h)
	}
}

func TestHygiene_NoteResolvedJudgesHeadAndChangedFiles(t *testing.T) {
	row := &db.PublishedFinding{Fingerprint: payload.Fingerprint("a.go", 10, "Critical thing."), LastSeenSHA: "sha-1"}
	var h Hygiene
	h.noteResolved(row, "sha-1", map[string]bool{"a.go": true})
	if len(h.SameCommitResolves) != 1 || len(h.FixedWithoutFileChange) != 0 {
		t.Fatalf("same head: %+v", h)
	}
	h = Hygiene{}
	h.noteResolved(row, "sha-2", nil)
	if len(h.SameCommitResolves)+len(h.FixedWithoutFileChange) != 0 {
		t.Fatalf("unknown changed files: %+v", h)
	}
	h = Hygiene{}
	h.noteResolved(row, "sha-2", map[string]bool{"b.go": true})
	if len(h.FixedWithoutFileChange) != 1 || !strings.Contains(h.FixedWithoutFileChange[0].Detail, "file=a.go from=sha-1 to=sha-2") {
		t.Fatalf("cited file unchanged: %+v", h)
	}
	h = Hygiene{}
	h.noteResolved(row, "sha-2", map[string]bool{"a.go": true})
	if len(h.SameCommitResolves)+len(h.FixedWithoutFileChange) != 0 {
		t.Fatalf("cited file changed: %+v", h)
	}
}
