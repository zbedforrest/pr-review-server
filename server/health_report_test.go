package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
)

func seedLease(t *testing.T, database *db.GormDB) {
	t.Helper()
	ok, err := database.TryAcquireOrRenewLeadership("prism-test", 1, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestDailyHealth_JobTokenRunsAndStoresAReport(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret-job-token"
	seedLease(t, database)

	req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
	req.Header.Set("X-Prism-Job-Token", "secret-job-token")
	w := httptest.NewRecorder()
	server.handleDailyHealthJob(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "warn", got["overall"], "an empty day is a quiet day, not a failure")
	assert.Contains(t, got["headline"], "Quiet day")

	rows, err := database.ListHealthReports(5)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "warn", rows[0].Overall)
	assert.True(t, strings.HasPrefix(rows[0].Markdown, "# PRism daily health"))
}

func TestDailyHealth_JobRejectsMissingOrWrongToken(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret-job-token"
	for _, token := range []string{"", "nope"} {
		req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
		if token != "" {
			req.Header.Set("X-Prism-Job-Token", token)
		}
		w := httptest.NewRecorder()
		server.handleDailyHealthJob(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "token %q", token)
	}
	server.cfg.HealthJobToken = ""
	req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
	req.Header.Set("X-Prism-Job-Token", "")
	w := httptest.NewRecorder()
	server.handleDailyHealthJob(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "an unconfigured job token never matches")
}

func TestDailyHealth_ReadReturnsLatestReportsAndMarkdown(t *testing.T) {
	server, database := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret-job-token"
	seedLease(t, database)
	req := httptest.NewRequest(http.MethodPost, "/api/health/daily", nil)
	req.Header.Set("X-Prism-Job-Token", "secret-job-token")
	server.handleDailyHealthJob(httptest.NewRecorder(), req)

	w := httptest.NewRecorder()
	server.handleDailyHealth(w, httptest.NewRequest(http.MethodGet, "/api/health/daily?limit=3", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var got struct {
		Reports []map[string]any `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Reports, 1)
	assert.Equal(t, "warn", got.Reports[0]["overall"])

	w = httptest.NewRecorder()
	server.handleDailyHealth(w, httptest.NewRequest(http.MethodGet, "/api/health/daily?format=md", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown"))
	assert.Contains(t, w.Body.String(), "# PRism daily health")
}
