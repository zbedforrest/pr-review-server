package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pr-review-server/db"

	"golang.org/x/oauth2"
)

const testSessionSecret = "unit-test-session-secret"

func TestTokenCrypt_RoundTripAndTamper(t *testing.T) {
	sealed, err := SealToken(testSessionSecret, "gho_secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1:") || strings.Contains(sealed, "gho_secret") {
		t.Fatalf("sealed form %q leaks or lacks version", sealed)
	}
	again, _ := SealToken(testSessionSecret, "gho_secret")
	if again == sealed {
		t.Fatal("expected a fresh nonce per seal")
	}
	got, err := OpenToken(testSessionSecret, sealed)
	if err != nil || got != "gho_secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := OpenToken("other-secret", sealed); err == nil {
		t.Fatal("expected a rotated secret to fail")
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := OpenToken(testSessionSecret, tampered); err == nil {
		t.Fatal("expected tampered ciphertext to fail")
	}
	if _, err := OpenToken(testSessionSecret, "v9:abc"); err == nil {
		t.Fatal("expected unknown version to fail")
	}
	if empty, err := SealToken(testSessionSecret, ""); err != nil || empty != "" {
		t.Fatalf("empty plaintext = %q, %v", empty, err)
	}
	if _, err := SealToken("", "tok"); err == nil {
		t.Fatal("expected empty secret to fail")
	}
}

// fakeTokenEndpoint emulates GitHub's OAuth token endpoint. It answers every
// grant with the configured token and records the grants it saw.
type fakeTokenEndpoint struct {
	srv    *httptest.Server
	calls  atomic.Int32
	grants []string
	status int
	body   map[string]any
	// errorBody, when set, is the JSON returned for a non-200 status.
	errorBody map[string]any
}

func newFakeTokenEndpoint(t *testing.T, mux *http.ServeMux) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{status: http.StatusOK, body: map[string]any{
		"access_token": "gho_new", "refresh_token": "ghr_new", "token_type": "bearer", "expires_in": 28800,
	}}
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		f.grants = append(f.grants, r.Form.Get("grant_type"))
		w.Header().Set("Content-Type", "application/json")
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			body := f.errorBody
			if body == nil {
				body = map[string]any{"message": "unavailable"}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_ = json.NewEncoder(w).Encode(f.body)
	})
	return f
}

// newTokenTestAuth wires an Auth whose GitHub API and OAuth token endpoint
// both point at the given mux.
func newTokenTestAuth(t *testing.T, mockDB *MockDatabase, mux *http.ServeMux) (*Auth, *fakeTokenEndpoint) {
	t.Helper()
	tokens := newFakeTokenEndpoint(t, mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tokens.srv = srv
	authInst := newTestAuth(mockDB)
	authInst.cfg.SessionSecret = testSessionSecret
	authInst.githubAPIBase = srv.URL
	authInst.oauthConfig.Endpoint = oauth2.Endpoint{AuthURL: srv.URL + "/login/oauth/authorize", TokenURL: srv.URL + "/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams}
	return authInst, tokens
}

func sealedSession(t *testing.T, id string, userID int, access, refresh string, expiresAt *time.Time) *db.Session {
	t.Helper()
	enc, err := SealToken(testSessionSecret, access)
	if err != nil {
		t.Fatal(err)
	}
	refreshEnc, err := SealToken(testSessionSecret, refresh)
	if err != nil {
		t.Fatal(err)
	}
	return &db.Session{ID: id, UserID: userID, ExpiresAt: time.Now().Add(time.Hour), GitHubTokenEnc: enc, GitHubRefreshTokenEnc: refreshEnc, GitHubTokenExpiresAt: expiresAt}
}

func TestHandleCallback_StoresEncryptedGitHubToken(t *testing.T) {
	mockDB := newMockDatabase()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_new" {
			t.Errorf("user lookup used %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(GitHubUser{ID: 42, Login: "alice"})
	})
	mux.HandleFunc("/orgs/test-org/members/alice", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	authInst, tokens := newTokenTestAuth(t, mockDB, mux)

	req := httptest.NewRequest("GET", "/auth/callback?state=abc&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "abc"})
	w := httptest.NewRecorder()
	authInst.HandleCallback(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected redirect, got %d body=%s", w.Code, w.Body.String())
	}
	if tokens.grants[0] != "authorization_code" {
		t.Errorf("first grant = %q", tokens.grants[0])
	}
	if len(mockDB.sessions) != 1 {
		t.Fatalf("expected one session, got %d", len(mockDB.sessions))
	}
	for _, s := range mockDB.sessions {
		if s.GitHubTokenEnc == "" || strings.Contains(s.GitHubTokenEnc, "gho_new") {
			t.Fatalf("access token not sealed: %q", s.GitHubTokenEnc)
		}
		if got, _ := OpenToken(testSessionSecret, s.GitHubTokenEnc); got != "gho_new" {
			t.Errorf("stored access = %q", got)
		}
		if got, _ := OpenToken(testSessionSecret, s.GitHubRefreshTokenEnc); got != "ghr_new" {
			t.Errorf("stored refresh = %q", got)
		}
		if s.GitHubTokenExpiresAt == nil || time.Until(*s.GitHubTokenExpiresAt) < 7*time.Hour {
			t.Errorf("expiry not stored: %v", s.GitHubTokenExpiresAt)
		}
	}
}

func runMiddlewareWithSession(t *testing.T, authInst *Auth, sessionID string) (token, source string, ok bool, code int) {
	t.Helper()
	handler := authInst.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, source, ok = GitHubUserToken(r)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/api/user", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return token, source, ok, w.Code
}

func TestMiddleware_AttachesSessionTokenSource(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	far := time.Now().Add(6 * time.Hour)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_live", "ghr_live", &far)

	token, source, ok, code := runMiddlewareWithSession(t, authInst, "s1")
	if code != http.StatusOK || !ok || token != "gho_live" || source != "session" {
		t.Fatalf("got token=%q source=%q ok=%v code=%d", token, source, ok, code)
	}
	if tokens.calls.Load() != 0 || mockDB.tokenUpdates != 0 {
		t.Errorf("a live token must not refresh or persist (calls=%d updates=%d)", tokens.calls.Load(), mockDB.tokenUpdates)
	}
}

func TestMiddleware_SessionWithoutTokenIsNotOK(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, _ := newTokenTestAuth(t, mockDB, http.NewServeMux())
	mockDB.sessions["s1"] = &db.Session{ID: "s1", UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour)}

	_, source, ok, code := runMiddlewareWithSession(t, authInst, "s1")
	if code != http.StatusOK || ok || source != "session" {
		t.Fatalf("got source=%q ok=%v code=%d", source, ok, code)
	}
}

func TestMiddleware_RotatedSecretReadsAsNoToken(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, _ := newTokenTestAuth(t, mockDB, http.NewServeMux())
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_live", "", nil)
	authInst.cfg.SessionSecret = "rotated"

	_, _, ok, code := runMiddlewareWithSession(t, authInst, "s1")
	if code != http.StatusOK || ok {
		t.Fatalf("expected the request to succeed without a token, ok=%v code=%d", ok, code)
	}
}

func TestMiddleware_BearerTokenIsNeverPersisted(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubUser{ID: 42, Login: "alice"})
	})
	authInst, _ := newTokenTestAuth(t, mockDB, mux)

	var token, source string
	var ok bool
	var refreshErr error
	handler := authInst.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, source, ok = GitHubUserToken(r)
		_, refreshErr = GitHubTokenSourceFromRequest(r).Refresh(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/api/user", nil)
	req.Header.Set("Authorization", "Bearer ghp_cli")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !ok || token != "ghp_cli" || source != "bearer" {
		t.Fatalf("got token=%q source=%q ok=%v code=%d", token, source, ok, w.Code)
	}
	if refreshErr != ErrTokenNotRefreshable {
		t.Errorf("bearer refresh = %v", refreshErr)
	}
	if len(mockDB.sessions) != 0 || mockDB.tokenUpdates != 0 {
		t.Errorf("bearer PAT reached storage: sessions=%d updates=%d", len(mockDB.sessions), mockDB.tokenUpdates)
	}
}

func TestDevModeMiddleware_AttachesDevPAT(t *testing.T) {
	mockDB := newMockDatabase()
	authInst := newTestAuth(mockDB)
	authInst.cfg.GitHubToken = "ghp_dev"

	var token, source string
	var ok bool
	handler := authInst.DevModeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, source, ok = GitHubUserToken(r)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/user", nil))
	if !ok || token != "ghp_dev" || source != "dev_pat" {
		t.Fatalf("got token=%q source=%q ok=%v", token, source, ok)
	}
}

func TestTokenSource_RefreshesAndPersists(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	soon := time.Now().Add(5 * time.Minute)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_old", "ghr_old", &soon)

	token, source, ok, code := runMiddlewareWithSession(t, authInst, "s1")
	if code != http.StatusOK || !ok || token != "gho_new" || source != "session" {
		t.Fatalf("got token=%q source=%q ok=%v code=%d", token, source, ok, code)
	}
	if tokens.calls.Load() != 1 || tokens.grants[0] != "refresh_token" {
		t.Fatalf("token endpoint calls=%d grants=%v", tokens.calls.Load(), tokens.grants)
	}
	stored := mockDB.sessions["s1"]
	if got, _ := OpenToken(testSessionSecret, stored.GitHubTokenEnc); got != "gho_new" {
		t.Errorf("persisted access = %q", got)
	}
	if got, _ := OpenToken(testSessionSecret, stored.GitHubRefreshTokenEnc); got != "ghr_new" {
		t.Errorf("persisted refresh = %q", got)
	}
	if stored.GitHubTokenExpiresAt == nil || time.Until(*stored.GitHubTokenExpiresAt) < 7*time.Hour {
		t.Errorf("persisted expiry = %v", stored.GitHubTokenExpiresAt)
	}

	token, _, ok, _ = runMiddlewareWithSession(t, authInst, "s1")
	if !ok || token != "gho_new" || tokens.calls.Load() != 1 {
		t.Fatalf("second request should reuse the persisted token: token=%q ok=%v calls=%d", token, ok, tokens.calls.Load())
	}
}

func TestTokenSource_RefreshRefusedClearsStoredToken(t *testing.T) {
	// GitHub reports a dead refresh token as an OAuth error code, usually in
	// a 200 body; a 400 or 401 without a code is refused too.
	for name, setup := range map[string]func(*fakeTokenEndpoint){
		"error code in 200 body": func(f *fakeTokenEndpoint) {
			f.body = map[string]any{"error": "bad_refresh_token", "error_description": "The refresh token passed is incorrect or expired."}
		},
		"400": func(f *fakeTokenEndpoint) { f.status = http.StatusBadRequest },
		"401": func(f *fakeTokenEndpoint) { f.status = http.StatusUnauthorized },
	} {
		t.Run(name, func(t *testing.T) {
			mockDB := newMockDatabase()
			user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
			_ = mockDB.CreateUser(user)
			authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
			setup(tokens)
			expired := time.Now().Add(-time.Minute)
			mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_old", "ghr_revoked", &expired)

			_, source, ok, code := runMiddlewareWithSession(t, authInst, "s1")
			if code != http.StatusOK || ok || source != "session" {
				t.Fatalf("expected no token, got ok=%v source=%q code=%d", ok, source, code)
			}
			stored := mockDB.sessions["s1"]
			if stored.GitHubTokenEnc != "" || stored.GitHubRefreshTokenEnc != "" || stored.GitHubTokenExpiresAt != nil {
				t.Errorf("stored token not cleared: %+v", stored)
			}
			if mockDB.tokenUpdates != 1 {
				t.Errorf("expected one clearing write, got %d", mockDB.tokenUpdates)
			}
		})
	}
}

func TestTokenSource_TransientEndpointStatusKeepsStoredToken(t *testing.T) {
	for name, setup := range map[string]func(*fakeTokenEndpoint){
		"429": func(f *fakeTokenEndpoint) { f.status = http.StatusTooManyRequests },
		"500": func(f *fakeTokenEndpoint) { f.status = http.StatusInternalServerError },
		"502": func(f *fakeTokenEndpoint) { f.status = http.StatusBadGateway },
		"503 with oauth error code": func(f *fakeTokenEndpoint) {
			f.status = http.StatusServiceUnavailable
			f.errorBody = map[string]any{"error": "temporarily_unavailable"}
		},
		"200 with transient code": func(f *fakeTokenEndpoint) { f.body = map[string]any{"error": "server_error"} },
		"400 with server_error code": func(f *fakeTokenEndpoint) {
			f.status = http.StatusBadRequest
			f.errorBody = map[string]any{"error": "temporarily_unavailable"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mockDB := newMockDatabase()
			user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
			_ = mockDB.CreateUser(user)
			authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
			setup(tokens)
			soon := time.Now().Add(5 * time.Minute)
			mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_old", "ghr_old", &soon)

			_, _, ok, _ := runMiddlewareWithSession(t, authInst, "s1")
			if ok {
				t.Fatal("expected no token while the endpoint is failing")
			}
			if mockDB.tokenUpdates != 0 || mockDB.sessions["s1"].GitHubRefreshTokenEnc == "" {
				t.Fatalf("a %s must not clear the stored token (updates=%d)", name, mockDB.tokenUpdates)
			}

			tokens.status, tokens.errorBody = http.StatusOK, nil
			tokens.body = map[string]any{"access_token": "gho_new", "refresh_token": "ghr_new", "token_type": "bearer", "expires_in": 28800}
			token, _, ok, _ := runMiddlewareWithSession(t, authInst, "s1")
			if !ok || token != "gho_new" {
				t.Fatalf("expected the refresh to succeed once GitHub recovers, got token=%q ok=%v", token, ok)
			}
		})
	}
}

func TestTokenSource_ClearLosesToSiblingRefresh(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	tokens.status = http.StatusBadRequest
	expired := time.Now().Add(-time.Minute)
	stale := sealedSession(t, "s1", user.ID, "gho_old", "ghr_spent", &expired)
	far := time.Now().Add(6 * time.Hour)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_sibling", "ghr_sibling", &far)

	src := authInst.newSessionTokenSource(stale)
	src.loaded, src.token = true, &oauth2.Token{AccessToken: "gho_old", RefreshToken: "ghr_spent", Expiry: expired}
	src.session.GitHubTokenEnc = "v1:not-what-is-stored"

	token, err := src.Refresh(context.Background())
	if err != nil || token != "gho_sibling" {
		t.Fatalf("expected to adopt the sibling's token, got %q, %v", token, err)
	}
	if mockDB.tokenUpdates != 0 || mockDB.sessions["s1"].GitHubTokenEnc == "" {
		t.Errorf("the fenced clear must not erase the sibling's token (updates=%d)", mockDB.tokenUpdates)
	}
}

func TestTokenSource_TransientRefreshFailureKeepsStoredToken(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	soon := time.Now().Add(5 * time.Minute)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_old", "ghr_old", &soon)
	tokens.srv.Close()

	_, _, ok, _ := runMiddlewareWithSession(t, authInst, "s1")
	if ok {
		t.Fatal("expected no token while the endpoint is unreachable")
	}
	if mockDB.tokenUpdates != 0 {
		t.Errorf("a network failure must not clear the stored token (updates=%d)", mockDB.tokenUpdates)
	}
}

func TestTokenSource_ExpiredWithoutRefreshTokenClears(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	expired := time.Now().Add(-time.Minute)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_old", "", &expired)

	_, _, ok, _ := runMiddlewareWithSession(t, authInst, "s1")
	if ok || tokens.calls.Load() != 0 || mockDB.sessions["s1"].GitHubTokenEnc != "" {
		t.Fatalf("ok=%v calls=%d enc=%q", ok, tokens.calls.Load(), mockDB.sessions["s1"].GitHubTokenEnc)
	}
}

func TestTokenSource_ForceRefreshAfter401(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	far := time.Now().Add(6 * time.Hour)
	session := sealedSession(t, "s1", user.ID, "gho_old", "ghr_old", &far)
	mockDB.sessions["s1"] = session

	src := authInst.newSessionTokenSource(session)
	first, err := src.Token(context.Background())
	if err != nil || first != "gho_old" {
		t.Fatalf("first token = %q, %v", first, err)
	}
	second, err := src.Refresh(context.Background())
	if err != nil || second != "gho_new" {
		t.Fatalf("refreshed token = %q, %v", second, err)
	}
	if tokens.calls.Load() != 1 || mockDB.tokenUpdates != 1 {
		t.Errorf("calls=%d updates=%d", tokens.calls.Load(), mockDB.tokenUpdates)
	}
}

func TestTokenSource_AdoptsSiblingRefresh(t *testing.T) {
	mockDB := newMockDatabase()
	user := &db.User{GitHubID: 42, GitHubUsername: "alice"}
	_ = mockDB.CreateUser(user)
	authInst, tokens := newTokenTestAuth(t, mockDB, http.NewServeMux())
	soon := time.Now().Add(5 * time.Minute)
	stale := sealedSession(t, "s1", user.ID, "gho_old", "ghr_old", &soon)
	far := time.Now().Add(6 * time.Hour)
	mockDB.sessions["s1"] = sealedSession(t, "s1", user.ID, "gho_sibling", "ghr_sibling", &far)

	token, err := authInst.newSessionTokenSource(stale).Token(context.Background())
	if err != nil || token != "gho_sibling" {
		t.Fatalf("token = %q, %v", token, err)
	}
	if tokens.calls.Load() != 0 {
		t.Errorf("must not spend the refresh token when a sibling already refreshed (calls=%d)", tokens.calls.Load())
	}
}
