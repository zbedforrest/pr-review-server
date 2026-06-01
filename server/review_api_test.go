package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/pkg/reviewer/payload"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReviewAPITestServer wires a test server with a real on-disk ReviewsDir
// (so the GCS-miss → local-fallback branch in handleGetReview is what the
// tests actually exercise) and a registered test user.
func newReviewAPITestServer(t *testing.T) (*Server, *db.GormDB, *db.User, string) {
	t.Helper()
	server, database := newTestServer(t, "testuser")
	t.Cleanup(func() { _ = database.Close() })

	dir := t.TempDir()
	server.cfg.ReviewsDir = dir

	user := createTestUser(t, database, "testuser")
	return server, database, user, dir
}

func writeReviewFile(t *testing.T, dir, owner, repo string, pr int, shortSHA, body string) string {
	t.Helper()
	name := owner + "_" + repo + "_" + itoa(pr) + "_" + shortSHA + ".html"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return name
}

// writeSidecarFile drops a .json findings sidecar alongside a .html review
// file. Mirrors what the poller does after MarkPRCompleted.
func writeSidecarFile(t *testing.T, dir, htmlFilename string, pl payload.Payload) {
	t.Helper()
	sidecarName := gcs.ReviewJSONFileName(htmlFilename)
	body, err := json.Marshal(pl)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, sidecarName), body, 0o644))
}

func itoa(n int) string {
	// Avoid pulling strconv into multiple test helpers — Sprintf is plenty fast for tests.
	return jsonNumberToString(n)
}

func jsonNumberToString(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func doReviewAPIGet(t *testing.T, server *Server, user *db.User, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = addUserToRequest(req, user)
	w := httptest.NewRecorder()
	server.handleGetReview(w, req)
	return w
}

// 1. PR not in DB → 404
func TestReviewAPI_404_when_PR_missing(t *testing.T) {
	server, _, user, _ := newReviewAPITestServer(t)

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/999")

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 2. PR exists but review_path empty → 404
func TestReviewAPI_404_when_no_review_yet(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc1234567890",
		Status:        "pending",
	}))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/1")

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 3. Happy path: PR + review file → 200 with full payload, html inlined.
func TestReviewAPI_200_returns_full_payload(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	reviewedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      7,
		LastCommitSHA: "abc1234deadbeef",
		Status:        "pending",
	}))
	// MarkPRCompleted is the production path that populates review_path +
	// importance counts atomically. Use it to lock in the contract.
	filename := writeReviewFile(t, dir, "owner", "repo", 7, "abc1234", "<html>review body</html>")
	writeSidecarFile(t, dir, filename, payload.Payload{
		SchemaVersion: "1",
		Owner:         "owner",
		Repo:          "repo",
		PRNumber:      7,
		CommitSHA:     "abc1234",
		Counts:        payload.Counts{Critical: 1, Medium: 3, Low: 2},
		Findings: []payload.Finding{
			{Severity: "critical", File: "main.go", Line: 42, Comment: "boom", DiffHunk: "@@ -40,3 +40,3 @@\n-old\n+new"},
		},
	})
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 7, "abc1234deadbeef", filename, 1, 3, 2))
	_ = reviewedAt // touched via DB row below

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/7")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "owner", resp.Owner)
	assert.Equal(t, "repo", resp.Repo)
	assert.Equal(t, 7, resp.PRNumber)
	assert.Equal(t, "abc1234", resp.CommitSHA)
	assert.Equal(t, "abc1234deadbeef", resp.HeadSHA)
	assert.False(t, resp.IsStale, "review SHA prefix-matches HEAD SHA")
	assert.Equal(t, filename, resp.ReviewPath)
	assert.Contains(t, resp.ReviewURL, "/reviews/"+filename)
	assert.Equal(t, 1, resp.Counts.Critical)
	assert.Equal(t, 3, resp.Counts.Medium)
	assert.Equal(t, 2, resp.Counts.Low)
	assert.True(t, resp.FindingsAvailable)
	assert.Equal(t, "1", resp.SchemaVersion)
	require.Len(t, resp.Findings, 1)
	assert.Equal(t, "main.go", resp.Findings[0].File)
	assert.Equal(t, 42, resp.Findings[0].Line)
	assert.Equal(t, "boom", resp.Findings[0].Comment)
	assert.Contains(t, resp.Findings[0].DiffHunk, "+new")
	require.NotNil(t, resp.GeneratedAt, "MarkPRCompleted should populate last_reviewed_at")
}

// 4. is_stale flips when the PR's HEAD has moved past the reviewed commit.
func TestReviewAPI_is_stale_when_head_moved(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      8,
		LastCommitSHA: "aaaaaaa1111111",
		Status:        "pending",
	}))
	filename := writeReviewFile(t, dir, "owner", "repo", 8, "aaaaaaa", "<html>old review</html>")
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 8, "aaaaaaa1111111", filename, 0, 0, 0))

	// Now move HEAD forward without regenerating the review.
	require.NoError(t, database.SetPRGenerating("owner", "repo", 8, "bbbbbbb2222222", "", "", nil, false))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/8")
	require.Equal(t, http.StatusOK, w.Code)

	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.IsStale, "review_sha=aaaaaaa, head_sha=bbbbbbb2222222 → stale")
	assert.Equal(t, "aaaaaaa", resp.CommitSHA)
	assert.Equal(t, "bbbbbbb2222222", resp.HeadSHA)
}

// 5. ?sha= pin returns the older review even when DB points at a newer one.
func TestReviewAPI_pin_by_sha(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      9,
		LastCommitSHA: "bbbbbbb2222",
		Status:        "pending",
	}))
	// Two review files on disk, one for each commit, each with its own sidecar.
	oldName := writeReviewFile(t, dir, "owner", "repo", 9, "aaaaaaa", "<html>OLD</html>")
	writeSidecarFile(t, dir, oldName, payload.Payload{
		SchemaVersion: "1",
		Findings:      []payload.Finding{{Severity: "low", File: "f.go", Line: 1, Comment: "OLD"}},
	})
	newName := writeReviewFile(t, dir, "owner", "repo", 9, "bbbbbbb", "<html>NEW</html>")
	writeSidecarFile(t, dir, newName, payload.Payload{
		SchemaVersion: "1",
		Findings:      []payload.Finding{{Severity: "low", File: "f.go", Line: 1, Comment: "NEW"}},
	})
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 9, "bbbbbbb2222", newName, 0, 0, 0))

	// No pin → returns the latest review (per DB).
	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/9")
	require.Equal(t, http.StatusOK, w.Code)
	var latest reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &latest))
	require.Len(t, latest.Findings, 1)
	assert.Equal(t, "NEW", latest.Findings[0].Comment)

	// Pin to the older sha → returns the older file even though DB points at the newer one.
	w = doReviewAPIGet(t, server, user, "/api/review/owner/repo/9?sha=aaaaaaa")
	require.Equal(t, http.StatusOK, w.Code)
	var pinned reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pinned))
	require.Len(t, pinned.Findings, 1)
	assert.Equal(t, "OLD", pinned.Findings[0].Comment)
	assert.Equal(t, "aaaaaaa", pinned.CommitSHA)
}

// 6. ?sha= pin to a sha with no on-disk file → 404.
func TestReviewAPI_pin_by_sha_404_if_missing(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      10,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/10?sha=deadbee")

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 7. Non-integer pr → 400.
func TestReviewAPI_400_bad_pr_number(t *testing.T) {
	server, _, user, _ := newReviewAPITestServer(t)

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/abc")

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// 8. Missing user in context → 401.
func TestReviewAPI_401_without_user(t *testing.T) {
	server, _, _, _ := newReviewAPITestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/review/owner/repo/1", nil)
	w := httptest.NewRecorder()
	server.handleGetReview(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// 9. ?format=html returns raw HTML, not JSON.
func TestReviewAPI_format_html_returns_raw(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      11,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	filename := writeReviewFile(t, dir, "owner", "repo", 11, "abc1234", "<html>raw passthrough</html>")
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 11, "abc1234", filename, 0, 0, 0))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/11?format=html")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "<html>raw passthrough</html>", w.Body.String())
}

// 10. ?format=md returns the compact Markdown export as a download attachment,
// built from the findings sidecar.
func TestReviewAPI_format_md_returns_attachment(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      12,
		LastCommitSHA: "abc1234deadbeef",
		Status:        "pending",
	}))
	filename := writeReviewFile(t, dir, "owner", "repo", 12, "abc1234", "<html>review</html>")
	writeSidecarFile(t, dir, filename, payload.Payload{
		SchemaVersion: "1",
		Owner:         "owner",
		Repo:          "repo",
		PRNumber:      12,
		CommitSHA:     "abc1234",
		Counts:        payload.Counts{Critical: 1, Medium: 0, Low: 0},
		Findings: []payload.Finding{
			{Severity: "critical", File: "main.go", Line: 42, Comment: "boom", DiffHunk: "@@ -40,3 +40,3 @@\n-old\n+new"},
		},
	})
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 12, "abc1234deadbeef", filename, 1, 0, 0))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/12?format=md")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "text/markdown; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, `attachment; filename="owner_repo_12_abc1234.md"`, w.Header().Get("Content-Disposition"))

	body := w.Body.String()
	assert.Contains(t, body, "PR: owner/repo#12")
	assert.Contains(t, body, "Counts: critical=1 medium=0 low=0")
	assert.Contains(t, body, "=== FINDINGS (1) ===")
	assert.Contains(t, body, "--- [CRITICAL] main.go:42 ---")
	assert.Contains(t, body, "boom")
	assert.Contains(t, body, "```diff")
}

// Backward-compat: an older review on disk without a .json sidecar still
// returns 200 + a meaningful envelope, with findings_available=false so the
// CLI can tell the user to regenerate.
func TestReviewAPI_findings_unavailable_when_no_sidecar(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      30,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	// HTML only — no sidecar written.
	filename := writeReviewFile(t, dir, "owner", "repo", 30, "abc1234", "<html>legacy</html>")
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 30, "abc1234", filename, 0, 0, 0))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/30")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.FindingsAvailable, "no sidecar → findings_available=false")
	assert.Empty(t, resp.Findings)
	// HTML view URL is still served so the user has somewhere to look.
	assert.Contains(t, resp.ReviewURL, "/reviews/"+filename)
}

// In-flight: PR exists, status=generating, no review_path → 202 Accepted
// with pr_status / is_in_flight set so the CLI knows to retry.
func TestReviewAPI_202_when_generating_no_prior_review(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      20,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	require.NoError(t, database.SetPRGenerating("owner", "repo", 20, "abc1234", "", "", nil, false))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/20")

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "generating", resp.PRStatus)
	assert.True(t, resp.IsInFlight)
	assert.False(t, resp.FindingsAvailable, "no findings in 202 response")
	assert.Empty(t, resp.Findings)
	assert.Empty(t, resp.ReviewPath)
	assert.Equal(t, "abc1234", resp.HeadSHA)
}

// In-flight: agent_reviewing is also surfaced as in-flight + 202.
func TestReviewAPI_202_when_agent_reviewing(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      21,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	require.NoError(t, database.SetPRGenerating("owner", "repo", 21, "abc1234", "", "", nil, false))
	require.NoError(t, database.SetPRAgentReviewing("owner", "repo", 21))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/21")

	require.Equal(t, http.StatusAccepted, w.Code)
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "agent_reviewing", resp.PRStatus)
	assert.True(t, resp.IsInFlight)
}

// Regenerating with a stale review present → 200 with both the prior html
// AND is_in_flight=true so the consumer knows a fresh one is en route.
func TestReviewAPI_200_with_in_flight_true_when_regenerating(t *testing.T) {
	server, database, user, dir := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      22,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	filename := writeReviewFile(t, dir, "owner", "repo", 22, "abc1234", "<html>previous</html>")
	writeSidecarFile(t, dir, filename, payload.Payload{
		SchemaVersion: "1",
		Findings:      []payload.Finding{{Severity: "low", File: "f.go", Line: 1, Comment: "previous"}},
	})
	require.NoError(t, database.MarkPRCompleted("owner", "repo", 22, "abc1234", filename, 0, 0, 0))
	// User triggers a re-review: poller flips to generating, but review_path
	// stays pointed at the prior html until MarkPRCompleted runs again.
	require.NoError(t, database.SetPRGenerating("owner", "repo", 22, "abc1234", "", "", nil, false))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/22")

	require.Equal(t, http.StatusOK, w.Code)
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "generating", resp.PRStatus)
	assert.True(t, resp.IsInFlight, "consumer must know a fresh review is in flight")
	require.Len(t, resp.Findings, 1, "old review still served while new one runs")
	assert.Equal(t, "previous", resp.Findings[0].Comment)
}

// Error state with no prior review → 424 Failed Dependency, error_message
// surfaced. Distinct from 404 (which is "no review and no attempt") and 202
// (which is "wait, one is coming").
func TestReviewAPI_424_when_error_no_prior_review(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      23,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))
	require.NoError(t, database.SetPRError("owner", "repo", 23, "anthropic api auth failed"))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/23")

	require.Equal(t, http.StatusFailedDependency, w.Code)
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "error", resp.PRStatus)
	assert.False(t, resp.IsInFlight)
	assert.Equal(t, "anthropic api auth failed", resp.ErrorMessage)
	assert.False(t, resp.FindingsAvailable)
	assert.Empty(t, resp.Findings)
}

// 404 path now also returns JSON with pr_status (not plain text), so callers
// can branch uniformly. The earlier test asserts the status code; this asserts
// the body shape.
func TestReviewAPI_404_body_includes_pr_status(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      24,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))

	w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/24")

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	var resp reviewAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "pending", resp.PRStatus)
	assert.False(t, resp.IsInFlight)
	assert.Empty(t, resp.ErrorMessage)
}

// 10. Path traversal via ?sha= is rejected.
func TestReviewAPI_path_traversal_rejected(t *testing.T) {
	server, database, user, _ := newReviewAPITestServer(t)

	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      12,
		LastCommitSHA: "abc1234",
		Status:        "pending",
	}))

	for _, malicious := range []string{
		"../../etc/passwd",
		"abc1234/../../secret",
		"..",
		"/etc/passwd",
	} {
		w := doReviewAPIGet(t, server, user, "/api/review/owner/repo/12?sha="+malicious)
		assert.Equal(t, http.StatusBadRequest, w.Code, "should reject sha=%q", malicious)
	}
}
