package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/auth"
	"pr-review-server/db"
)

const (
	qaHead     = "abc1234abc1234abc1234abc1234abc1234abc12"
	qaOtherSHA = "def5678def5678def5678def5678def5678def56"
	qaToken    = "gho_user_secret_token"
)

// fakeGitHubForQuickActions serves the two calls the endpoint makes and
// records the review POST bodies it received.
type fakeGitHubForQuickActions struct {
	srv          *httptest.Server
	head         string
	state        string
	merged       bool
	reviewStatus int
	reviewBody   string
	reviewPosts  atomic.Int32
	lastReview   map[string]any
	lastAuth     string
	failGetWith  int
	draftJSON    string
}

func newFakeGitHub(t *testing.T) *fakeGitHubForQuickActions {
	t.Helper()
	f := &fakeGitHubForQuickActions{head: qaHead, state: "open", reviewStatus: http.StatusOK, draftJSON: "false",
		reviewBody: `{"id": 2233, "state": "APPROVED", "commit_id": "` + qaHead + `", "html_url": "https://github.com/acme/example/pull/123#pullrequestreview-2233"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.lastAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/example/pulls/123":
			if f.failGetWith != 0 {
				w.WriteHeader(f.failGetWith)
				fmt.Fprint(w, `{"message":"nope"}`)
				return
			}
			fmt.Fprintf(w, `{"number":123,"state":%q,"merged":%t,"draft":%s,"head":{"sha":%q}}`, f.state, f.merged, f.draftJSON, f.head)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/example/pulls/123/reviews":
			f.reviewPosts.Add(1)
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &f.lastReview)
			if f.reviewStatus != http.StatusOK {
				w.WriteHeader(f.reviewStatus)
			}
			fmt.Fprint(w, f.reviewBody)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type quickActionEnv struct {
	server   *Server
	database *db.GormDB
	user     *db.User
	pr       *db.PR
	gh       *fakeGitHubForQuickActions
}

func newQuickActionEnv(t *testing.T) *quickActionEnv {
	t.Helper()
	t.Setenv("QUICK_ACTIONS_ENABLED", "true")
	server, database := newTestServer(t, "alice")
	t.Cleanup(func() { database.Close() })
	user := createTestUser(t, database, "alice")
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: "acme", RepoName: "example", PRNumber: 123, LastCommitSHA: qaHead,
		Status: "completed", Title: "Fix race", Author: "bob", PRState: "open",
	}))
	pr, err := database.GetPR("acme", "example", 123)
	require.NoError(t, err)
	ensureUserPRView(t, database, user.ID, pr.ID, false)
	gh := newFakeGitHub(t)
	server.quickActions.apiBase = gh.srv.URL
	return &quickActionEnv{server: server, database: database, user: user, pr: pr, gh: gh}
}

func quickActionBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"owner": "acme", "repo": "example", "number": 123, "action": "approve",
		"body": "LGTM", "expected_head_sha": qaHead, "request_id": "7e1c0000-req-1",
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	return body
}

func (e *quickActionEnv) do(t *testing.T, method string, body map[string]any, src auth.GitHubTokenSource, mutate func(*http.Request)) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, quickActionPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if e.user != nil {
		req = addUserToRequest(req, e.user)
	}
	if src != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.GitHubTokenContextKey, src))
	}
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	e.server.handleQuickAction(w, req)
	var parsed map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	return w, parsed
}

func sessionToken() auth.GitHubTokenSource { return auth.StaticGitHubTokenSource(qaToken, "session") }

func TestQuickAction_404WhenFlagOff(t *testing.T) {
	env := newQuickActionEnv(t)
	t.Setenv("QUICK_ACTIONS_ENABLED", "")
	w, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	w, _ = env.do(t, http.MethodGet, nil, sessionToken(), nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestQuickAction_405NonPost(t *testing.T) {
	env := newQuickActionEnv(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w, _ := env.do(t, m, nil, sessionToken(), nil)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, m)
	}
}

func TestQuickAction_400Validation(t *testing.T) {
	env := newQuickActionEnv(t)
	cases := map[string]map[string]any{
		"bad owner":            {"owner": "acme/evil"},
		"empty repo":           {"repo": ""},
		"zero number":          {"number": 0},
		"unknown action":       {"action": "merge"},
		"request_changes body": {"action": "request_changes", "body": "   "},
		"comment body":         {"action": "comment", "body": nil},
		"body too long":        {"body": strings.Repeat("x", quickActionMaxBodyLen+1)},
		"bad sha":              {"expected_head_sha": "not-hex"},
		"short sha":            {"expected_head_sha": "abc1234"},
		"short request id":     {"request_id": "abc"},
		"request id chars":     {"request_id": "abcdefgh!!"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			w, got := env.do(t, http.MethodPost, quickActionBody(overrides), sessionToken(), nil)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, "validation", got["code"])
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		})
	}
	req := httptest.NewRequest(http.MethodPost, quickActionPath, strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	env.server.handleQuickAction(w, addUserToRequest(req, env.user))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Zero(t, env.gh.reviewPosts.Load())
}

func TestQuickAction_428WithoutToken(t *testing.T) {
	env := newQuickActionEnv(t)
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, w.Code)
	assert.Equal(t, "reauth_required", got["code"])

	w, got = env.do(t, http.MethodPost, quickActionBody(nil), auth.StaticGitHubTokenSource("", "session"), nil)
	assert.Equal(t, http.StatusPreconditionRequired, w.Code)
	assert.Equal(t, "reauth_required", got["code"])
	assert.Zero(t, env.gh.reviewPosts.Load())
}

func TestQuickAction_404UnknownPR(t *testing.T) {
	env := newQuickActionEnv(t)
	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"number": 999}), sessionToken(), nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "pr_unknown", got["code"])
}

func TestQuickAction_422OwnPR(t *testing.T) {
	env := newQuickActionEnv(t)
	require.NoError(t, env.database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 123, LastCommitSHA: qaHead, Author: "Alice", Title: "mine"}))
	for _, action := range []string{"approve", "request_changes"} {
		w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": action, "body": "no"}), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code, action)
		assert.Equal(t, "own_pr", got["code"], action)
	}
	assert.Zero(t, env.gh.reviewPosts.Load())

	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "comment", "body": "note to self"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "success", got["status"])
}

func TestQuickAction_422ClosedPR(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.state, env.gh.merged = "closed", true
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "pr_closed", got["code"])
	assert.Zero(t, env.gh.reviewPosts.Load())

	env.gh.state, env.gh.merged = "closed", false
	w, got = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "pr_closed", got["code"])

	require.NoError(t, env.database.SetPRState("acme", "example", 123, "closed"))
	env.gh.state, env.gh.merged = "open", false
	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-3"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, "a PR reopened on GitHub works before the poller catches up")
	assert.Equal(t, int32(1), env.gh.reviewPosts.Load())
}

func TestQuickAction_428OnlyWhenNoToken_502WhenRefreshUnavailable(t *testing.T) {
	env := newQuickActionEnv(t)
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), &failingSource{err: auth.ErrNoGitHubToken}, nil)
	assert.Equal(t, http.StatusPreconditionRequired, w.Code)
	assert.Equal(t, "reauth_required", got["code"])

	w, got = env.do(t, http.MethodPost, quickActionBody(nil), &failingSource{err: errors.New("token endpoint 503")}, nil)
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Equal(t, "github_error", got["code"], "an intact token that could not refresh is not a sign-in problem")
	assert.Zero(t, env.gh.reviewPosts.Load())
}

type failingSource struct{ err error }

func (s *failingSource) Token(context.Context) (string, error)   { return "", s.err }
func (s *failingSource) Refresh(context.Context) (string, error) { return "", s.err }
func (s *failingSource) Source() string                          { return "session" }

func TestQuickAction_409HeadMoved(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.head = qaOtherSHA
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "head_moved", got["code"])
	details := got["details"].(map[string]any)
	assert.Equal(t, qaOtherSHA, details["head_sha"])
	assert.Zero(t, env.gh.reviewPosts.Load())

	w, got = env.do(t, http.MethodPost, quickActionBody(map[string]any{"expected_head_sha": qaOtherSHA, "request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, qaOtherSHA, got["head_sha"])
	assert.Equal(t, qaOtherSHA, env.gh.lastReview["commit_id"], "the review pins the head the server just read")
}

func TestQuickAction_200ApproveUpdatesReviewStatusAndBroadcasts(t *testing.T) {
	env := newQuickActionEnv(t)
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, map[string]any{
		"status": "success", "action": "approve", "review_id": float64(2233),
		"html_url": "https://github.com/acme/example/pull/123#pullrequestreview-2233",
		"state":    "APPROVED", "head_sha": qaHead, "actor": "alice",
	}, got)
	assert.Equal(t, "Bearer "+qaToken, env.gh.lastAuth)
	assert.Equal(t, "APPROVE", env.gh.lastReview["event"])
	assert.Equal(t, "LGTM", env.gh.lastReview["body"])
	assert.Equal(t, qaHead, env.gh.lastReview["commit_id"])

	view, err := env.database.GetUserPRAssignment(env.user.ID, env.pr.ID)
	require.NoError(t, err)
	assert.Equal(t, "APPROVED", view.MyReviewStatus)

	select {
	case msg := <-env.server.broadcastCh:
		assert.Equal(t, EventPRUpdated, msg.Type)
		assert.Equal(t, env.user.ID, msg.TargetUserID)
		payload := msg.Payload.(map[string]interface{})
		assert.Equal(t, 123, payload["number"])
	default:
		t.Fatal("expected a pr_updated broadcast to the acting user")
	}
}

func TestQuickAction_RequestChangesAndCommentMapEvents(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.reviewBody = `{"id": 5, "state": "CHANGES_REQUESTED", "html_url": "u"}`
	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "request_changes", "body": "please fix", "request_id": "7e1c0000-req-rc"}), sessionToken(), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "REQUEST_CHANGES", env.gh.lastReview["event"])
	assert.Equal(t, "CHANGES_REQUESTED", got["state"])

	env.gh.reviewBody = `{"id": 6, "html_url": "u"}`
	w, got = env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "comment", "body": "fyi", "request_id": "7e1c0000-req-c"}), sessionToken(), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "COMMENT", env.gh.lastReview["event"])
	assert.Equal(t, "COMMENTED", got["state"], "falls back to the action's state when GitHub omits it")
}

func TestQuickAction_ReplaysSameRequestID(t *testing.T) {
	env := newQuickActionEnv(t)
	first, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	require.Equal(t, http.StatusOK, first.Code)
	second, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, second.Code)
	assert.JSONEq(t, first.Body.String(), second.Body.String())
	assert.Equal(t, int32(1), env.gh.reviewPosts.Load(), "the replay never reaches GitHub")

	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "comment", "body": "other"}), sessionToken(), nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "duplicate", got["code"], "a request id reused with another payload is refused, not replayed")
	assert.Equal(t, int32(1), env.gh.reviewPosts.Load())
}

func TestQuickAction_409DuplicateWithinWindow(t *testing.T) {
	env := newQuickActionEnv(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	env.server.quickActions.now = func() time.Time { return now }

	w, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	require.Equal(t, http.StatusOK, w.Code)
	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "duplicate", got["code"])

	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "comment", "body": "different action", "request_id": "7e1c0000-req-3"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, "another action on the same PR is not a duplicate")

	now = now.Add(quickActionDuplicateTTL + time.Second)
	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-4"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, "the window has passed")
	assert.Equal(t, int32(3), env.gh.reviewPosts.Load())
}

func TestQuickAction_FailureReleasesDuplicateWindow(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.head = qaOtherSHA
	w, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	require.Equal(t, http.StatusConflict, w.Code)
	env.gh.head = qaHead
	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, "a rejected attempt must not block the retry")
}

func TestQuickAction_AmbiguousFailureKeepsDuplicateWindow(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.reviewStatus, env.gh.reviewBody = http.StatusBadGateway, `{"message":"boom"}`
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, got["error"], "check the PR on GitHub before retrying")

	env.gh.reviewStatus, env.gh.reviewBody = http.StatusOK, `{"id": 2233, "state": "APPROVED", "html_url": "u"}`
	w, got = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	assert.Equal(t, http.StatusConflict, w.Code, "the review may already exist, so a reflexive retry is held for the window")
	assert.Equal(t, "duplicate", got["code"])
	assert.Equal(t, int32(1), env.gh.reviewPosts.Load())
}

func TestQuickAction_429PerUserRateLimit(t *testing.T) {
	env := newQuickActionEnv(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	env.server.quickActions.now = func() time.Time { return now }
	// Rotating actions keeps each one outside the duplicate window while all
	// thirty land inside the rate window.
	actions := []string{"approve", "request_changes", "comment"}
	for i := 0; i < quickActionRateLimit; i++ {
		now = now.Add(1800 * time.Millisecond)
		w, _ := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": actions[i%3], "body": "c", "request_id": fmt.Sprintf("7e1c0000-req-%03d", i)}), sessionToken(), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	now = now.Add(1800 * time.Millisecond)
	w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": actions[quickActionRateLimit%3], "body": "c", "request_id": "7e1c0000-req-over"}), sessionToken(), nil)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "rate_limited", got["code"])
	assert.NotEmpty(t, got["details"].(map[string]any)["retry_after_seconds"])
	assert.Equal(t, int32(quickActionRateLimit), env.gh.reviewPosts.Load())
}

func TestQuickAction_403AdminOnly(t *testing.T) {
	env := newQuickActionEnv(t)
	env.server.cfg.GitHubAppClientID = "app"
	env.server.cfg.AdminLogins = []string{"carol"}
	t.Setenv("QUICK_ACTIONS_ADMIN_ONLY", "true")
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "no_permission", got["code"])

	env.server.cfg.AdminLogins = []string{"alice"}
	w, _ = env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestQuickAction_CSRFRejectsCrossSiteAndForeignOrigin(t *testing.T) {
	env := newQuickActionEnv(t)
	env.server.cfg.BaseURL = "https://prism.example.com"

	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "no_permission", got["code"])

	w, _ = env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.net") })
	assert.Equal(t, http.StatusForbidden, w.Code)

	w, _ = env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), func(r *http.Request) {
		r.Header.Set("Origin", "https://prism.example.com")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int32(1), env.gh.reviewPosts.Load())
}

func TestQuickAction_MapsGitHubErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{"own pr", 422, `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"]}`, 422, "own_pr"},
		{"validation", 422, `{"message":"Validation Failed","errors":[{"message":"bad commit"}]}`, 422, "validation"},
		{"no permission", 403, `{"message":"Resource not accessible"}`, 403, "no_permission"},
		{"reauth", 401, `{"message":"Bad credentials"}`, 428, "reauth_required"},
		{"server error", 502, `{"message":"boom"}`, 502, "github_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newQuickActionEnv(t)
			env.gh.reviewStatus, env.gh.reviewBody = tc.status, tc.body
			w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tc.wantCode, got["code"])
			view, _ := env.database.GetUserPRAssignment(env.user.ID, env.pr.ID)
			assert.Empty(t, view.MyReviewStatus, "no status change on failure")
		})
	}
}

func TestQuickAction_NoPermissionWhenTokenCannotSeeRepo(t *testing.T) {
	env := newQuickActionEnv(t)
	env.gh.failGetWith = http.StatusNotFound
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "no_permission", got["code"])
	assert.Zero(t, env.gh.reviewPosts.Load())
}

// refreshingSource hands out a dead token first and a live one after Refresh.
type refreshingSource struct {
	tokens   []string
	refreshs int
}

func (s *refreshingSource) Token(context.Context) (string, error) { return s.tokens[0], nil }
func (s *refreshingSource) Refresh(context.Context) (string, error) {
	s.refreshs++
	s.tokens = s.tokens[1:]
	return s.tokens[0], nil
}
func (s *refreshingSource) Source() string { return "session" }

func TestQuickAction_RefreshesTokenAfter401(t *testing.T) {
	env := newQuickActionEnv(t)
	handlerAuth := env.gh.srv.Config.Handler
	env.gh.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer gho_dead" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		handlerAuth.ServeHTTP(w, r)
	})
	src := &refreshingSource{tokens: []string{"gho_dead", "gho_live"}}
	w, got := env.do(t, http.MethodPost, quickActionBody(nil), src, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "success", got["status"])
	assert.Equal(t, 1, src.refreshs)
	assert.Equal(t, "Bearer gho_live", env.gh.lastAuth)
}

func TestQuickAction_NeverLogsBodyOrToken(t *testing.T) {
	env := newQuickActionEnv(t)
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	env.server.quickActions.now = func() time.Time { return now }
	secretBody := "SECRET-REVIEW-BODY-9f8e7d"
	w, _ := env.do(t, http.MethodPost, quickActionBody(map[string]any{"body": secretBody}), sessionToken(), nil)
	require.Equal(t, http.StatusOK, w.Code)
	now = now.Add(time.Minute)
	env.gh.reviewStatus, env.gh.reviewBody = 422, `{"message":"Unprocessable Entity","errors":["Review Can not approve your own pull request"]}`
	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"body": secretBody, "request_id": "7e1c0000-req-2"}), sessionToken(), nil)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	now = now.Add(time.Minute)
	env.gh.head = qaOtherSHA
	w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"body": secretBody, "request_id": "7e1c0000-req-3"}), sessionToken(), nil)
	require.Equal(t, http.StatusConflict, w.Code)

	logs := buf.String()
	assert.Contains(t, logs, "[PR-ACTION] actor=alice user_id="+fmt.Sprint(env.user.ID)+" action=approve pr=acme/example#123 head="+qaHead+" source=session review_id=2233 body_len="+fmt.Sprint(len(secretBody)))
	assert.Contains(t, logs, "failed status=422 code=own_pr")
	assert.Contains(t, logs, "rejected code=head_moved expected="+qaHead+" actual="+qaOtherSHA)
	assert.NotContains(t, logs, secretBody)
	assert.NotContains(t, logs, qaToken)
}

// setDraftReviewState marks the PR a draft on GitHub and in the row, and
// stores the automated verdicts the gate reads.
func setDraftReviewState(t *testing.T, env *quickActionEnv, verdict string, critical int, greptile, greptileSHA string) {
	t.Helper()
	env.gh.draftJSON = "true"
	require.NoError(t, env.database.UpdatePRDraft("acme", "example", 123, true))
	require.NoError(t, env.database.MarkPRCompleted("acme", "example", 123, qaHead, "r.html", critical, 0, 0, verdict, false))
	if greptile != "" {
		_, err := env.database.SetPRGreptileStatus("acme", "example", 123, greptileSHA, greptile, 1)
		require.NoError(t, err)
	}
}

func TestQuickAction_DraftApproveGate(t *testing.T) {
	t.Run("both green approves", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "approve_suggestions", 0, "green", qaHead)
		w, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
	t.Run("prism red", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "request_changes", 0, "green", qaHead)
		w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, "draft_not_green", got["code"])
		details := got["details"].(map[string]any)
		assert.Equal(t, "red", details["prism"])
		assert.Equal(t, "green", details["greptile"])
		assert.Contains(t, got["error"], "PRism not green")
	})
	t.Run("critical finding", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "approve", 1, "green", qaHead)
		w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, "red", got["details"].(map[string]any)["prism"])
	})
	t.Run("greptile red", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "approve", 0, "red", qaHead)
		w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, "red", got["details"].(map[string]any)["greptile"])
		assert.Contains(t, got["error"], "Greptile not green")
	})
	t.Run("greptile absent or stale", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "approve", 0, "", "")
		w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, "absent", got["details"].(map[string]any)["greptile"])

		_, err := env.database.SetPRGreptileStatus("acme", "example", 123, qaOtherSHA, "green", 1)
		require.NoError(t, err)
		w, got = env.do(t, http.MethodPost, quickActionBody(map[string]any{"request_id": "7e1c0000-req-2"}), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Equal(t, "absent", got["details"].(map[string]any)["greptile"], "a verdict for another head does not count")
	})
	t.Run("github draft flag wins over a stale row", func(t *testing.T) {
		env := newQuickActionEnv(t)
		require.NoError(t, env.database.MarkPRCompleted("acme", "example", 123, qaHead, "r.html", 0, 0, 0, "request_changes", false))
		env.gh.draftJSON = "true"
		w, got := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
		assert.Equal(t, "draft_not_green", got["code"])
		assert.Zero(t, env.gh.reviewPosts.Load())
	})
	t.Run("a row still marked draft does not gate a PR GitHub says is ready", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "request_changes", 3, "red", qaHead)
		env.gh.draftJSON = "false"
		w, _ := env.do(t, http.MethodPost, quickActionBody(nil), sessionToken(), nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
	t.Run("verdicts for an older head do not count", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "approve", 0, "green", qaHead)
		env.gh.head = qaOtherSHA
		w, got := env.do(t, http.MethodPost, quickActionBody(map[string]any{"expected_head_sha": qaOtherSHA}), sessionToken(), nil)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
		assert.Equal(t, "draft_not_green", got["code"])
		details := got["details"].(map[string]any)
		assert.Equal(t, "absent", details["prism"])
		assert.Equal(t, "absent", details["greptile"])
		assert.Zero(t, env.gh.reviewPosts.Load())
	})
	t.Run("request changes and comment stay allowed", func(t *testing.T) {
		env := newQuickActionEnv(t)
		setDraftReviewState(t, env, "request_changes", 2, "red", qaHead)
		env.gh.reviewBody = `{"id": 5, "state": "CHANGES_REQUESTED", "html_url": "u"}`
		w, _ := env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "request_changes", "body": "fix"}), sessionToken(), nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w, _ = env.do(t, http.MethodPost, quickActionBody(map[string]any{"action": "comment", "body": "note", "request_id": "7e1c0000-req-2"}), sessionToken(), nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
}

func TestGreptileStatusFor(t *testing.T) {
	assert.Equal(t, "absent", greptileStatusFor(db.PR{LastCommitSHA: qaHead}))
	assert.Equal(t, "green", greptileStatusFor(db.PR{LastCommitSHA: qaHead, GreptileStatus: "green", GreptileStatusSHA: qaHead}))
	assert.Equal(t, "red", greptileStatusFor(db.PR{LastCommitSHA: qaHead, GreptileStatus: "red", GreptileStatusSHA: qaHead[:7]}), "short SHA matches")
	assert.Equal(t, "absent", greptileStatusFor(db.PR{LastCommitSHA: qaHead, GreptileStatus: "green", GreptileStatusSHA: qaOtherSHA}))
}

func TestPRResponse_CarriesGreptileStatus(t *testing.T) {
	server, database := newTestServer(t, "alice")
	defer database.Close()
	user := createTestUser(t, database, "alice")
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 9, LastCommitSHA: qaHead, Title: "t", Author: "bob"}))
	pr, _ := database.GetPR("acme", "example", 9)
	ensureUserPRView(t, database, user.ID, pr.ID, false)

	resp := server.getPRResponseForUser(user.ID, "acme", "example", 9)
	require.NotNil(t, resp)
	assert.Equal(t, "absent", resp.GreptileStatus)

	_, err := database.SetPRGreptileStatus("acme", "example", 9, qaHead, "green", 1)
	require.NoError(t, err)
	resp = server.getPRResponseForUser(user.ID, "acme", "example", 9)
	assert.Equal(t, "green", resp.GreptileStatus)

	w := httptest.NewRecorder()
	server.handleGetPRs(w, addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/prs", nil), user))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"greptile_status":"green"`)
}

func TestGetUser_QuickActionFields(t *testing.T) {
	server, _ := newNonDevTestServer(t)
	user := &db.User{ID: 1, GitHubUsername: "alice"}

	got := getUserAs(t, server, user)
	assert.Equal(t, false, got["quick_actions_enabled"])
	assert.Equal(t, false, got["github_actions_available"])

	t.Setenv("QUICK_ACTIONS_ENABLED", "true")
	w := httptest.NewRecorder()
	req := addUserToRequest(httptest.NewRequest(http.MethodGet, "/api/user", nil), user)
	req = req.WithContext(context.WithValue(req.Context(), auth.GitHubTokenContextKey, sessionToken()))
	server.handleGetUser(w, req)
	var withToken map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &withToken))
	assert.Equal(t, true, withToken["quick_actions_enabled"])
	assert.Equal(t, true, withToken["github_actions_available"])
	assert.NotContains(t, w.Body.String(), qaToken)

	t.Setenv("QUICK_ACTIONS_ADMIN_ONLY", "true")
	server.cfg.AdminLogins = []string{"carol"}
	got = getUserAs(t, server, user)
	assert.Equal(t, false, got["quick_actions_enabled"], "admin-only hides the feature from members")
	server.cfg.AdminLogins = []string{"alice"}
	got = getUserAs(t, server, user)
	assert.Equal(t, true, got["quick_actions_enabled"])
}

func TestTelemetry_QuickActionEventsAllowed(t *testing.T) {
	for _, action := range []string{"quick_actions_open", "quick_action_approve", "quick_action_request_changes", "quick_action_comment", "quick_action_copy_link", "quick_action_failed"} {
		assert.True(t, allowedActions[action], action)
	}
}
