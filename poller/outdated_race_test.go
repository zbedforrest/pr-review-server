package poller

import (
	"context"
	"testing"

	"pr-review-server/db"
)

// When the fenced reset reports that the row already carries the new head
// (a review run claimed the PR after the poller's snapshot), outdated
// detection must not kill the tracked review, count the PR as outdated, or
// broadcast a reset.
func TestCheckForOutdatedReviews_RowAlreadyOnNewHead_SkipsKillAndReset(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()

	// Snapshot shows the old head while generating; the DB has since moved
	// to the new head under a live run.
	mockDB.PRs["owner/repo/1"] = &db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "oldcommit123456789012345678901234567890ab",
		Status:        "generating",
	}
	mockDB.ResetPRToOutdatedNoop = true

	mockGH.GetPRHeadSHAResults["owner/repo/1"] = struct {
		SHA string
		Err error
	}{"newcommit987654321098765432109876543210fe", nil}

	poller := newTestPoller(mockGH, mockDB)
	poller.trackReview(context.Background(), "owner", "repo", 1, 99999)

	outdated, err := poller.checkForOutdatedReviews(context.Background())
	if err != nil {
		t.Fatalf("checkForOutdatedReviews returned error: %v", err)
	}
	if outdated != 0 {
		t.Errorf("expected 0 outdated PRs when the reset did not apply, got %d", outdated)
	}
	if len(mockDB.ResetPRToOutdatedCalls) != 1 {
		t.Fatalf("expected exactly 1 fenced reset attempt, got %d", len(mockDB.ResetPRToOutdatedCalls))
	}
	if !poller.isTracked("owner", "repo", 1) {
		t.Errorf("the live review must not be killed when the reset did not apply")
	}
	if got := mockDB.PRs["owner/repo/1"].Status; got != "generating" {
		t.Errorf("PR status must be untouched, got %q", got)
	}
}
