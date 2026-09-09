package publisher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pr-review-server/db"
)

func TestClassifyReply(t *testing.T) {
	cases := map[string]ReplyClass{
		"Fixed":  ReplyResolution,
		"fixed.": ReplyResolution,
		"Done":   ReplyResolution,
		"Addressed in 9de3bed. The 15 s fast-abort stays for obvious misconfig.": ReplyResolution,
		"Accepted for now: XO-291 hasn't landed yet.":                            ReplyResolution,
		"Good catch, removed the extra call.":                                    ReplyResolution,
		"Thanks!":                                                                ReplyOther,
		"ok":                                                                     ReplyOther,
		"Will fix":                                                               ReplyOther,
		"Not fixed, the guard still runs twice.":                                 ReplyPushback,
		"Fixed?":                                                                 ReplyQuestion,
		"Fixed this, but why is the guard insufficient?":                                         ReplyQuestion,
		"`DmVideos` perform toggle is not active on prod.":                                       ReplyPushback,
		"Disabling standard shows does not pass a price of 0, it passes the last enabled price.": ReplyPushback,
	}
	for body, want := range cases {
		if got := ClassifyReply(body); got != want {
			t.Errorf("ClassifyReply(%q) = %s, want %s", body, got, want)
		}
	}
}

func TestFindAuthorRepliesKeepsOnlyTheAuthorsRepliesToOurRoots(t *testing.T) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	roots := map[int64]string{100: "a.go:1:abc", 200: "b.go:4:def"}
	comments := []ThreadComment{
		{ID: 100, AuthorID: 1, Body: "<!-- prism:finding:a.go:1:abc -->\nours"},
		{ID: 101, InReplyToID: 100, AuthorID: 42, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		{ID: 102, InReplyToID: 100, AuthorID: 7, Body: "third party pushback", CreatedAt: t0.Add(time.Minute)},
		{ID: 103, InReplyToID: 100, AuthorID: 1, Body: "our own reply", CreatedAt: t0.Add(time.Minute)},
		{ID: 104, InReplyToID: 999, AuthorID: 42, Body: "reply to someone else's thread", CreatedAt: t0.Add(time.Minute)},
		{ID: 105, InReplyToID: 200, AuthorID: 42, Body: "Why does this matter?", CreatedAt: t0.Add(-time.Hour)},
		{ID: 106, InReplyToID: 200, AuthorID: 42, Body: "Is this reachable?", CreatedAt: t0.Add(2 * time.Minute)},
	}
	got := FindAuthorReplies(comments, roots, 42, t0)
	if len(got) != 2 {
		t.Fatalf("got %d replies, want 2: %+v", len(got), got)
	}
	if got[0].CommentID != 101 || got[0].RootCommentID != 100 || got[0].Fingerprint != "a.go:1:abc" || got[0].Class != ReplyResolution {
		t.Errorf("first reply = %+v", got[0])
	}
	if got[1].CommentID != 106 || got[1].Fingerprint != "b.go:4:def" || got[1].Class != ReplyQuestion {
		t.Errorf("second reply = %+v", got[1])
	}
}

type fakeReplyGH struct {
	threads   map[string][]ThreadComment
	reactions []int64
}

func (f *fakeReplyGH) ListThread(_ context.Context, owner, repo string, number int) ([]ThreadComment, error) {
	return f.threads[fmt.Sprintf("%s/%s#%d", owner, repo, number)], nil
}

func (f *fakeReplyGH) React(_ context.Context, _, _ string, commentID int64) error {
	f.reactions = append(f.reactions, commentID)
	return nil
}

type fakeReplyLedger struct {
	targets  []db.PublishedReplyTarget
	rows     []db.PublishedReply
	unlinked []db.UnlinkedPublishedFinding
	linked   map[uint]int64
}

func (f *fakeReplyLedger) ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error) {
	return f.unlinked, nil
}
func (f *fakeReplyLedger) LinkPublishedFindingComment(id uint, commentID int64) error {
	if f.linked == nil {
		f.linked = map[uint]int64{}
	}
	f.linked[id] = commentID
	return nil
}

func (f *fakeReplyLedger) ListPublishedReplyTargets() ([]db.PublishedReplyTarget, error) {
	return f.targets, nil
}
func (f *fakeReplyLedger) GetPublishedReplyIDsForPR(owner, repo string, number int) (map[int64]bool, error) {
	seen := map[int64]bool{}
	for _, r := range f.rows {
		if r.RepoOwner == owner && r.RepoName == repo && r.PRNumber == number {
			seen[r.AuthorCommentID] = true
		}
	}
	return seen, nil
}
func (f *fakeReplyLedger) RecordPublishedReply(r *db.PublishedReply) (bool, error) {
	for _, x := range f.rows {
		if x.AuthorCommentID == r.AuthorCommentID {
			return false, nil
		}
	}
	f.rows = append(f.rows, *r)
	return true, nil
}

func reactorFixture(mode string) (*ReplyReactor, *fakeReplyGH, *fakeReplyLedger) {
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	gh := &fakeReplyGH{threads: map[string][]ThreadComment{
		"acme/example#7": {
			{ID: 100, AuthorID: 1, Body: "ours"},
			{ID: 101, InReplyToID: 100, AuthorID: 42, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
			{ID: 102, InReplyToID: 100, AuthorID: 42, Body: "Is this reachable at all?", CreatedAt: t0.Add(2 * time.Minute)},
			{ID: 103, InReplyToID: 100, AuthorID: 42, Body: "old", CreatedAt: t0.Add(-time.Hour)},
		},
		"acme/example#8": {
			{ID: 200, AuthorID: 1, Body: "ours"},
			{ID: 201, InReplyToID: 200, AuthorID: 43, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		},
		"acme/example#9": {
			{ID: 300, AuthorID: 1, Body: "ours"},
			{ID: 301, InReplyToID: 300, AuthorID: 44, Body: "Fixed", CreatedAt: t0.Add(time.Minute)},
		},
	}}
	ledger := &fakeReplyLedger{targets: []db.PublishedReplyTarget{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Roots: map[int64]string{100: "a.go:1:abc"}},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 8, Roots: map[int64]string{200: "b.go:1:abc"}},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 9, Roots: map[int64]string{300: "c.go:1:abc"}},
	}}
	states := map[int]PRState{
		7: {Open: true, AuthorID: 42, AuthorLogin: "pilot"},
		8: {Open: true, Draft: true, AuthorID: 43, AuthorLogin: "pilot"},
		9: {Open: true, AuthorID: 44, AuthorLogin: "stranger"},
	}
	r := &ReplyReactor{
		GH: gh, Ledger: ledger, Mode: mode, Since: t0,
		PR:      func(_ context.Context, _, _ string, number int) (PRState, error) { return states[number], nil },
		Allowed: func(login string) bool { return login == "pilot" },
	}
	return r, gh, ledger
}

func TestReplyReactor_ReactsOnceToEachNewAuthorReplyOnEligiblePRs(t *testing.T) {
	r, gh, ledger := reactorFixture("react")
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.reactions) != "[101 102]" {
		t.Fatalf("reactions = %v, want only PR 7's new replies (8 is draft, 9 not allowlisted)", gh.reactions)
	}
	if rep.Reacted != 2 || len(ledger.rows) != 2 || ledger.rows[0].Class != "resolution" || ledger.rows[1].Class != "question" || ledger.rows[0].Action != "reacted" {
		t.Errorf("report=%+v rows=%+v", rep, ledger.rows)
	}

	gh.reactions = nil
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || rep.Reacted != 0 {
		t.Errorf("second cycle must be a no-op, got reactions=%v report=%+v", gh.reactions, rep)
	}
}

func TestReplyReactor_ObserveRecordsWithoutReacting(t *testing.T) {
	r, gh, ledger := reactorFixture("observe")
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || len(ledger.rows) != 2 || ledger.rows[0].Action != "observed" {
		t.Errorf("observe mode: reactions=%v rows=%+v", gh.reactions, ledger.rows)
	}
}

func TestMatchUnlinkedRootsUsesReviewIDAndMarkerTogether(t *testing.T) {
	rows := []db.UnlinkedPublishedFinding{
		{ID: 1, ReviewID: 500, Fingerprint: "a.go:1:abc"},
		{ID: 2, ReviewID: 500, Fingerprint: "b.go:2:def"},
		{ID: 3, ReviewID: 501, Fingerprint: "c.go:3:ghi"},
	}
	comments := []ThreadComment{
		{ID: 100, ReviewID: 500, Body: FindingMarker("a.go:1:abc") + "\nours"},
		{ID: 101, ReviewID: 500, InReplyToID: 100, Body: FindingMarker("a.go:1:abc") + " quoted back by the author"},
		{ID: 102, ReviewID: 999, Body: FindingMarker("b.go:2:def") + "\nsame marker, someone else's review"},
		{ID: 103, ReviewID: 501, Body: "no marker"},
	}
	got := MatchUnlinkedRoots(rows, comments)
	if len(got) != 1 || got[1] != 100 {
		t.Fatalf("MatchUnlinkedRoots = %v, want only row 1 -> comment 100", got)
	}
}

func TestReplyReactor_LinkRootsRecordsCommentIDsForReviewPostedFindings(t *testing.T) {
	gh := &fakeReplyGH{threads: map[string][]ThreadComment{
		"acme/example#7": {
			{ID: 100, ReviewID: 500, AuthorID: 1, Body: FindingMarker("a.go:1:abc") + "\nours"},
			{ID: 110, ReviewID: 500, AuthorID: 1, Body: FindingMarker("z.go:9:zzz") + "\nours, not in the ledger"},
		},
		"acme/example#8": {
			{ID: 200, ReviewID: 600, AuthorID: 1, Body: "marker gone"},
		},
	}}
	ledger := &fakeReplyLedger{unlinked: []db.UnlinkedPublishedFinding{
		{ID: 1, RepoOwner: "acme", RepoName: "example", PRNumber: 7, ReviewID: 500, Fingerprint: "a.go:1:abc"},
		{ID: 2, RepoOwner: "acme", RepoName: "example", PRNumber: 8, ReviewID: 600, Fingerprint: "b.go:2:def"},
	}}
	r := &ReplyReactor{GH: gh, Ledger: ledger}
	rep := r.LinkRoots(context.Background(), ledger.unlinked)
	if rep.Linked != 1 || rep.Unmatched != 1 || rep.PRsListed != 2 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if ledger.linked[1] != 100 || len(ledger.linked) != 1 {
		t.Errorf("linked = %v, want {1:100}", ledger.linked)
	}
}

func TestReplyReactor_ReportCountsWhatWasSkippedAndWhy(t *testing.T) {
	r, _, ledger := reactorFixture("react")
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.PRsSkipped["draft"] != 1 || rep.PRsSkipped["not_allowlisted"] != 1 || rep.PRsScanned != 1 {
		t.Errorf("skip breakdown = %v scanned=%d", rep.PRsSkipped, rep.PRsScanned)
	}
	if rep.RepliesSeen != 2 || rep.AlreadyHandled != 0 || len(rep.Handled) != 2 || rep.Handled[1].Class != "question" {
		t.Errorf("replies: seen=%d already=%d handled=%+v", rep.RepliesSeen, rep.AlreadyHandled, rep.Handled)
	}
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RepliesSeen != 2 || rep.AlreadyHandled != 2 || len(rep.Handled) != 0 || len(ledger.rows) != 2 {
		t.Errorf("second cycle: seen=%d already=%d handled=%d", rep.RepliesSeen, rep.AlreadyHandled, len(rep.Handled))
	}
}
