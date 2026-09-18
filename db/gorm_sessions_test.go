package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createSessionForTest(t *testing.T, database *GormDB) *Session {
	t.Helper()
	user := &User{GitHubID: 777, GitHubUsername: "alice"}
	require.NoError(t, database.CreateUser(user))
	session := &Session{ID: "sess-1", UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, database.CreateSession(session))
	return session
}

func TestUpdateSessionGitHubToken_RoundTrip(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	session := createSessionForTest(t, database)

	expires := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, database.UpdateSessionGitHubToken(session.ID, "v1:access", "v1:refresh", &expires))

	got, err := database.GetSession(session.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "v1:access", got.GitHubTokenEnc)
	assert.Equal(t, "v1:refresh", got.GitHubRefreshTokenEnc)
	require.NotNil(t, got.GitHubTokenExpiresAt)
	assert.True(t, expires.Equal(*got.GitHubTokenExpiresAt))

	require.NoError(t, database.UpdateSessionGitHubToken(session.ID, "", "", nil))
	got, err = database.GetSession(session.ID)
	require.NoError(t, err)
	assert.Empty(t, got.GitHubTokenEnc)
	assert.Empty(t, got.GitHubRefreshTokenEnc)
	assert.Nil(t, got.GitHubTokenExpiresAt)
}

func TestCreateSession_PersistsTokenFields(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	user := &User{GitHubID: 778, GitHubUsername: "bob"}
	require.NoError(t, database.CreateUser(user))
	expires := time.Now().Add(time.Hour)
	require.NoError(t, database.CreateSession(&Session{
		ID: "sess-2", UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour),
		GitHubTokenEnc: "v1:a", GitHubRefreshTokenEnc: "v1:r", GitHubTokenExpiresAt: &expires,
	}))
	got, err := database.GetSession("sess-2")
	require.NoError(t, err)
	assert.Equal(t, "v1:a", got.GitHubTokenEnc)
	assert.Equal(t, "v1:r", got.GitHubRefreshTokenEnc)
	assert.NotNil(t, got.GitHubTokenExpiresAt)
}

func TestDeleteSession_RemovesToken(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	session := createSessionForTest(t, database)
	require.NoError(t, database.UpdateSessionGitHubToken(session.ID, "v1:access", "v1:refresh", nil))

	require.NoError(t, database.DeleteSession(session.ID))

	got, err := database.GetSession(session.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	var count int64
	require.NoError(t, database.db.Model(&SessionModel{}).Where("github_token_enc <> ''").Count(&count).Error)
	assert.Zero(t, count)
}

func TestSetPRGreptileStatus_RoundTripAndValidation(t *testing.T) {
	database := newTestDB(t)
	defer database.Close()
	require.NoError(t, database.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 5, LastCommitSHA: "abc123"}))

	require.NoError(t, database.SetPRGreptileStatus("acme", "example", 5, "abc123", "red"))
	pr, err := database.GetPR("acme", "example", 5)
	require.NoError(t, err)
	assert.Equal(t, "red", pr.GreptileStatus)
	assert.Equal(t, "abc123", pr.GreptileStatusSHA)

	assert.Error(t, database.SetPRGreptileStatus("acme", "example", 5, "abc123", "purple"))
	assert.Error(t, database.SetPRGreptileStatus("acme", "example", 5, "", "green"))

	require.NoError(t, database.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 5, LastCommitSHA: "def456", Title: "moved"}))
	pr, err = database.GetPR("acme", "example", 5)
	require.NoError(t, err)
	assert.Equal(t, "red", pr.GreptileStatus, "poll upserts leave the verdict in place")
	assert.Equal(t, "abc123", pr.GreptileStatusSHA, "readers detect staleness by SHA")
}
