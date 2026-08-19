package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/poller"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reviewAPITestPoller struct {
	database     db.Database
	defaults     runconfig.Effective
	policy       runconfig.Policy
	jobs         []poller.ReviewJob
	prepareCalls int
	processCalls int
	processErr   error
}

func newReviewAPITestPoller(database db.Database) *reviewAPITestPoller {
	defaults := runconfig.Effective{
		SchemaVersion: runconfig.SchemaVersion,
		Agent: runconfig.Agent{
			Enabled: true, Backend: service.AgentBackendClaude, Model: "claude-fable-5",
			Effort: "medium", WallClockSeconds: 360, MaxTurns: 40,
		},
		FirstPass: runconfig.FirstPass{Samples: 3},
	}
	return &reviewAPITestPoller{
		database: database,
		defaults: defaults,
		policy: runconfig.Policy{
			Backends: map[string]runconfig.BackendPolicy{
				service.AgentBackendClaude: {
					Available: true, Models: []string{"claude-fable-5"}, Efforts: []string{"medium", "high"},
				},
				service.AgentBackendOpenRouter: {
					Available: true, Models: []string{"openai/gpt-5.6-sol"}, Efforts: []string{"medium", "high"},
				},
			},
			MaxWallClockSeconds: 900, MaxTurns: 120, MaxFirstPassSamples: 5,
		},
	}
}

func (p *reviewAPITestPoller) GetReviewerStatus() (bool, time.Duration) { return false, 0 }
func (p *reviewAPITestPoller) GetLastPollTime() time.Time               { return time.Time{} }
func (p *reviewAPITestPoller) GetPollingInterval() time.Duration        { return time.Minute }
func (p *reviewAPITestPoller) GetSecondsUntilNextPoll() int             { return 60 }
func (p *reviewAPITestPoller) ProcessReviewImmediate(context.Context, string, string, int, string, string, string, *time.Time, bool, bool) {
}
func (p *reviewAPITestPoller) IsReviewTracked(string, string, int) bool { return false }

func (p *reviewAPITestPoller) ReviewConfigDefaultsAndPolicy() (runconfig.Effective, runconfig.Policy, error) {
	return p.defaults, p.policy, nil
}

func (p *reviewAPITestPoller) PrepareReviewJob(pr github.PullRequest, requested runconfig.Overrides, force bool, triggerSource string, requestedByUserID *int) (poller.ReviewJob, error) {
	p.prepareCalls++
	snapshot, err := runconfig.Resolve(requested, p.defaults, p.policy)
	if err != nil {
		return poller.ReviewJob{}, err
	}
	return poller.ReviewJob{
		PR: pr, RunID: fmt.Sprintf("run-%032x", p.prepareCalls), Config: snapshot,
		TriggerSource: triggerSource, RequestedByUserID: requestedByUserID, Force: force,
	}, nil
}

func (p *reviewAPITestPoller) ProcessReviewJob(_ context.Context, job poller.ReviewJob) error {
	p.processCalls++
	if p.processErr != nil {
		return p.processErr
	}
	p.jobs = append(p.jobs, job)
	requestedJSON, err := json.Marshal(job.Config.Requested)
	if err != nil {
		return err
	}
	effectiveJSON, err := json.Marshal(job.Config.Effective)
	if err != nil {
		return err
	}
	sourcesJSON, err := json.Marshal(job.Config.Sources)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return p.database.CreateReviewRun(&db.ReviewRun{
		RunID: job.RunID, RepoOwner: job.PR.Owner, RepoName: job.PR.Repo, PRNumber: job.PR.Number,
		CommitSHA: job.PR.CommitSHA, RequestedByUserID: job.RequestedByUserID, TriggerSource: job.TriggerSource,
		Status: db.ReviewRunStatusQueued, RequestedConfigJSON: string(requestedJSON),
		EffectiveConfigJSON: string(effectiveJSON), ConfigSourcesJSON: string(sourcesJSON),
		ConfigHash: job.Config.Hash, ConfigSchemaVersion: job.Config.Effective.SchemaVersion,
		AgentBackend: job.Config.Effective.Agent.Backend, AgentModel: job.Config.Effective.Agent.Model,
		AgentEffort: job.Config.Effective.Agent.Effort, AgentWallClockSec: job.Config.Effective.Agent.WallClockSeconds,
		AgentMaxTurns: job.Config.Effective.Agent.MaxTurns, AcceptedAt: now, QueuedAt: now,
		IdempotencyScope: job.IdempotencyScope, IdempotencyKeyHash: job.IdempotencyKeyHash,
		RequestHash: job.RequestHash,
	})
}

func newReviewAPIServer(t *testing.T, githubHandler http.HandlerFunc) (*Server, *db.GormDB, *reviewAPITestPoller, *int) {
	t.Helper()
	githubServer := httptest.NewServer(githubHandler)
	t.Cleanup(githubServer.Close)
	server, database := newTestServerWithGH(t, "reviewer", github.NewTestClient(githubServer.URL, "reviewer"))
	server.cfg.ReviewerEnabled = true
	t.Cleanup(func() { _ = database.Close() })
	requester := createTestUser(t, database, "requester")
	apiPoller := newReviewAPITestPoller(database)
	server.SetPoller(apiPoller)
	return server, database, apiPoller, &requester.ID
}

func addReviewAPIUser(req *http.Request, userID int) *http.Request {
	return addUserToRequest(req, &db.User{ID: userID, GitHubUsername: "requester"})
}

func githubPRResponse(headSHA string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"number":42,"title":"Configurable review","state":"open","draft":false,"user":{"login":"alice"},"head":{"sha":"%s"},"base":{"repo":{"name":"Widgets","owner":{"login":"Acme"}}},"html_url":"https://github.com/acme/widgets/pull/42","created_at":"2026-08-19T12:00:00Z"}`, headSHA)
	}
}

func configurableReviewRequest(headSHA string) string {
	return fmt.Sprintf(`{
		"target":{"owner":"acme","repo":"widgets","pull_request":42,"expected_head_sha":"%s"},
		"config":{"agent":{"backend":"openrouter","model":"openai/gpt-5.6-sol","effort":"high","wall_clock_seconds":720,"max_turns":100},"first_pass":{"samples":2},"required_checks":true}
	}`, headSHA)
}

func TestCreateReviewRunPersistsResolvedConfigAndIdempotency(t *testing.T) {
	headSHA := "0123456789abcdef0123456789abcdef01234567"
	githubCalls := 0
	s, database, apiPoller, userID := newReviewAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		githubCalls++
		githubPRResponse(headSHA)(w, r)
	})
	githubUpdatedAt := time.Date(2026, 8, 19, 11, 55, 0, 0, time.UTC)
	require.NoError(t, database.UpsertPR(&db.PR{
		RepoOwner: "Acme", RepoName: "Widgets", PRNumber: 42, LastCommitSHA: headSHA,
		Status: "completed", Title: "Previously polled", ApprovalCount: 3,
		MyReviewStatus: "APPROVED", CIState: "failure", CIFailedChecks: `["lint"]`,
		GitHubUpdatedAt: &githubUpdatedAt,
	}))
	body := strings.ReplaceAll(configurableReviewRequest(headSHA), `"owner":"acme","repo":"widgets"`, `"owner":"Acme","repo":"WIDGETS"`)

	request := httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(body))
	request.Header.Set("Idempotency-Key", "experiment-42")
	request = addReviewAPIUser(request, *userID)
	recorder := httptest.NewRecorder()
	s.handleReviewRuns(recorder, request)

	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
	require.Len(t, apiPoller.jobs, 1)
	job := apiPoller.jobs[0]
	assert.True(t, job.Force)
	assert.Equal(t, "api_v1", job.TriggerSource)
	assert.Equal(t, userID, job.RequestedByUserID)
	assert.Equal(t, service.AgentBackendOpenRouter, job.Config.Effective.Agent.Backend)
	assert.Equal(t, "Acme", job.PR.Owner)
	assert.Equal(t, "Widgets", job.PR.Repo)
	assert.Equal(t, 720, job.Config.Effective.Agent.WallClockSeconds)
	assert.Equal(t, 100, job.Config.Effective.Agent.MaxTurns)
	assert.Equal(t, 2, job.Config.Effective.FirstPass.Samples)
	assert.NotEmpty(t, job.RequestHash)
	assert.NotContains(t, job.IdempotencyKeyHash, "experiment-42")
	assert.Equal(t, "user:"+strconv.Itoa(*userID), job.IdempotencyScope)

	persisted, err := database.GetReviewRun(job.RunID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, job.IdempotencyKeyHash, persisted.IdempotencyKeyHash)
	assert.Equal(t, job.Config.Hash, persisted.ConfigHash)
	assert.Equal(t, 1, githubCalls)
	assert.Equal(t, reviewRunsPathPrefix+job.RunID, recorder.Header().Get("Location"))
	assert.NotContains(t, recorder.Body.String(), "experiment-42")
	allPRs, err := database.GetAllPRs()
	require.NoError(t, err)
	assert.Len(t, allPRs, 1, "canonical GitHub casing must reuse the poll-created PR projection")
	persistedPR, err := database.GetPR("Acme", "Widgets", 42)
	require.NoError(t, err)
	require.NotNil(t, persistedPR)
	assert.Equal(t, 3, persistedPR.ApprovalCount)
	assert.Equal(t, "APPROVED", persistedPR.MyReviewStatus)
	assert.Equal(t, "failure", persistedPR.CIState)
	assert.Equal(t, `["lint"]`, persistedPR.CIFailedChecks)
	require.NotNil(t, persistedPR.GitHubUpdatedAt)
	assert.Equal(t, githubUpdatedAt, *persistedPR.GitHubUpdatedAt)

	// Identical retries replay the durable run before making another GitHub or
	// admission call, even if the original run is still queued.
	replay := httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(body))
	replay.Header.Set("Idempotency-Key", "experiment-42")
	replay = addReviewAPIUser(replay, *userID)
	replayRecorder := httptest.NewRecorder()
	s.handleReviewRuns(replayRecorder, replay)
	assert.Equal(t, http.StatusAccepted, replayRecorder.Code)
	assert.Equal(t, "true", replayRecorder.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, 1, githubCalls)
	assert.Equal(t, 1, apiPoller.prepareCalls)
	assert.Equal(t, 1, apiPoller.processCalls)

	different := strings.Replace(body, `"samples":2`, `"samples":3`, 1)
	conflict := httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(different))
	conflict.Header.Set("Idempotency-Key", "experiment-42")
	conflict = addReviewAPIUser(conflict, *userID)
	conflictRecorder := httptest.NewRecorder()
	s.handleReviewRuns(conflictRecorder, conflict)
	assert.Equal(t, http.StatusConflict, conflictRecorder.Code)
	assert.Contains(t, conflictRecorder.Body.String(), "idempotency_key_reused")
}

func TestCreateReviewRunStrictInputAndRejectedRequestsHaveNoPRSideEffects(t *testing.T) {
	headSHA := "0123456789abcdef0123456789abcdef01234567"
	s, database, apiPoller, userID := newReviewAPIServer(t, githubPRResponse(headSHA))

	unknown := httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(`{"target":{"owner":"acme","repo":"widgets","pull_request":42},"config":{},"surprise":true}`))
	unknown = addReviewAPIUser(unknown, *userID)
	unknownRecorder := httptest.NewRecorder()
	s.handleReviewRuns(unknownRecorder, unknown)
	assert.Equal(t, http.StatusBadRequest, unknownRecorder.Code)

	mismatched := strings.Replace(configurableReviewRequest(headSHA), headSHA, "1123456789abcdef0123456789abcdef01234567", 1)
	request := httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(mismatched))
	request = addReviewAPIUser(request, *userID)
	recorder := httptest.NewRecorder()
	s.handleReviewRuns(recorder, request)
	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "head_sha_mismatch")
	assert.Zero(t, apiPoller.processCalls)

	invalidConfig := strings.Replace(configurableReviewRequest(headSHA), "openai/gpt-5.6-sol", "vendor/not-allowed", 1)
	invalidRequest := addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(invalidConfig)), *userID)
	invalidRecorder := httptest.NewRecorder()
	s.handleReviewRuns(invalidRecorder, invalidRequest)
	assert.Equal(t, http.StatusUnprocessableEntity, invalidRecorder.Code)
	pr, err := database.GetPR("acme", "widgets", 42)
	require.NoError(t, err)
	assert.Nil(t, pr, "rejected custom requests must not create an auto-review candidate")
}

func TestWaitForIdempotentReviewRunBridgesAdmissionCommitWindow(t *testing.T) {
	s, _, apiPoller, _ := newReviewAPIServer(t, githubPRResponse("0123456789abcdef0123456789abcdef01234567"))
	job, err := apiPoller.PrepareReviewJob(github.PullRequest{
		Owner: "acme", Repo: "widgets", Number: 42,
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}, runconfig.Overrides{}, true, "api_v1", nil)
	require.NoError(t, err)
	job.IdempotencyScope = "user:17"
	job.IdempotencyKeyHash = "delayed-key-hash"
	job.RequestHash = "request-hash"
	created := make(chan error, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		created <- apiPoller.ProcessReviewJob(context.Background(), job)
	}()

	found, err := s.waitForIdempotentReviewRun(context.Background(), job.IdempotencyScope, job.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NoError(t, <-created)
	require.NotNil(t, found)
	assert.Equal(t, job.RunID, found.RunID)
}

func TestReviewCapabilitiesExposePolicyButNoSecrets(t *testing.T) {
	s, _, apiPoller, userID := newReviewAPIServer(t, githubPRResponse("0123456789abcdef0123456789abcdef01234567"))
	apiPoller.policy.Backends[service.AgentBackendOpenRouter] = runconfig.BackendPolicy{
		Available: false, Models: []string{"openai/gpt-5.6-sol"}, Efforts: []string{"medium", "high"},
	}

	request := addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewCapabilitiesPath, nil), *userID)
	recorder := httptest.NewRecorder()
	s.handleReviewCapabilities(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"openrouter":{"available":false`)
	assert.Contains(t, recorder.Body.String(), `"max_wall_clock_seconds":900`)
	assert.NotContains(t, strings.ToLower(recorder.Body.String()), "api_key")
	assert.NotContains(t, strings.ToLower(recorder.Body.String()), "base_url")

	apiPoller.prepareCalls = 0
	s.cfg.ReviewerEnabled = false
	unavailableRequest := addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(configurableReviewRequest("0123456789abcdef0123456789abcdef01234567"))), *userID)
	unavailableRecorder := httptest.NewRecorder()
	s.handleReviewRuns(unavailableRecorder, unavailableRequest)
	assert.Equal(t, http.StatusServiceUnavailable, unavailableRecorder.Code)
	assert.Zero(t, apiPoller.prepareCalls)

	unavailableCapabilities := addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewCapabilitiesPath, nil), *userID)
	unavailableCapabilitiesRecorder := httptest.NewRecorder()
	s.handleReviewCapabilities(unavailableCapabilitiesRecorder, unavailableCapabilities)
	assert.Equal(t, http.StatusOK, unavailableCapabilitiesRecorder.Code)
	assert.Contains(t, unavailableCapabilitiesRecorder.Body.String(), `"available":false`)
}

func TestV1AuthWrapperUsesJSONErrorContract(t *testing.T) {
	authMiddleware := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		})
	}
	handlerCalled := false
	protected := withV1Auth(authMiddleware, func(http.ResponseWriter, *http.Request) {
		handlerCalled = true
	})
	recorder := httptest.NewRecorder()
	protected.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, reviewCapabilitiesPath, nil))

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	assert.False(t, handlerCalled)
	var envelope v1ErrorEnvelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	assert.Equal(t, "unauthorized", envelope.Error.Code)
	assert.Equal(t, "authentication is required", envelope.Error.Message)
	assert.NotContains(t, recorder.Body.String(), "Unauthorized\n")

	jsonProtected := withV1Auth(func(next http.Handler) http.Handler { return next }, func(w http.ResponseWriter, _ *http.Request) {
		writeV1Error(w, http.StatusUnauthorized, "expired_token", "the access token expired")
	})
	jsonRecorder := httptest.NewRecorder()
	jsonProtected.ServeHTTP(jsonRecorder, httptest.NewRequest(http.MethodGet, reviewCapabilitiesPath, nil))
	require.Equal(t, http.StatusUnauthorized, jsonRecorder.Code)
	require.NoError(t, json.Unmarshal(jsonRecorder.Body.Bytes(), &envelope))
	assert.Equal(t, "expired_token", envelope.Error.Code)
	assert.Equal(t, "the access token expired", envelope.Error.Message)
}

func TestReviewRunGetAndCursorListExposeSafeMetadata(t *testing.T) {
	s, database, apiPoller, userID := newReviewAPIServer(t, githubPRResponse("0123456789abcdef0123456789abcdef01234567"))
	acceptedAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	runIDs := []string{
		"run-30000000000000000000000000000000",
		"run-20000000000000000000000000000000",
		"run-10000000000000000000000000000000",
	}
	for _, runID := range runIDs {
		insertReviewAPIRun(t, database, apiPoller, runID, acceptedAt)
	}
	cachedHTMLPath := gcs.ReviewFileName("acme", "widgets", 42, "0123456789abcdef0123456789abcdef01234567")
	cachedJSONPath := gcs.ReviewJSONFileName(cachedHTMLPath)
	errorSummary := "provider stderr contained a sensitive internal path"
	require.NoError(t, database.PatchReviewRun(runIDs[0], db.ReviewRunPatch{
		HTMLPath: &cachedHTMLPath, JSONPath: &cachedJSONPath, ErrorSummary: &errorSummary,
	}))
	require.NoError(t, database.UpsertReviewStageAttempt(&db.ReviewStageAttempt{
		RunID: runIDs[0], ExecutionAttempt: 1, Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "openrouter", Backend: "openrouter", RequestedModel: "openai/gpt-5.6-sol",
		ResolvedModel: "openai/gpt-5.6-sol", Status: "completed", ErrorSummary: "internal provider detail",
	}))

	getRequest := addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewRunsPathPrefix+runIDs[0], nil), *userID)
	getRecorder := httptest.NewRecorder()
	s.handleReviewRunByID(getRecorder, getRequest)
	require.Equal(t, http.StatusOK, getRecorder.Code)
	assert.Contains(t, getRecorder.Body.String(), `"attempts":[{`)
	assert.Contains(t, getRecorder.Body.String(), `"requested_model":"openai/gpt-5.6-sol"`)
	assert.NotContains(t, getRecorder.Body.String(), "internal provider detail")
	assert.NotContains(t, getRecorder.Body.String(), "sensitive internal path")
	assert.Contains(t, getRecorder.Body.String(), `"message":"the review did not complete successfully"`)
	assert.Contains(t, getRecorder.Body.String(), `"review":"/reviews/`+cachedHTMLPath+`"`)
	assert.NotContains(t, getRecorder.Body.String(), "lease_holder")
	assert.NotContains(t, getRecorder.Body.String(), "idempotency")

	firstRequest := addReviewAPIUser(httptest.NewRequest(http.MethodGet, reviewRunsPath+"?owner=acme&repo=widgets&pull_request=42&limit=2", nil), *userID)
	firstRecorder := httptest.NewRecorder()
	s.handleReviewRuns(firstRecorder, firstRequest)
	require.Equal(t, http.StatusOK, firstRecorder.Code)
	var first reviewRunListResponse
	require.NoError(t, json.Unmarshal(firstRecorder.Body.Bytes(), &first))
	require.Len(t, first.Runs, 2)
	assert.Equal(t, runIDs[:2], []string{first.Runs[0].RunID, first.Runs[1].RunID})
	assert.NotEmpty(t, first.NextCursor)
	assert.Empty(t, first.Runs[0].Attempts, "list must not issue per-run stage-attempt queries")

	secondRequest := addReviewAPIUser(httptest.NewRequest(http.MethodGet,
		reviewRunsPath+"?owner=acme&repo=widgets&pull_request=42&limit=2&cursor="+first.NextCursor, nil), *userID)
	secondRecorder := httptest.NewRecorder()
	s.handleReviewRuns(secondRecorder, secondRequest)
	require.Equal(t, http.StatusOK, secondRecorder.Code)
	var second reviewRunListResponse
	require.NoError(t, json.Unmarshal(secondRecorder.Body.Bytes(), &second))
	require.Len(t, second.Runs, 1)
	assert.Equal(t, runIDs[2], second.Runs[0].RunID)
	assert.Empty(t, second.NextCursor)
}

func insertReviewAPIRun(t *testing.T, database db.Database, apiPoller *reviewAPITestPoller, runID string, acceptedAt time.Time) {
	t.Helper()
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, apiPoller.defaults, apiPoller.policy)
	require.NoError(t, err)
	requestedJSON, err := json.Marshal(snapshot.Requested)
	require.NoError(t, err)
	effectiveJSON, err := json.Marshal(snapshot.Effective)
	require.NoError(t, err)
	sourcesJSON, err := json.Marshal(snapshot.Sources)
	require.NoError(t, err)
	completedAt := acceptedAt.Add(time.Minute)
	require.NoError(t, database.CreateReviewRun(&db.ReviewRun{
		RunID: runID, RepoOwner: "acme", RepoName: "widgets", PRNumber: 42,
		CommitSHA: "0123456789abcdef0123456789abcdef01234567", TriggerSource: "api_v1",
		Status: db.ReviewRunStatusCompleted, RequestedConfigJSON: string(requestedJSON),
		EffectiveConfigJSON: string(effectiveJSON), ConfigSourcesJSON: string(sourcesJSON),
		ConfigHash: snapshot.Hash, ConfigSchemaVersion: runconfig.SchemaVersion,
		AgentBackend: snapshot.Effective.Agent.Backend, AgentModel: snapshot.Effective.Agent.Model,
		AgentEffort: snapshot.Effective.Agent.Effort, AgentWallClockSec: snapshot.Effective.Agent.WallClockSeconds,
		AgentMaxTurns: snapshot.Effective.Agent.MaxTurns, AcceptedAt: acceptedAt, QueuedAt: acceptedAt,
		CompletedAt: &completedAt, DurationMS: int64(time.Minute / time.Millisecond), TerminalCode: "success",
		LeaseHolder: "internal-worker", IdempotencyScope: "user:999", IdempotencyKeyHash: runID,
		RequestHash: "hidden-request-hash",
	}))
}
