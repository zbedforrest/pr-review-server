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
	written, err := database.UpdateSessionGitHubToken(session.ID, "", "v1:access", "v1:refresh", &expires)
	require.NoError(t, err)
	assert.True(t, written)

	got, err := database.GetSession(session.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "v1:access", got.GitHubTokenEnc)
	assert.Equal(t, "v1:refresh", got.GitHubRefreshTokenEnc)
	require.NotNil(t, got.GitHubTokenExpiresAt)
	assert.True(t, expires.Equal(*got.GitHubTokenExpiresAt))

	written, err = database.UpdateSessionGitHubToken(session.ID, "v1:stale", "v1:other", "", nil)
	require.NoError(t, err)
	assert.False(t, written, "a write fenced on a superseded ciphertext is refused")

	written, err = database.UpdateSessionGitHubToken(session.ID, "v1:access", "", "", nil)
	require.NoError(t, err)
	assert.True(t, written)
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
	_, err := database.UpdateSessionGitHubToken(session.ID, "", "v1:access", "v1:refresh", nil)
	require.NoError(t, err)

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

	written, err := database.SetPRGreptileStatus("acme", "example", 5, "abc123", "red", 2)
	require.NoError(t, err)
	assert.True(t, written)
	pr, err := database.GetPR("acme", "example", 5)
	require.NoError(t, err)
	assert.Equal(t, "red", pr.GreptileStatus)
	assert.Equal(t, "abc123", pr.GreptileStatusSHA)
	assert.Equal(t, 2, pr.GreptileReviewCount)

	_, err = database.SetPRGreptileStatus("acme", "example", 5, "abc123", "purple", 1)
	assert.Error(t, err)
	_, err = database.SetPRGreptileStatus("acme", "example", 5, "", "green", 1)
	assert.Error(t, err)

	require.NoError(t, database.UpsertPR(&PR{RepoOwner: "acme", RepoName: "example", PRNumber: 5, LastCommitSHA: "def456", Title: "moved"}))
	pr, err = database.GetPR("acme", "example", 5)
	require.NoError(t, err)
	assert.Equal(t, "red", pr.GreptileStatus, "poll upserts leave the verdict in place")
	assert.Equal(t, "abc123", pr.GreptileStatusSHA, "readers detect staleness by SHA")

	written, err = database.SetPRGreptileStatus("acme", "example", 5, "def456", "green", 1)
	require.NoError(t, err)
	assert.True(t, written, "the current head may replace a verdict for an older one")
	written, err = database.SetPRGreptileStatus("acme", "example", 5, "abc123", "red", 3)
	require.NoError(t, err)
	assert.False(t, written, "a late result for an older head never regresses the current verdict")
	pr, _ = database.GetPR("acme", "example", 5)
	assert.Equal(t, "green", pr.GreptileStatus)
	assert.Equal(t, "def456", pr.GreptileStatusSHA)
}
