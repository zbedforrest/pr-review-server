package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	gh "pr-review-server/github"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerReview_PublishFlagDefaultsTrueAndCanBeDisabled(t *testing.T) {
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"number":1,"head":{"sha":"abcdef1234567890abcdef1234567890abcdef12"}}`)
	}))
	defer mockGH.Close()
	server, database := newTestServerWithGH(t, "testuser", gh.NewTestClient(mockGH.URL, "testuser"))
	defer database.Close()
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "o", RepoName: "r", PRNumber: 1, LastCommitSHA: "old", Status: "completed", Title: "t", Author: "alice"}))
	rec := &publishRecordingPoller{}
	server.poller = rec

	for _, body := range []string{
		`{"owner":"o","repo":"r","number":1}`,
		`{"owner":"o","repo":"r","number":1,"publish":false}`,
	} {
		require.NoError(t, database.UpdatePRStatus("o", "r", 1, "completed"))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/prs/trigger-review", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.handleTriggerReview(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	assert.Equal(t, []bool{true, false}, rec.immediatePublish)
}

// The PR list tells the dashboard which reviews are actually on GitHub so the
// review menu can show it.
func TestHandleGetPRs_PublishedToGitHubFlag(t *testing.T) {
	server, database := newTestServer(t, "me")
	defer database.Close()
	user := createTestUser(t, database, "me")
	for n := 1; n <= 2; n++ {
		require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "Owner", RepoName: "Repo", PRNumber: n, LastCommitSHA: "sha", Status: "completed", Title: "t", Author: "me"}))
		pr, err := database.GetPR("Owner", "Repo", n)
		require.NoError(t, err)
		ensureUserPRView(t, database, user.ID, pr.ID, true)
	}
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "Owner", RepoName: "Repo", PRNumber: 1, Kind: db.PublishedKindSummary, Fingerprint: "summary",
		ReviewedSHA: "sha", LastSeenSHA: "sha", CommentID: 42, State: db.PublishedStateOpen, Rounds: 3, PublishedAt: time.Now(),
	}))

	req := addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/prs", nil), user)
	w := httptest.NewRecorder()
	server.handleGetPRs(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var got []PRResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	byNumber := map[int]PRResponse{}
	for _, p := range got {
		byNumber[p.Number] = p
	}
	assert.True(t, byNumber[1].PublishedToGitHub)
	assert.Equal(t, 3, byNumber[1].PublishedRounds)
	assert.False(t, byNumber[2].PublishedToGitHub)
}
