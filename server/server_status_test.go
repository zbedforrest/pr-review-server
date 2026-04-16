package server

import (
	"context"
	"testing"
	"time"

	"pr-review-server/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildStatusSnapshot_IncludesCurrentStatusAndAbsoluteTimes(t *testing.T) {
	server, database := newTestServer(t, "status-user")
	defer database.Close()

	server.startTime = time.Now().Add(-2 * time.Minute)

	completedAt := time.Now().Add(-5 * time.Minute)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "sha-1",
		Status:         "completed",
		Title:          "Completed PR",
		Author:         "alice",
		LastReviewedAt: &completedAt,
	}))

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "sha-2",
		Status:        "pending",
		Title:         "",
		Author:        "",
	}))

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      3,
		LastCommitSHA: "sha-3",
		Status:        "generating",
		Title:         "Generating PR",
		Author:        "bob",
	}))

	snapshot, err := server.buildStatusSnapshot(context.Background())
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	assert.Equal(t, 1, snapshot.Counts.Completed)
	assert.Equal(t, 1, snapshot.Counts.Pending)
	assert.Equal(t, 1, snapshot.Counts.Generating)
	assert.Equal(t, 0, snapshot.Counts.Error)
	assert.Equal(t, 1, snapshot.MissingMetadataCount)
	assert.False(t, snapshot.ReviewerRunning)
	assert.Equal(t, 0, snapshot.ReviewerDurationSeconds)
	assert.Nil(t, snapshot.ReviewerStartedAtUnix)
	assert.Nil(t, snapshot.NextPollAtUnix)
	assert.Greater(t, snapshot.UptimeSeconds, 0)
	assert.NotZero(t, snapshot.ServerTimeUnix)
	assert.NotZero(t, snapshot.ServerStartedAtUnix)
	assert.NotZero(t, snapshot.Timestamp)

	require.Len(t, snapshot.RecentCompletions, 1)
	assert.Equal(t, 1, snapshot.RecentCompletions[0].Number)
	assert.Equal(t, "owner/repo", snapshot.RecentCompletions[0].Repo)
	assert.NotEmpty(t, snapshot.RecentCompletions[0].ReviewedAt)
}
