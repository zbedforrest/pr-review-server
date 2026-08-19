package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pr-review-server/auth"
	"pr-review-server/db"
	"pr-review-server/gcs"
	localgithub "pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/poller"
)

const (
	reviewRunsPath         = "/api/v1/review-runs"
	reviewRunsPathPrefix   = reviewRunsPath + "/"
	reviewCapabilitiesPath = "/api/v1/review-capabilities"
	maxReviewRunBodyBytes  = 64 << 10
	maxIdempotencyKeyBytes = 200
)

type createReviewRunRequest struct {
	Target createReviewRunTarget `json:"target"`
	Config runconfig.Overrides   `json:"config"`
}

type createReviewRunTarget struct {
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	PullRequest     int    `json:"pull_request"`
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
}

type reviewRunResponse struct {
	RunID            string                     `json:"run_id"`
	Target           reviewRunTargetResponse    `json:"target"`
	Status           string                     `json:"status"`
	TriggerSource    string                     `json:"trigger_source"`
	AcceptedAt       time.Time                  `json:"accepted_at"`
	QueuedAt         time.Time                  `json:"queued_at"`
	StartedAt        *time.Time                 `json:"started_at,omitempty"`
	CompletedAt      *time.Time                 `json:"completed_at,omitempty"`
	DurationMS       int64                      `json:"duration_ms"`
	ExecutionAttempt int                        `json:"execution_attempt"`
	TerminalCode     string                     `json:"terminal_code,omitempty"`
	FailureStage     string                     `json:"failure_stage,omitempty"`
	Config           reviewRunConfigResponse    `json:"config"`
	Result           reviewRunResultResponse    `json:"result"`
	Models           []payload.ModelUse         `json:"models"`
	Attempts         []reviewRunAttemptResponse `json:"attempts,omitempty"`
	Failure          *reviewRunFailureResponse  `json:"failure,omitempty"`
	Links            reviewRunLinksResponse     `json:"links"`
}

type reviewRunTargetResponse struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	PullRequest int    `json:"pull_request"`
	CommitSHA   string `json:"commit_sha"`
}

type reviewRunConfigResponse struct {
	Requested     runconfig.Overrides `json:"requested"`
	Effective     runconfig.Effective `json:"effective"`
	Sources       map[string]string   `json:"sources"`
	Hash          string              `json:"hash"`
	SchemaVersion int                 `json:"schema_version"`
}

type reviewRunResultResponse struct {
	Critical                 int    `json:"critical"`
	Medium                   int    `json:"medium"`
	Low                      int    `json:"low"`
	Verdict                  string `json:"verdict,omitempty"`
	ModelFallback            bool   `json:"model_fallback"`
	ServingModelVerification string `json:"serving_model_verification,omitempty"`
	PublicationStatus        string `json:"publication_status,omitempty"`
}

type reviewRunFailureResponse struct {
	Code    string `json:"code,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message,omitempty"`
}

type reviewRunLinksResponse struct {
	Self     string `json:"self"`
	Review   string `json:"review"`
	Findings string `json:"findings"`
}

type reviewRunAttemptResponse struct {
	ExecutionAttempt     int        `json:"execution_attempt"`
	Stage                string     `json:"stage"`
	InvocationNumber     int        `json:"invocation_number"`
	AttemptNumber        int        `json:"attempt_number"`
	Provider             string     `json:"provider,omitempty"`
	Backend              string     `json:"backend,omitempty"`
	RequestedModel       string     `json:"requested_model,omitempty"`
	ResolvedModel        string     `json:"resolved_model,omitempty"`
	ObservedServedModels []string   `json:"observed_served_models,omitempty"`
	PrimaryServedModel   string     `json:"primary_served_model,omitempty"`
	ServedModelSource    string     `json:"served_model_source,omitempty"`
	ServingModelVerified bool       `json:"serving_model_verified"`
	Fallback             bool       `json:"fallback"`
	FallbackReason       string     `json:"fallback_reason,omitempty"`
	MatcherVersion       string     `json:"matcher_version,omitempty"`
	Effort               string     `json:"effort,omitempty"`
	Status               string     `json:"status"`
	AssistantTurns       int        `json:"assistant_turns"`
	InputTokens          int64      `json:"input_tokens"`
	OutputTokens         int64      `json:"output_tokens"`
	TotalTokens          int64      `json:"total_tokens"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	DurationMS           int64      `json:"duration_ms"`
	StopReason           string     `json:"stop_reason,omitempty"`
	ErrorCode            string     `json:"error_code,omitempty"`
}

type reviewRunCursor struct {
	AcceptedAt time.Time `json:"accepted_at"`
	RunID      string    `json:"run_id"`
}

type reviewRunListResponse struct {
	Runs       []reviewRunResponse `json:"runs"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type reviewCapabilitiesResponse struct {
	SchemaVersion int                                `json:"schema_version"`
	Available     bool                               `json:"available"`
	Defaults      runconfig.Effective                `json:"defaults"`
	Backends      map[string]reviewBackendCapability `json:"backends"`
	Limits        reviewCustomizationLimits          `json:"limits"`
}

type reviewBackendCapability struct {
	Available bool     `json:"available"`
	Models    []string `json:"models"`
	Efforts   []string `json:"efforts"`
}

type reviewCustomizationLimits struct {
	MaxWallClockSeconds int `json:"max_wall_clock_seconds"`
	MaxTurns            int `json:"max_turns"`
	MaxFirstPassSamples int `json:"max_first_pass_samples"`
}

type v1ErrorEnvelope struct {
	Error v1ErrorBody `json:"error"`
}

type v1ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// withV1Auth adapts the existing browser/API authentication middleware to the
// versioned API's stable JSON error contract. Successful responses pass
// through untouched; a middleware-generated plain-text 401 is replaced before
// any bytes reach the client.
func withV1Auth(authMiddleware func(http.Handler) http.Handler, handler http.HandlerFunc) http.Handler {
	protected := authMiddleware(http.HandlerFunc(handler))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected.ServeHTTP(&v1AuthResponseWriter{ResponseWriter: w}, r)
	})
}

type v1AuthResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
	denied      bool
}

func (w *v1AuthResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status == http.StatusUnauthorized {
		// Inner v1 handlers already speak the versioned JSON contract. Only
		// replace the legacy auth middleware's plain-text response.
		if strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "application/json") {
			w.ResponseWriter.WriteHeader(status)
			return
		}
		w.denied = true
		w.Header().Del("Content-Length")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(status)
		_ = json.NewEncoder(w.ResponseWriter).Encode(v1ErrorEnvelope{ // nolint:errcheck
			Error: v1ErrorBody{Code: "unauthorized", Message: "authentication is required"},
		})
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *v1AuthResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.denied {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (s *Server) handleReviewRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateReviewRun(w, r)
	case http.MethodGet:
		s.handleListReviewRuns(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeV1Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleCreateReviewRun(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r)
	if user == nil {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	if s.poller == nil || s.ghClient == nil || s.cfg == nil || !s.cfg.ReviewerEnabled {
		writeV1Error(w, http.StatusServiceUnavailable, "review_service_unavailable", "review service is unavailable")
		return
	}

	var request createReviewRunRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxReviewRunBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeV1Error(w, status, "invalid_request", "request body must be one valid JSON object: "+err.Error())
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := normalizeCreateReviewRunRequest(&request); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_target", err.Error())
		return
	}

	requestHash, err := hashCreateReviewRunRequest(request)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "internal_error", "failed to canonicalize request")
		return
	}
	idempotencyKey, err := parseIdempotencyKey(r.Header.Values("Idempotency-Key"))
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	idempotencyScope := ""
	idempotencyKeyHash := ""
	if idempotencyKey != "" {
		idempotencyScope = "user:" + strconv.Itoa(user.ID)
		idempotencyKeyHash = sha256Hex(idempotencyKey)
		existing, lookupErr := s.db.GetReviewRunByIdempotency(idempotencyScope, idempotencyKeyHash)
		if lookupErr != nil {
			writeV1Error(w, http.StatusInternalServerError, "database_error", "failed to look up idempotent request")
			return
		}
		if existing != nil {
			s.writeIdempotentReviewRun(w, existing, requestHash)
			return
		}
	}

	target := request.Target
	ghPR, ghResponse, err := s.ghClient.GetPR(r.Context(), target.Owner, target.Repo, target.PullRequest)
	if err != nil {
		status := http.StatusBadGateway
		code := "github_error"
		if ghResponse != nil && ghResponse.StatusCode == http.StatusNotFound {
			status = http.StatusNotFound
			code = "pull_request_not_found"
		}
		writeV1Error(w, status, code, "failed to fetch pull request from GitHub")
		return
	}
	if ghPR == nil || ghPR.GetHead().GetSHA() == "" {
		writeV1Error(w, http.StatusBadGateway, "invalid_github_response", "GitHub returned no pull request HEAD commit")
		return
	}
	headSHA := strings.ToLower(ghPR.GetHead().GetSHA())
	if target.ExpectedHeadSHA != "" && !strings.EqualFold(target.ExpectedHeadSHA, headSHA) {
		writeV1Error(w, http.StatusConflict, "head_sha_mismatch",
			fmt.Sprintf("pull request HEAD is %s, not expected commit %s", headSHA, target.ExpectedHeadSHA))
		return
	}

	createdAt := ghPR.GetCreatedAt().Time
	var createdAtPtr *time.Time
	if !createdAt.IsZero() {
		createdAt = createdAt.UTC()
		createdAtPtr = &createdAt
	}
	// GitHub returns the target repository's canonical display casing in the
	// base repository. Reuse it for the mutable PR projection so an older
	// poll-created row such as Acme/Widgets is not split from an API request for
	// acme/widgets. The review-run DB invariant remains case-insensitive.
	canonicalOwner, canonicalRepo := target.Owner, target.Repo
	if base := ghPR.GetBase(); base != nil {
		if baseRepo := base.GetRepo(); baseRepo != nil {
			if owner := strings.TrimSpace(baseRepo.GetOwner().GetLogin()); isSafeGitHubName(owner) {
				canonicalOwner = owner
			}
			if repo := strings.TrimSpace(baseRepo.GetName()); isSafeGitHubName(repo) {
				canonicalRepo = repo
			}
		}
	}
	pr := localgithub.PullRequest{
		Owner: canonicalOwner, Repo: canonicalRepo, Number: target.PullRequest, CommitSHA: headSHA,
		Title: ghPR.GetTitle(), Author: ghPR.GetUser().GetLogin(), URL: ghPR.GetHTMLURL(),
		CreatedAt: createdAtPtr, Draft: ghPR.GetDraft(),
	}
	job, err := s.poller.PrepareReviewJob(pr, request.Config, true, "api_v1", &user.ID)
	if err != nil {
		var validationErr *runconfig.ValidationError
		if errors.As(err, &validationErr) {
			writeV1Error(w, http.StatusUnprocessableEntity, "invalid_review_config", validationErr.Error())
			return
		}
		writeV1Error(w, http.StatusInternalServerError, "review_config_error", "failed to resolve review configuration")
		return
	}
	job.IdempotencyScope = idempotencyScope
	job.IdempotencyKeyHash = idempotencyKeyHash
	job.RequestHash = requestHash
	if err := s.poller.ProcessReviewJob(r.Context(), job); err != nil {
		if idempotencyScope != "" {
			existing, lookupErr := s.waitForIdempotentReviewRun(r.Context(), idempotencyScope, idempotencyKeyHash)
			if lookupErr != nil && !errors.Is(lookupErr, context.Canceled) && !errors.Is(lookupErr, context.DeadlineExceeded) {
				writeV1Error(w, http.StatusInternalServerError, "database_error", "failed to resolve idempotent admission")
				return
			}
			if existing != nil {
				s.writeIdempotentReviewRun(w, existing, requestHash)
				return
			}
		}
		if errors.Is(err, poller.ErrReviewAlreadyTracked) || errors.Is(err, db.ErrReviewRunActiveConflict) {
			writeV1Error(w, http.StatusConflict, "review_already_active", "a review is already active for this pull request")
			return
		}
		var validationErr *runconfig.ValidationError
		if errors.As(err, &validationErr) {
			writeV1Error(w, http.StatusUnprocessableEntity, "invalid_review_config", validationErr.Error())
			return
		}
		writeV1Error(w, http.StatusInternalServerError, "review_admission_failed", "failed to accept review run")
		return
	}

	// Admission owns the ordering: never expose a new pending PR until its run
	// is durable. Otherwise a rejected custom request could be picked up by the
	// automatic poller and reviewed with deployment defaults instead.
	// UpsertPR intentionally refreshes poller-owned approval and CI columns.
	// Preserve those fields by merging API metadata into the current projection
	// rather than constructing a partial row that would clear them on conflict.
	prRow, getPRErr := s.db.GetPR(pr.Owner, pr.Repo, pr.Number)
	if getPRErr != nil {
		log.Printf("[API/v1] accepted run %s but failed to load PR metadata: %v", job.RunID, getPRErr)
	} else {
		if prRow == nil {
			prRow = &db.PR{Status: "pending"}
		}
		prRow.RepoOwner, prRow.RepoName, prRow.PRNumber = pr.Owner, pr.Repo, pr.Number
		prRow.LastCommitSHA = pr.CommitSHA
		prRow.Title, prRow.Author, prRow.CreatedAt, prRow.Draft = pr.Title, pr.Author, pr.CreatedAt, pr.Draft
		if err := s.db.UpsertPR(prRow); err != nil {
			log.Printf("[API/v1] accepted run %s but failed to persist PR metadata: %v", job.RunID, err)
			prRow = nil
		}
	}
	if prRow != nil {
		prState := strings.ToLower(ghPR.GetState())
		if ghPR.GetMerged() {
			prState = "merged"
		}
		if err := s.db.SetPRState(pr.Owner, pr.Repo, pr.Number, prState); err != nil {
			log.Printf("[API/v1] accepted run %s but failed to persist PR state: %v", job.RunID, err)
		}
	}

	accepted, err := s.db.GetReviewRun(job.RunID)
	if err != nil || accepted == nil {
		writeV1Error(w, http.StatusInternalServerError, "database_error", "accepted review run could not be loaded")
		return
	}
	response, err := s.buildReviewRunResponse(accepted, false)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "invalid_run_metadata", "accepted review run metadata is invalid")
		return
	}
	w.Header().Set("Location", response.Links.Self)
	writeV1JSON(w, http.StatusAccepted, response)
}

func (s *Server) writeIdempotentReviewRun(w http.ResponseWriter, run *db.ReviewRun, requestHash string) {
	if run.RequestHash != requestHash {
		writeV1Error(w, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used for a different request")
		return
	}
	response, err := s.buildReviewRunResponse(run, false)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "invalid_run_metadata", "stored review run metadata is invalid")
		return
	}
	status := http.StatusOK
	if isLiveReviewRunStatus(run.Status) {
		status = http.StatusAccepted
	}
	w.Header().Set("Location", response.Links.Self)
	w.Header().Set("Idempotency-Replayed", "true")
	writeV1JSON(w, status, response)
}

// waitForIdempotentReviewRun closes the narrow same-instance race where the
// winning request has acquired in-memory PR ownership but has not committed
// its durable run row yet. Cross-instance unique-key arbitration normally
// resolves on the first lookup; bounded retries keep different-key conflicts
// fast while ensuring identical requests replay the winner.
func (s *Server) waitForIdempotentReviewRun(ctx context.Context, scope, keyHash string) (*db.ReviewRun, error) {
	const attempts = 5
	const delay = 10 * time.Millisecond
	for attempt := 0; attempt < attempts; attempt++ {
		run, err := s.db.GetReviewRunByIdempotency(scope, keyHash)
		if err != nil || run != nil {
			return run, err
		}
		if attempt == attempts-1 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, nil
}

func (s *Server) handleReviewRunByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeV1Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if auth.GetCurrentUser(r) == nil {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	runID := strings.TrimPrefix(r.URL.Path, reviewRunsPathPrefix)
	if !isSafeRunID(runID) || strings.Contains(runID, "/") {
		writeV1Error(w, http.StatusBadRequest, "invalid_run_id", "run_id is invalid")
		return
	}
	run, err := s.db.GetReviewRun(runID)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "database_error", "failed to load review run")
		return
	}
	if run == nil {
		writeV1Error(w, http.StatusNotFound, "review_run_not_found", "review run was not found")
		return
	}
	response, err := s.buildReviewRunResponse(run, true)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "invalid_run_metadata", "stored review run metadata is invalid")
		return
	}
	writeV1JSON(w, http.StatusOK, response)
}

func (s *Server) handleListReviewRuns(w http.ResponseWriter, r *http.Request) {
	if auth.GetCurrentUser(r) == nil {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	query := r.URL.Query()
	owner := strings.TrimSpace(query.Get("owner"))
	repo := strings.TrimSpace(query.Get("repo"))
	if (owner == "") != (repo == "") {
		writeV1Error(w, http.StatusBadRequest, "invalid_filter", "owner and repo must be supplied together")
		return
	}
	if owner != "" && (!isSafeGitHubName(owner) || !isSafeGitHubName(repo)) {
		writeV1Error(w, http.StatusBadRequest, "invalid_filter", "owner or repo is invalid")
		return
	}
	prNumber, err := parseOptionalPositiveInt(query.Get("pull_request"), "pull_request")
	if err != nil || (prNumber > 0 && owner == "") {
		writeV1Error(w, http.StatusBadRequest, "invalid_filter", "pull_request requires owner and repo and must be positive")
		return
	}
	commitSHA := strings.ToLower(strings.TrimSpace(query.Get("commit_sha")))
	if commitSHA != "" && !isFullSHA(commitSHA) {
		writeV1Error(w, http.StatusBadRequest, "invalid_filter", "commit_sha must be a full 40-character hexadecimal SHA")
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isReviewRunStatus(status) {
		writeV1Error(w, http.StatusBadRequest, "invalid_filter", "status is invalid")
		return
	}
	limit := 50
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 || limit > 100 {
			writeV1Error(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
			return
		}
	}
	var cursor reviewRunCursor
	if rawCursor := strings.TrimSpace(query.Get("cursor")); rawCursor != "" {
		cursor, err = decodeReviewRunCursor(rawCursor)
		if err != nil {
			writeV1Error(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
	}
	runs, err := s.db.ListReviewRuns(db.ReviewRunFilter{
		RepoOwner: owner, RepoName: repo, PRNumber: prNumber, CommitSHA: commitSHA, Status: status,
		BeforeAcceptedAt: cursor.AcceptedAt, BeforeRunID: cursor.RunID, Limit: limit + 1,
	})
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "database_error", "failed to list review runs")
		return
	}
	hasMore := len(runs) > limit
	if hasMore {
		runs = runs[:limit]
	}
	responses := make([]reviewRunResponse, 0, len(runs))
	for i := range runs {
		response, buildErr := s.buildReviewRunResponse(&runs[i], false)
		if buildErr != nil {
			writeV1Error(w, http.StatusInternalServerError, "invalid_run_metadata", "stored review run metadata is invalid")
			return
		}
		responses = append(responses, response)
	}
	result := reviewRunListResponse{Runs: responses}
	if hasMore && len(runs) > 0 {
		result.NextCursor, err = encodeReviewRunCursor(reviewRunCursor{
			AcceptedAt: runs[len(runs)-1].AcceptedAt,
			RunID:      runs[len(runs)-1].RunID,
		})
		if err != nil {
			writeV1Error(w, http.StatusInternalServerError, "internal_error", "failed to encode pagination cursor")
			return
		}
	}
	writeV1JSON(w, http.StatusOK, result)
}

func (s *Server) handleReviewCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeV1Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if auth.GetCurrentUser(r) == nil {
		writeV1Error(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	if s.poller == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "review_service_unavailable", "review service is unavailable")
		return
	}
	defaults, policy, err := s.poller.ReviewConfigDefaultsAndPolicy()
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "review_config_error", "failed to load review capabilities")
		return
	}
	backends := make(map[string]reviewBackendCapability, len(policy.Backends))
	for name, backend := range policy.Backends {
		backends[name] = reviewBackendCapability{
			Available: backend.Available,
			Models:    append([]string(nil), backend.Models...),
			Efforts:   append([]string(nil), backend.Efforts...),
		}
	}
	writeV1JSON(w, http.StatusOK, reviewCapabilitiesResponse{
		SchemaVersion: runconfig.SchemaVersion,
		Available:     s.cfg != nil && s.cfg.ReviewerEnabled,
		Defaults:      defaults,
		Backends:      backends,
		Limits: reviewCustomizationLimits{
			MaxWallClockSeconds: policy.MaxWallClockSeconds,
			MaxTurns:            policy.MaxTurns,
			MaxFirstPassSamples: policy.MaxFirstPassSamples,
		},
	})
}

func (s *Server) buildReviewRunResponse(run *db.ReviewRun, includeAttempts bool) (reviewRunResponse, error) {
	var requested runconfig.Overrides
	if err := json.Unmarshal([]byte(run.RequestedConfigJSON), &requested); err != nil {
		return reviewRunResponse{}, fmt.Errorf("decode requested config: %w", err)
	}
	var effective runconfig.Effective
	if err := json.Unmarshal([]byte(run.EffectiveConfigJSON), &effective); err != nil {
		return reviewRunResponse{}, fmt.Errorf("decode effective config: %w", err)
	}
	var sources map[string]string
	if err := json.Unmarshal([]byte(run.ConfigSourcesJSON), &sources); err != nil {
		return reviewRunResponse{}, fmt.Errorf("decode config sources: %w", err)
	}
	models := make([]payload.ModelUse, 0)
	if run.ActualModelsJSON != "" {
		if err := json.Unmarshal([]byte(run.ActualModelsJSON), &models); err != nil {
			return reviewRunResponse{}, fmt.Errorf("decode actual models: %w", err)
		}
	}
	htmlPath := run.HTMLPath
	if htmlPath == "" {
		htmlPath = gcs.ReviewRunFileName(run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA, run.RunID)
	}
	jsonPath := run.JSONPath
	if jsonPath == "" {
		jsonPath = gcs.ReviewRunJSONFileName(run.RepoOwner, run.RepoName, run.PRNumber, run.CommitSHA, run.RunID)
	}
	response := reviewRunResponse{
		RunID: run.RunID,
		Target: reviewRunTargetResponse{
			Owner: run.RepoOwner, Repo: run.RepoName, PullRequest: run.PRNumber, CommitSHA: run.CommitSHA,
		},
		Status: run.Status, TriggerSource: run.TriggerSource, AcceptedAt: run.AcceptedAt, QueuedAt: run.QueuedAt,
		StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, DurationMS: run.DurationMS,
		ExecutionAttempt: run.ExecutionAttempt, TerminalCode: run.TerminalCode, FailureStage: run.FailureStage,
		Config: reviewRunConfigResponse{
			Requested: requested, Effective: effective, Sources: sources,
			Hash: run.ConfigHash, SchemaVersion: run.ConfigSchemaVersion,
		},
		Result: reviewRunResultResponse{
			Critical: run.CriticalCount, Medium: run.MediumCount, Low: run.LowCount, Verdict: run.Verdict,
			ModelFallback: run.ModelFallback, ServingModelVerification: run.ServingModelVerification,
			PublicationStatus: run.PublicationStatus,
		},
		Models: models,
		Links: reviewRunLinksResponse{
			Self:     reviewRunsPathPrefix + url.PathEscape(run.RunID),
			Review:   reviewURL(htmlPath),
			Findings: reviewURL(jsonPath),
		},
	}
	if run.ErrorSummary != "" {
		response.Failure = &reviewRunFailureResponse{
			Code: run.TerminalCode, Stage: run.FailureStage, Message: publicFailureMessage(run.TerminalCode),
		}
	}
	if includeAttempts {
		attempts, err := s.db.ListReviewStageAttempts(run.RunID)
		if err != nil {
			return reviewRunResponse{}, err
		}
		response.Attempts = make([]reviewRunAttemptResponse, 0, len(attempts))
		for _, attempt := range attempts {
			response.Attempts = append(response.Attempts, reviewRunAttemptResponse{
				ExecutionAttempt: attempt.ExecutionAttempt, Stage: attempt.Stage,
				InvocationNumber: attempt.InvocationNumber, AttemptNumber: attempt.AttemptNumber,
				Provider: attempt.Provider, Backend: attempt.Backend, RequestedModel: attempt.RequestedModel,
				ResolvedModel: attempt.ResolvedModel, ObservedServedModels: attempt.ObservedServedModels,
				PrimaryServedModel: attempt.PrimaryServedModel, ServedModelSource: attempt.ServedModelSource,
				ServingModelVerified: attempt.ServingModelVerified, Fallback: attempt.Fallback,
				FallbackReason: attempt.FallbackReason, MatcherVersion: attempt.MatcherVersion,
				Effort: attempt.Effort, Status: attempt.Status, AssistantTurns: attempt.AssistantTurns,
				InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, TotalTokens: attempt.TotalTokens,
				StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt, DurationMS: attempt.DurationMS,
				StopReason: attempt.StopReason, ErrorCode: attempt.ErrorCode,
			})
		}
	}
	return response, nil
}

func normalizeCreateReviewRunRequest(request *createReviewRunRequest) error {
	request.Target.Owner = strings.ToLower(strings.TrimSpace(request.Target.Owner))
	request.Target.Repo = strings.ToLower(strings.TrimSpace(request.Target.Repo))
	request.Target.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(request.Target.ExpectedHeadSHA))
	if !isSafeGitHubName(request.Target.Owner) || !isSafeGitHubName(request.Target.Repo) || request.Target.PullRequest <= 0 {
		return errors.New("target.owner, target.repo, and a positive target.pull_request are required")
	}
	if request.Target.ExpectedHeadSHA != "" && !isFullSHA(request.Target.ExpectedHeadSHA) {
		return errors.New("target.expected_head_sha must be a full 40-character hexadecimal SHA")
	}
	return nil
}

func isSafeGitHubName(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}

func isFullSHA(value string) bool {
	return len(value) == 40 && isSafeSHA(value)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func hashCreateReviewRunRequest(request createReviewRunRequest) (string, error) {
	body, err := json.Marshal(struct {
		Version int                   `json:"version"`
		Target  createReviewRunTarget `json:"target"`
		Config  runconfig.Overrides   `json:"config"`
	}{Version: 1, Target: request.Target, Config: request.Config})
	if err != nil {
		return "", err
	}
	return sha256Hex(string(body)), nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func parseIdempotencyKey(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", errors.New("exactly one Idempotency-Key header is allowed")
	}
	value := values[0]
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxIdempotencyKeyBytes {
		return "", fmt.Errorf("Idempotency-Key must contain 1 to %d visible ASCII characters", maxIdempotencyKeyBytes)
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return "", errors.New("Idempotency-Key must contain visible ASCII characters only")
		}
	}
	return value, nil
}

func parseOptionalPositiveInt(raw, field string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return value, nil
}

func isReviewRunStatus(status string) bool {
	switch status {
	case db.ReviewRunStatusQueued, db.ReviewRunStatusRunning, db.ReviewRunStatusCompleted,
		db.ReviewRunStatusFailed, db.ReviewRunStatusTimedOut, db.ReviewRunStatusCancelled:
		return true
	default:
		return false
	}
}

func isLiveReviewRunStatus(status string) bool {
	return status == db.ReviewRunStatusQueued || status == db.ReviewRunStatusRunning
}

func encodeReviewRunCursor(cursor reviewRunCursor) (string, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func decodeReviewRunCursor(value string) (reviewRunCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return reviewRunCursor{}, err
	}
	var cursor reviewRunCursor
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return reviewRunCursor{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return reviewRunCursor{}, err
	}
	if cursor.AcceptedAt.IsZero() || !isSafeRunID(cursor.RunID) {
		return reviewRunCursor{}, errors.New("cursor is incomplete")
	}
	cursor.AcceptedAt = cursor.AcceptedAt.UTC()
	return cursor, nil
}

func publicFailureMessage(terminalCode string) string {
	switch terminalCode {
	case "provider_init_failed":
		return "the configured review provider could not be initialized"
	case "dispatch_failed", "dispatch_lost":
		return "the accepted review could not be dispatched"
	case "review_failed", "artifact_save_failed":
		return "review generation failed"
	case "run_timeout", "lease_abandoned", "queue_abandoned":
		return "the review exceeded its execution budget"
	case "cache_restore_failed":
		return "a cached review artifact could not be restored"
	case "commit_outdated":
		return "the pull request HEAD changed while the review was running"
	case "cancelled", "pr_already_claimed", "superseded":
		return "the review was cancelled before publication"
	case "claim_reload_failed":
		return "the accepted review could not acquire execution ownership"
	default:
		return "the review did not complete successfully"
	}
}

func writeV1Error(w http.ResponseWriter, status int, code, message string) {
	writeV1JSON(w, status, v1ErrorEnvelope{Error: v1ErrorBody{Code: code, Message: message}})
}

func writeV1JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value) // nolint:errcheck
}
