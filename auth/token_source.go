package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"pr-review-server/db"

	"golang.org/x/oauth2"
)

// GitHubTokenContextKey holds the request's GitHubTokenSource.
const GitHubTokenContextKey contextKey = "github_token"

// tokenRefreshLeeway is how close to expiry a session token is refreshed
// before use, so a request never posts with a token about to lapse.
const tokenRefreshLeeway = 10 * time.Minute

// ErrNoGitHubToken means the request's human has no usable GitHub token and
// must sign in again to get one.
var ErrNoGitHubToken = errors.New("no GitHub token available for this session")

// ErrTokenNotRefreshable is returned by Refresh on sources that carry a fixed
// token (a bearer PAT or the dev PAT).
var ErrTokenNotRefreshable = errors.New("token source cannot refresh")

// GitHubTokenSource yields the token a request may use to act on GitHub as
// the signed-in human. Source names where it came from: session, bearer or
// dev_pat. Token values must never be logged.
type GitHubTokenSource interface {
	Token(ctx context.Context) (string, error)
	// Refresh discards the current access token after GitHub rejected it and
	// obtains a new one.
	Refresh(ctx context.Context) (string, error)
	Source() string
}

// sessionTokenStore is the narrow persistence capability behind session
// tokens, implemented by *db.GormDB and type-asserted so db.Database mocks
// stay untouched.
type sessionTokenStore interface {
	UpdateSessionGitHubToken(id, expectedEnc, enc, refreshEnc string, expiresAt *time.Time) (bool, error)
}

type staticTokenSource struct {
	token  string
	source string
}

// StaticGitHubTokenSource wraps a fixed token; the middleware uses it for
// bearer PATs and the dev PAT, and tests attach it to request contexts.
func StaticGitHubTokenSource(token, source string) GitHubTokenSource {
	return staticTokenSource{token: token, source: source}
}

func (s staticTokenSource) Token(context.Context) (string, error) {
	if s.token == "" {
		return "", ErrNoGitHubToken
	}
	return s.token, nil
}

func (s staticTokenSource) Refresh(context.Context) (string, error) {
	return "", ErrTokenNotRefreshable
}

func (s staticTokenSource) Source() string { return s.source }

// sessionTokenSource decrypts the session's stored OAuth token on first use,
// refreshes it when it is close to expiry or after a 401, and persists what
// it refreshed. A refresh GitHub refuses clears the stored token so the UI
// falls back to "sign in again"; a transient failure leaves it in place.
type sessionTokenSource struct {
	auth    *Auth
	session *db.Session
	mu      sync.Mutex
	token   *oauth2.Token
	loaded  bool
	loadErr error
}

func (a *Auth) newSessionTokenSource(session *db.Session) *sessionTokenSource {
	return &sessionTokenSource{auth: a, session: session}
}

func (s *sessionTokenSource) Source() string { return "session" }

func (s *sessionTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	if s.token.AccessToken == "" {
		return "", ErrNoGitHubToken
	}
	if !s.token.Expiry.IsZero() && time.Until(s.token.Expiry) < tokenRefreshLeeway {
		return s.refresh(ctx)
	}
	return s.token.AccessToken, nil
}

func (s *sessionTokenSource) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	return s.refresh(ctx)
}

func (s *sessionTokenSource) load() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	s.token, s.loadErr = s.auth.openSessionToken(s.session)
	if s.loadErr != nil {
		log.Printf("[AUTH] session token unreadable, treating as absent: %v", s.loadErr)
		s.loadErr = ErrNoGitHubToken
	}
	return s.loadErr
}

// refresh serializes on the session. GitHub rotates refresh tokens, so two
// requests refreshing the same session at once would leave one holding a
// dead token: the per-session lock covers siblings in this process and the
// re-read adopts a token another request (or instance) already persisted
// instead of spending the refresh token a second time.
func (s *sessionTokenSource) refresh(ctx context.Context) (string, error) {
	unlock := s.auth.lockSession(s.session.ID)
	defer unlock()
	if s.adoptStoredToken() && (s.token.Expiry.IsZero() || time.Until(s.token.Expiry) >= tokenRefreshLeeway) {
		return s.token.AccessToken, nil
	}
	if s.token.RefreshToken == "" {
		s.clearStored()
		return "", ErrNoGitHubToken
	}
	expired := *s.token
	expired.Expiry = time.Now().Add(-time.Minute)
	refreshed, err := s.auth.oauthConfig.TokenSource(ctx, &expired).Token()
	if err != nil {
		if refreshRefused(err) {
			log.Printf("[AUTH] GitHub refused token refresh for user %d, clearing stored token: %v", s.session.UserID, err)
			s.clearStored()
			return "", ErrNoGitHubToken
		}
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = s.token.RefreshToken
	}
	s.token = refreshed
	if enc, refreshEnc, expiresAt, sealErr := s.auth.sealOAuthToken(refreshed); sealErr != nil {
		log.Printf("[AUTH] failed to seal refreshed token for user %d: %v", s.session.UserID, sealErr)
	} else if s.auth.storeSessionToken(s.session.ID, s.session.GitHubTokenEnc, enc, refreshEnc, expiresAt) {
		s.session.GitHubTokenEnc, s.session.GitHubRefreshTokenEnc, s.session.GitHubTokenExpiresAt = enc, refreshEnc, expiresAt
	}
	return refreshed.AccessToken, nil
}

// refreshRefused tells a dead refresh token apart from a token endpoint that
// is merely unavailable. GitHub answers a bad or expired refresh token with
// an OAuth error code (often inside a 200 body); 429 and 5xx are transient.
func refreshRefused(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return false
	}
	if re.ErrorCode != "" {
		return true
	}
	status := 0
	if re.Response != nil {
		status = re.Response.StatusCode
	}
	return status == http.StatusBadRequest || status == http.StatusUnauthorized
}

// adoptStoredToken re-reads the session row and, when another request has
// already stored a different token, switches to it. Reports whether it did.
func (s *sessionTokenSource) adoptStoredToken() bool {
	fresh, err := s.auth.db.GetSession(s.session.ID)
	if err != nil || fresh == nil || fresh.GitHubTokenEnc == s.session.GitHubTokenEnc {
		return false
	}
	tok, err := s.auth.openSessionToken(fresh)
	if err != nil || tok.AccessToken == "" {
		return false
	}
	s.session, s.token = fresh, tok
	return true
}

// clearStored drops the token so the UI offers "sign in again". The clear is
// fenced on the ciphertext this request read: if a sibling refreshed in the
// meantime its token survives and is adopted instead.
func (s *sessionTokenSource) clearStored() {
	if s.auth.storeSessionToken(s.session.ID, s.session.GitHubTokenEnc, "", "", nil) {
		s.token = &oauth2.Token{}
		s.session.GitHubTokenEnc, s.session.GitHubRefreshTokenEnc, s.session.GitHubTokenExpiresAt = "", "", nil
		return
	}
	s.adoptStoredToken()
}

// lockSession serializes token refreshes for one session within this process.
func (a *Auth) lockSession(sessionID string) func() {
	mu, _ := a.sessionLocks.LoadOrStore(sessionID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

func (a *Auth) openSessionToken(session *db.Session) (*oauth2.Token, error) {
	access, err := OpenToken(a.cfg.SessionSecret, session.GitHubTokenEnc)
	if err != nil {
		return nil, err
	}
	refresh, err := OpenToken(a.cfg.SessionSecret, session.GitHubRefreshTokenEnc)
	if err != nil {
		return nil, err
	}
	tok := &oauth2.Token{AccessToken: access, RefreshToken: refresh}
	if session.GitHubTokenExpiresAt != nil {
		tok.Expiry = *session.GitHubTokenExpiresAt
	}
	return tok, nil
}

func (a *Auth) sealOAuthToken(tok *oauth2.Token) (enc, refreshEnc string, expiresAt *time.Time, err error) {
	if enc, err = SealToken(a.cfg.SessionSecret, tok.AccessToken); err != nil {
		return "", "", nil, err
	}
	if refreshEnc, err = SealToken(a.cfg.SessionSecret, tok.RefreshToken); err != nil {
		return "", "", nil, err
	}
	if !tok.Expiry.IsZero() {
		expiry := tok.Expiry.UTC()
		expiresAt = &expiry
	}
	return enc, refreshEnc, expiresAt, nil
}

// storeSessionToken writes sealed tokens fenced on the ciphertext currently
// stored and reports whether the row was written.
func (a *Auth) storeSessionToken(sessionID, expectedEnc, enc, refreshEnc string, expiresAt *time.Time) bool {
	store, ok := a.db.(sessionTokenStore)
	if !ok {
		log.Printf("[AUTH] database cannot store session tokens; GitHub actions stay unavailable")
		return false
	}
	written, err := store.UpdateSessionGitHubToken(sessionID, expectedEnc, enc, refreshEnc, expiresAt)
	if err != nil {
		log.Printf("[AUTH] failed to store user token for session: %v", err)
		return false
	}
	if !written {
		log.Printf("[AUTH] session token changed underneath this request; keeping the newer token")
		return false
	}
	if enc == "" {
		log.Printf("[AUTH] cleared user token for session")
	} else {
		log.Printf("[AUTH] stored user token for session (expires=%s)", formatExpiry(expiresAt))
	}
	return true
}

func formatExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// GitHubTokenSourceFromRequest returns the request's token source, or nil
// when the middleware attached none.
func GitHubTokenSourceFromRequest(r *http.Request) GitHubTokenSource {
	src, _ := r.Context().Value(GitHubTokenContextKey).(GitHubTokenSource)
	return src
}

// GitHubUserToken resolves the token the request's human may act with,
// refreshing a session token when it is about to expire. ok is false when
// the user must sign in again.
func GitHubUserToken(r *http.Request) (token, source string, ok bool) {
	src := GitHubTokenSourceFromRequest(r)
	if src == nil {
		return "", "", false
	}
	token, err := src.Token(r.Context())
	if err != nil || token == "" {
		return "", src.Source(), false
	}
	return token, src.Source(), true
}
