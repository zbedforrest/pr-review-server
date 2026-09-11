package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A review run that claims the PR on the current head between outdated
// detection's snapshot and its reset must keep the projection: the reset is
// fenced on the stored commit still being behind, so it is a no-op here.
func TestGormDBOutdatedResetSkipsRowAlreadyOnNewHead(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner  = "acme"
		repo   = "widgets"
		prNum  = 44
		runID  = "run-000000000000000000000000000000cd"
		oldSHA = "0123456789abcdef0123456789abcdef01234567"
		newSHA = "89abcdef0123456789abcdef0123456789abcdef"
	)
	// Prior completed review on the old head.
	require.NoError(t, database.UpsertPR(&PR{
		RepoOwner: owner, RepoName: repo, PRNumber: prNum, LastCommitSHA: oldSHA,
		Status: "completed", ReviewHTMLPath: "old.html",
	}))
	// A new run claims the PR on the new head (this is what the poller's
	// stale snapshot does not see).
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, newSHA, "Title", "alice", nil, false, runID))

	reset, err := database.ResetPRToOutdated(owner, repo, prNum, oldSHA, newSHA)
	require.NoError(t, err)
	assert.False(t, reset, "reset must not apply when the row already carries the new head")

	projected, err := database.SetPRAgentReviewingForReviewRun(owner, repo, prNum, runID)
	require.NoError(t, err)
	assert.True(t, projected, "the claiming run must still own the projection")

	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, "agent_reviewing", model.Status)
	assert.Equal(t, newSHA, model.LastCommitSHA)
	assert.Equal(t, runID, model.ProjectionRunID)

	// A genuinely newer head still resets and invalidates the projection.
	const newerSHA = "fedcba9876543210fedcba9876543210fedcba98"
	reset, err = database.ResetPRToOutdated(owner, repo, prNum, newSHA, newerSHA)
	require.NoError(t, err)
	assert.True(t, reset)
	projected, err = database.SetPRAgentReviewingForReviewRun(owner, repo, prNum, runID)
	require.NoError(t, err)
	assert.False(t, projected)
}

func TestResetPRToOutdated_DoesNotResetARowThatMovedToALaterHead(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()

	const (
		owner    = "acme"
		repo     = "example"
		prNum    = 45
		runID    = "run-000000000000000000000000000000ce"
		snapshot = "0123456789abcdef0123456789abcdef01234567"
		fetched  = "89abcdef0123456789abcdef0123456789abcdef"
		later    = "fedcba9876543210fedcba9876543210fedcba98"
	)
	require.NoError(t, database.UpsertPR(&PR{
		RepoOwner: owner, RepoName: repo, PRNumber: prNum, LastCommitSHA: snapshot, Status: "completed",
	}))
	// Between the poller's GitHub fetch (which saw `fetched`) and its reset, a
	// run claimed an even newer head.
	require.NoError(t, database.SetPRGeneratingForReviewRun(owner, repo, prNum, later, "Title", "alice", nil, false, runID))

	reset, err := database.ResetPRToOutdated(owner, repo, prNum, snapshot, fetched)
	require.NoError(t, err)
	assert.False(t, reset, "the row no longer carries the snapshot head, so the poller's view is stale")

	var model PRModel
	require.NoError(t, database.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNum).First(&model).Error)
	assert.Equal(t, later, model.LastCommitSHA)
	assert.Equal(t, runID, model.ProjectionRunID)
}
