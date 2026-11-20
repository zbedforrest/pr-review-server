package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

type exportPage struct {
	Table      string           `json:"table"`
	Rows       []map[string]any `json:"rows"`
	NextOffset int              `json:"next_offset"`
	Tables     []string         `json:"tables"`
}

func getExport(t *testing.T, s *Server, user *db.User, query string) (*httptest.ResponseRecorder, exportPage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, exportPath+query, nil)
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	s.handleExport(w, req)
	var page exportPage
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	}
	return w, page
}

func seedPublishedFindings(t *testing.T, store *db.GormDB, n int) {
	t.Helper()
	for i := range n {
		require.NoError(t, store.UpsertPublishedFinding(&db.PublishedFinding{
			RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: fmt.Sprintf("a.go:%d:abc", i),
			Kind: "inline", Severity: "MEDIUM", ReviewedSHA: "abc", LastSeenSHA: "abc", State: "open",
			CommentID: int64(100 + i), PublishedAt: time.Now().UTC(), CommentText: "body",
		}))
	}
}

func TestExport_ListsTablesWithoutSecretBearingOnes(t *testing.T) {
	s, _ := newTestServer(t, "tester")
	w, page := getExport(t, s, &db.User{GitHubUsername: "tester"}, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, page.Tables, "published_findings")
	for _, banned := range []string{"users", "sessions", "settings"} {
		assert.NotContains(t, page.Tables, banned)
	}
}

func TestExport_PagesThroughRows(t *testing.T) {
	s, store := newTestServer(t, "tester")
	seedPublishedFindings(t, store, 3)
	user := &db.User{GitHubUsername: "tester"}

	w, page := getExport(t, s, user, "?table=published_findings&limit=2")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, page.Rows, 2)
	assert.Equal(t, 2, page.NextOffset)
	assert.Equal(t, float64(100), page.Rows[0]["comment_id"])

	_, page = getExport(t, s, user, "?table=published_findings&limit=2&offset=2")
	require.Len(t, page.Rows, 1)
	assert.Equal(t, -1, page.NextOffset)
	assert.Equal(t, float64(102), page.Rows[0]["comment_id"])
}

func TestExport_DropsBulkyPRColumns(t *testing.T) {
	s, store := newTestServer(t, "tester")
	require.NoError(t, store.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Title: "t", ReviewRunJSON: "{}", Notes: "n"}))
	_, page := getExport(t, s, &db.User{GitHubUsername: "tester"}, "?table=prs")
	require.Len(t, page.Rows, 1)
	assert.NotContains(t, page.Rows[0], "review_run_json")
	assert.NotContains(t, page.Rows[0], "notes")
	assert.Equal(t, "t", page.Rows[0]["title"])
}

func TestExport_RejectsUnlistedTable(t *testing.T) {
	s, _ := newTestServer(t, "tester")
	w, _ := getExport(t, s, &db.User{GitHubUsername: "tester"}, "?table=sessions")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestExport_RejectsBadOffset(t *testing.T) {
	s, _ := newTestServer(t, "tester")
	w, _ := getExport(t, s, &db.User{GitHubUsername: "tester"}, "?table=prs&offset=-1")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestExport_RequiresAdmin(t *testing.T) {
	s, _ := newNonDevTestServer(t)
	s.cfg.AdminLogins = []string{"alice"}

	w, _ := getExport(t, s, nil, "?table=prs")
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	w, _ = getExport(t, s, &db.User{GitHubUsername: "bob"}, "?table=prs")
	assert.Equal(t, http.StatusForbidden, w.Code)

	w, _ = getExport(t, s, &db.User{GitHubUsername: "alice"}, "?table=prs")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
