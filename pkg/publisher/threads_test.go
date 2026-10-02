package publisher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"pr-review-server/db"
)

// fakeThreadGitHub is fakeGitHub with review threads: every inline comment
// it created opens thread "T<comment id>", plus any extra threads a test
// seeds for rows the ledger held before this GitHub existed.
type fakeThreadGitHub struct {
	*fakeGitHub
	extra      []ReviewThread
	outdated   map[int64]bool
	resolved   map[string]bool
	resolves   []string
	unresolves []string
	listings   int
	listErr    error
	resolveErr error
}

func newFakeThreadGitHub() *fakeThreadGitHub {
	return &fakeThreadGitHub{fakeGitHub: newFakeGitHub(), outdated: map[int64]bool{}, resolved: map[string]bool{}}
}

func threadNode(commentID int64) string { return fmt.Sprintf("T%d", commentID) }

func sorted(ids []string) string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return fmt.Sprint(out)
}

func (g *fakeThreadGitHub) ListReviewThreads(context.Context, string, string, int) ([]ReviewThread, error) {
	g.listings++
	if g.listErr != nil {
		return nil, g.listErr
	}
	out := append([]ReviewThread(nil), g.extra...)
	for _, rv := range g.reviews {
		for _, id := range rv.ids {
			out = append(out, ReviewThread{NodeID: threadNode(id), RootCommentID: id, Resolved: g.resolved[threadNode(id)], Outdated: g.outdated[id]})
		}
	}
	return out, nil
}

func (g *fakeThreadGitHub) ResolveThread(_ context.Context, _, _, nodeID string) error {
	if g.resolveErr != nil {
		return g.resolveErr
	}
	g.resolved[nodeID] = true
	g.resolves = append(g.resolves, nodeID)
	return nil
}

func (g *fakeThreadGitHub) UnresolveThread(_ context.Context, _, _, nodeID string) error {
	delete(g.resolved, nodeID)
	g.unresolves = append(g.unresolves, nodeID)
	return nil
}

func publishThreads(t *testing.T, gh *fakeThreadGitHub, ledger *fakeLedger, r Round) Report {
	t.Helper()
	return publishThreadsWith(t, gh, ledger, r, testPolicy())
}

func publishThreadsWith(t *testing.T, gh *fakeThreadGitHub, ledger *fakeLedger, r Round, pol Policy) Report {
	t.Helper()
	p := &Publisher{GH: gh, Ledger: ledger, Policy: pol}
	rep, err := p.Publish(context.Background(), r)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return rep
}

func TestThreads_NodeIDIsWrittenWhenTheRootIsPosted(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	if row := ledger.get(db.PublishedKindFinding, c1ID); row == nil || row.CommentID != 1001 || row.ThreadNodeID != "T1001" {
		t.Fatalf("c1 row = %+v, want thread T1001", row)
	}
	if row := ledger.get(db.PublishedKindFinding, m1ID); row == nil || row.ThreadNodeID != "T1002" {
		t.Fatalf("m1 row = %+v, want thread T1002", row)
	}
	if row := ledger.get(db.PublishedKindAnnotation, m2ID); row == nil || row.ThreadNodeID != "" {
		t.Fatalf("an annotation opens no thread: %+v", row)
	}
	if gh.listings != 1 {
		t.Fatalf("threads listed %d times, want once after the review", gh.listings)
	}
}

func TestThreads_FixedFindingGetsOneNoteAndItsThreadResolved(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadsResolved != 1 || rep.ThreadResolveFailures != 0 || rep.ThreadReplies != 1 {
		t.Fatalf("report = %+v, want c1 and the m2 annotation fixed, one note, one thread resolved", rep)
	}
	if got := gh.repliesTo(1001); len(got) != 1 || got[0] != "Not seen at sha-2." {
		t.Fatalf("c1 thread replies = %q", got)
	}
	if fmt.Sprint(gh.resolves) != "[T1001]" {
		t.Fatalf("resolved = %v, want the c1 thread only", gh.resolves)
	}
	if got := gh.repliesTo(1002); len(got) != 0 {
		t.Fatalf("m1 is still open and its thread must stay quiet: %q", got)
	}
	if row := ledger.get(db.PublishedKindFinding, m1ID); row.State != db.PublishedStateOpen {
		t.Fatalf("m1 in the untouched b.go stays open: %+v", row)
	}

	again := summaryOnly("sha-3")
	again.Changes = changed("a.go")
	rep = publishThreads(t, gh, ledger, again)
	if rep.ThreadsResolved != 0 || len(gh.resolves) != 1 || len(gh.repliesTo(1001)) != 1 {
		t.Fatalf("a fixed row is resolved once: %+v resolves=%v replies=%q", rep, gh.resolves, gh.repliesTo(1001))
	}
}

func TestThreads_ReopenedFindingUnresolvesItsThread(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())

	gone := summaryOnly("sha-2")
	gone.Changes = changed("a.go", "b.go")
	publishThreads(t, gh, ledger, gone)
	if sorted(gh.resolves) != "[T1001 T1002]" {
		t.Fatalf("precondition: both threads resolved, got %v", gh.resolves)
	}

	back := ledgerRoundOne()
	back.HeadSHA, back.RoundNumber = "sha-3", 0
	back.Changes = changed("a.go", "b.go")
	rep := publishThreads(t, gh, ledger, back)
	if rep.Reopened != 3 || rep.ThreadsUnresolved != 2 || rep.InlinePosted != 0 {
		t.Fatalf("report = %+v, want the two threads reopened and no new root", rep)
	}
	if sorted(gh.unresolves) != "[T1001 T1002]" || gh.resolved["T1001"] || gh.resolved["T1002"] {
		t.Fatalf("unresolves = %v resolved = %v", gh.unresolves, gh.resolved)
	}
	if got := gh.repliesTo(1001); len(got) != 2 || got[1] != "Back at sha-3." {
		t.Fatalf("c1 thread replies = %q", got)
	}
}

func TestThreads_RowWithoutStoredNodeIDIsResolvedThroughTheListing(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	seed := ledgerRoundOne()
	ledger.rows[c1ID] = &db.PublishedFinding{
		RepoOwner: seed.Owner, RepoName: seed.Repo, PRNumber: seed.Number,
		Kind: db.PublishedKindFinding, Fingerprint: c1ID, Severity: "critical",
		ReviewedSHA: "sha-round-1", LastSeenSHA: "sha-round-1", CommentID: 777, State: db.PublishedStateOpen,
		CommentText: "Critical thing.",
	}
	gh.extra = []ReviewThread{{NodeID: "T777", RootCommentID: 777}}

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 1 || rep.ThreadsResolved != 1 || fmt.Sprint(gh.resolves) != "[T777]" {
		t.Fatalf("report = %+v resolves = %v", rep, gh.resolves)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateFixed || row.ThreadNodeID != "T777" {
		t.Fatalf("the row keeps the node id the listing found: %+v", row)
	}
}

func TestThreads_UnknownCompareLeavesAnOutdatedThreadOpen(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	gh.outdated[1001] = true

	r2 := summaryOnly("sha-2")
	r2.Changes = unknownChanges
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 0 || len(gh.resolves) != 0 || len(gh.replies) != 0 {
		t.Fatalf("report = %+v resolves=%v replies=%v, want nothing fixed on an unknown compare", rep, gh.resolves, gh.replies)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateOpen {
		t.Fatalf("c1 = %+v", row)
	}
}

func TestThreads_AlreadyResolvedThreadGetsNoNote(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	gh.resolved["T1001"] = true

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadsResolved != 1 || rep.ThreadReplies != 0 || len(gh.repliesTo(1001)) != 0 {
		t.Fatalf("report = %+v replies=%q, want the row fixed and the human's resolution left silent", rep, gh.repliesTo(1001))
	}
}

func TestThreads_UnlistedThreadGetsNoNote(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	ledger.rows[c1ID].ThreadNodeID = ""
	gh.listErr = errors.New("502")

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadResolveFailures != 1 || rep.ThreadReplies != 0 || len(gh.replies) != 0 {
		t.Fatalf("report = %+v replies=%v, want the resolve counted lost and no note", rep, gh.replies)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateFixed {
		t.Fatalf("c1 = %+v", row)
	}
}

func TestThreads_StoredNodeIDResolvesWithoutANoteWhenTheListingFails(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	gh.listErr = errors.New("502")

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadsResolved != 1 || rep.ThreadResolveFailures != 0 || rep.ThreadReplies != 0 || rep.ThreadReplyFailures != 1 || len(gh.replies) != 0 {
		t.Fatalf("report = %+v replies=%v, want the stored id resolved, the note counted lost and nothing posted into an unverified thread", rep, gh.replies)
	}
	if sorted(gh.resolves) != "[T1001]" {
		t.Fatalf("resolves = %v", gh.resolves)
	}
}

func TestThreads_WrittenRowsKeepTheirThreadChangeWhenALaterWriteFails(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())

	threadRowWritten := false
	ledger.failUpsert = func(pf *db.PublishedFinding) error {
		if pf.State != db.PublishedStateFixed {
			return nil
		}
		if threadRowWritten {
			return errors.New("db gone")
		}
		threadRowWritten = pf.CommentID != 0
		return nil
	}
	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go", "b.go")
	p := &Publisher{GH: gh, Ledger: ledger, Policy: testPolicy()}
	rep, err := p.Publish(context.Background(), r2)
	if err == nil {
		t.Fatal("want the write failure surfaced")
	}
	if len(gh.resolves) != 1 || rep.ThreadsResolved != 1 {
		t.Fatalf("resolves=%v rep=%+v, want the one fixed row written before the failure resolved", gh.resolves, rep)
	}
}

func TestThreads_SameHeadNeverResolves(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	gh.outdated[1001] = true
	rerun := summaryOnly("sha-round-1")
	rerun.Changes = changed("a.go", "b.go")
	pol := testPolicy()
	pol.RepublishSameCommit = true
	rep := publishThreadsWith(t, gh, ledger, rerun, pol)
	if rep.Fixed != 0 || len(gh.resolves) != 0 || len(gh.replies) != 0 {
		t.Fatalf("report = %+v resolves=%v replies=%v", rep, gh.resolves, gh.replies)
	}
}

func TestThreads_KillSwitchLeavesThreadsAlone(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	pol := testPolicy()
	pol.ResolveThreads = false
	publishThreadsWith(t, gh, ledger, ledgerRoundOne(), pol)
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.ThreadNodeID != "" {
		t.Fatalf("no thread lookups with the switch off: %+v", row)
	}

	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreadsWith(t, gh, ledger, r2, pol)
	if rep.Fixed != 2 || rep.ThreadsResolved != 0 || len(gh.resolves) != 0 || len(gh.replies) != 0 || gh.listings != 0 {
		t.Fatalf("the ledger still moves, GitHub threads do not: %+v resolves=%v replies=%v listings=%d", rep, gh.resolves, gh.replies, gh.listings)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateFixed {
		t.Fatalf("c1 = %+v", row)
	}
}

func TestThreads_GitHubWithoutAResolverOnlyMovesTheLedger(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishRound(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadsResolved != 0 || len(gh.replies) != 0 {
		t.Fatalf("report = %+v replies=%v", rep, gh.replies)
	}
}

func TestThreads_FailuresAreCountedNotFatal(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())

	gh.resolveErr = errors.New("403")
	r2 := summaryOnly("sha-2")
	r2.Changes = changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	if rep.Fixed != 2 || rep.ThreadsResolved != 0 || rep.ThreadResolveFailures != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.State != db.PublishedStateFixed {
		t.Fatalf("the ledger is written before the thread call: %+v", row)
	}

	gh, ledger = newFakeThreadGitHub(), newFakeLedger()
	gh.listErr = errors.New("502")
	rep = publishThreads(t, gh, ledger, ledgerRoundOne())
	if rep.InlinePosted != 2 {
		t.Fatalf("a failed listing must not block the round: %+v", rep)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row.ThreadNodeID != "" || row.CommentID != 1001 {
		t.Fatalf("c1 = %+v", row)
	}
}

func TestThreadResolutionFromEnv(t *testing.T) {
	for value, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "0": false, "off": false, "NO": false} {
		t.Setenv(ThreadResolutionEnv, value)
		if got := ThreadResolutionFromEnv(); got != want {
			t.Errorf("%s=%q: got %t, want %t", ThreadResolutionEnv, value, got, want)
		}
	}
}

func TestThreads_NoteWordingNamesTheHead(t *testing.T) {
	if got := notSeenNote("abcdef1234567"); !strings.HasPrefix(got, "Not seen at abcdef1") {
		t.Fatalf("note = %q", got)
	}
}
