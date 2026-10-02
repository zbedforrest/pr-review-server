package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v57/github"
	"golang.org/x/sync/singleflight"

	"pr-review-server/auth"
	"pr-review-server/db"
	"pr-review-server/pkg/approval"
)

type approvalScanRequest struct {
	Targets []approval.Target `json:"targets"`
	Scope   json.RawMessage   `json:"scope"`
}

type approvalTargetResponse struct {
	db.ApprovalTarget
	ReasonCodes []string             `json:"reason_codes"`
	Snapshot    *approval.Snapshot   `json:"snapshot,omitempty"`
	Assessment  *approval.Assessment `json:"assessment"`
	Score       *approval.Score      `json:"score,omitempty"`
	Sources     []approval.Source    `json:"sources"`
}

type approvalScanResponse struct {
	db.ApprovalScan
	Scope json.RawMessage `json:"scope"`
}

func approvalScanDTO(scan db.ApprovalScan) approvalScanResponse {
	scope := json.RawMessage(scan.ScopeJSON)
	if !json.Valid(scope) {
		scope = json.RawMessage(`{}`)
	}
	return approvalScanResponse{ApprovalScan: scan, Scope: scope}
}

func approvalID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func approvalKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s/%d", strings.ToLower(owner), strings.ToLower(repo), number)
}
func approvalJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *Server) approvalStore() db.ApprovalStore { store, _ := s.db.(db.ApprovalStore); return store }
func (s *Server) approvalAvailable() string {
	if s.cfg == nil || !s.cfg.ApprovalCandidates().Enabled {
		return "Approval candidates are disabled"
	}
	if reason := s.cfg.ApprovalCandidates().UnavailableReason; reason != "" {
		return reason
	}
	if s.approvalStore() == nil || s.ghClient == nil {
		return "Approval investigation storage or GitHub access is unavailable"
	}
	return ""
}

func (s *Server) handleApprovalCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeV1Error(w, 405, "method_not_allowed", "Use GET")
		return
	}
	if auth.GetCurrentUser(r) == nil {
		writeV1Error(w, 401, "unauthorized", "Authentication required")
		return
	}
	enabled := s.cfg != nil && s.cfg.ApprovalCandidates().Enabled
	writeV1JSON(w, 200, map[string]any{"enabled": enabled, "available": s.approvalAvailable() == "", "unavailable_reason": s.approvalAvailable(), "max_targets": db.MaxApprovalTargetsPerScan, "policy_version": approval.PolicyVersion})
}

func (s *Server) approvalRequest(w http.ResponseWriter, r *http.Request) (*db.User, bool) {
	w.Header().Set("Cache-Control", "no-store")
	user := auth.GetCurrentUser(r)
	if user == nil {
		writeV1Error(w, 401, "unauthorized", "Authentication required")
		return nil, false
	}
	if s.cfg == nil || !s.cfg.ApprovalCandidates().Enabled {
		writeV1Error(w, 404, "not_found", "Approval candidates are disabled")
		return nil, false
	}
	if s.approvalStore() == nil {
		writeV1Error(w, 503, "unavailable", "Approval storage unavailable")
		return nil, false
	}
	if r.Method == http.MethodPost {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" || r.Header.Get("X-PRism-Request") != "1" {
			writeV1Error(w, 400, "invalid_request", "JSON and X-PRism-Request: 1 are required")
			return nil, false
		}
		if origin := r.Header.Get("Origin"); origin != "" && !approvalOriginMatches(origin, s.cfg.BaseURL) {
			writeV1Error(w, 403, "invalid_origin", "Origin does not match this application")
			return nil, false
		}
	}
	return user, true
}

func approvalOriginMatches(origin, base string) bool {
	a, err := url.Parse(origin)
	if err != nil || a.User != nil || a.Path != "" || a.RawQuery != "" || a.Fragment != "" || a.Host == "" || (a.Scheme != "https" && a.Scheme != "http") {
		return false
	}
	b, err := url.Parse(base)
	if err != nil || b.User != nil || b.Host == "" || (b.Scheme != "https" && b.Scheme != "http") {
		return false
	}
	return sameOrigin(origin, base)
}

func decodeApprovalRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeV1Error(w, 400, "invalid_request", "Invalid JSON request")
		return false
	}
	if err := requireJSONEOF(decoder); err != nil {
		writeV1Error(w, 400, "invalid_request", "Expected one JSON object")
		return false
	}
	return true
}

func approvalError(w http.ResponseWriter, err error) {
	var active *db.ApprovalActiveConflict
	switch {
	case errors.As(err, &active):
		writeV1JSON(w, 409, map[string]any{"error": map[string]string{"code": "active_scan", "message": "An approval scan is already active", "scan_id": active.ScanID}})
	case errors.Is(err, db.ErrApprovalIdempotency):
		writeV1Error(w, 409, "idempotency_conflict", "This idempotency key has different inputs")
	case errors.Is(err, db.ErrApprovalBudget):
		now := time.Now().UTC()
		reset := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		w.Header().Set("Retry-After", strconv.Itoa(int(reset.Sub(now).Seconds())+1))
		writeV1Error(w, 429, "daily_budget", "The daily investigation budget is exhausted")
	case errors.Is(err, db.ErrApprovalNotFound):
		writeV1Error(w, 404, "not_found", "Approval record not found")
	default:
		writeV1Error(w, 503, "unavailable", "Approval operation is temporarily unavailable")
	}
}

func approvalPage(r *http.Request) (int, string, error) {
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return 0, "", errors.New("limit must be 1 through 100")
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		if len(cursor) != 32 {
			return 0, "", errors.New("invalid cursor")
		}
		if _, err := hex.DecodeString(cursor); err != nil {
			return 0, "", errors.New("invalid cursor")
		}
	}
	return limit, cursor, nil
}

func (s *Server) approvalInventory(user int) (map[string]db.PRWithUserView, error) {
	rows, err := s.db.GetPRsForUserWithNotes(user)
	if err != nil {
		return nil, err
	}
	result := make(map[string]db.PRWithUserView, len(rows))
	for _, row := range rows {
		result[approvalKey(row.RepoOwner, row.RepoName, row.PRNumber)] = row
	}
	return result, nil
}

// approvalInventoryCache shares a user's dashboard inventory across the
// targets of a running scan for a few seconds.
type approvalInventoryCache struct {
	mu      sync.Mutex
	entries map[int]approvalInventoryEntry
	flight  singleflight.Group
}

type approvalInventoryEntry struct {
	rows    map[string]db.PRWithUserView
	fetched time.Time
}

const approvalInventoryFreshness = 5 * time.Second

// approvalInventoryCached is approvalInventory for worker checks, which run
// for every target of a scan at once. Callers must not modify the result.
func (s *Server) approvalInventoryCached(user int) (map[string]db.PRWithUserView, error) {
	c := &s.approvalInventories
	c.mu.Lock()
	if e, ok := c.entries[user]; ok && time.Since(e.fetched) < approvalInventoryFreshness {
		c.mu.Unlock()
		return e.rows, nil
	}
	c.mu.Unlock()
	v, err, _ := c.flight.Do(strconv.Itoa(user), func() (any, error) {
		rows, err := s.approvalInventory(user)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries == nil {
			c.entries = map[int]approvalInventoryEntry{}
		}
		for id, e := range c.entries {
			if now.Sub(e.fetched) >= approvalInventoryFreshness {
				delete(c.entries, id)
			}
		}
		c.entries[user] = approvalInventoryEntry{rows: rows, fetched: now}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]db.PRWithUserView), nil
}

func (s *Server) handleApprovalScans(w http.ResponseWriter, r *http.Request) {
	user, ok := s.approvalRequest(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var request approvalScanRequest
		if !decodeApprovalRequest(w, r, &request) {
			return
		}
		s.admitApprovalScan(w, r, user, request, "full")
		return
	}
	if r.Method != http.MethodGet {
		writeV1Error(w, 405, "method_not_allowed", "Use GET or POST")
		return
	}
	limit, cursor, err := approvalPage(r)
	if err != nil {
		writeV1Error(w, 400, "invalid_request", err.Error())
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "full" && kind != "recheck" {
		writeV1Error(w, 400, "invalid_request", "Invalid scan kind")
		return
	}
	scans, err := s.approvalStore().ListApprovalScans(user.ID, kind, limit+1, cursor)
	if err != nil {
		approvalError(w, err)
		return
	}
	next := ""
	if len(scans) > limit {
		scans = scans[:limit]
		next = scans[len(scans)-1].ID
	}
	results := make([]approvalScanResponse, 0, len(scans))
	for _, scan := range scans {
		results = append(results, approvalScanDTO(scan))
	}
	writeV1JSON(w, 200, map[string]any{"scans": results, "next_cursor": next})
}

func (s *Server) admitApprovalScan(w http.ResponseWriter, r *http.Request, user *db.User, request approvalScanRequest, kind string) {
	key, err := parseIdempotencyKey(r.Header.Values("Idempotency-Key"))
	if err != nil || key == "" {
		writeV1Error(w, 400, "invalid_request", "A valid Idempotency-Key is required")
		return
	}
	unique := map[string]approval.Target{}
	for _, target := range request.Targets {
		if !isSafeGitHubName(target.Owner) || !isSafeGitHubName(target.Repo) || target.Number < 1 || !isFullSHA(target.ExpectedHeadSHA) {
			writeV1Error(w, 400, "invalid_request", "Targets require repository coordinates and a full head SHA")
			return
		}
		target.Owner = strings.ToLower(target.Owner)
		target.Repo = strings.ToLower(target.Repo)
		target.ExpectedHeadSHA = strings.ToLower(target.ExpectedHeadSHA)
		identity := approvalKey(target.Owner, target.Repo, target.Number)
		if old, exists := unique[identity]; exists && old.ExpectedHeadSHA != target.ExpectedHeadSHA {
			writeV1Error(w, 400, "invalid_request", "Conflicting target revisions")
			return
		}
		unique[identity] = target
	}
	if len(unique) == 0 || len(unique) > db.MaxApprovalTargetsPerScan {
		writeV1Error(w, 422, "invalid_scope", fmt.Sprintf("Choose between 1 and %d pull requests", db.MaxApprovalTargetsPerScan))
		return
	}
	request.Targets = nil
	for _, target := range unique {
		request.Targets = append(request.Targets, target)
	}
	sort.Slice(request.Targets, func(i, j int) bool {
		a, b := request.Targets[i], request.Targets[j]
		return approvalKey(a.Owner, a.Repo, a.Number) < approvalKey(b.Owner, b.Repo, b.Number)
	})
	if len(request.Scope) > 0 {
		var scope any
		if err := json.Unmarshal(request.Scope, &scope); err != nil {
			writeV1Error(w, 400, "invalid_request", "Invalid scope")
			return
		}
		request.Scope = json.RawMessage(approvalJSON(scope))
	}
	requestHash := sha256Hex(kind + approvalJSON(request))
	previous, err := s.approvalStore().GetApprovalScanByIdempotency(user.ID, key)
	if err == nil {
		if previous.RequestHash != requestHash {
			approvalError(w, db.ErrApprovalIdempotency)
			return
		}
		w.Header().Set("Location", "/api/v1/approval-scans/"+previous.ID)
		writeV1JSON(w, 200, approvalScanDTO(*previous))
		return
	}
	if !errors.Is(err, db.ErrApprovalNotFound) {
		approvalError(w, err)
		return
	}
	if reason := s.approvalAvailable(); reason != "" {
		writeV1Error(w, 503, "unavailable", reason)
		return
	}
	inventory, err := s.approvalInventory(user.ID)
	if err != nil {
		approvalError(w, err)
		return
	}
	now := time.Now().UTC()
	scanID := approvalID()
	targets := make([]db.ApprovalTarget, 0, len(unique))
	eligible := 0
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	for _, target := range request.Targets {
		if _, exists := inventory[approvalKey(target.Owner, target.Repo, target.Number)]; !exists {
			writeV1Error(w, 404, "not_found", "Pull request is not in your dashboard")
			return
		}
	}
	checks := s.fetchApprovalChecks(ctx, request.Targets)
	for i, target := range request.Targets {
		row := inventory[approvalKey(target.Owner, target.Repo, target.Number)]
		pr, err := checks[i].pr, checks[i].prErr
		if s.approvalRate.note(err, time.Now()) {
			writeV1Error(w, 429, "github_rate_limited", "GitHub's rate limit is exhausted; try again after it resets")
			return
		}
		if err != nil {
			writeV1Error(w, 404, "not_found", "Pull request is inaccessible")
			return
		}
		if pr.GetHead().GetSHA() != target.ExpectedHeadSHA {
			writeV1Error(w, 409, "head_changed", "A pull request changed; refresh the dashboard")
			return
		}
		exclusions := []string{}
		if row.UserHidden {
			exclusions = append(exclusions, "hidden")
		}
		if pr.GetState() != "open" {
			exclusions = append(exclusions, "closed")
		}
		if approvalViewerMatches(user, pr.GetUser().GetID(), pr.GetUser().GetLogin()) {
			exclusions = append(exclusions, "self_authored")
		}
		reviews, err := checks[i].reviews, checks[i].reviewsErr
		if err != nil {
			writeV1Error(w, 503, "collection_failed", "Could not verify your review status")
			return
		}
		standingState, standingSHA := "", ""
		var latest int64
		var latestSubmitted time.Time
		for _, review := range reviews {
			submitted := review.GetSubmittedAt().Time
			newer := submitted.After(latestSubmitted) || submitted.Equal(latestSubmitted) && review.GetID() > latest
			if approvalViewerMatches(user, review.GetUser().GetID(), review.GetUser().GetLogin()) && newer && (review.GetState() == "APPROVED" || review.GetState() == "CHANGES_REQUESTED" || review.GetState() == "DISMISSED") {
				latest = review.GetID()
				latestSubmitted = submitted
				standingState = review.GetState()
				standingSHA = review.GetCommitID()
			}
		}
		if standingState == "APPROVED" && standingSHA == target.ExpectedHeadSHA {
			exclusions = append(exclusions, "already_approved")
		}
		t := db.ApprovalTarget{ID: approvalID(), ScanID: scanID, UserID: user.ID, Owner: target.Owner, Repo: target.Repo, Number: target.Number, ExpectedHeadSHA: target.ExpectedHeadSHA, RepositoryID: pr.GetBase().GetRepo().GetID(), AccessPartition: fmt.Sprint(user.ID), ExecutionStatus: "queued", Freshness: "expired", CreatedAt: now}
		if len(exclusions) > 0 {
			t.ExecutionStatus = "completed"
			t.Decision = "excluded"
			t.ReasonCodesJSON = approvalJSON(exclusions)
			t.CompletedAt = &now
		} else {
			eligible++
		}
		targets = append(targets, t)
	}
	if eligible == 0 {
		writeV1Error(w, 422, "no_eligible_targets", "None of these pull requests are eligible")
		return
	}
	cfg := s.cfg.ApprovalCandidates()
	limits := map[string]any{"max_targets": db.MaxApprovalTargetsPerScan, "target_seconds": int(approvalTargetDuration.Seconds()), "max_rounds": approval.MaxRounds, "max_tool_calls": approval.MaxToolCalls, "max_tool_bytes": approval.MaxToolBytes, "max_citation_reads": approval.MaxCitationReads, "max_input_tokens": approval.TargetInputTokens, "max_output_tokens": approval.TargetOutputTokens, "max_call_input_tokens": approval.CallInputTokens, "daily_input_tokens": cfg.DailyInput, "daily_output_tokens": cfg.DailyOutput, "provider": cfg.Provider, "model": cfg.Model, "policy_version": approval.PolicyVersion, "prompt_version": approval.PromptVersion, "runtime_version": approval.RuntimeVersion}
	scan, replayed, err := s.approvalStore().AdmitApprovalScan(db.ApprovalAdmission{Scan: db.ApprovalScan{ID: scanID, UserID: user.ID, Kind: kind, Status: "queued", IdempotencyKey: key, RequestHash: requestHash, ScopeJSON: string(request.Scope), LimitsJSON: approvalJSON(limits), CreatedAt: now, Deadline: approvalScanDeadline(now, len(targets), approvalSlots()), Total: len(targets)}, Targets: targets, Now: now, DailyInputLimit: cfg.DailyInput, DailyOutputLimit: cfg.DailyOutput, TargetInputLimit: approval.TargetInputTokens, TargetOutputLimit: approval.TargetOutputTokens})
	if err != nil {
		approvalError(w, err)
		return
	}
	status := 202
	if replayed {
		status = 200
	}
	s.approvalWake.signal()
	w.Header().Set("Location", "/api/v1/approval-scans/"+scan.ID)
	w.Header().Set("Retry-After", "2")
	writeV1JSON(w, status, approvalScanDTO(*scan))
}

// approvalScanDeadline gives queued targets time for every wave of slots, plus
// one target's duration and a queue allowance.
func approvalScanDeadline(now time.Time, targets, slots int) time.Time {
	slots = max(slots, 1)
	waves := (targets + slots - 1) / slots
	return now.Add(time.Duration(waves)*approvalTargetDuration + approvalTargetDuration + 10*time.Minute)
}

func (s *Server) approvalResponse(target db.ApprovalTarget, detail bool) approvalTargetResponse {
	response := approvalTargetResponse{ApprovalTarget: target, ReasonCodes: []string{}, Sources: []approval.Source{}}
	_ = json.Unmarshal([]byte(target.ReasonCodesJSON), &response.ReasonCodes)
	if target.Freshness == "current" && (target.ValidUntil == nil || !target.ValidUntil.After(time.Now())) {
		response.Freshness = "expired"
	}
	if target.SnapshotJSON != "" {
		var snapshot approval.Snapshot
		if json.Unmarshal([]byte(target.SnapshotJSON), &snapshot) == nil {
			response.Sources = snapshot.Sources
			if detail {
				response.Snapshot = &snapshot
			}
		}
	}
	if target.AssessmentJSON != "" {
		var assessment approval.Assessment
		if json.Unmarshal([]byte(target.AssessmentJSON), &assessment) == nil {
			if len(assessment.Sources) > 0 {
				response.Sources = assessment.Sources
			}
			response.Score = assessment.Score
			if detail {
				response.Assessment = &assessment
			}
		}
	}
	return response
}

func (s *Server) approvalReadResponse(ctx context.Context, user *db.User, target db.ApprovalTarget, detail bool) (approvalTargetResponse, bool) {
	inventory, err := s.approvalInventory(user.ID)
	if err != nil {
		return approvalTargetResponse{}, false
	}
	return s.approvalReadResponseFrom(ctx, user, inventory, target, detail)
}

// approvalReadResponses builds the responses for a page of targets with one
// inventory read and bounded parallel PR reads, keeping the page order.
func (s *Server) approvalReadResponses(ctx context.Context, user *db.User, rows []db.ApprovalTarget) []approvalTargetResponse {
	inventory, err := s.approvalInventory(user.ID)
	if err != nil {
		return []approvalTargetResponse{}
	}
	built := make([]*approvalTargetResponse, len(rows))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, row := range rows {
		wg.Add(1)
		go func(i int, row db.ApprovalTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if response, ok := s.approvalReadResponseFrom(ctx, user, inventory, row, false); ok {
				built[i] = &response
			}
		}(i, row)
	}
	wg.Wait()
	out := []approvalTargetResponse{}
	for _, response := range built {
		if response != nil {
			out = append(out, *response)
		}
	}
	return out
}

func (s *Server) approvalReadResponseFrom(ctx context.Context, user *db.User, inventory map[string]db.PRWithUserView, target db.ApprovalTarget, detail bool) (approvalTargetResponse, bool) {
	if s.ghClient == nil {
		return approvalTargetResponse{}, false
	}
	row, exists := inventory[approvalKey(target.Owner, target.Repo, target.Number)]
	if !exists {
		return approvalTargetResponse{}, false
	}
	var pr *gh.PullRequest
	var err error
	if target.Decision == "candidate" {
		pr, err = s.approvalLivePR(ctx, target.Owner, target.Repo, target.Number)
	} else {
		pr, err = s.approvalPR(ctx, target.Owner, target.Repo, target.Number, approvalPRFreshness)
	}
	if err != nil || pr.GetBase().GetRepo().GetID() != target.RepositoryID {
		return approvalTargetResponse{}, false
	}
	stored := &target
	if detail || target.Decision == "candidate" {
		// Listed rows omit the snapshot, which detail views and the candidate base check need.
		if stored, err = s.approvalStore().GetApprovalTarget(user.ID, target.ScanID, target.ID); err != nil {
			return approvalTargetResponse{}, false
		}
	}
	response := s.approvalResponse(*stored, detail)
	if target.Decision == "candidate" {
		reason := ""
		switch {
		case pr.GetHead().GetSHA() != target.ExpectedHeadSHA:
			reason = "head_changed"
		case pr.GetDraft():
			reason = "pr_draft"
		case pr.GetState() != "open":
			reason = "closed"
		case row.CIState == "failure" || row.CIState == "pending":
			reason = "observed_ci_change"
		case row.ReviewDecision == "CHANGES_REQUESTED":
			reason = "observed_review_change"
		case row.UserHidden:
			reason = "hidden"
		case approvalViewerMatches(user, pr.GetUser().GetID(), pr.GetUser().GetLogin()):
			reason = "self_authored"
		}
		var snapshot approval.Snapshot
		if reason == "" && json.Unmarshal([]byte(stored.SnapshotJSON), &snapshot) == nil && snapshot.Revision.Base != "" && snapshot.Revision.Base != pr.GetBase().GetSHA() {
			reason = "base_changed"
		}
		if reason != "" {
			response.Freshness = "stale"
			response.ReasonCodes = append(response.ReasonCodes, reason)
		}
	}
	return response, true
}

func (s *Server) handleApprovalScanByID(w http.ResponseWriter, r *http.Request) {
	user, ok := s.approvalRequest(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/approval-scans/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeV1Error(w, 404, "not_found", "Scan not found")
		return
	}
	scan, err := s.approvalStore().GetApprovalScan(user.ID, parts[0])
	if err != nil {
		approvalError(w, err)
		return
	}
	if len(parts) == 2 && parts[1] == "progress" {
		if r.Method != http.MethodGet {
			writeV1Error(w, 405, "method_not_allowed", "Use GET")
			return
		}
		s.handleApprovalProgress(w, *scan)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		var body struct{}
		if !decodeApprovalRequest(w, r, &body) {
			return
		}
		if err := s.approvalStore().CancelApprovalScan(user.ID, scan.ID, time.Now()); err != nil {
			approvalError(w, err)
			return
		}
		updated, err := s.approvalStore().GetApprovalScan(user.ID, scan.ID)
		if err != nil {
			approvalError(w, err)
			return
		}
		writeV1JSON(w, 202, map[string]string{"scan_id": scan.ID, "status": updated.Status})
		return
	}
	if len(parts) >= 3 && parts[1] == "targets" {
		target, err := s.approvalStore().GetApprovalTarget(user.ID, scan.ID, parts[2])
		if err != nil {
			approvalError(w, err)
			return
		}
		if !s.approvalCanRead(r.Context(), user.ID, *target) {
			writeV1Error(w, 404, "not_found", "Target not accessible")
			return
		}
		if len(parts) == 4 && parts[3] == "recheck" && r.Method == http.MethodPost {
			var body struct {
				ExpectedHeadSHA string `json:"expected_head_sha"`
			}
			if !decodeApprovalRequest(w, r, &body) {
				return
			}
			s.admitApprovalScan(w, r, user, approvalScanRequest{Targets: []approval.Target{{Owner: target.Owner, Repo: target.Repo, Number: target.Number, ExpectedHeadSHA: body.ExpectedHeadSHA}}}, "recheck")
			return
		}
		if len(parts) == 3 && r.Method == http.MethodGet {
			response, ok := s.approvalReadResponse(r.Context(), user, *target, true)
			if !ok {
				writeV1Error(w, 404, "not_found", "Target not accessible")
				return
			}
			writeV1JSON(w, 200, response)
			return
		}
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		limit, cursor, err := approvalPage(r)
		if err != nil {
			writeV1Error(w, 400, "invalid_request", err.Error())
			return
		}
		rows, err := s.approvalStore().ListApprovalTargets(user.ID, scan.ID, limit+1, cursor)
		if err != nil {
			approvalError(w, err)
			return
		}
		next := ""
		if len(rows) > limit {
			rows = rows[:limit]
			next = rows[len(rows)-1].ID
		}
		targets := s.approvalReadResponses(r.Context(), user, rows)
		writeV1JSON(w, 200, map[string]any{"scan": approvalScanDTO(*scan), "targets": targets, "next_cursor": next})
		return
	}
	if len(parts) == 1 || (len(parts) == 2 && parts[1] == "cancel") || (len(parts) == 3 && parts[1] == "targets") || (len(parts) == 4 && parts[1] == "targets" && parts[3] == "recheck") {
		writeV1Error(w, 405, "method_not_allowed", "Unsupported method for approval operation")
		return
	}
	writeV1Error(w, 404, "not_found", "Unknown approval operation")
}

func (s *Server) handleApprovalCandidates(w http.ResponseWriter, r *http.Request) {
	user, ok := s.approvalRequest(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeV1Error(w, 405, "method_not_allowed", "Use GET")
		return
	}
	limit, cursor, err := approvalPage(r)
	if err != nil {
		writeV1Error(w, 400, "invalid_request", err.Error())
		return
	}
	rows, err := s.approvalStore().ListCurrentApprovalTargets(user.ID, limit+1, cursor)
	if err != nil {
		approvalError(w, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].ID
	}
	targets := s.approvalReadResponses(r.Context(), user, rows)
	writeV1JSON(w, 200, map[string]any{"targets": targets, "next_cursor": next})
}

func approvalViewerMatches(user *db.User, id int64, login string) bool {
	if user.GitHubID > 0 && id > 0 {
		return user.GitHubID == id
	}
	return strings.EqualFold(login, user.GitHubUsername)
}

// approvalCheckConcurrency bounds the GitHub reads admission makes in parallel;
// the reads are independent per PR, and a serial pass over a full scan took
// most of a minute.
const approvalCheckConcurrency = 16

type approvalCheck struct {
	pr         *gh.PullRequest
	prErr      error
	reviews    []*gh.PullRequestReview
	reviewsErr error
}

// fetchApprovalChecks reads each target's PR and reviews, several at a time,
// and returns the results in target order so admission reports the same
// first failure a serial pass would.
func (s *Server) fetchApprovalChecks(ctx context.Context, targets []approval.Target) []approvalCheck {
	checks := make([]approvalCheck, len(targets))
	sem := make(chan struct{}, approvalCheckConcurrency)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target approval.Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := &checks[i]
			c.pr, _, c.prErr = s.ghClient.GetPR(ctx, target.Owner, target.Repo, target.Number)
			if c.prErr == nil {
				s.approvalPRs.put(approvalPRKey(target.Owner, target.Repo, target.Number), c.pr, time.Now())
				c.reviews, c.reviewsErr = s.ghClient.ListAllReviews(ctx, target.Owner, target.Repo, target.Number)
			}
		}(i, target)
	}
	wg.Wait()
	return checks
}
