package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pr-review-server/auth"
	"pr-review-server/db"
	"pr-review-server/github"
)

// POST /api/prs/quick-action posts one review (approve, request changes,
// comment) on GitHub as the signed-in human, with their own token. The App
// installation token is never used here, so nothing can be attributed to the
// bot. Feature-gated by QUICK_ACTIONS_ENABLED; QUICK_ACTIONS_ADMIN_ONLY
// narrows it to admins during rollout.

const quickActionPath = "/api/prs/quick-action"

const (
	quickActionMaxBodyLen    = 20000
	quickActionReplayTTL     = 10 * time.Minute
	quickActionDuplicateTTL  = 5 * time.Second
	quickActionRateLimit     = 30
	quickActionRateWindow    = time.Minute
	quickActionMaxRequestLen = 64 * 1024
)

var (
	quickActionRepoNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)
	quickActionRequestIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
)

// quickActionEvents maps the API action to GitHub's review event and the
// review state it produces.
var quickActionEvents = map[string]struct{ event, state string }{
	"approve":         {"APPROVE", "APPROVED"},
	"request_changes": {"REQUEST_CHANGES", "CHANGES_REQUESTED"},
	"comment":         {"COMMENT", "COMMENTED"},
}

func quickActionsEnabled() bool   { return os.Getenv("QUICK_ACTIONS_ENABLED") == "true" }
func quickActionsAdminOnly() bool { return os.Getenv("QUICK_ACTIONS_ADMIN_ONLY") == "true" }

// quickActionsAvailableTo is what /api/user reports: the flag is on and the
// admin-only stage, when set, admits this user.
func (s *Server) quickActionsAvailableTo(user *db.User) bool {
	if !quickActionsEnabled() {
		return false
	}
	return !quickActionsAdminOnly() || s.isAdmin(user)
}

type quickActionRequest struct {
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Number          int    `json:"number"`
	Action          string `json:"action"`
	Body            string `json:"body"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
	RequestID       string `json:"request_id"`
}

type quickActionResponse struct {
	Status   string `json:"status"`
	Action   string `json:"action"`
	ReviewID int64  `json:"review_id"`
	HTMLURL  string `json:"html_url"`
	State    string `json:"state"`
	HeadSHA  string `json:"head_sha"`
	Actor    string `json:"actor"`
}

type quickActionError struct {
	status  int
	code    string
	message string
	details map[string]string
}

type replayEntry struct {
	payload  string
	response quickActionResponse
	expires  time.Time
}

// quickActionState is per instance. A retry that lands on another instance,
// or after a restart, is not caught: it posts a second review (a duplicate
// approval is inert; a duplicate comment is visible). Accepted for the
// pilot; a shared ledger is the upgrade if the logs show it happening.
type quickActionState struct {
	mu       sync.Mutex
	replays  map[string]replayEntry
	recent   map[string]time.Time
	attempts map[int][]time.Time
	now      func() time.Time
	// apiBase overrides the GitHub REST root in tests; empty means api.github.com.
	apiBase string
}

func newQuickActionState() *quickActionState {
	return &quickActionState{
		replays:  map[string]replayEntry{},
		recent:   map[string]time.Time{},
		attempts: map[int][]time.Time{},
		now:      time.Now,
	}
}

func quickActionKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func (q *quickActionState) sweepLocked(now time.Time) {
	for k, e := range q.replays {
		if now.After(e.expires) {
			delete(q.replays, k)
		}
	}
	for k, exp := range q.recent {
		if now.After(exp) {
			delete(q.recent, k)
		}
	}
}

// admit runs the replay, duplicate and rate-limit checks in one critical
// section and reserves the duplicate window so a concurrent double-click sees
// it. It returns the cached response for a replayed request id, and refuses
// a request id reused with a different payload.
func (q *quickActionState) admit(userID int, requestID, payload, dupKey string) (*quickActionResponse, *quickActionError) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	q.sweepLocked(now)
	if e, ok := q.replays[quickActionKey(fmt.Sprint(userID), requestID)]; ok {
		if e.payload != payload {
			return nil, &quickActionError{http.StatusConflict, "duplicate", "request_id was already used for a different action", nil}
		}
		resp := e.response
		return &resp, nil
	}
	if _, ok := q.recent[dupKey]; ok {
		return nil, &quickActionError{http.StatusConflict, "duplicate", "The same action was just submitted for this PR", nil}
	}
	recent := q.attempts[userID][:0]
	for _, t := range q.attempts[userID] {
		if now.Sub(t) < quickActionRateWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= quickActionRateLimit {
		retry := quickActionRateWindow - now.Sub(recent[0])
		q.attempts[userID] = recent
		return nil, &quickActionError{http.StatusTooManyRequests, "rate_limited", "Too many actions, try again shortly", map[string]string{"retry_after_seconds": fmt.Sprint(int(retry.Seconds()) + 1)}}
	}
	q.attempts[userID] = append(recent, now)
	q.recent[dupKey] = now.Add(quickActionDuplicateTTL)
	return nil, nil
}

func (q *quickActionState) remember(userID int, requestID, payload string, resp quickActionResponse) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.replays[quickActionKey(fmt.Sprint(userID), requestID)] = replayEntry{payload: payload, response: resp, expires: q.now().Add(quickActionReplayTTL)}
}

func (req *quickActionRequest) payloadKey() string {
	return quickActionKey(req.Owner, req.Repo, fmt.Sprint(req.Number), req.Action, strings.ToLower(req.ExpectedHeadSHA), req.Body)
}

// release frees the duplicate window after a failure so the user can retry
// at once instead of waiting it out.
func (q *quickActionState) release(dupKey string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.recent, dupKey)
}

func writeQuickActionError(w http.ResponseWriter, e *quickActionError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	payload := map[string]any{"error": e.message, "code": e.code}
	if len(e.details) > 0 {
		payload["details"] = e.details
	}
	_ = json.NewEncoder(w).Encode(payload) // nolint:errcheck
}

// rejectCrossSite refuses browser requests from other origins. The session
// cookie is SameSite=Lax, so this is belt and braces on top of it; CLI
// callers send neither header and pass through.
func (s *Server) rejectCrossSite(r *http.Request) *quickActionError {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return &quickActionError{http.StatusForbidden, "no_permission", "Cross-site request rejected", nil}
	}
	origin := r.Header.Get("Origin")
	if origin == "" || s.cfg.BaseURL == "" {
		return nil
	}
	if !sameOrigin(origin, s.cfg.BaseURL) {
		return &quickActionError{http.StatusForbidden, "no_permission", "Origin does not match this server", nil}
	}
	return nil
}

func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func validateQuickAction(req *quickActionRequest) *quickActionError {
	bad := func(msg string) *quickActionError {
		return &quickActionError{http.StatusBadRequest, "validation", msg, nil}
	}
	if !quickActionRepoNameRe.MatchString(req.Owner) || !quickActionRepoNameRe.MatchString(req.Repo) {
		return bad("owner and repo must be 1-255 characters of letters, digits, '_', '.' or '-'")
	}
	if req.Number <= 0 {
		return bad("number must be positive")
	}
	if _, ok := quickActionEvents[req.Action]; !ok {
		return bad("action must be one of approve, request_changes, comment")
	}
	req.Body = strings.TrimSpace(req.Body)
	if utf8.RuneCountInString(req.Body) > quickActionMaxBodyLen {
		return bad(fmt.Sprintf("body must be at most %d characters", quickActionMaxBodyLen))
	}
	if req.Action != "approve" && req.Body == "" {
		return bad("body is required for " + req.Action)
	}
	if len(req.ExpectedHeadSHA) != 40 || !isSafeSHA(req.ExpectedHeadSHA) {
		return bad("expected_head_sha must be the full 40 char hex commit SHA")
	}
	if !quickActionRequestIDRe.MatchString(req.RequestID) {
		return bad("request_id must be 8-64 characters of letters, digits or '-'")
	}
	return nil
}

// draftApproveGate allows approving a draft only when both automated
// reviews are green on the head GitHub just reported. Stored verdicts belong
// to pr.LastCommitSHA, so a row that has not caught up with the head counts
// as absent. Details name the failing signal.
func draftApproveGate(pr *db.PR, head string) *quickActionError {
	prism, greptile := "absent", "absent"
	if strings.EqualFold(pr.LastCommitSHA, head) {
		prism = "red"
		if (pr.ReviewVerdict == "approve" || pr.ReviewVerdict == "approve_suggestions") && pr.CriticalCount == 0 && pr.Status == "completed" {
			prism = "green"
		}
		greptile = greptileStatusFor(*pr)
	}
	if prism == "green" && greptile == "green" {
		return nil
	}
	var failing []string
	if prism != "green" {
		failing = append(failing, "PRism")
	}
	if greptile != "green" {
		failing = append(failing, "Greptile")
	}
	return &quickActionError{
		http.StatusUnprocessableEntity, "draft_not_green",
		"Draft PR: approve needs PRism and Greptile green on this head (" + strings.Join(failing, " and ") + " not green)",
		map[string]string{"prism": prism, "greptile": greptile},
	}
}

// greptileStatusFor reads the stored verdict, which only counts for the head
// it was computed on.
func greptileStatusFor(pr db.PR) string {
	if pr.GreptileStatus == "" || pr.GreptileStatusSHA == "" || pr.LastCommitSHA == "" {
		return "absent"
	}
	stored, head := strings.ToLower(pr.GreptileStatusSHA), strings.ToLower(pr.LastCommitSHA)
	if !strings.HasPrefix(stored, head) && !strings.HasPrefix(head, stored) {
		return "absent"
	}
	return pr.GreptileStatus
}

func userReviewToQuickActionError(err error) *quickActionError {
	var ure *github.UserReviewError
	if !errors.As(err, &ure) {
		return &quickActionError{http.StatusBadGateway, "github_error", "GitHub request failed: " + err.Error(), nil}
	}
	msg := ure.Message
	if msg == "" {
		msg = "GitHub rejected the request"
	}
	switch ure.Code {
	case "reauth_required":
		return &quickActionError{http.StatusPreconditionRequired, ure.Code, "GitHub authorization expired, sign in again", nil}
	case "own_pr":
		return &quickActionError{http.StatusUnprocessableEntity, ure.Code, "GitHub does not allow reviewing your own pull request", nil}
	case "pr_closed":
		return &quickActionError{http.StatusUnprocessableEntity, ure.Code, "The pull request is closed on GitHub", nil}
	case "validation":
		return &quickActionError{http.StatusUnprocessableEntity, ure.Code, "GitHub rejected the review: " + msg, nil}
	case "no_permission":
		return &quickActionError{http.StatusForbidden, ure.Code, "Your GitHub account cannot review this repository", nil}
	case "rate_limited":
		retry := int(ure.RetryAfter.Seconds()) + 1
		if retry < 1 {
			retry = 60
		}
		return &quickActionError{http.StatusTooManyRequests, ure.Code, "GitHub rate limit reached", map[string]string{"retry_after_seconds": fmt.Sprint(retry)}}
	default:
		return &quickActionError{http.StatusBadGateway, "github_error", "GitHub returned an error: " + msg, nil}
	}
}

// tokenError maps a token source failure: no usable token means sign in
// again, anything else (the token endpoint is down) is a retryable error.
func tokenError(err error) *github.UserReviewError {
	if errors.Is(err, auth.ErrNoGitHubToken) {
		return &github.UserReviewError{Code: "reauth_required", Message: err.Error()}
	}
	return &github.UserReviewError{Code: "github_error", Message: "GitHub token refresh failed, try again: " + err.Error()}
}

// withUserToken runs call with the human's token, refreshing once and
// retrying when GitHub answered 401 with a session token.
func withUserToken(ctx context.Context, src auth.GitHubTokenSource, call func(token string) error) error {
	token, err := src.Token(ctx)
	if err != nil {
		return tokenError(err)
	}
	err = call(token)
	var ure *github.UserReviewError
	if !errors.As(err, &ure) || ure.Code != "reauth_required" {
		return err
	}
	refreshed, refreshErr := src.Refresh(ctx)
	if refreshErr != nil {
		return err
	}
	return call(refreshed)
}

func (s *Server) handleQuickAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if !quickActionsEnabled() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := auth.GetCurrentUser(r)
	if user == nil {
		writeQuickActionError(w, &quickActionError{http.StatusUnauthorized, "no_permission", "Unauthorized", nil})
		return
	}
	if e := s.rejectCrossSite(r); e != nil {
		writeQuickActionError(w, e)
		return
	}
	if quickActionsAdminOnly() && !s.isAdmin(user) {
		writeQuickActionError(w, &quickActionError{http.StatusForbidden, "no_permission", "Quick actions are limited to admins for now", nil})
		return
	}

	var req quickActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, quickActionMaxRequestLen)).Decode(&req); err != nil {
		writeQuickActionError(w, &quickActionError{http.StatusBadRequest, "validation", "Invalid JSON body", nil})
		return
	}
	if e := validateQuickAction(&req); e != nil {
		writeQuickActionError(w, e)
		return
	}
	prRef := fmt.Sprintf("%s/%s#%d", req.Owner, req.Repo, req.Number)
	actor := user.GitHubUsername

	src := auth.GitHubTokenSourceFromRequest(r)
	if src == nil {
		writeQuickActionError(w, &quickActionError{http.StatusPreconditionRequired, "reauth_required", "GitHub authorization missing, sign in again", nil})
		return
	}
	source := src.Source()
	if _, err := src.Token(r.Context()); err != nil {
		e := userReviewToQuickActionError(tokenError(err))
		if e.code == "reauth_required" {
			e.message = "GitHub authorization missing, sign in again"
		}
		writeQuickActionError(w, e)
		return
	}

	pr, err := s.db.GetPR(req.Owner, req.Repo, req.Number)
	if err != nil {
		writeQuickActionError(w, &quickActionError{http.StatusInternalServerError, "github_error", "Failed to load PR", nil})
		return
	}
	if pr == nil {
		writeQuickActionError(w, &quickActionError{http.StatusNotFound, "pr_unknown", "PR not found", nil})
		return
	}
	// GitHub is the authority on who may review, but authorship never changes
	// and this answer saves a round trip. Open/closed is read live below: the
	// stored state lags a reopen until the next poll.
	if req.Action != "comment" && strings.EqualFold(pr.Author, actor) {
		s.logQuickActionFailure(actor, user.ID, req.Action, prRef, req.ExpectedHeadSHA, source, http.StatusUnprocessableEntity, "own_pr")
		writeQuickActionError(w, &quickActionError{http.StatusUnprocessableEntity, "own_pr", "GitHub does not allow reviewing your own pull request", nil})
		return
	}

	dupKey := quickActionKey(fmt.Sprint(user.ID), prRef, req.Action)
	payload := req.payloadKey()
	cached, e := s.quickActions.admit(user.ID, req.RequestID, payload, dupKey)
	if e != nil {
		writeQuickActionError(w, e)
		return
	}
	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cached) // nolint:errcheck
		return
	}

	ctx := r.Context()
	var live *github.UserPRHead
	err = withUserToken(ctx, src, func(token string) error {
		var callErr error
		live, callErr = github.GetPRHeadAsUser(ctx, s.quickActions.apiBase, token, req.Owner, req.Repo, req.Number)
		return callErr
	})
	if err != nil {
		s.quickActions.release(dupKey)
		e := userReviewToQuickActionError(err)
		s.logQuickActionFailure(actor, user.ID, req.Action, prRef, req.ExpectedHeadSHA, source, e.status, e.code)
		writeQuickActionError(w, e)
		return
	}
	head := live.SHA
	if !strings.EqualFold(head, req.ExpectedHeadSHA) {
		s.quickActions.release(dupKey)
		log.Printf("[PR-ACTION] actor=%s user_id=%d action=%s pr=%s rejected code=head_moved expected=%s actual=%s", actor, user.ID, req.Action, prRef, req.ExpectedHeadSHA, head)
		writeQuickActionError(w, &quickActionError{http.StatusConflict, "head_moved", "The PR head moved since this row loaded", map[string]string{"head_sha": head}})
		return
	}
	if live.Merged || !strings.EqualFold(live.State, "open") {
		s.quickActions.release(dupKey)
		s.logQuickActionFailure(actor, user.ID, req.Action, prRef, head, source, http.StatusUnprocessableEntity, "pr_closed")
		writeQuickActionError(w, &quickActionError{http.StatusUnprocessableEntity, "pr_closed", "The pull request is closed on GitHub", nil})
		return
	}
	// GitHub's draft flag is the one that counts; the row may lag a conversion either way.
	if req.Action == "approve" && live.Draft {
		if e := draftApproveGate(pr, head); e != nil {
			s.quickActions.release(dupKey)
			s.logQuickActionFailure(actor, user.ID, req.Action, prRef, head, source, e.status, e.code)
			writeQuickActionError(w, e)
			return
		}
	}

	mapping := quickActionEvents[req.Action]
	var result *github.UserReviewResult
	err = withUserToken(ctx, src, func(token string) error {
		var callErr error
		result, callErr = github.SubmitReviewAsUser(ctx, s.quickActions.apiBase, token, req.Owner, req.Repo, req.Number, head, mapping.event, req.Body)
		return callErr
	})
	if err != nil {
		// A definite rejection frees the duplicate window for an immediate
		// retry. An unknown outcome (transport failure, GitHub 5xx) keeps it:
		// the review may exist, so a reflexive second click must not post again.
		if isDefiniteRejection(err) {
			s.quickActions.release(dupKey)
		}
		e := userReviewToQuickActionError(err)
		if !isDefiniteRejection(err) {
			e.message += "; check the PR on GitHub before retrying"
		}
		s.logQuickActionFailure(actor, user.ID, req.Action, prRef, head, source, e.status, e.code)
		writeQuickActionError(w, e)
		return
	}

	reviewState := result.State
	if reviewState == "" {
		reviewState = mapping.state
	}
	if err := s.db.UpdateUserReviewStatus(user.ID, pr.ID, reviewState); err != nil && !errors.Is(err, db.ErrUserPRViewNotFound) {
		log.Printf("[PR-ACTION] actor=%s user_id=%d pr=%s review status not updated: %v", actor, user.ID, prRef, err)
	}
	s.BroadcastEventToUser(user.ID, EventPRUpdated, map[string]interface{}{
		"owner":  req.Owner,
		"repo":   req.Repo,
		"number": req.Number,
	})
	resp := quickActionResponse{
		Status: "success", Action: req.Action, ReviewID: result.ReviewID, HTMLURL: result.HTMLURL,
		State: reviewState, HeadSHA: head, Actor: actor,
	}
	s.quickActions.remember(user.ID, req.RequestID, payload, resp)
	log.Printf("[PR-ACTION] actor=%s user_id=%d action=%s pr=%s head=%s source=%s review_id=%d body_len=%d",
		actor, user.ID, req.Action, prRef, head, source, result.ReviewID, len(req.Body))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // nolint:errcheck
}

func isDefiniteRejection(err error) bool {
	var ure *github.UserReviewError
	return errors.As(err, &ure) && ure.Status >= 400 && ure.Status < 500
}

func (s *Server) logQuickActionFailure(actor string, userID int, action, prRef, head, source string, status int, code string) {
	log.Printf("[PR-ACTION] actor=%s user_id=%d action=%s pr=%s head=%s source=%s failed status=%d code=%s",
		actor, userID, action, prRef, head, source, status, code)
}
