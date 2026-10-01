package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	gh "pr-review-server/github"
	"pr-review-server/pkg/approval"

	"github.com/stretchr/testify/require"
)

type approvalAPIFixture struct {
	mu                                 sync.Mutex
	head, base, state, author, reviews string
	draft, inaccessible                bool
	reads, writes                      int
}

func newApprovalAPITestServer(t *testing.T) (*Server, *db.GormDB, *db.User, *approvalAPIFixture) {
	t.Helper()
	t.Setenv("APPROVAL_CANDIDATES_ENABLED", "true")
	t.Setenv("APPROVAL_CANDIDATES_PROVIDER", "anthropic")
	t.Setenv("APPROVAL_CANDIDATES_MODEL", "fixture-model")
	t.Setenv("APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS", "12000000")
	t.Setenv("APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS", "800000")
	t.Setenv("APPROVAL_CANDIDATES_PROVIDER_IDENTITIES", "")
	f := &approvalAPIFixture{head: strings.Repeat("a", 40), base: strings.Repeat("b", 40), state: "open", author: "contributor", reviews: "[]"}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			f.writes++
			http.Error(w, "mutation forbidden", 405)
			return
		}
		f.reads++
		if f.inaccessible {
			http.Error(w, "missing", 404)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			fmt.Fprint(w, f.reviews)
			return
		}
		fmt.Fprintf(w, `{"number":1,"state":%q,"draft":%v,"user":{"login":%q},"head":{"sha":%q},"base":{"sha":%q,"repo":{"id":100}}}`, f.state, f.draft, f.author, f.head, f.base)
	}))
	t.Cleanup(remote.Close)
	database, err := db.NewGormSQLite(filepath.Join(t.TempDir(), "approval.db") + "?_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	s := New(&config.Config{GitHubUsername: "reviewer", BaseURL: "https://reviews.example", AnthropicAPIKey: "fixture-key"}, database, gh.NewTestClient(remote.URL, "reviewer"), nil)
	user := createTestUser(t, database, "reviewer")
	for _, number := range []int{1, 2} {
		require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: number, LastCommitSHA: f.head, Author: "contributor", Title: "Example change", Status: "completed"}))
		pr, err := database.GetPR("acme", "example", number)
		require.NoError(t, err)
		ensureUserPRView(t, database, user.ID, pr.ID, false)
	}
	t.Cleanup(func() { f.mu.Lock(); defer f.mu.Unlock(); require.Zero(t, f.writes) })
	return s, database, user, f
}

func approvalAPICall(s *Server, user *db.User, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != nil {
		r = addUserToRequest(r, user)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-PRism-Request", "1")
		r.Header.Set("Origin", "https://reviews.example")
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	switch {
	case path == "/api/v1/approval-capabilities":
		s.handleApprovalCapabilities(w, r)
	case strings.HasPrefix(path, "/api/v1/approval-scans/"):
		s.handleApprovalScanByID(w, r)
	case strings.HasPrefix(path, "/api/v1/approval-scans"):
		s.handleApprovalScans(w, r)
	default:
		s.handleApprovalCandidates(w, r)
	}
	return w
}
func approvalAPIBody(head string, numbers ...int) string {
	targets := []approval.Target{}
	for _, number := range numbers {
		targets = append(targets, approval.Target{Owner: "acme", Repo: "example", Number: number, ExpectedHeadSHA: head})
	}
	return approvalJSON(approvalScanRequest{Targets: targets, Scope: json.RawMessage(`{"search":""}`)})
}
func approvalAPIAdmit(t *testing.T, s *Server, user *db.User, head string, numbers ...int) db.ApprovalScan {
	t.Helper()
	w := approvalAPICall(s, user, "POST", "/api/v1/approval-scans", approvalAPIBody(head, numbers...), approvalID())
	require.Equal(t, 202, w.Code, w.Body.String())
	var scan db.ApprovalScan
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &scan))
	require.Equal(t, "full", scan.Kind)
	return scan
}

func TestApprovalAPIIdempotencyBeforeLiveStateAndCanonicalization(t *testing.T) {
	s, database, user, f := newApprovalAPITestServer(t)
	body := approvalAPIBody(f.head, 2, 1, 1)
	w := approvalAPICall(s, user, "POST", "/api/v1/approval-scans", body, "stable-key")
	require.Equal(t, 202, w.Code, w.Body.String())
	require.NotEmpty(t, w.Header().Get("Location"))
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var original db.ApprovalScan
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &original))
	require.Equal(t, 2, original.Total)
	f.mu.Lock()
	f.head = strings.Repeat("c", 40)
	f.inaccessible = true
	reads := f.reads
	f.mu.Unlock()
	replay := strings.ReplaceAll(approvalAPIBody(strings.Repeat("a", 40), 1, 2), "acme", "ACME")
	w = approvalAPICall(s, user, "POST", "/api/v1/approval-scans", replay, "stable-key")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), original.ID)
	f.mu.Lock()
	require.Equal(t, reads, f.reads)
	f.mu.Unlock()
	w = approvalAPICall(s, user, "POST", "/api/v1/approval-scans", approvalAPIBody(strings.Repeat("c", 40), 1), "stable-key")
	require.Equal(t, 409, w.Code)
	require.Contains(t, w.Body.String(), "idempotency_conflict")
	scans, err := database.ListApprovalScans(user.ID, "", 100, "")
	require.NoError(t, err)
	require.Len(t, scans, 1)
}

func TestApprovalAPIAdmissionAtomicAndStandingApproval(t *testing.T) {
	for _, test := range []struct {
		name    string
		numbers []int
		reviews string
		draft   bool
		head    string
		code    int
	}{
		{name: "missing target", numbers: []int{1, 99}, code: 404},
		{name: "wrong head", numbers: []int{1}, head: strings.Repeat("f", 40), code: 409},
		{name: "draft", numbers: []int{1}, draft: true, code: 422},
		{name: "standing approval survives comment", numbers: []int{1}, reviews: fmt.Sprintf(`[{"id":1,"user":{"login":"reviewer"},"state":"APPROVED","commit_id":%q},{"id":2,"user":{"login":"reviewer"},"state":"COMMENTED"}]`, strings.Repeat("a", 40)), code: 422},
		{name: "later submission wins over creation ID", numbers: []int{1}, reviews: fmt.Sprintf(`[{"id":2,"user":{"login":"reviewer"},"state":"APPROVED","commit_id":%q,"submitted_at":"2026-09-27T12:00:00Z"},{"id":1,"user":{"login":"reviewer"},"state":"CHANGES_REQUESTED","commit_id":%q,"submitted_at":"2026-09-28T12:00:00Z"}]`, strings.Repeat("a", 40), strings.Repeat("a", 40)), code: 202},
		{name: "old approval eligible", numbers: []int{1}, reviews: fmt.Sprintf(`[{"id":1,"user":{"login":"reviewer"},"state":"APPROVED","commit_id":%q}]`, strings.Repeat("f", 40)), code: 202},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, database, user, f := newApprovalAPITestServer(t)
			f.mu.Lock()
			f.draft = test.draft
			if test.reviews != "" {
				f.reviews = test.reviews
			}
			head := f.head
			f.mu.Unlock()
			if test.head != "" {
				head = test.head
			}
			w := approvalAPICall(s, user, "POST", "/api/v1/approval-scans", approvalAPIBody(head, test.numbers...), "admit")
			require.Equal(t, test.code, w.Code, w.Body.String())
			scans, err := database.ListApprovalScans(user.ID, "", 100, "")
			require.NoError(t, err)
			if test.code != 202 {
				require.Empty(t, scans)
			}
		})
	}
}

func TestApprovalAPIWriteBoundaryAndDisabledReads(t *testing.T) {
	s, _, user, f := newApprovalAPITestServer(t)
	body := approvalAPIBody(f.head, 1)
	for _, test := range []struct {
		name, origin, content, custom, body string
		code                                int
	}{
		{name: "cross origin", origin: "https://attacker.example", content: "application/json", custom: "1", body: body, code: 403},
		{name: "origin userinfo", origin: "https://attacker@reviews.example", content: "application/json", custom: "1", body: body, code: 403},
		{name: "origin path", origin: "https://reviews.example/path", content: "application/json", custom: "1", body: body, code: 403},
		{name: "missing custom", content: "application/json", body: body, code: 400},
		{name: "form content", content: "text/plain", custom: "1", body: body, code: 400},
		{name: "unknown field", content: "application/json", custom: "1", body: `{"targets":[],"extra":true}`, code: 400},
		{name: "multiple documents", content: "application/json", custom: "1", body: body + ` {}`, code: 400},
		{name: "oversized", content: "application/json", custom: "1", body: `{"scope":"` + strings.Repeat("x", 33000) + `"}`, code: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/approval-scans", strings.NewReader(test.body))
			r = addUserToRequest(r, user)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Content-Type", test.content)
			r.Header.Set("X-PRism-Request", test.custom)
			r.Header.Set("Idempotency-Key", "key")
			w := httptest.NewRecorder()
			s.handleApprovalScans(w, r)
			require.Equal(t, test.code, w.Code, w.Body.String())
		})
	}
	require.Equal(t, 401, approvalAPICall(s, nil, "GET", "/api/v1/approval-scans", "", "").Code)
	require.Equal(t, 405, approvalAPICall(s, user, "DELETE", "/api/v1/approval-scans", "", "").Code)
	t.Setenv("APPROVAL_CANDIDATES_ENABLED", "false")
	require.Equal(t, 404, approvalAPICall(s, user, "GET", "/api/v1/approval-scans", "", "").Code)
	w := approvalAPICall(s, user, "GET", "/api/v1/approval-capabilities", "", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"enabled":false`)
}

func TestApprovalAPIReadAccessAndHeadFreshness(t *testing.T) {
	s, database, user, f := newApprovalAPITestServer(t)
	scan := approvalAPIAdmit(t, s, user, f.head, 1)
	now := time.Now().UTC()
	target, err := database.ClaimApprovalTarget(db.ApprovalClaim{Worker: "test", Now: now, LeaseDuration: time.Minute, TargetDuration: 3 * time.Minute, MaxSlots: 2})
	require.NoError(t, err)
	require.NoError(t, database.SaveApprovalSnapshot(target.ID, target.LeaseToken, approvalJSON(approval.Snapshot{Revision: approval.Revision{Head: f.head, Base: f.base}}), now))
	until := now.Add(5 * time.Minute)
	require.NoError(t, database.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, db.ApprovalFinalization{ExecutionStatus: "completed", Decision: "candidate", Freshness: "current", AssessmentJSON: approvalJSON(approval.Assessment{Decision: "candidate", Summary: "Fixture evidence"}), ValidatedAt: &now, ValidUntil: &until}))
	path := "/api/v1/approval-scans/" + scan.ID + "/targets/" + target.ID
	other := &db.User{ID: user.ID + 1, GitHubUsername: "other"}
	require.Equal(t, 404, approvalAPICall(s, other, "GET", path, "", "").Code)
	w := approvalAPICall(s, user, "GET", "/api/v1/approval-candidates", "", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"freshness_state":"current"`)
	f.mu.Lock()
	f.head = strings.Repeat("c", 40)
	f.mu.Unlock()
	w = approvalAPICall(s, user, "GET", "/api/v1/approval-candidates", "", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"freshness_state":"stale"`)
	require.Contains(t, w.Body.String(), "head_changed")
	saved, err := database.GetApprovalTarget(user.ID, scan.ID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "current", saved.Freshness)
	t.Setenv("APPROVAL_CANDIDATES_MODEL", "")
	require.Equal(t, 200, approvalAPICall(s, user, "GET", path, "", "").Code)
	_, err = database.DeleteAllUserPRViews(user.ID)
	require.NoError(t, err)
	require.Equal(t, 404, approvalAPICall(s, user, "GET", path, "", "").Code)
	w = approvalAPICall(s, user, "GET", "/api/v1/approval-candidates", "", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"targets":[]`)
}

func TestApprovalAPICancelAndRecheckRequiresExplicitHead(t *testing.T) {
	s, database, user, f := newApprovalAPITestServer(t)
	scan := approvalAPIAdmit(t, s, user, f.head, 1)
	targets, err := database.ListApprovalTargets(user.ID, scan.ID, 25, "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	path := "/api/v1/approval-scans/" + scan.ID
	w := approvalAPICall(s, user, "POST", path+"/cancel", `{}`, "")
	require.Equal(t, 202, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"status":"cancelled"`)
	recheck := path + "/targets/" + targets[0].ID + "/recheck"
	w = approvalAPICall(s, user, "POST", recheck, `{}`, "recheck")
	require.Equal(t, 400, w.Code)
	w = approvalAPICall(s, user, "POST", recheck, fmt.Sprintf(`{"expected_head_sha":%q}`, strings.Repeat("f", 40)), "recheck")
	require.Equal(t, 409, w.Code)
	w = approvalAPICall(s, user, "POST", recheck, fmt.Sprintf(`{"expected_head_sha":%q}`, f.head), "recheck")
	require.Equal(t, 202, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"kind":"recheck"`)
	require.Equal(t, 405, approvalAPICall(s, user, "GET", path+"/cancel", "", "").Code)
}

func TestApprovalAPIAcceptsThirtyFiveTargets(t *testing.T) {
	s, database, user, f := newApprovalAPITestServer(t)
	numbers := make([]int, 35)
	for i := range numbers {
		number := i + 1
		numbers[i] = number
		if number <= 2 {
			continue
		}
		require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: number, LastCommitSHA: f.head, Author: "contributor", Title: "Example change", Status: "completed"}))
		pr, err := database.GetPR("acme", "example", number)
		require.NoError(t, err)
		ensureUserPRView(t, database, user.ID, pr.ID, false)
	}
	scan := approvalAPIAdmit(t, s, user, f.head, numbers...)
	require.Equal(t, 35, scan.Total)
	capabilities := approvalAPICall(s, user, "GET", "/api/v1/approval-capabilities", "", "")
	require.Equal(t, 200, capabilities.Code)
	var response struct {
		MaxTargets int `json:"max_targets"`
	}
	require.NoError(t, json.Unmarshal(capabilities.Body.Bytes(), &response))
	require.Equal(t, db.MaxApprovalTargetsPerScan, response.MaxTargets)
}
