package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pr-review-server/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFindingOutcomesTestServer wires a test server + user with the feature
// flag enabled (per-request env read, so t.Setenv scopes it to the test).
func newFindingOutcomesTestServer(t *testing.T) (*Server, *db.User) {
	t.Helper()
	t.Setenv("FINDING_OUTCOMES_ENABLED", "true")
	server, database := newTestServer(t, "testuser")
	t.Cleanup(func() { _ = database.Close() })
	user := createTestUser(t, database, "testuser")
	return server, user
}

func doFindingOutcomesPost(t *testing.T, server *Server, user *db.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, findingOutcomesPath, strings.NewReader(body))
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	server.handleFindingOutcomes(w, req)
	return w
}

func doFindingOutcomesGet(t *testing.T, server *Server, user *db.User, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, findingOutcomesPath+query, nil)
	if user != nil {
		req = addUserToRequest(req, user)
	}
	w := httptest.NewRecorder()
	server.handleFindingOutcomes(w, req)
	return w
}

const validOutcomeBody = `{
	"owner": "owner", "repo": "repo", "number": 7, "sha": "abc1234",
	"file": "pkg/api/handler.go", "line": 42,
	"comment": "Nil map write: sessions[id] is assigned before the map is initialized",
	"severity": "critical", "provenance": "agent",
	"outcome": "acknowledged", "reason": ""
}`

// Feature flag unset → 404 on every method. The 404 is the frontend's
// feature probe, so this is the contract that keeps default behavior
// identical to a build without the feature.
func TestFindingOutcomes_404_when_disabled(t *testing.T) {
	t.Setenv("FINDING_OUTCOMES_ENABLED", "")
	server, database := newTestServer(t, "testuser")
	t.Cleanup(func() { _ = database.Close() })
	user := createTestUser(t, database, "testuser")

	w := doFindingOutcomesGet(t, server, user, "?owner=owner&repo=repo&number=7")
	assert.Equal(t, http.StatusNotFound, w.Code)

	w = doFindingOutcomesPost(t, server, user, validOutcomeBody)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// No authenticated user on the request → 401, like the other mutating
// /api/prs/* handlers.
func TestFindingOutcomes_401_without_user(t *testing.T) {
	server, _ := newFindingOutcomesTestServer(t)

	w := doFindingOutcomesPost(t, server, nil, validOutcomeBody)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	w = doFindingOutcomesGet(t, server, nil, "?owner=owner&repo=repo&number=7")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestFindingOutcomes_POST_validation(t *testing.T) {
	server, user := newFindingOutcomesTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"unknown outcome", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","file":"f.go","line":1,"comment":"c","outcome":"wontfix"}`},
		{"dismissed without reason", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","file":"f.go","line":1,"comment":"c","outcome":"dismissed","reason":"  "}`},
		{"missing owner", `{"repo":"r","number":1,"sha":"abc1234","file":"f.go","line":1,"comment":"c","outcome":"acknowledged"}`},
		{"bad sha", `{"owner":"o","repo":"r","number":1,"sha":"../etc","file":"f.go","line":1,"comment":"c","outcome":"acknowledged"}`},
		{"missing file", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","line":1,"comment":"c","outcome":"acknowledged"}`},
		{"missing comment", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","file":"f.go","line":1,"outcome":"acknowledged"}`},
		{"negative line", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","file":"f.go","line":-3,"comment":"c","outcome":"acknowledged"}`},
		{"malformed json", `{"owner":`},
		// Oversized values must 400 (client error), not surface Postgres
		// value-too-long failures as 500s: owner/repo are varchar(255) and
		// the fingerprint (file + bucket + hash) is varchar(512).
		{"oversized owner", `{"owner":"` + strings.Repeat("o", 256) + `","repo":"r","number":1,"sha":"abc1234","file":"f.go","line":1,"comment":"c","outcome":"acknowledged"}`},
		{"oversized repo", `{"owner":"o","repo":"` + strings.Repeat("r", 256) + `","number":1,"sha":"abc1234","file":"f.go","line":1,"comment":"c","outcome":"acknowledged"}`},
		{"oversized file path", `{"owner":"o","repo":"r","number":1,"sha":"abc1234","file":"` + strings.Repeat("d/", 300) + `f.py","line":1,"comment":"c","outcome":"acknowledged"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doFindingOutcomesPost(t, server, user, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}

// Happy path: record an acknowledge, list it back, then dismiss the same
// finding — the dismissal overwrites the acknowledge (upsert, not append).
func TestFindingOutcomes_POST_then_GET_roundtrip(t *testing.T) {
	server, user := newFindingOutcomesTestServer(t)

	w := doFindingOutcomesPost(t, server, user, validOutcomeBody)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var postResp struct {
		Status  string             `json:"status"`
		Outcome findingOutcomeJSON `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &postResp))
	assert.Equal(t, "success", postResp.Status)
	assert.Equal(t, "acknowledged", postResp.Outcome.Outcome)
	assert.Equal(t, "testuser", postResp.Outcome.DecidedBy, "decided_by comes from the session, not the body")
	assert.True(t, strings.HasPrefix(postResp.Outcome.Fingerprint, "pkg/api/handler.go:4:"),
		"fingerprint is file:line-bucket:hash, got %q", postResp.Outcome.Fingerprint)

	w = doFindingOutcomesGet(t, server, user, "?owner=owner&repo=repo&number=7")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var listResp struct {
		Outcomes []findingOutcomeJSON `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	require.Len(t, listResp.Outcomes, 1)
	assert.Equal(t, postResp.Outcome.Fingerprint, listResp.Outcomes[0].Fingerprint)
	assert.Equal(t, "critical", listResp.Outcomes[0].Severity)
	assert.Equal(t, "agent", listResp.Outcomes[0].Provenance)
	assert.Equal(t, "abc1234", listResp.Outcomes[0].ReviewedSHA)

	// Re-decide the same finding as dismissed-with-reason.
	dismissed := strings.Replace(validOutcomeBody, `"outcome": "acknowledged", "reason": ""`,
		`"outcome": "dismissed", "reason": "guarded by the nil check above"`, 1)
	w = doFindingOutcomesPost(t, server, user, dismissed)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doFindingOutcomesGet(t, server, user, "?owner=owner&repo=repo&number=7")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	require.Len(t, listResp.Outcomes, 1, "same finding must overwrite, not append")
	assert.Equal(t, "dismissed", listResp.Outcomes[0].Outcome)
	assert.Equal(t, "guarded by the nil check above", listResp.Outcomes[0].Reason)
}

func TestFindingOutcomes_GET_requires_pr_params(t *testing.T) {
	server, user := newFindingOutcomesTestServer(t)

	for _, query := range []string{"", "?owner=o&repo=r", "?owner=o&repo=r&number=0", "?owner=o&number=1"} {
		w := doFindingOutcomesGet(t, server, user, query)
		assert.Equal(t, http.StatusBadRequest, w.Code, "query %q", query)
	}
}

// truncateString backs varchar columns Postgres measures in characters: it
// must clamp by runes and never cut a multibyte rune mid-encoding (which
// Postgres would reject as invalid UTF-8, turning the POST into a 500).
func TestTruncateString_RuneSafe(t *testing.T) {
	assert.Equal(t, "abc", truncateString("abc", 32), "short strings pass through")
	assert.Equal(t, "abcd", truncateString("abcdef", 4), "ASCII clamps at n")

	// "héllo-wörld" repeated crosses a byte boundary mid-rune under a byte
	// clamp; the rune clamp must yield valid UTF-8 of at most n runes.
	s := strings.Repeat("é", 20) // 40 bytes, 20 runes
	got := truncateString(s, 32)
	assert.Equal(t, strings.Repeat("é", 20), got, "20 runes fit within a 32-char column")
	got = truncateString(s, 7)
	assert.Equal(t, strings.Repeat("é", 7), got)
	assert.True(t, strings.ToValidUTF8(got, "") == got, "truncation must not produce invalid UTF-8")
}
