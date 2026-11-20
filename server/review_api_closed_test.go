package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/gcs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storeCompletedRun(t *testing.T, database *db.GormDB, dir, owner, repo string, pr int, sha, runID, body string, acceptedAt time.Time) string {
	t.Helper()
	htmlPath := gcs.ReviewRunFileName(owner, repo, pr, sha, runID)
	completedAt := acceptedAt.Add(time.Minute)
	require.NoError(t, database.CreateReviewRun(&db.ReviewRun{
		RunID: runID, RepoOwner: owner, RepoName: repo, PRNumber: pr, CommitSHA: sha,
		TriggerSource: "auto_ready", Status: db.ReviewRunStatusCompleted,
		RequestedConfigJSON: "{}", EffectiveConfigJSON: "{}", ConfigSourcesJSON: "{}",
		AcceptedAt: acceptedAt, QueuedAt: acceptedAt, CompletedAt: &completedAt,
		HTMLPath: htmlPath, JSONPath: gcs.ReviewRunJSONFileName(owner, repo, pr, sha, runID),
		MediumCount: 1, LowCount: 2,
	}))
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, htmlPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, htmlPath), []byte(body), 0o644))
	return htmlPath
}

func TestReviewAPI_closed_PR_serves_its_last_completed_run(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	storeCompletedRun(t, database, dir, "owner", "repo", 40, "aaa1111deadbeef", "run-a0000000000000000000000000000000", "<html><body>older</body></html>", base)
	latest := storeCompletedRun(t, database, dir, "owner", "repo", 40, "bbb2222deadbeef", "run-b0000000000000000000000000000000", "<html><body>latest</body></html>", base.Add(time.Hour))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/40?format=html")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "latest")
	assert.Contains(t, w.Body.String(), "Reviewed at bbb2222. This pull request is closed")
	assert.Less(t, strings.Index(w.Body.String(), "Reviewed at"), strings.Index(w.Body.String(), "latest"))

	w = doReviewAPIGet(t, server, user, "/api/review/owner/repo/40")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "closed", resp.PRStatus)
	assert.Equal(t, latest, resp.ReviewPath)
	assert.Equal(t, reviewAPICounts{Medium: 1, Low: 2}, resp.Counts)
}

func TestReviewAPI_closed_PR_serves_a_pinned_run(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	const runID = "run-a0000000000000000000000000000000"
	storeCompletedRun(t, database, dir, "owner", "repo", 41, "aaa1111deadbeef", runID, "<html><body>older</body></html>", base)
	storeCompletedRun(t, database, dir, "owner", "repo", 41, "bbb2222deadbeef", "run-b0000000000000000000000000000000", "<html><body>latest</body></html>", base.Add(time.Hour))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/41?format=html&sha=aaa1111deadbeef&run_id="+runID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "older")
}

func TestReviewAPI_closed_PR_rejects_a_run_of_another_PR(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)
	const runID = "run-a0000000000000000000000000000000"
	storeCompletedRun(t, database, dir, "owner", "repo", 42, "aaa1111deadbeef", runID, "<html><body>other</body></html>", time.Now().UTC())

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/43?format=html&sha=aaa1111deadbeef&run_id="+runID)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestReviewAPI_rebased_PR_shows_its_last_run_to_people_and_pending_to_the_API(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)
	const (
		oldSHA = "ccc3333deadbeef"
		newSHA = "ddd4444deadbeef"
		runID  = "run-c0000000000000000000000000000000"
	)
	htmlPath := storeCompletedRun(t, database, dir, "owner", "repo", 44, oldSHA, runID, "<html><body>last run</body></html>", time.Now().UTC())
	runJSON, err := json.Marshal(map[string]string{"run_id": runID, "html_path": htmlPath, "json_path": gcs.ReviewRunJSONFileName("owner", "repo", 44, oldSHA, runID)})
	require.NoError(t, err)
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "owner", RepoName: "repo", PRNumber: 44, LastCommitSHA: oldSHA, Status: "pending"}))
	require.NoError(t, database.SetPRGeneratingForReviewRun("owner", "repo", 44, oldSHA, "t", "a", nil, false, runID))
	projected, err := database.MarkPRCompletedForReviewRun("owner", "repo", 44, runID, runID, oldSHA, gcs.ReviewFileName("owner", "repo", 44, oldSHA), 0, 1, 2, "approve", false, string(runJSON))
	require.NoError(t, err)
	require.True(t, projected)
	applied, err := database.ResetPRToOutdated("owner", "repo", 44, oldSHA, newSHA)
	require.NoError(t, err)
	require.True(t, applied)

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/44?format=html")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "last run")
	assert.Contains(t, w.Body.String(), "Reviewed at ccc3333. The pull request has newer commits that were not reviewed")

	w = doReviewAPIGet(t, server, user, "/api/review/owner/repo/44")
	assert.Equal(t, http.StatusNotFound, w.Code)
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "pending", resp.PRStatus)
}

func TestReviewAPI_rejects_dot_segment_owner_without_a_PR_row(t *testing.T) {
	server, _, user, _ := newReviewAPITestServer(t)
	for _, target := range []string{
		"/api/review/../repo/1?format=html",
		"/api/review/owner/../1?format=html&sha=abc1234&run_id=run-a0000000000000000000000000000000",
		"/api/review/ow%2Fner/repo/1",
	} {
		w := doReviewAPIGet(t, server, user, target)
		assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, w.Code, target)
		assert.NotContains(t, w.Body.String(), "<html", target)
	}
}
