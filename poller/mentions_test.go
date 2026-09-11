package poller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/runconfig"
)

func TestMentionCommandRecognisesReviewRequestsAndNothingElse(t *testing.T) {
	handle := "prism-pr-review-server"
	yes := []string{
		"@prism-pr-review-server review",
		"@Prism-PR-Review-Server please review this",
		"can you take another look? @prism-pr-review-server re-review",
		"@prism-pr-review-server rereview",
	}
	for _, body := range yes {
		if !mentionCommand(body, handle) {
			t.Errorf("%q should trigger", body)
		}
	}
	no := []string{
		"@prism-pr-review-server thanks",
		"@prism-pr-review-server what does this finding mean?",
		"please review @someone-else",
		"the review from @prism-pr-review-serverbot was useful",
		"email me at prism-pr-review-server@example.com to review",
		"> @prism-pr-review-server review\n\nquoting what Zed said above",
		"the review from @prism-pr-review-server was useful",
		"@prism-pr-review-server don't review this yet",
		"@prism-pr-review-server why did the review flag X?",
	}
	for _, body := range no {
		if mentionCommand(body, handle) {
			t.Errorf("%q should not trigger", body)
		}
	}
}

func TestMentionCandidatesUseTheCachedRowsThatMoved(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	t1, t2 := now.Add(-2*time.Minute), now.Add(-time.Minute)
	prs := []*db.PR{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 1, PRState: "open", GitHubUpdatedAt: &t2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 2, PRState: "open", GitHubUpdatedAt: &t1},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 3, PRState: "closed", GitHubUpdatedAt: &t2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 4, PRState: "open"},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 5, PRState: "", GitHubUpdatedAt: &t2},
	}
	last := map[string]time.Time{"acme/example#1": t1, "acme/example#2": t1}
	got := mentionCandidates(prs, last, false, now)
	if len(got) != 3 || got[0].PRNumber != 1 || got[1].PRNumber != 4 || got[2].PRNumber != 5 {
		t.Fatalf("incremental = %+v, want the moved open PR, the one without a cached time, and the legacy empty-state row", got)
	}
	last["acme/example#4"] = now.Add(-time.Minute)
	if got := mentionCandidates(prs, last, false, now); len(got) != 2 {
		t.Fatalf("a PR without a cached time just scanned waits for the rescan timer, got %+v", got)
	}
	last["acme/example#4"] = now.Add(-11 * time.Minute)
	if got := mentionCandidates(prs, last, false, now); len(got) != 3 {
		t.Fatalf("and is re-read once the timer lapses, got %+v", got)
	}
	if got := mentionCandidates(prs, last, true, now); len(got) != 4 {
		t.Fatalf("full scan = %d open PRs, want 4", len(got))
	}
}

func TestMentionTelemetryEvent(t *testing.T) {
	ev := mentionTelemetryEvent(github.PullRequest{Owner: "acme", Repo: "example", Number: 7}, "alice", true, 3)
	if ev.Action != "mention_trigger" || ev.Label != "by=alice publish=true" || ev.PRNumber != 7 || ev.UserID != 3 {
		t.Fatalf("event = %+v", ev)
	}
}

type fakeMentionGH struct {
	comments  []github.IssueCommentInfo
	live      mentionPR
	liveCalls int
	reacted   []int64
	reactions []string
}

func (f *fakeMentionGH) ListIssueComments(context.Context, string, string, int) ([]github.IssueCommentInfo, error) {
	return f.comments, nil
}
func (f *fakeMentionGH) LivePR(context.Context, string, string, int) (mentionPR, error) {
	f.liveCalls++
	return f.live, nil
}
func (f *fakeMentionGH) ReactToIssueComment(_ context.Context, _, _ string, id int64, reaction string) error {
	f.reacted = append(f.reacted, id)
	f.reactions = append(f.reactions, reaction)
	return nil
}

type fakeMentionLedger struct {
	rows map[int64]*db.MentionTrigger
}

func (f *fakeMentionLedger) MentionHandled(id int64, now time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	return r.Queued || r.TriggeredAt.After(now.Add(-10*time.Minute)), nil
}
func (f *fakeMentionLedger) ReserveMention(m *db.MentionTrigger) (bool, error) {
	if f.rows == nil {
		f.rows = map[int64]*db.MentionTrigger{}
	}
	if r, ok := f.rows[m.CommentID]; ok && (r.Queued || r.TriggeredAt.After(m.TriggeredAt.Add(-10*time.Minute))) {
		return false, nil
	}
	cp := *m
	f.rows[m.CommentID] = &cp
	return true, nil
}
func (f *fakeMentionLedger) FinalizeMention(id int64, holder string) (bool, error) {
	if r, ok := f.rows[id]; ok && r.Holder == holder {
		r.Queued = true
		return true, nil
	}
	return false, nil
}
func (f *fakeMentionLedger) ReleaseMention(id int64, holder string) error {
	if r, ok := f.rows[id]; ok && !r.Queued && r.Holder == holder {
		delete(f.rows, id)
	}
	return nil
}

func mentionFixture(admit func(context.Context, github.PullRequest, bool) error) (*mentionScanner, *fakeMentionGH, *fakeMentionLedger, *db.PR) {
	t0 := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	gh := &fakeMentionGH{
		live: mentionPR{Open: true, HeadSHA: "abc1234def", Title: "t", Author: "alice"},
		comments: []github.IssueCommentInfo{
			{ID: 1, Author: "alice", Association: "MEMBER", Body: "@prism-pr-review-server review", CreatedAt: t0.Add(time.Minute)},
			{ID: 2, Author: "stranger", Association: "NONE", Body: "@prism-pr-review-server review", CreatedAt: t0.Add(time.Minute)},
			{ID: 3, Author: "prism[bot]", IsBot: true, Body: "@prism-pr-review-server review", CreatedAt: t0.Add(time.Minute)},
			{ID: 4, Author: "bob", Association: "COLLABORATOR", Body: "@prism-pr-review-server review", CreatedAt: t0.Add(-time.Hour)},
			{ID: 5, Author: "carol", Association: "MEMBER", Body: "thanks @prism-pr-review-server", CreatedAt: t0.Add(time.Minute)},
		},
	}
	ledger := &fakeMentionLedger{}
	var logs []string
	m := &mentionScanner{
		gh: gh, ledger: ledger, handle: "prism-pr-review-server", holder: "me", since: t0,
		admit: func(ctx context.Context, pr github.PullRequest, _ int64, publish bool) error {
			return admit(ctx, pr, publish)
		},
		allowed: func(a string) bool { return a == "alice" },
		now:     func() time.Time { return t0.Add(2 * time.Minute) },
		log:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	pr := &db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 7, PRState: "open", Author: "alice", LastCommitSHA: "stale"}
	return m, gh, ledger, pr
}

func TestMentionScanner_AdmitsOnceAndAcknowledgesOnlyAllowedRequests(t *testing.T) {
	var admitted []github.PullRequest
	m, gh, ledger, pr := mentionFixture(func(_ context.Context, p github.PullRequest, publish bool) error {
		admitted = append(admitted, p)
		if !publish {
			t.Fatalf("alice is in the pilot and the PR is not a draft")
		}
		return nil
	})
	res, err := m.handlePR(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if res.triggered != 1 || len(admitted) != 1 || admitted[0].CommitSHA != "abc1234def" {
		t.Fatalf("only the member's request is admitted, at the live head: res=%+v admitted=%+v", res, admitted)
	}
	if len(gh.reacted) != 1 || gh.reacted[0] != 1 || gh.reactions[0] != "eyes" {
		t.Fatalf("the admitted request gets an eyes reaction and nothing else: reacted=%v reactions=%v", gh.reacted, gh.reactions)
	}
	if !ledger.rows[1].Queued {
		t.Fatalf("the ledger row is finalised: %+v", ledger.rows[1])
	}
	res, _ = m.handlePR(context.Background(), pr)
	if res.triggered != 0 || len(admitted) != 1 {
		t.Fatalf("a handled request is never admitted twice: res=%+v", res)
	}
}

func TestMentionScanner_DefersBehindAnActiveReviewWithoutRecordingIt(t *testing.T) {
	calls := 0
	m, gh, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("%w: acme/example#7", ErrReviewAlreadyTracked)
		}
		return nil
	})
	res, err := m.handlePR(context.Background(), pr)
	if err != nil || res.deferred != 1 || res.triggered != 0 || len(gh.reacted) != 0 || len(ledger.rows) != 0 {
		t.Fatalf("deferred requests leave no trace: err=%v res=%+v reacted=%v rows=%d", err, res, gh.reacted, len(ledger.rows))
	}
	res, _ = m.handlePR(context.Background(), pr)
	if res.triggered != 1 || len(gh.reacted) != 1 {
		t.Fatalf("the next cycle admits it: res=%+v reacted=%v", res, gh.reacted)
	}
}

func TestMentionScanner_TransientAndConfigurationErrorsAreRetried(t *testing.T) {
	m, _, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error { return fmt.Errorf("db down") })
	res, err := m.handlePR(context.Background(), pr)
	if err == nil || res.deferred != 1 || len(ledger.rows) != 0 {
		t.Fatalf("a transient error releases the reservation and surfaces: err=%v res=%+v rows=%d", err, res, len(ledger.rows))
	}
	m, gh, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error {
		return &runconfig.ValidationError{Field: "config.agent.model", Message: "not allowed"}
	})
	res, err = m.handlePR(context.Background(), pr)
	if err != nil || res.deferred != 1 || len(gh.reacted) != 0 || len(ledger.rows) != 0 {
		t.Fatalf("a deployment whose defaults fail validation retries the request once fixed: err=%v res=%+v rows=%d", err, res, len(ledger.rows))
	}
}

func TestMentionScanner_DraftOrOutsidePilotIsAcknowledgedWithoutANote(t *testing.T) {
	var publishSeen []bool
	m, gh, _, pr := mentionFixture(func(_ context.Context, _ github.PullRequest, publish bool) error {
		publishSeen = append(publishSeen, publish)
		return nil
	})
	gh.live.Draft = true
	if _, err := m.handlePR(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	if len(publishSeen) != 1 || publishSeen[0] || len(gh.reactions) != 1 || gh.reactions[0] != "eyes" {
		t.Fatalf("publish=%v reactions=%v", publishSeen, gh.reactions)
	}
}

func TestMentionScanner_IgnoresRequestsOnAPRThatClosedMeanwhile(t *testing.T) {
	m, gh, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error {
		t.Fatal("must not admit a review of a closed PR")
		return nil
	})
	gh.live.Open = false
	res, err := m.handlePR(context.Background(), pr)
	if err != nil || res.triggered != 0 || len(ledger.rows) != 0 {
		t.Fatalf("err=%v res=%+v rows=%d", err, res, len(ledger.rows))
	}
}

func TestMentionScanner_ChecksTheCachedAuthorBeforeAnyLiveRead(t *testing.T) {
	m, gh, _, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error { return nil })
	gh.comments = gh.comments[1:2] // only the stranger
	if _, err := m.handlePR(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	if gh.liveCalls != 0 {
		t.Fatalf("an unauthorised request must not cost a GitHub read, got %d", gh.liveCalls)
	}
	m, gh, _, pr = mentionFixture(func(context.Context, github.PullRequest, bool) error { return nil })
	gh.comments = append(gh.comments, github.IssueCommentInfo{ID: 6, Author: "alice", Association: "MEMBER", Body: "@prism-pr-review-server review", CreatedAt: gh.comments[0].CreatedAt})
	m.handlePR(context.Background(), pr)
	if gh.liveCalls != 1 {
		t.Fatalf("the live PR is read once per PR, got %d", gh.liveCalls)
	}
}

func TestMentionScanner_CoalescesRequestsOnARecentlyReviewedHead(t *testing.T) {
	admitted := 0
	m, gh, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error { admitted++; return nil })
	m.reviewed = func(_, _ string, _ int, head string, _ bool, _ time.Time) (bool, error) {
		return head == "abc1234def", nil
	}
	res, err := m.handlePR(context.Background(), pr)
	if err != nil || admitted != 0 || res.triggered != 0 || len(gh.reactions) != 1 || gh.reactions[0] != "+1" || !ledger.rows[1].Queued {
		t.Fatalf("a fresh review of the same head is answered with a thumbs-up and no new run: err=%v admitted=%d res=%+v reactions=%v row=%+v", err, admitted, res, gh.reactions, ledger.rows[1])
	}
}

func TestMentionScanner_AnAlreadyAdmittedRequestIsFinalisedWithoutASecondRun(t *testing.T) {
	m, gh, ledger, pr := mentionFixture(func(context.Context, github.PullRequest, bool) error {
		return fmt.Errorf("%w: run_id=x", db.ErrReviewRunConflict)
	})
	res, err := m.handlePR(context.Background(), pr)
	if err != nil || res.triggered != 1 || !ledger.rows[1].Queued || len(gh.reacted) != 1 {
		t.Fatalf("err=%v res=%+v row=%+v reacted=%v", err, res, ledger.rows[1], gh.reacted)
	}
}
