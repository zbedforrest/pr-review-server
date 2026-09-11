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

// scoreCompletedPR completes Owner/Repo#n under a run of its own and stores
// score against that run, the only path the fenced setter accepts.
func scoreCompletedPR(t *testing.T, database *db.GormDB, n int, score int) {
	t.Helper()
	runID := fmt.Sprintf("run-000000000000000000000000000000%02d", n)
	require.NoError(t, database.SetPRGeneratingForReviewRun("Owner", "Repo", n, "sha", "t", "me", nil, false, runID))
	completed, err := database.MarkPRCompletedForReviewRun("Owner", "Repo", n, runID, runID, "sha", "review.html", 0, 0, 0, "", false, "")
	require.NoError(t, err)
	require.True(t, completed)
	stored, err := database.SetPRMergeConfidence("Owner", "Repo", n, runID, score)
	require.NoError(t, err)
	require.True(t, stored)
}

// The medal column reads merge_confidence as a number or null, never a
// missing key, so the row can tell "not scored" from "unknown field".
func TestHandleGetPRs_MergeConfidenceIsNumberOrNull(t *testing.T) {
	server, database := newTestServer(t, "me")
	defer database.Close()
	user := createTestUser(t, database, "me")
	for n, status := range map[int]string{1: "completed", 2: "pending", 3: "completed"} {
		require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "Owner", RepoName: "Repo", PRNumber: n, LastCommitSHA: "sha", Status: status, Title: "t", Author: "me"}))
		pr, err := database.GetPR("Owner", "Repo", n)
		require.NoError(t, err)
		ensureUserPRView(t, database, user.ID, pr.ID, true)
	}
	scoreCompletedPR(t, database, 1, 4)
	scoreCompletedPR(t, database, 3, 5)
	require.NoError(t, database.SetPRError("Owner", "Repo", 3, "agent crashed"))

	req := addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/prs", nil), user)
	w := httptest.NewRecorder()
	server.handleGetPRs(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var got []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	byNumber := map[string]map[string]json.RawMessage{}
	for _, p := range got {
		byNumber[string(p["number"])] = p
	}
	assert.JSONEq(t, `4`, string(byNumber["1"]["merge_confidence"]))
	pending, present := byNumber["2"]["merge_confidence"]
	require.True(t, present, "merge_confidence must be emitted even when unscored")
	assert.JSONEq(t, `null`, string(pending))
	assert.JSONEq(t, `null`, string(byNumber["3"]["merge_confidence"]), "a stored score must not surface on a row that is no longer completed")

	ws := server.getPRResponseForUser(user.ID, "Owner", "Repo", 1)
	require.NotNil(t, ws)
	require.NotNil(t, ws.MergeConfidence)
	assert.Equal(t, 4, *ws.MergeConfidence)
	errored := server.getPRResponseForUser(user.ID, "Owner", "Repo", 3)
	require.NotNil(t, errored)
	assert.Nil(t, errored.MergeConfidence)
}
