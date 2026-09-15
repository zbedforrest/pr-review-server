package poller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/github"
)

const detachTestSHA = "abc123def456789012345678901234567890abcd"

func blockedReviewPoller(t *testing.T) (*Poller, *MockDatabase, *MockGitHubClient, *blockingReviewGenerator) {
	t.Helper()
	mockDB := NewMockDatabase()
	mockDB.PRs["owner/repo/1"] = &db.PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1, LastCommitSHA: detachTestSHA,
		Status: "pending", Title: "PR", Author: "author",
	}
	mockGH := NewMockGitHubClient()
	mockGH.PRsRequestingReview = []github.PullRequest{}
	mockGH.MyOpenPRs = []github.PullRequest{}
	mockGH.BatchGetPRStateResults = map[string]*github.PRState{
		"owner/repo/1": {Owner: "owner", Repo: "repo", Number: 1, State: "OPEN", HeadRefOid: detachTestSHA},
	}
	gen := &blockingReviewGenerator{started: make(chan int), release: make(chan struct{})}
	p := newTestPollerWithGenerator(mockGH, mockDB, NewMockReviewStorage(), gen)
	return p, mockDB, mockGH, gen
}

func TestProcessPRBatchReturnsAfterAdmissionWhileReviewRuns(t *testing.T) {
	p, mockDB, _, gen := blockedReviewPoller(t)
	prs := []github.PullRequest{{Owner: "owner", Repo: "repo", Number: 1, CommitSHA: detachTestSHA, Title: "PR", Author: "author"}}

	done := make(chan error, 1)
	go func() { done <- p.processPRBatch(context.Background(), prs, true) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("processPRBatch blocked behind the running review")
	}
	assert.True(t, p.IsReviewTracked("owner", "repo", 1), "the batch returns only after the worker owns the PR")

	select {
	case <-gen.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached worker never started the review")
	}
	close(gen.release)
	waitForDetachedReviews(t, p)
	assert.Equal(t, "completed", mockDB.PRs["owner/repo/1"].Status)
}

func TestStartPollGuardClearsWhileReviewStillRuns(t *testing.T) {
	p, mockDB, _, gen := blockedReviewPoller(t)
	p.startPoll(context.Background(), "test")

	select {
	case <-gen.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never admitted the pending PR")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.pollMutex.Lock()
		polling := p.polling
		p.pollMutex.Unlock()
		if !polling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll guard stayed held while the review was running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, p.IsReviewTracked("owner", "repo", 1), "the review keeps running after the poll completed")

	close(gen.release)
	waitForDetachedReviews(t, p)
	assert.Equal(t, "completed", mockDB.PRs["owner/repo/1"].Status)
}
