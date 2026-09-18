package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
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
	UpdateSessionGitHubToken(id string, enc, refreshEnc string, expiresAt *time.Time) error
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
	token   *oauth2.Token
	loaded  bool
	loadErr error
}

func (a *Auth) newSessionTokenSource(session *db.Session) *sessionTokenSource {
	return &sessionTokenSource{auth: a, session: session}
}

func (s *sessionTokenSource) Source() string { return "session" }

func (s *sessionTokenSource) Token(ctx context.Context) (string, error) {
	s.auth.tokenRefreshMu.Lock()
	defer s.auth.tokenRefreshMu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	if s.token.AccessToken == "" {
		return "", ErrNoGitHubToken
	}
	if !s.token.Expiry.IsZero() && time.Until(s.token.Expiry) < tokenRefreshLeeway {
		return s.refreshLocked(ctx)
	}
	return s.token.AccessToken, nil
}

func (s *sessionTokenSource) Refresh(ctx context.Context) (string, error) {
	s.auth.tokenRefreshMu.Lock()
	defer s.auth.tokenRefreshMu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	return s.refreshLocked(ctx)
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

// refreshLocked runs under tokenRefreshMu. GitHub rotates refresh tokens, so
// two requests refreshing the same session at once would leave one holding a
// dead token: the mutex serializes them and the re-read adopts a sibling's
// fresh token instead of spending the refresh token a second time.
func (s *sessionTokenSource) refreshLocked(ctx context.Context) (string, error) {
	if fresh, err := s.auth.db.GetSession(s.session.ID); err == nil && fresh != nil && fresh.GitHubTokenEnc != s.session.GitHubTokenEnc {
		if tok, openErr := s.auth.openSessionToken(fresh); openErr == nil && tok.AccessToken != "" {
			s.session, s.token = fresh, tok
			if tok.Expiry.IsZero() || time.Until(tok.Expiry) >= tokenRefreshLeeway {
				return tok.AccessToken, nil
			}
		}
	}
	if s.token.RefreshToken == "" {
		s.clearStored()
		return "", ErrNoGitHubToken
	}
	expired := *s.token
	expired.Expiry = time.Now().Add(-time.Minute)
	refreshed, err := s.auth.oauthConfig.TokenSource(ctx, &expired).Token()
	if err != nil {
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) {
			log.Printf("[AUTH] GitHub refused token refresh for user %d (status %d), clearing stored token", s.session.UserID, refused.Response.StatusCode)
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
	} else {
		s.session.GitHubTokenEnc, s.session.GitHubRefreshTokenEnc, s.session.GitHubTokenExpiresAt = enc, refreshEnc, expiresAt
		s.auth.storeSessionToken(s.session.ID, enc, refreshEnc, expiresAt)
	}
	return refreshed.AccessToken, nil
}

func (s *sessionTokenSource) clearStored() {
	s.token = &oauth2.Token{}
	s.session.GitHubTokenEnc, s.session.GitHubRefreshTokenEnc, s.session.GitHubTokenExpiresAt = "", "", nil
	s.auth.storeSessionToken(s.session.ID, "", "", nil)
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

func (a *Auth) storeSessionToken(sessionID, enc, refreshEnc string, expiresAt *time.Time) {
	store, ok := a.db.(sessionTokenStore)
	if !ok {
		log.Printf("[AUTH] database cannot store session tokens; GitHub actions stay unavailable")
		return
	}
	if err := store.UpdateSessionGitHubToken(sessionID, enc, refreshEnc, expiresAt); err != nil {
		log.Printf("[AUTH] failed to store user token for session: %v", err)
		return
	}
	if enc == "" {
		log.Printf("[AUTH] cleared user token for session")
		return
	}
	log.Printf("[AUTH] stored user token for session (expires=%s)", formatExpiry(expiresAt))
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
