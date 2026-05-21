package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

// contextKey is a type for context keys to avoid collisions
type contextKey string

const (
	// UserContextKey is the key used to store the user in the request context
	UserContextKey contextKey = "user"

	// SessionCookieName is the name of the session cookie
	SessionCookieName = "pr_review_session"

	// SessionDuration is how long a session lasts (7 days)
	SessionDuration = 7 * 24 * time.Hour

	// StateCookieName is the name of the OAuth state cookie
	StateCookieName = "oauth_state"
)

// Auth handles OAuth authentication
type Auth struct {
	cfg         *config.Config
	db          db.Database
	oauthConfig *oauth2.Config
	httpClient  *http.Client

	// githubAPIBase lets tests point /user lookups at a fake server.
	// Production leaves it empty; bearerLookup falls back to the real API.
	githubAPIBase string

	// bearerCache memoizes Authorization: Bearer <pat> → resolved username
	// for 5 minutes so a CLI hammering the endpoint doesn't burn a GitHub
	// API request per call. Keyed by sha256(token) so the raw PAT never
	// sits in process memory.
	bearerCacheMux sync.RWMutex
	bearerCache    map[string]bearerCacheEntry
}

type bearerCacheEntry struct {
	login     string
	expiresAt time.Time
}

// bearerCacheTTL is how long a successful Bearer-token → login mapping is
// trusted before we re-validate against api.github.com/user.
const bearerCacheTTL = 5 * time.Minute

// GitHubUser represents the user info returned from GitHub
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// NewAuth creates a new Auth instance
func NewAuth(cfg *config.Config, database db.Database) *Auth {
	oauthConfig := &oauth2.Config{
		ClientID:     cfg.GitHubAppClientID,
		ClientSecret: cfg.GitHubAppClientSecret,
		Scopes:       []string{"read:user", "read:org"},
		Endpoint:     github.Endpoint,
		RedirectURL:  cfg.OAuthCallbackURL,
	}

	return &Auth{
		cfg:         cfg,
		db:          database,
		oauthConfig: oauthConfig,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		bearerCache: make(map[string]bearerCacheEntry),
	}
}

// generateState creates a random state string for CSRF protection
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// HandleLogin redirects the user to GitHub for OAuth
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := generateState()
	if err != nil {
		log.Printf("[AUTH] Error generating state: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Store state in a cookie
	http.SetCookie(w, &http.Cookie{
		Name:     StateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   300, // 5 minutes
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.cfg.BaseURL, "https"),
		SameSite: http.SameSiteLaxMode,
	})

	// Redirect to GitHub
	url := a.oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
	log.Printf("[AUTH] Redirecting to GitHub OAuth: %s", url)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// HandleCallback handles the OAuth callback from GitHub
func (a *Auth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state
	stateCookie, err := r.Cookie(StateCookieName)
	if err != nil {
		log.Printf("[AUTH] Missing state cookie: %v", err)
		http.Error(w, "Missing state cookie", http.StatusBadRequest)
		return
	}

	state := r.URL.Query().Get("state")
	if state != stateCookie.Value {
		log.Printf("[AUTH] Invalid state: expected %s, got %s", stateCookie.Value, state)
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name:   StateCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Exchange code for token
	code := r.URL.Query().Get("code")
	if code == "" {
		log.Printf("[AUTH] Missing code parameter")
		http.Error(w, "Missing code parameter", http.StatusBadRequest)
		return
	}

	token, err := a.oauthConfig.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("[AUTH] Token exchange failed: %v", err)
		http.Error(w, "Failed to exchange token", http.StatusInternalServerError)
		return
	}

	// Fetch user info from GitHub
	ghUser, err := a.fetchGitHubUser(r.Context(), token.AccessToken)
	if err != nil {
		log.Printf("[AUTH] Failed to fetch GitHub user: %v", err)
		http.Error(w, "Failed to fetch user info", http.StatusInternalServerError)
		return
	}

	// Verify org membership
	if err := a.verifyOrgMembership(r.Context(), token.AccessToken, ghUser.Login); err != nil {
		log.Printf("[AUTH] User %s is not a member of %s: %v", ghUser.Login, a.cfg.GitHubOrgName, err)
		http.Error(w, fmt.Sprintf("You must be a member of the %s organization to use this application", a.cfg.GitHubOrgName), http.StatusForbidden)
		return
	}

	// Create or update user in database
	user, err := a.db.GetUserByGitHubID(ghUser.ID)
	if err != nil {
		log.Printf("[AUTH] Error checking for existing user: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if user == nil {
		// Create new user
		user = &db.User{
			GitHubID:        ghUser.ID,
			GitHubUsername:  ghUser.Login,
			GitHubAvatarURL: ghUser.AvatarURL,
		}
		if err := a.db.CreateUser(user); err != nil {
			log.Printf("[AUTH] Failed to create user: %v", err)
			http.Error(w, "Failed to create user", http.StatusInternalServerError)
			return
		}
		log.Printf("[AUTH] Created new user: %s (ID: %d)", ghUser.Login, user.ID)
	} else {
		// Update last login
		if err := a.db.UpdateUserLastLogin(user.ID); err != nil {
			log.Printf("[AUTH] Failed to update last login: %v", err)
			// Don't fail the request, just log
		}
		log.Printf("[AUTH] Existing user logged in: %s (ID: %d)", ghUser.Login, user.ID)
	}

	// Create session
	sessionID, err := generateState() // Reuse state generation for session ID
	if err != nil {
		log.Printf("[AUTH] Failed to generate session ID: %v", err)
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}

	session := &db.Session{
		ID:        sessionID,
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(SessionDuration),
	}

	if err := a.db.CreateSession(session); err != nil {
		log.Printf("[AUTH] Failed to create session: %v", err)
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}

	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(SessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.cfg.BaseURL, "https"),
		SameSite: http.SameSiteLaxMode,
	})

	log.Printf("[AUTH] Session created for user %s", ghUser.Login)

	// Redirect to dashboard
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// HandleLogout clears the session and redirects to login
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// Get session cookie
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil && cookie.Value != "" {
		// Delete session from database
		if err := a.db.DeleteSession(cookie.Value); err != nil {
			log.Printf("[AUTH] Failed to delete session: %v", err)
		}
	}

	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:   SessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Redirect to login
	http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
}

// fetchGitHubUser fetches the authenticated user from GitHub
func (a *Auth) fetchGitHubUser(ctx context.Context, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}

	return &user, nil
}

// verifyOrgMembership checks if the user is a member of the configured organization
func (a *Auth) verifyOrgMembership(ctx context.Context, accessToken, username string) error {
	if a.cfg.GitHubOrgName == "" {
		// No org configured, allow all users
		return nil
	}

	url := fmt.Sprintf("https://api.github.com/orgs/%s/members/%s", a.cfg.GitHubOrgName, username)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 204 = member, 404 = not a member, 302 = requester is not an org member
	if resp.StatusCode == http.StatusNoContent {
		return nil // User is a member
	}

	return fmt.Errorf("user is not a member (status: %d)", resp.StatusCode)
}

// GetCurrentUser gets the current user from the request context
func GetCurrentUser(r *http.Request) *db.User {
	user, ok := r.Context().Value(UserContextKey).(*db.User)
	if !ok {
		return nil
	}
	return user
}

// Middleware creates an authentication middleware that validates the session
// and adds the user to the request context
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer-token branch (for CLI consumers like the prism-review skill).
		// We accept a GitHub PAT, validate it against api.github.com/user, and
		// look up the matching prism user by login. Falls through to the
		// cookie path if no Authorization header is set.
		if user, ok := a.tryBearerAuth(r); ok {
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if hasBearerHeader(r) {
			// An Authorization header was present but didn't resolve to a
			// known user. Don't fall through to the cookie path — that would
			// confuse a CLI caller with a redirect.
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Get session cookie
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil || cookie.Value == "" {
			// No session, redirect to login for HTML requests
			// Return 401 for API requests
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
			return
		}

		// Validate session
		session, err := a.db.GetSession(cookie.Value)
		if err != nil {
			log.Printf("[AUTH] Error fetching session: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if session == nil || session.ExpiresAt.Before(time.Now()) {
			// Session expired or invalid
			http.SetCookie(w, &http.Cookie{
				Name:   SessionCookieName,
				Value:  "",
				Path:   "/",
				MaxAge: -1,
			})
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "Session expired", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
			return
		}

		// Get user
		user, err := a.db.GetUserByID(session.UserID)
		if err != nil || user == nil {
			log.Printf("[AUTH] Error fetching user for session: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Add user to context
		ctx := context.WithValue(r.Context(), UserContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAuth wraps a handler function with authentication middleware
func (a *Auth) RequireAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.Middleware(http.HandlerFunc(handler)).ServeHTTP(w, r)
	}
}

// DevModeMiddleware creates a middleware that auto-injects a dev user based on GITHUB_USERNAME
// This allows local development without OAuth setup while still using the multi-user data model
func (a *Auth) DevModeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get or create the dev user
		user, err := a.getOrCreateDevUser()
		if err != nil {
			log.Printf("[AUTH-DEV] Error getting dev user: %v", err)
			http.Error(w, "Failed to get dev user", http.StatusInternalServerError)
			return
		}

		// Add user to context
		ctx := context.WithValue(r.Context(), UserContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getOrCreateDevUser gets or creates a user based on the configured GITHUB_USERNAME
func (a *Auth) getOrCreateDevUser() (*db.User, error) {
	username := a.cfg.GitHubUsername
	if username == "" {
		return nil, fmt.Errorf("GITHUB_USERNAME is required for dev mode")
	}

	// Try to find existing user by username
	user, err := a.db.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup user: %w", err)
	}

	if user != nil {
		return user, nil
	}

	// Create new dev user (using a placeholder GitHub ID based on username hash)
	// In dev mode, we don't have a real GitHub ID, so we use a deterministic fake one
	githubID := int64(hashUsername(username))

	user = &db.User{
		GitHubID:        githubID,
		GitHubUsername:  username,
		GitHubAvatarURL: fmt.Sprintf("https://github.com/%s.png", username),
	}

	if err := a.db.CreateUser(user); err != nil {
		return nil, fmt.Errorf("failed to create dev user: %w", err)
	}

	log.Printf("[AUTH-DEV] Created dev user: %s (ID: %d)", username, user.ID)
	return user, nil
}

// hashUsername creates a deterministic hash of a username for use as a fake GitHub ID
func hashUsername(username string) uint32 {
	// Simple hash using djb2 algorithm
	var hash uint32 = 5381
	for _, c := range username {
		hash = ((hash << 5) + hash) + uint32(c)
	}
	// Ensure it's positive and in a reasonable range
	return hash & 0x7FFFFFFF
}

// GetDevUser returns the dev user for use in non-HTTP contexts (e.g., poller)
func (a *Auth) GetDevUser() (*db.User, error) {
	return a.getOrCreateDevUser()
}

// hasBearerHeader reports whether the request carries an Authorization: Bearer
// header (regardless of whether the token is valid). Used by Middleware to
// decide between "fall through to cookie auth" and "reject with 401".
func hasBearerHeader(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if h == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(h), "bearer ")
}

// tryBearerAuth attempts to authenticate the request using a GitHub PAT in the
// Authorization header. Returns (user, true) on success; (nil, false) if there
// is no Bearer header OR the token doesn't resolve to a known prism user.
//
// The decision to map "valid PAT for an unregistered login" to a 401 (rather
// than auto-creating a user row) is intentional: prism users are provisioned
// by OAuth login or by the seed-users tool. CLI auth piggybacks on existing
// records, it doesn't create them.
func (a *Auth) tryBearerAuth(r *http.Request) (*db.User, bool) {
	token := extractBearerToken(r)
	if token == "" {
		return nil, false
	}

	login, ok := a.bearerLookup(r.Context(), token)
	if !ok {
		return nil, false
	}

	user, err := a.db.GetUserByUsername(login)
	if err != nil {
		log.Printf("[AUTH-BEARER] db lookup error for login=%s: %v", login, err)
		return nil, false
	}
	if user == nil {
		log.Printf("[AUTH-BEARER] no prism user matches GitHub login %q", login)
		return nil, false
	}
	return user, true
}

// extractBearerToken pulls the token out of an `Authorization: Bearer <tok>`
// header. Returns empty string if absent or malformed.
func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// bearerLookup resolves a GitHub PAT to a login, using a 5-minute cache to
// keep CLI traffic from burning api.github.com rate limit. Returns
// (login, true) on a successful identification.
func (a *Auth) bearerLookup(ctx context.Context, token string) (string, bool) {
	key := hashBearerToken(token)

	a.bearerCacheMux.RLock()
	entry, ok := a.bearerCache[key]
	a.bearerCacheMux.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.login, true
	}

	login, err := a.fetchGitHubLogin(ctx, token)
	if err != nil {
		log.Printf("[AUTH-BEARER] github /user lookup failed: %v", err)
		return "", false
	}

	a.bearerCacheMux.Lock()
	a.bearerCache[key] = bearerCacheEntry{
		login:     login,
		expiresAt: time.Now().Add(bearerCacheTTL),
	}
	a.bearerCacheMux.Unlock()

	return login, true
}

// hashBearerToken returns a stable cache key for a token without keeping the
// token itself in process memory beyond the request that introduced it.
func hashBearerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// fetchGitHubLogin calls api.github.com/user with the given PAT and returns
// the login on success. Test code overrides githubAPIBase to point at a fake.
func (a *Auth) fetchGitHubLogin(ctx context.Context, token string) (string, error) {
	base := a.githubAPIBase
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github /user returned %d", resp.StatusCode)
	}

	var u GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("decode /user: %w", err)
	}
	if u.Login == "" {
		return "", fmt.Errorf("github /user returned empty login")
	}
	return u.Login, nil
}
