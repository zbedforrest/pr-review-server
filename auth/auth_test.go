package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
)

// MockDatabase implements db.Database for testing
type MockDatabase struct {
	users    map[int64]*db.User // keyed by GitHub ID
	userByID map[int]*db.User   // keyed by internal ID
	sessions map[string]*db.Session
	nextID   int

	// Error injection
	GetUserByGitHubIDErr error
	GetUserByIDErr       error
	GetUserByUsernameErr error
	CreateUserErr        error
	CreateSessionErr     error
	GetSessionErr        error
	DeleteSessionErr     error
}

func newMockDatabase() *MockDatabase {
	return &MockDatabase{
		users:    make(map[int64]*db.User),
		userByID: make(map[int]*db.User),
		sessions: make(map[string]*db.Session),
		nextID:   1,
	}
}

func (m *MockDatabase) GetUserByGitHubID(githubID int64) (*db.User, error) {
	if m.GetUserByGitHubIDErr != nil {
		return nil, m.GetUserByGitHubIDErr
	}
	return m.users[githubID], nil
}

func (m *MockDatabase) GetUserByID(id int) (*db.User, error) {
	if m.GetUserByIDErr != nil {
		return nil, m.GetUserByIDErr
	}
	return m.userByID[id], nil
}

func (m *MockDatabase) GetUserByUsername(username string) (*db.User, error) {
	if m.GetUserByUsernameErr != nil {
		return nil, m.GetUserByUsernameErr
	}
	for _, u := range m.users {
		if u.GitHubUsername == username {
			return u, nil
		}
	}
	return nil, nil
}

func (m *MockDatabase) GetAllUsers() ([]db.User, error) {
	return []db.User{}, nil
}

func (m *MockDatabase) CreateUser(user *db.User) error {
	if m.CreateUserErr != nil {
		return m.CreateUserErr
	}
	user.ID = m.nextID
	m.nextID++
	m.users[user.GitHubID] = user
	m.userByID[user.ID] = user
	return nil
}

func (m *MockDatabase) UpdateUserLastLogin(userID int) error {
	return nil
}

func (m *MockDatabase) CreateSession(session *db.Session) error {
	if m.CreateSessionErr != nil {
		return m.CreateSessionErr
	}
	m.sessions[session.ID] = session
	return nil
}

func (m *MockDatabase) GetSession(id string) (*db.Session, error) {
	if m.GetSessionErr != nil {
		return nil, m.GetSessionErr
	}
	return m.sessions[id], nil
}

func (m *MockDatabase) DeleteSession(id string) error {
	if m.DeleteSessionErr != nil {
		return m.DeleteSessionErr
	}
	delete(m.sessions, id)
	return nil
}

// Stub implementations for unused methods
func (m *MockDatabase) GetPR(owner, repo string, prNumber int) (*db.PR, error) { return nil, nil }
func (m *MockDatabase) UpsertPR(pr *db.PR) error                               { return nil }
func (m *MockDatabase) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	return nil
}
func (m *MockDatabase) ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error {
	return nil
}
func (m *MockDatabase) SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool) error {
	return nil
}
func (m *MockDatabase) SetPRAgentReviewing(owner, repo string, prNumber int) error { return nil }
func (m *MockDatabase) SetPRError(owner, repo string, prNumber int, message string) error {
	return nil
}
func (m *MockDatabase) MarkPRCompleted(owner, repo string, prNumber int, commitSHA, reviewPath string, critical, medium, low int) error {
	return nil
}
func (m *MockDatabase) GetAllPRs() ([]db.PR, error)                             { return nil, nil }
func (m *MockDatabase) DeletePR(owner, repo string, prNumber int) error         { return nil }
func (m *MockDatabase) ResetStaleGeneratingPRs(timeoutMinutes int) (int, error) { return 0, nil }
func (m *MockDatabase) ResetErrorPRs(maxAgeMinutes int, maxRetries int) (int, error) {
	return 0, nil
}
func (m *MockDatabase) GetPRsWithMissingMetadata() ([]db.PR, error) { return nil, nil }
func (m *MockDatabase) UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error {
	return nil
}
func (m *MockDatabase) UpdatePRNotes(owner, repo string, prNumber int, notes string) error {
	return nil
}
func (m *MockDatabase) GetPRsWithMissingCreatedAt() ([]db.PR, error) { return nil, nil }
func (m *MockDatabase) UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error {
	return nil
}
func (m *MockDatabase) UpdatePRGitHubUpdatedAt(owner, repo string, prNumber int, updatedAt time.Time) error {
	return nil
}
func (m *MockDatabase) GetSetting(key string) (string, error)        { return "", nil }
func (m *MockDatabase) SetSetting(key, value string) error           { return nil }
func (m *MockDatabase) GetAutoReviewRequestedPRs() (bool, error)     { return false, nil }
func (m *MockDatabase) SetAutoReviewRequestedPRs(enabled bool) error { return nil }
func (m *MockDatabase) GetReviewNRequests() (int, error)             { return 0, nil }
func (m *MockDatabase) SetReviewNRequests(n int) error               { return nil }
func (m *MockDatabase) GetGenerateHTML() (bool, error)               { return false, nil }
func (m *MockDatabase) SetGenerateHTML(enabled bool) error           { return nil }
func (m *MockDatabase) GetUserPRAssignment(userID, prID int) (*db.UserPRAssignment, error) {
	return nil, nil
}
func (m *MockDatabase) UpsertUserPRAssignment(assignment *db.UserPRAssignment) error { return nil }
func (m *MockDatabase) GetPRsForUser(userID int) ([]db.PR, error)                    { return nil, nil }
func (m *MockDatabase) GetPRsForUserWithNotes(userID int) ([]db.PRWithUserView, error) {
	return nil, nil
}
func (m *MockDatabase) UpdateUserPRNotes(userID, prID int, notes string) error       { return nil }
func (m *MockDatabase) UpdateUserReviewStatus(userID, prID int, status string) error { return nil }
func (m *MockDatabase) UpdateUserViaTeams(userID, prID int, viaTeams []string) error { return nil }
func (m *MockDatabase) DeleteAllUserPRViews(userID int) (int64, error)               { return 0, nil }
func (m *MockDatabase) HidePRForUser(userID, prID int) error                         { return nil }
func (m *MockDatabase) EnsureUserPRView(userID, prID int, isAuthor bool) error       { return nil }
func (m *MockDatabase) BatchUpsertPRs(prs []*db.PR) error                            { return nil }
func (m *MockDatabase) BatchUpsertUserPRViews(views []db.UserPRViewBatchItem) error  { return nil }
func (m *MockDatabase) MigrateLegacyNotes(userID int) (int, error)                   { return 0, nil }
func (m *MockDatabase) CreateTelemetryEvents(events []db.TelemetryEvent) error       { return nil }
func (m *MockDatabase) GetTelemetryStats(days int) (*db.TelemetryStats, error) {
	return &db.TelemetryStats{}, nil
}
func (m *MockDatabase) Close() error { return nil }

// Test helper to create a test Auth instance
func newTestAuth(database db.Database) *Auth {
	cfg := &config.Config{
		GitHubAppClientID:     "test-client-id",
		GitHubAppClientSecret: "test-client-secret",
		OAuthCallbackURL:      "http://localhost:8080/auth/callback",
		BaseURL:               "http://localhost:8080",
		GitHubOrgName:         "test-org",
		GitHubUsername:        "testuser",
	}
	return NewAuth(cfg, database)
}

// --- generateState tests ---

func TestGenerateState_ReturnsNonEmptyString(t *testing.T) {
	state, err := generateState()
	if err != nil {
		t.Fatalf("generateState() returned error: %v", err)
	}
	if state == "" {
		t.Error("generateState() returned empty string")
	}
}

func TestGenerateState_ReturnsUniqueValues(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		state, err := generateState()
		if err != nil {
			t.Fatalf("generateState() returned error: %v", err)
		}
		if seen[state] {
			t.Errorf("generateState() returned duplicate value: %s", state)
		}
		seen[state] = true
	}
}

func TestGenerateState_ReturnsBase64URLEncoded(t *testing.T) {
	state, err := generateState()
	if err != nil {
		t.Fatalf("generateState() returned error: %v", err)
	}
	// Base64 URL encoding should be 44 characters for 32 bytes (with padding)
	if len(state) != 44 {
		t.Errorf("generateState() returned unexpected length: got %d, want 44", len(state))
	}
}

// --- hashUsername tests ---

func TestHashUsername_Deterministic(t *testing.T) {
	username := "testuser"
	hash1 := hashUsername(username)
	hash2 := hashUsername(username)
	if hash1 != hash2 {
		t.Errorf("hashUsername() not deterministic: %d != %d", hash1, hash2)
	}
}

func TestHashUsername_DifferentUsernamesDifferentHashes(t *testing.T) {
	hash1 := hashUsername("user1")
	hash2 := hashUsername("user2")
	if hash1 == hash2 {
		t.Error("hashUsername() returned same hash for different usernames")
	}
}

func TestHashUsername_ReturnsPositiveValue(t *testing.T) {
	usernames := []string{"test", "user123", "a", "verylongusernamethatmightcauseissues"}
	for _, u := range usernames {
		hash := hashUsername(u)
		// Check that the top bit is cleared (positive in signed context)
		if hash&0x80000000 != 0 {
			t.Errorf("hashUsername(%q) returned value with top bit set: %d", u, hash)
		}
	}
}

// --- HandleLogin tests ---

func TestHandleLogin_RedirectsToGitHub(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()

	auth.HandleLogin(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("expected status %d, got %d", http.StatusTemporaryRedirect, resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location == "" {
		t.Error("expected Location header, got empty")
	}
	if !contains(location, "github.com") {
		t.Errorf("expected redirect to github.com, got %s", location)
	}
}

func TestHandleLogin_SetsStateCookie(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()

	auth.HandleLogin(w, req)

	cookies := w.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == StateCookieName {
			stateCookie = c
			break
		}
	}

	if stateCookie == nil {
		t.Fatal("expected state cookie to be set")
	}
	if stateCookie.Value == "" {
		t.Error("expected state cookie to have a value")
	}
	if !stateCookie.HttpOnly {
		t.Error("expected state cookie to be HttpOnly")
	}
	if stateCookie.MaxAge != 300 {
		t.Errorf("expected state cookie MaxAge 300, got %d", stateCookie.MaxAge)
	}
}

func TestHandleLogin_StateInRedirectMatchesCookie(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()

	auth.HandleLogin(w, req)

	// Get state from cookie
	var stateCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == StateCookieName {
			stateCookie = c
			break
		}
	}

	// Get state from redirect URL (URL-encoded)
	location := w.Result().Header.Get("Location")
	// The state is URL-encoded in the redirect, so we need to check for the encoded value
	// Base64 URL encoding uses = which gets encoded as %3D
	encodedState := strings.ReplaceAll(stateCookie.Value, "=", "%3D")
	if !contains(location, "state="+encodedState) {
		t.Errorf("redirect URL state does not match cookie state.\nCookie state: %s\nEncoded: %s\nLocation: %s", stateCookie.Value, encodedState, location)
	}
}

// --- HandleCallback tests ---

func TestHandleCallback_MissingStateCookie_ReturnsBadRequest(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/callback?state=abc&code=xyz", nil)
	w := httptest.NewRecorder()

	auth.HandleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleCallback_StateMismatch_ReturnsBadRequest(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/callback?state=wrong&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "correct"})
	w := httptest.NewRecorder()

	auth.HandleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleCallback_MissingCode_ReturnsBadRequest(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/callback?state=abc", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "abc"})
	w := httptest.NewRecorder()

	auth.HandleCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleCallback_ClearsStateCookie(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// This will fail at token exchange, but we can check the state cookie is cleared
	req := httptest.NewRequest("GET", "/auth/callback?state=abc&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: StateCookieName, Value: "abc"})
	w := httptest.NewRecorder()

	auth.HandleCallback(w, req)

	// Check that state cookie is cleared
	for _, c := range w.Result().Cookies() {
		if c.Name == StateCookieName && c.MaxAge == -1 {
			return // Cookie was properly cleared
		}
	}
	t.Error("expected state cookie to be cleared")
}

// --- HandleLogout tests ---

func TestHandleLogout_ClearsSessionCookie(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "session-id"})
	w := httptest.NewRecorder()

	// Add session to mock
	mockDB.sessions["session-id"] = &db.Session{
		ID:        "session-id",
		UserID:    1,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	auth.HandleLogout(w, req)

	// Check cookie is cleared
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			if c.MaxAge != -1 {
				t.Errorf("expected session cookie MaxAge -1, got %d", c.MaxAge)
			}
			if c.Value != "" {
				t.Errorf("expected empty session cookie value, got %s", c.Value)
			}
			break
		}
	}
}

func TestHandleLogout_DeletesSessionFromDatabase(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	mockDB.sessions["session-id"] = &db.Session{
		ID:        "session-id",
		UserID:    1,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	req := httptest.NewRequest("GET", "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "session-id"})
	w := httptest.NewRecorder()

	auth.HandleLogout(w, req)

	if _, exists := mockDB.sessions["session-id"]; exists {
		t.Error("expected session to be deleted from database")
	}
}

func TestHandleLogout_RedirectsToLogin(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	req := httptest.NewRequest("GET", "/auth/logout", nil)
	w := httptest.NewRecorder()

	auth.HandleLogout(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected status %d, got %d", http.StatusTemporaryRedirect, w.Code)
	}
	if w.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login, got %s", w.Header().Get("Location"))
	}
}

// --- Middleware tests ---

func TestMiddleware_NoSession_RedirectsToLogin(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected status %d, got %d", http.StatusTemporaryRedirect, w.Code)
	}
	if w.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login, got %s", w.Header().Get("Location"))
	}
}

func TestMiddleware_NoSession_APIReturns401(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/prs", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestMiddleware_ExpiredSession_RedirectsToLogin(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// Create expired session
	mockDB.sessions["expired-session"] = &db.Session{
		ID:        "expired-session",
		UserID:    1,
		ExpiresAt: time.Now().Add(-time.Hour), // Expired
	}

	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "expired-session"})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected status %d, got %d", http.StatusTemporaryRedirect, w.Code)
	}
}

func TestMiddleware_ExpiredSession_ClearsCookie(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// Create expired session
	mockDB.sessions["expired-session"] = &db.Session{
		ID:        "expired-session",
		UserID:    1,
		ExpiresAt: time.Now().Add(-time.Hour), // Expired
	}

	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "expired-session"})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Check cookie is cleared
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge == -1 {
			return // Cookie was properly cleared
		}
	}
	t.Error("expected session cookie to be cleared")
}

func TestMiddleware_ValidSession_AddsUserToContext(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// Create user and session
	user := &db.User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	_ = mockDB.CreateUser(user)
	mockDB.sessions["valid-session"] = &db.Session{
		ID:        "valid-session",
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	var contextUser *db.User
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextUser = GetCurrentUser(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "valid-session"})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if contextUser == nil {
		t.Fatal("expected user in context, got nil")
	}
	if contextUser.GitHubUsername != "testuser" {
		t.Errorf("expected username 'testuser', got '%s'", contextUser.GitHubUsername)
	}
}

// --- GetCurrentUser tests ---

func TestGetCurrentUser_UserInContext_ReturnsUser(t *testing.T) {
	user := &db.User{
		ID:             1,
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}

	req := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(req.Context(), UserContextKey, user)
	req = req.WithContext(ctx)

	result := GetCurrentUser(req)
	if result == nil {
		t.Fatal("expected user, got nil")
	}
	if result.ID != user.ID {
		t.Errorf("expected user ID %d, got %d", user.ID, result.ID)
	}
}

func TestGetCurrentUser_NoUserInContext_ReturnsNil(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	result := GetCurrentUser(req)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

// --- DevModeMiddleware tests ---

func TestDevModeMiddleware_InjectsDevUser(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	var contextUser *db.User
	handler := auth.DevModeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextUser = GetCurrentUser(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if contextUser == nil {
		t.Fatal("expected user in context, got nil")
	}
	if contextUser.GitHubUsername != "testuser" {
		t.Errorf("expected username 'testuser', got '%s'", contextUser.GitHubUsername)
	}
}

func TestDevModeMiddleware_CreatesUserIfNotExists(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	handler := auth.DevModeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Check user was created
	user, _ := mockDB.GetUserByUsername("testuser")
	if user == nil {
		t.Fatal("expected dev user to be created")
	}
}

func TestDevModeMiddleware_ReusesExistingUser(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// Pre-create user
	existingUser := &db.User{
		GitHubID:       int64(hashUsername("testuser")),
		GitHubUsername: "testuser",
	}
	_ = mockDB.CreateUser(existingUser)

	handler := auth.DevModeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Should still have only one user
	if len(mockDB.users) != 1 {
		t.Errorf("expected 1 user, got %d", len(mockDB.users))
	}
}

func TestDevModeMiddleware_NoUsername_ReturnsError(t *testing.T) {
	mockDB := newMockDatabase()
	cfg := &config.Config{
		GitHubAppClientID:     "test-client-id",
		GitHubAppClientSecret: "test-client-secret",
		OAuthCallbackURL:      "http://localhost:8080/auth/callback",
		BaseURL:               "http://localhost:8080",
		GitHubUsername:        "", // No username
	}
	auth := NewAuth(cfg, mockDB)

	handler := auth.DevModeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

// --- fetchGitHubUser tests ---

func TestFetchGitHubUser_Success(t *testing.T) {
	// Create mock GitHub API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		user := GitHubUser{
			ID:        12345,
			Login:     "testuser",
			AvatarURL: "https://github.com/testuser.png",
		}
		_ = json.NewEncoder(w).Encode(user)
	}))
	defer server.Close()

	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)
	// Override httpClient to use test server
	auth.httpClient = server.Client()

	// We can't easily test this without modifying the code to accept a configurable
	// GitHub API URL. For now, this test documents what should be tested.
	// In a real scenario, you'd inject the URL or use dependency injection.
}

func TestFetchGitHubUser_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// Similar to above, documenting test intent
}

// --- verifyOrgMembership tests ---

func TestVerifyOrgMembership_NoOrgConfigured_Succeeds(t *testing.T) {
	mockDB := newMockDatabase()
	cfg := &config.Config{
		GitHubAppClientID:     "test-client-id",
		GitHubAppClientSecret: "test-client-secret",
		OAuthCallbackURL:      "http://localhost:8080/auth/callback",
		BaseURL:               "http://localhost:8080",
		GitHubOrgName:         "", // No org restriction
	}
	auth := NewAuth(cfg, mockDB)

	err := auth.verifyOrgMembership(context.Background(), "token", "anyuser")
	if err != nil {
		t.Errorf("expected no error when no org configured, got: %v", err)
	}
}

// --- NewAuth tests ---

func TestNewAuth_InitializesOAuthConfig(t *testing.T) {
	mockDB := newMockDatabase()
	cfg := &config.Config{
		GitHubAppClientID:     "my-client-id",
		GitHubAppClientSecret: "my-secret",
		OAuthCallbackURL:      "http://localhost:8080/callback",
		BaseURL:               "http://localhost:8080",
	}

	auth := NewAuth(cfg, mockDB)

	if auth.oauthConfig.ClientID != "my-client-id" {
		t.Errorf("expected ClientID 'my-client-id', got '%s'", auth.oauthConfig.ClientID)
	}
	if auth.oauthConfig.ClientSecret != "my-secret" {
		t.Errorf("expected ClientSecret 'my-secret', got '%s'", auth.oauthConfig.ClientSecret)
	}
	if auth.oauthConfig.RedirectURL != "http://localhost:8080/callback" {
		t.Errorf("expected RedirectURL 'http://localhost:8080/callback', got '%s'", auth.oauthConfig.RedirectURL)
	}
}

func TestNewAuth_SetsCorrectScopes(t *testing.T) {
	mockDB := newMockDatabase()
	cfg := &config.Config{
		GitHubAppClientID:     "client-id",
		GitHubAppClientSecret: "secret",
	}

	auth := NewAuth(cfg, mockDB)

	scopes := auth.oauthConfig.Scopes
	if len(scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(scopes))
	}
	if scopes[0] != "read:user" {
		t.Errorf("expected scope 'read:user', got '%s'", scopes[0])
	}
	if scopes[1] != "read:org" {
		t.Errorf("expected scope 'read:org', got '%s'", scopes[1])
	}
}

// --- RequireAuth tests ---

func TestRequireAuth_WrapsHandler(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	// Create user and session
	user := &db.User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	_ = mockDB.CreateUser(user)
	mockDB.sessions["session"] = &db.Session{
		ID:        "session",
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	handlerCalled := false
	handler := auth.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "session"})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !handlerCalled {
		t.Error("expected handler to be called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestRequireAuth_NoSession_Redirects(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	handlerCalled := false
	handler := auth.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	req := httptest.NewRequest("GET", "/protected", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if handlerCalled {
		t.Error("expected handler NOT to be called")
	}
	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected status %d, got %d", http.StatusTemporaryRedirect, w.Code)
	}
}

// --- GetDevUser tests ---

func TestGetDevUser_ReturnsUser(t *testing.T) {
	mockDB := newMockDatabase()
	auth := newTestAuth(mockDB)

	user, err := auth.GetDevUser()
	if err != nil {
		t.Fatalf("GetDevUser() returned error: %v", err)
	}
	if user == nil {
		t.Fatal("GetDevUser() returned nil user")
	}
	if user.GitHubUsername != "testuser" {
		t.Errorf("expected username 'testuser', got '%s'", user.GitHubUsername)
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
