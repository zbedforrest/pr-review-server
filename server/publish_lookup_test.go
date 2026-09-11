package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func TestGetPRResponseForUser_CarriesPublicationState(t *testing.T) {
	server, database := newTestServer(t, "testuser")
	defer database.Close()
	user := createTestUser(t, database, "testuser")

	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "owner", RepoName: "repo", PRNumber: 1, LastCommitSHA: "abc123", Status: "completed", Title: "t", Author: "a"}))
	prFromDB, err := database.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	ensureUserPRView(t, database, user.ID, prFromDB.ID, false)
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1, Kind: db.PublishedKindSummary,
		Fingerprint: "SUMMARY", ReviewedSHA: "abc123", LastSeenSHA: "abc123", CommentID: 5,
		State: db.PublishedStateOpen, Rounds: 2, PublishedAt: time.Now(),
	}))

	response := server.getPRResponseForUser(user.ID, "owner", "repo", 1)
	require.NotNil(t, response)
	assert.True(t, response.PublishedToGitHub, "websocket payloads must not clear the posted-to-PR badge")
	assert.Equal(t, 2, response.PublishedRounds)
}
