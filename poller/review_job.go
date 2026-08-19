package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
)

const ReviewLeaseCompletionGrace = 2 * time.Minute

const reviewLedgerRetryAttempts = 3
const reviewLedgerRetryBaseDelay = 100 * time.Millisecond

var ErrReviewRunNotClaimed = errors.New("review run was not claimed")
var ErrReviewAlreadyTracked = errors.New("a review is already active for this PR")

const (
	fallbackReviewMaxWallClockSec     = 360
	fallbackClaudeMaxTurns            = 40
	fallbackOpenRouterMaxTurns        = 200
	fallbackReviewMaxFirstPassSamples = 3
	fallbackReviewFirstPassConcurrent = 5
	fallbackAgentConcurrent           = 5
)

// ReviewJob is the immutable execution handoff. Config must already be fully
// resolved and policy-validated by the caller; Validate below enforces only
// structural completeness and hash integrity. Workers never consult mutable
// deployment defaults for caller-customizable review behavior.
type ReviewJob struct {
	PR                 github.PullRequest
	RunID              string
	Config             runconfig.Snapshot
	TriggerSource      string
	RequestedByUserID  *int
	IdempotencyScope   string
	IdempotencyKeyHash string
	RequestHash        string
	QueueLeaseHolder   string
	Force              bool
}

type reviewExecution struct {
	Job               ReviewJob
	Holder            string
	ExecutionAttempt  int
	AttemptStartedAt  time.Time
	RunStartedAt      time.Time
	Timeout           time.Duration
	AgentSlotReserved bool
	attemptsMu        sync.Mutex
	providerAttempts  map[string]service.ProviderAttemptEvent
}

func (e *reviewExecution) recordProviderAttempt(event service.ProviderAttemptEvent) {
	e.attemptsMu.Lock()
	defer e.attemptsMu.Unlock()
	if e.providerAttempts == nil {
		e.providerAttempts = make(map[string]service.ProviderAttemptEvent)
	}
	event.ObservedServedModels = append([]string(nil), event.ObservedServedModels...)
	key := fmt.Sprintf("%s/%09d/%09d", event.Stage, event.InvocationNumber, event.AttemptNumber)
	e.providerAttempts[key] = event
}

func (e *reviewExecution) providerModelUses() []payload.ModelUse {
	e.attemptsMu.Lock()
	type invocationKey struct {
		stage      string
		invocation int
	}
	latest := make(map[invocationKey]service.ProviderAttemptEvent, len(e.providerAttempts))
	for _, event := range e.providerAttempts {
		if event.Status != "completed" {
			continue
		}
		key := invocationKey{stage: event.Stage, invocation: event.InvocationNumber}
		if previous, ok := latest[key]; !ok || event.AttemptNumber > previous.AttemptNumber {
			latest[key] = event
		}
	}
	e.attemptsMu.Unlock()
	events := make([]service.ProviderAttemptEvent, 0, len(latest))
	for _, event := range latest {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		left, right := providerStageOrder(events[i].Stage), providerStageOrder(events[j].Stage)
		if left != right {
			return left < right
		}
		if events[i].InvocationNumber != events[j].InvocationNumber {
			return events[i].InvocationNumber < events[j].InvocationNumber
		}
		return events[i].AttemptNumber < events[j].AttemptNumber
	})
	uses := make([]payload.ModelUse, 0, len(events))
	for _, event := range events {
		requested := event.RequestedModel
		if requested == "" {
			requested = event.ResolvedModel
		}
		uses = append(uses, payload.ModelUse{
			Stage: event.Stage, Provider: event.Provider, Backend: event.Backend,
			RequestedModel: requested, ServedModel: event.PrimaryServedModel,
			ServingModelVerified: event.ServingModelVerified, Effort: event.Effort, Fallback: event.Fallback,
		})
	}
	return uses
}

func providerStageOrder(stage string) int {
	switch stage {
	case "first_pass":
		return 10
	case "classification", "classification_summary":
		return 20
	case "summary":
		return 30
	case "agent":
		return 40
	default:
		return 90
	}
}

func (j ReviewJob) Validate() error {
	if j.RunID == "" || j.PR.Owner == "" || j.PR.Repo == "" || j.PR.Number <= 0 || j.PR.CommitSHA == "" {
		return fmt.Errorf("review job: run ID and complete PR target are required")
	}
	if j.TriggerSource == "" {
		return fmt.Errorf("review job %s: trigger source is required", j.RunID)
	}
	if (j.IdempotencyScope == "") != (j.IdempotencyKeyHash == "") {
		return fmt.Errorf("review job %s: idempotency scope and key hash must be set together", j.RunID)
	}
	if j.IdempotencyScope != "" && j.RequestHash == "" {
		return fmt.Errorf("review job %s: idempotent requests require a request hash", j.RunID)
	}
	if j.Config.Effective.SchemaVersion != runconfig.SchemaVersion || j.Config.Effective.FirstPass.Samples <= 0 {
		return fmt.Errorf("review job %s: resolved config is incomplete", j.RunID)
	}
	if j.Config.Effective.Agent.Enabled && (j.Config.Effective.Agent.Backend == "" || j.Config.Effective.Agent.Model == "" ||
		j.Config.Effective.Agent.Effort == "" || j.Config.Effective.Agent.WallClockSeconds <= 0 || j.Config.Effective.Agent.MaxTurns <= 0) {
		return fmt.Errorf("review job %s: enabled agent config is incomplete", j.RunID)
	}
	hash, err := runconfig.Hash(j.Config.Effective)
	if err != nil {
		return fmt.Errorf("review job %s: hash config: %w", j.RunID, err)
	}
	if j.Config.Hash == "" || j.Config.Hash != hash {
		return fmt.Errorf("review job %s: config hash does not match effective config", j.RunID)
	}
	return nil
}

func (p *Poller) validateReviewJob(job ReviewJob) error {
	if err := job.Validate(); err != nil {
		return err
	}
	maxWallClock := 0
	if p.cfg != nil {
		maxWallClock = reviewPolicyMaximum(p.cfg.ReviewMaxWallClockSec, p.cfg.AgentWallClockSec, fallbackReviewMaxWallClockSec)
	}
	if maxWallClock > 0 && job.Config.Effective.Agent.WallClockSeconds > maxWallClock {
		return fmt.Errorf("review job %s: agent wall clock %ds exceeds deployment maximum %ds",
			job.RunID, job.Config.Effective.Agent.WallClockSeconds, maxWallClock)
	}
	return nil
}

// ReviewConfigDefaultsAndPolicy returns the caller-visible deployment defaults
// and safety policy. Credentials and provider URLs are deliberately excluded.
func (p *Poller) ReviewConfigDefaultsAndPolicy() (runconfig.Effective, runconfig.Policy, error) {
	nRequests, err := p.db.GetReviewNRequests()
	if err != nil || nRequests <= 0 {
		nRequests = 1
	}
	backend := strings.ToLower(strings.TrimSpace(p.cfg.AgentBackend))
	if backend == "" {
		backend = service.AgentBackendClaude
	}
	model := strings.TrimSpace(p.cfg.AgentModel)
	if model == "" {
		if backend == service.AgentBackendOpenRouter {
			model = service.DefaultOpenRouterAgentModel
		} else {
			model = service.DefaultAgentModel
		}
	}
	effort := strings.ToLower(strings.TrimSpace(p.cfg.AgentEffort))
	if effort == "" {
		effort = service.DefaultAgentEffort
	}
	claudeModels := policyValues(p.cfg.ReviewAgentModelsClaude, service.DefaultAgentModel)
	openRouterModels := policyValues(p.cfg.ReviewAgentModelsOpenRouter, service.DefaultOpenRouterAgentModel)
	claudeEfforts := policyValues(p.cfg.ReviewAgentEffortsClaude, service.DefaultAgentEffort)
	openRouterEfforts := policyValues(p.cfg.ReviewAgentEffortsOpenRouter, service.DefaultAgentEffort)
	if backend == service.AgentBackendClaude {
		claudeModels = appendPolicyValue(claudeModels, model)
		claudeEfforts = appendPolicyValue(claudeEfforts, effort)
	} else if backend == service.AgentBackendOpenRouter {
		openRouterModels = appendPolicyValue(openRouterModels, model)
		openRouterEfforts = appendPolicyValue(openRouterEfforts, effort)
	}
	defaults := runconfig.Effective{
		SchemaVersion: runconfig.SchemaVersion,
		Agent: runconfig.Agent{
			Enabled:          p.cfg.AgenticReviews,
			Backend:          backend,
			Model:            model,
			Effort:           effort,
			WallClockSeconds: p.cfg.AgentWallClockSec,
			MaxTurns:         p.cfg.AgentMaxTurns,
		},
		FirstPass:      runconfig.FirstPass{Samples: nRequests},
		RequiredChecks: p.cfg.RequiredChecks,
	}
	defaults.Agent.TurnBudgetUnit, defaults.Agent.TurnBudgetVersion = runconfig.TurnBudgetSemantics(backend)
	gitAvailable := p.executableAvailable("git")
	claudeDefaultMaxTurns := reviewBackendDefaultMaxTurns(service.AgentBackendClaude, backend, defaults.Agent.MaxTurns)
	openRouterDefaultMaxTurns := reviewBackendDefaultMaxTurns(service.AgentBackendOpenRouter, backend, defaults.Agent.MaxTurns)
	claudeMaxTurns := reviewBackendMaxTurns(service.AgentBackendClaude, backend, defaults.Agent.MaxTurns, claudeDefaultMaxTurns, p.cfg.ReviewMaxTurns, p.cfg.ReviewMaxTurnsConfigured)
	openRouterMaxTurns := reviewBackendMaxTurns(service.AgentBackendOpenRouter, backend, defaults.Agent.MaxTurns, openRouterDefaultMaxTurns, p.cfg.ReviewMaxTurns, p.cfg.ReviewMaxTurnsConfigured)
	// The legacy global field follows the active backend so existing clients
	// remain conservative; per-backend capability fields are authoritative.
	maxTurns := claudeMaxTurns
	if backend == service.AgentBackendOpenRouter {
		maxTurns = openRouterMaxTurns
	}
	policy := runconfig.Policy{
		Backends: map[string]runconfig.BackendPolicy{
			// Claude credentials are advisory readiness metadata: the CLI may use
			// its own OAuth session, and its native authentication flow is retained.
			service.AgentBackendClaude: p.reviewBackendPolicy(
				service.AgentBackendClaude, "claude", gitAvailable, strings.TrimSpace(p.cfg.AnthropicAPIKey) != "", false,
				claudeDefaultMaxTurns, claudeMaxTurns, claudeModels, claudeEfforts,
			),
			service.AgentBackendOpenRouter: p.reviewBackendPolicy(
				service.AgentBackendOpenRouter, "codex", gitAvailable, strings.TrimSpace(p.cfg.OpenRouterAPIKey) != "", true,
				openRouterDefaultMaxTurns, openRouterMaxTurns, openRouterModels, openRouterEfforts,
			),
		},
		MaxWallClockSeconds: reviewPolicyMaximum(p.cfg.ReviewMaxWallClockSec, defaults.Agent.WallClockSeconds, fallbackReviewMaxWallClockSec),
		MaxTurns:            maxTurns,
		MaxFirstPassSamples: reviewPolicyMaximum(p.cfg.ReviewMaxFirstPassSamples, defaults.FirstPass.Samples, fallbackReviewMaxFirstPassSamples),
	}
	return defaults, policy, nil
}

func (p *Poller) reviewBackendPolicy(backend, command string, gitAvailable, credentialConfigured, credentialRequired bool, defaultMaxTurns, maxTurns int, models, efforts []string) runconfig.BackendPolicy {
	policyEnabled := p.cfg.AgenticReviews
	cliAvailable := p.executableAvailable(command)
	reasons := make([]string, 0, 4)
	if !policyEnabled {
		reasons = append(reasons, runconfig.BackendUnavailablePolicyDisabled)
	}
	if credentialRequired && !credentialConfigured {
		reasons = append(reasons, runconfig.BackendUnavailableCredentialMissing)
	}
	if !gitAvailable {
		reasons = append(reasons, runconfig.BackendUnavailableGitMissing)
	}
	if !cliAvailable {
		reasons = append(reasons, runconfig.BackendUnavailableCLIMissing)
	}
	turnBudgetUnit, turnBudgetVersion := runconfig.TurnBudgetSemantics(backend)
	ready := policyEnabled && (!credentialRequired || credentialConfigured) && gitAvailable && cliAvailable
	if maxTurns > 0 && defaultMaxTurns > maxTurns {
		defaultMaxTurns = maxTurns
	}
	return runconfig.BackendPolicy{
		Available:     ready,
		Ready:         ready,
		PolicyEnabled: policyEnabled, CredentialConfigured: credentialConfigured, CredentialRequired: credentialRequired,
		ExecutableAvailable: gitAvailable && cliAvailable, UnavailableReasons: reasons,
		TurnBudgetUnit: turnBudgetUnit, TurnBudgetVersion: turnBudgetVersion,
		DefaultMaxTurns: defaultMaxTurns, MaxTurns: maxTurns,
		Models: append([]string(nil), models...), Efforts: append([]string(nil), efforts...),
	}
}

func reviewBackendDefaultMaxTurns(backend, activeBackend string, activeMaxTurns int) int {
	if backend == activeBackend && activeMaxTurns > 0 {
		return activeMaxTurns
	}
	if backend == service.AgentBackendOpenRouter {
		return fallbackOpenRouterMaxTurns
	}
	return fallbackClaudeMaxTurns
}

func reviewBackendMaxTurns(backend, activeBackend string, activeMaxTurns, backendDefault, configured int, explicitlyConfigured bool) int {
	maximum := backendDefault
	if explicitlyConfigured && configured > 0 {
		maximum = configured
	}
	if backend == activeBackend && activeMaxTurns > maximum {
		maximum = activeMaxTurns
	}
	return maximum
}

func policyValues(configured []string, fallback string) []string {
	values := make([]string, 0, len(configured)+1)
	for _, value := range configured {
		values = appendPolicyValue(values, value)
	}
	if len(values) == 0 {
		values = append(values, fallback)
	}
	return values
}

func appendPolicyValue(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func reviewPolicyMaximum(configured, active, fallback int) int {
	if configured <= 0 {
		if active > 0 {
			return active
		}
		return fallback
	}
	if active > configured {
		return active
	}
	return configured
}

func (p *Poller) defaultReviewSnapshot() (runconfig.Snapshot, error) {
	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	if err != nil {
		return runconfig.Snapshot{}, err
	}
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, defaults, policy)
	if err != nil {
		return runconfig.Snapshot{}, fmt.Errorf("resolve deployment review defaults: %w", err)
	}
	return snapshot, nil
}

// PrepareReviewJob resolves caller overrides exactly once, before durable
// admission. The returned snapshot is immutable execution input.
func (p *Poller) PrepareReviewJob(pr github.PullRequest, requested runconfig.Overrides, force bool, triggerSource string, requestedByUserID *int) (ReviewJob, error) {
	defaults, policy, err := p.ReviewConfigDefaultsAndPolicy()
	if err != nil {
		return ReviewJob{}, err
	}
	if requested.Agent != nil && requested.Agent.Backend != nil && requested.Agent.Model == nil &&
		!strings.EqualFold(strings.TrimSpace(*requested.Agent.Backend), defaults.Agent.Backend) {
		return ReviewJob{}, &runconfig.ValidationError{Field: "agent.model", Message: "is required when changing agent.backend"}
	}
	snapshot, err := runconfig.Resolve(requested, defaults, policy)
	if err != nil {
		return ReviewJob{}, err
	}
	return ReviewJob{
		PR: pr, RunID: newReviewRunID(), Config: snapshot, TriggerSource: triggerSource,
		RequestedByUserID: requestedByUserID, Force: force,
	}, nil
}

func (p *Poller) defaultReviewJob(pr github.PullRequest, force bool, triggerSource string) (ReviewJob, error) {
	return p.PrepareReviewJob(pr, runconfig.Overrides{}, force, triggerSource, nil)
}

// ProcessReviewJob durably accepts one job and starts it asynchronously. The
// review-run row and active-review entry exist before this method returns, so
// callers can immediately poll by run ID without racing worker startup.
func (p *Poller) ProcessReviewJob(ctx context.Context, job ReviewJob) error {
	if err := p.validateReviewJob(job); err != nil {
		return err
	}
	queueHolder := newHolderID()
	job.QueueLeaseHolder = queueHolder
	now := time.Now().UTC()
	queueLeaseExpiresAt := now.Add(ReviewQueueLeaseTTL)
	var reviewCtx context.Context
	admissionErr := func() error {
		p.reviewAdmissionMutex.Lock()
		defer p.reviewAdmissionMutex.Unlock()

		// Acceptance is durable and execution is asynchronous, so an HTTP request
		// ending must not cancel the accepted run. Preserve context values while
		// replacing the caller's cancellation/deadline with the run's own timeout.
		var tracked bool
		reviewCtx, tracked = p.tryTrackReviewJob(context.WithoutCancel(ctx), job)
		if !tracked {
			return fmt.Errorf("%w: %s/%s#%d", ErrReviewAlreadyTracked, job.PR.Owner, job.PR.Repo, job.PR.Number)
		}
		if err := p.ensureReviewRunWithQueueLease(job, queueHolder, queueLeaseExpiresAt); err != nil {
			p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
			if errors.Is(err, db.ErrReviewRunActiveConflict) {
				return fmt.Errorf("%w: %s/%s#%d", ErrReviewAlreadyTracked, job.PR.Owner, job.PR.Repo, job.PR.Number)
			}
			return err
		}
		return nil
	}()
	if admissionErr != nil {
		return admissionErr
	}
	leased, err := p.db.ClaimOrRenewQueuedReviewRunLease(job.RunID, queueHolder, now, queueLeaseExpiresAt)
	if err != nil || !leased {
		p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
		if err != nil {
			return fmt.Errorf("lease accepted review run %s: %w", job.RunID, err)
		}
		return fmt.Errorf("%w: queued run %s has another dispatcher", ErrReviewAlreadyTracked, job.RunID)
	}
	if !p.setTrackedQueueLease(job, queueHolder) {
		trackErr := fmt.Errorf("track accepted review run %s queue lease", job.RunID)
		p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "dispatch_lost", "dispatch", trackErr)
		p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
		return trackErr
	}
	go func() {
		if err := p.generateReviewJobs(reviewCtx, []ReviewJob{job}); err != nil {
			log.Printf("[IMMEDIATE] ERROR: review job %s failed for %s/%s#%d: %v", job.RunID, job.PR.Owner, job.PR.Repo, job.PR.Number, err)
			p.rejectQueuedReviewJob(job, db.ReviewRunStatusFailed, "dispatch_failed", "dispatch", err)
			p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
		}
	}()
	return nil
}

func reviewTimeout(cfg runconfig.Effective) time.Duration {
	return reviewTimeoutWithMargin(cfg, ReviewPipelineMargin)
}

func reviewTimeoutWithMargin(cfg runconfig.Effective, margin time.Duration) time.Duration {
	timeout := margin
	// Preserve the deployment's historical total-review allowance even when
	// the agent stage is disabled; first-pass-only reviews can still be large.
	if cfg.Agent.WallClockSeconds > 0 {
		timeout += time.Duration(cfg.Agent.WallClockSeconds) * time.Second
	}
	return timeout
}

func (p *Poller) reviewTimeout(cfg runconfig.Effective) time.Duration {
	margin := p.reviewPipelineMargin
	if margin <= 0 {
		margin = ReviewPipelineMargin
	}
	return reviewTimeoutWithMargin(cfg, margin)
}

func (p *Poller) agentConfigForExecution(exec *reviewExecution, gitToken string) service.AgentConfig {
	agent := exec.Job.Config.Effective.Agent
	return service.AgentConfig{
		CloneRootDir: p.cfg.AgentCloneRootDir, LogsDir: p.cfg.AgentLogsDir,
		WallClock: time.Duration(agent.WallClockSeconds) * time.Second, MaxTurns: agent.MaxTurns,
		GitHubToken: gitToken, Backend: agent.Backend, Model: agent.Model, Effort: agent.Effort,
		AnthropicAPIKey:   p.cfg.AnthropicAPIKey,
		OpenRouterAPIKey:  p.cfg.OpenRouterAPIKey,
		OpenRouterBaseURL: p.cfg.OpenRouterBaseURL, BugMemory: p.bugMemory,
		RequiredChecks: exec.Job.Config.Effective.RequiredChecks, FailureLogSink: p.persistAgentFailureLog,
		AttemptObserver: p.providerAttemptObserver(exec),
	}
}

func (p *Poller) ensureReviewRun(job ReviewJob) error {
	return p.ensureReviewRunWithQueueLease(job, "", time.Time{})
}

func (p *Poller) admitReviewRunForExecution(job ReviewJob) error {
	now := time.Now().UTC()
	return p.ensureReviewRunWithQueueLease(job, newHolderID(), now.Add(ReviewQueueLeaseTTL))
}

func (p *Poller) ensureReviewRunWithQueueLease(job ReviewJob, queueHolder string, queueLeaseExpiresAt time.Time) error {
	if err := p.validateReviewJob(job); err != nil {
		return err
	}
	if (queueHolder == "") != queueLeaseExpiresAt.IsZero() {
		return fmt.Errorf("review run %s: queued lease holder and expiry must be provided together", job.RunID)
	}
	requestedJSON, err := json.Marshal(job.Config.Requested)
	if err != nil {
		return fmt.Errorf("marshal requested config: %w", err)
	}
	effectiveJSON, err := json.Marshal(job.Config.Effective)
	if err != nil {
		return fmt.Errorf("marshal effective config: %w", err)
	}
	sourcesJSON, err := json.Marshal(job.Config.Sources)
	if err != nil {
		return fmt.Errorf("marshal config sources: %w", err)
	}
	existing, err := p.db.GetReviewRun(job.RunID)
	if err != nil {
		return fmt.Errorf("get review run %s: %w", job.RunID, err)
	}
	if existing != nil {
		if existing.RepoOwner != job.PR.Owner || existing.RepoName != job.PR.Repo || existing.PRNumber != job.PR.Number ||
			existing.CommitSHA != job.PR.CommitSHA || existing.ConfigHash != job.Config.Hash ||
			existing.RequestedConfigJSON != string(requestedJSON) || existing.EffectiveConfigJSON != string(effectiveJSON) ||
			existing.ConfigSourcesJSON != string(sourcesJSON) || existing.TriggerSource != job.TriggerSource ||
			!equalOptionalInt(existing.RequestedByUserID, job.RequestedByUserID) ||
			existing.IdempotencyScope != job.IdempotencyScope || existing.IdempotencyKeyHash != job.IdempotencyKeyHash ||
			existing.RequestHash != job.RequestHash {
			return fmt.Errorf("review run %s already exists with a different target or config", job.RunID)
		}
		return nil
	}
	now := time.Now().UTC()
	var prID *int
	if current, getErr := p.db.GetPR(job.PR.Owner, job.PR.Repo, job.PR.Number); getErr == nil && current != nil && current.ID > 0 {
		id := current.ID
		prID = &id
	}
	run := &db.ReviewRun{
		RunID: job.RunID, PRID: prID, RepoOwner: job.PR.Owner, RepoName: job.PR.Repo,
		PRNumber: job.PR.Number, CommitSHA: job.PR.CommitSHA, RequestedByUserID: job.RequestedByUserID,
		TriggerSource: job.TriggerSource, Status: db.ReviewRunStatusQueued,
		RequestedConfigJSON: string(requestedJSON), EffectiveConfigJSON: string(effectiveJSON),
		ConfigSourcesJSON: string(sourcesJSON), ConfigHash: job.Config.Hash,
		ConfigSchemaVersion: job.Config.Effective.SchemaVersion,
		AgentBackend:        job.Config.Effective.Agent.Backend, AgentModel: job.Config.Effective.Agent.Model,
		AgentEffort: job.Config.Effective.Agent.Effort, AgentWallClockSec: job.Config.Effective.Agent.WallClockSeconds,
		AgentMaxTurns: job.Config.Effective.Agent.MaxTurns, AcceptedAt: now, QueuedAt: now,
		ServiceRevision: revisionName(), LeaseHolder: queueHolder,
		IdempotencyScope: job.IdempotencyScope, IdempotencyKeyHash: job.IdempotencyKeyHash,
		RequestHash: job.RequestHash,
	}
	if !queueLeaseExpiresAt.IsZero() {
		expiresAt := queueLeaseExpiresAt
		run.LeaseExpiresAt = &expiresAt
	}
	if err := p.db.CreateReviewRun(run); err != nil {
		return fmt.Errorf("create review run %s: %w", job.RunID, err)
	}
	return nil
}

func equalOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (p *Poller) beginReviewExecution(job ReviewJob) (*reviewExecution, error) {
	if err := p.ensureReviewRun(job); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	timeout := p.reviewTimeout(job.Config.Effective)
	holder := newHolderID()
	// The lease deliberately spans the frozen execution budget plus completion
	// grace. There is no execution heartbeat yet, so crash recovery latency is
	// bounded by that interval; publication renews without shortening it.
	claimed, err := p.db.ClaimReviewRun(job.RunID, holder, now, now.Add(timeout+ReviewLeaseCompletionGrace))
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: %s", ErrReviewRunNotClaimed, job.RunID)
	}
	run, err := p.db.GetReviewRun(job.RunID)
	if err != nil || run == nil {
		reloadErr := fmt.Errorf("reload claimed review run %s: not found", job.RunID)
		if err != nil {
			reloadErr = fmt.Errorf("reload claimed review run %s: %w", job.RunID, err)
		}
		status := db.ReviewRunStatusFailed
		terminalCode := "claim_reload_failed"
		failureStage := "dispatch"
		errorSummary := reloadErr.Error()
		completedAt := time.Now().UTC()
		emptyHolder := ""
		zeroLease := time.Time{}
		updated, patchErr := p.db.PatchReviewRunAsHolder(job.RunID, holder, completedAt, db.ReviewRunPatch{
			Status: &status, CompletedAt: &completedAt, TerminalCode: &terminalCode,
			FailureStage: &failureStage, ErrorSummary: &errorSummary,
			LeaseHolder: &emptyHolder, LeaseExpiresAt: &zeroLease,
		})
		if patchErr != nil {
			log.Printf("[REVIEWER] WARN: release lease for run %s after reload failure: %v", job.RunID, patchErr)
		} else if !updated {
			log.Printf("[REVIEWER] WARN: run %s lost its lease before reload-failure cleanup", job.RunID)
		}
		return nil, reloadErr
	}
	runStartedAt := now
	if run.StartedAt != nil {
		runStartedAt = *run.StartedAt
	}
	return &reviewExecution{
		Job: job, Holder: holder, ExecutionAttempt: run.ExecutionAttempt,
		AttemptStartedAt: now, RunStartedAt: runStartedAt, Timeout: timeout,
	}, nil
}

func (p *Poller) reviewRunArtifactInfo(exec *reviewExecution) *payload.ReviewRunInfo {
	return &payload.ReviewRunInfo{
		RunID: exec.Job.RunID, ExecutionAttempt: exec.ExecutionAttempt,
		HTMLPath:  gcs.ReviewRunFileName(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.PR.CommitSHA, exec.Job.RunID),
		JSONPath:  gcs.ReviewRunJSONFileName(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.PR.CommitSHA, exec.Job.RunID),
		StartedAt: exec.RunStartedAt, Config: &exec.Job.Config,
	}
}

func (p *Poller) reviewRunInfo(exec *reviewExecution, completedAt time.Time) *payload.ReviewRunInfo {
	info := p.reviewRunArtifactInfo(exec)
	info.CompletedAt = completedAt
	info.DurationMS = completedAt.Sub(exec.RunStartedAt).Milliseconds()
	return info
}

func (p *Poller) finishReviewExecution(exec *reviewExecution, patch db.ReviewRunPatch) bool {
	now := time.Now().UTC()
	completedAt := now
	durationMS := now.Sub(exec.RunStartedAt).Milliseconds()
	emptyHolder := ""
	zeroLease := time.Time{}
	patch.CompletedAt = &completedAt
	patch.DurationMS = &durationMS
	patch.LeaseHolder = &emptyHolder
	patch.LeaseExpiresAt = &zeroLease
	for attempt := 1; attempt <= reviewLedgerRetryAttempts; attempt++ {
		updated, err := p.db.PatchReviewRunAsHolder(exec.Job.RunID, exec.Holder, time.Now().UTC(), patch)
		if err == nil {
			return updated
		}
		if attempt == reviewLedgerRetryAttempts {
			log.Printf("[REVIEWER] WARN: finalize review run %s failed after %d attempts: %v", exec.Job.RunID, attempt, err)
			return false
		}
		time.Sleep(reviewLedgerRetryBaseDelay << (attempt - 1))
	}
	return false
}

func (p *Poller) finishSupersededReviewExecution(exec *reviewExecution, cause error) bool {
	status := db.ReviewRunStatusCancelled
	terminalCode := "superseded"
	failureStage := "projection"
	errorSummary := "review run no longer owns the PR projection"
	if cause != nil {
		errorSummary = cause.Error()
	}
	return p.finishReviewExecution(exec, db.ReviewRunPatch{
		Status: &status, TerminalCode: &terminalCode, FailureStage: &failureStage, ErrorSummary: &errorSummary,
	})
}

// finishInterruptedReviewExecution distinguishes external cancellation (whose
// caller owns the PR projection) from the run's organic wall-clock timeout.
// Organic timeouts become PR errors so the bounded error-retry policy applies
// instead of stale reset creating an endless timeout/requeue loop.
func (p *Poller) finishInterruptedReviewExecution(exec *reviewExecution, ctx context.Context, failureStage string, cause error) bool {
	status := db.ReviewRunStatusCancelled
	terminalCode := "cancelled"
	budgetTimeout := errors.Is(context.Cause(ctx), errReviewRunBudgetExceeded)
	if budgetTimeout {
		status = db.ReviewRunStatusTimedOut
		terminalCode = "run_timeout"
	}
	errorSummary := "review execution interrupted"
	if cause != nil {
		errorSummary = cause.Error()
	}
	if contextCause := context.Cause(ctx); budgetTimeout && contextCause != nil {
		if cause != nil && !errors.Is(cause, errReviewRunBudgetExceeded) && cause.Error() != contextCause.Error() {
			errorSummary = fmt.Sprintf("%s: %s", contextCause.Error(), cause.Error())
		} else {
			errorSummary = contextCause.Error()
		}
	}
	finished := p.finishReviewExecution(exec, db.ReviewRunPatch{
		Status: &status, TerminalCode: &terminalCode, FailureStage: &failureStage, ErrorSummary: &errorSummary,
	})
	if !budgetTimeout {
		return finished
	}
	if !finished {
		log.Printf("[REVIEWER] STALE WORKER: run %s no longer owns its lease; skipping timeout error projection", exec.Job.RunID)
		return false
	}
	projected, err := p.db.SetPRErrorForReviewRun(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.RunID, reviewBudgetExceededMessage)
	if err != nil {
		log.Printf("[REVIEWER] WARNING: failed to persist timeout status for PR %d: %v", exec.Job.PR.Number, err)
	}
	if !projected {
		log.Printf("[REVIEWER] STALE WORKER: run %s was replaced; skipping timeout error projection", exec.Job.RunID)
		return true
	}
	p.broadcastPRUpdate(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number)
	return true
}

func (p *Poller) renewReviewExecutionForPublication(exec *reviewExecution) bool {
	for attempt := 1; attempt <= reviewLedgerRetryAttempts; attempt++ {
		now := time.Now().UTC()
		leaseExpiresAt := now.Add(ReviewLeaseCompletionGrace)
		if run, err := p.db.GetReviewRun(exec.Job.RunID); err == nil && run != nil && run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(leaseExpiresAt) {
			leaseExpiresAt = *run.LeaseExpiresAt
		}
		renewed, err := p.db.RenewReviewRunLease(exec.Job.RunID, exec.Holder, now, leaseExpiresAt)
		if err == nil {
			return renewed
		}
		if attempt == reviewLedgerRetryAttempts {
			log.Printf("[REVIEWER] WARN: renew review run %s before publication failed after %d attempts: %v", exec.Job.RunID, attempt, err)
			return false
		}
		time.Sleep(reviewLedgerRetryBaseDelay << (attempt - 1))
	}
	return false
}

func (p *Poller) finalizeCompletedReviewExecution(exec *reviewExecution, result *ReviewResult, reviewRunJSON string) (db.ReviewRunFinalizationResult, error) {
	if result == nil || result.ReviewRun == nil {
		return db.ReviewRunFinalizationResult{}, fmt.Errorf("finalize review run %s: review-run metadata is required", exec.Job.RunID)
	}
	ledger, ok := p.db.(db.ReviewRunLedger)
	if !ok {
		return db.ReviewRunFinalizationResult{}, fmt.Errorf("finalize review run %s: database does not support worker ledger writes", exec.Job.RunID)
	}
	verdict := service.VerdictFromComments(result.Comments)
	modelsJSON, err := json.Marshal(result.ReviewRun.Models)
	if err != nil {
		modelsJSON = []byte("[]")
	}
	verification := "not_reported"
	for _, model := range result.ReviewRun.Models {
		if model.ServingModelVerified {
			verification = "verified"
			break
		}
		if model.Stage == "agent" {
			verification = "unverified"
		}
	}
	input := db.ReviewRunSuccessFinalization{
		RunID: exec.Job.RunID, Holder: exec.Holder, ExecutionAttempt: exec.ExecutionAttempt,
		CompletedAt: result.ReviewRun.CompletedAt, DurationMS: result.ReviewRun.DurationMS,
		HTMLPath: result.ReviewRun.HTMLPath, JSONPath: result.ReviewRun.JSONPath,
		CanonicalPath: gcs.ReviewFileName(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.PR.CommitSHA),
		Critical:      result.CriticalCount, Medium: result.MediumCount, Low: result.LowCount,
		Verdict: verdict, ModelFallback: result.ModelFallback, ServingModelVerification: verification,
		ActualModelsJSON: string(modelsJSON), ReviewRunJSON: reviewRunJSON,
	}
	var finalizationErr error
	for attempt := 1; attempt <= reviewLedgerRetryAttempts; attempt++ {
		input.LeaseCheckedAt = time.Now().UTC()
		outcome, err := ledger.FinalizeReviewRunSuccess(input)
		if err == nil {
			return outcome, nil
		}
		finalizationErr = err
		if attempt < reviewLedgerRetryAttempts {
			time.Sleep(reviewLedgerRetryBaseDelay << (attempt - 1))
		}
	}
	return db.ReviewRunFinalizationResult{}, fmt.Errorf("finalize review run %s after %d attempts: %w", exec.Job.RunID, reviewLedgerRetryAttempts, finalizationErr)
}

func (p *Poller) rejectQueuedReviewJob(job ReviewJob, status, terminalCode, failureStage string, cause error) bool {
	if job.TriggerSource == "poller" {
		// Automatic candidates are not durably accepted until execution begins.
		// A dispatch/cache/cancellation rejection must not manufacture a fresh
		// terminal row on every poll cycle. Future replay of an already-persisted
		// poll-sourced run still reaches the normal queued patch below.
		existing, err := p.db.GetReviewRun(job.RunID)
		if err != nil {
			log.Printf("[REVIEWER] WARN: inspect rejected poll-sourced run %s: %v", job.RunID, err)
			return false
		}
		if existing == nil {
			return false
		}
	}
	if job.QueueLeaseHolder != "" {
		now := time.Now().UTC()
		owned, err := p.db.ClaimOrRenewQueuedReviewRunLease(job.RunID, job.QueueLeaseHolder, now, now.Add(ReviewQueueLeaseTTL))
		if err != nil {
			log.Printf("[REVIEWER] WARN: verify queued ownership before rejecting run %s: %v", job.RunID, err)
			return false
		}
		if !owned {
			return false
		}
	}
	if err := p.ensureReviewRun(job); err != nil {
		log.Printf("[REVIEWER] WARN: persist rejected run %s: %v", job.RunID, err)
		return false
	}
	completedAt := time.Now().UTC()
	errorSummary := "review job rejected"
	if cause != nil {
		errorSummary = cause.Error()
	}
	emptyHolder := ""
	zeroLease := time.Time{}
	updated, err := p.db.PatchQueuedReviewRun(job.RunID, db.ReviewRunPatch{
		Status: &status, CompletedAt: &completedAt, TerminalCode: &terminalCode,
		FailureStage: &failureStage, ErrorSummary: &errorSummary,
		LeaseHolder: &emptyHolder, LeaseExpiresAt: &zeroLease,
	})
	if err != nil {
		log.Printf("[REVIEWER] WARN: reject queued run %s: %v", job.RunID, err)
		return false
	}
	return updated
}

func (p *Poller) completeQueuedReviewJobFromCache(job ReviewJob, htmlPath string, criticalCount, mediumCount, lowCount int, verdict string, modelFallback bool, reviewRunJSON string) bool {
	if job.TriggerSource == "poller" {
		// Automatic cache hits repair the mutable PR projection without creating
		// one durable no-op run on every poll cycle.
		existing, err := p.db.GetReviewRun(job.RunID)
		if err != nil {
			log.Printf("[REVIEWER] WARN: inspect cached poll-sourced run %s: %v", job.RunID, err)
			return false
		}
		if existing == nil {
			return false
		}
	}
	if job.QueueLeaseHolder != "" {
		now := time.Now().UTC()
		owned, err := p.db.ClaimOrRenewQueuedReviewRunLease(job.RunID, job.QueueLeaseHolder, now, now.Add(ReviewQueueLeaseTTL))
		if err != nil {
			log.Printf("[REVIEWER] WARN: verify queued ownership before completing cached run %s: %v", job.RunID, err)
			return false
		}
		if !owned {
			return false
		}
	}
	if err := p.ensureReviewRun(job); err != nil {
		log.Printf("[REVIEWER] WARN: persist cached run %s: %v", job.RunID, err)
		return false
	}

	actualModelsJSON := "[]"
	verification := "not_reported"
	if reviewRunJSON != "" {
		var cachedRun payload.ReviewRunInfo
		if err := json.Unmarshal([]byte(reviewRunJSON), &cachedRun); err != nil {
			log.Printf("[REVIEWER] WARN: parse cached model metadata for run %s: %v", job.RunID, err)
		} else if encoded, err := json.Marshal(cachedRun.Models); err != nil {
			log.Printf("[REVIEWER] WARN: encode cached model metadata for run %s: %v", job.RunID, err)
		} else {
			actualModelsJSON = string(encoded)
			for _, model := range cachedRun.Models {
				if model.ServingModelVerified {
					verification = "verified"
					break
				}
				if model.Stage == "agent" {
					verification = "unverified"
				}
			}
		}
	}

	status := db.ReviewRunStatusCompleted
	terminalCode := "cache_restored"
	publicationStatus := "restored_from_cache"
	jsonPath := gcs.ReviewJSONFileName(htmlPath)
	completedAt := time.Now().UTC()
	emptyHolder := ""
	zeroLease := time.Time{}
	updated, err := p.db.PatchQueuedReviewRun(job.RunID, db.ReviewRunPatch{
		Status: &status, CompletedAt: &completedAt, TerminalCode: &terminalCode,
		HTMLPath: &htmlPath, JSONPath: &jsonPath,
		CriticalCount: &criticalCount, MediumCount: &mediumCount, LowCount: &lowCount,
		Verdict: &verdict, ModelFallback: &modelFallback,
		ServingModelVerification: &verification, ActualModelsJSON: &actualModelsJSON,
		PublicationStatus: &publicationStatus, LeaseHolder: &emptyHolder, LeaseExpiresAt: &zeroLease,
	})
	if err != nil {
		log.Printf("[REVIEWER] WARN: complete cached run %s: %v", job.RunID, err)
		return false
	}
	return updated
}

func (p *Poller) rejectProviderInitJobs(jobs []ReviewJob, cause error) {
	for _, job := range jobs {
		persistFailure := job.TriggerSource != "poller"
		if !persistFailure {
			// Automatic polling has not durably accepted a run yet. A deployment-
			// wide provider outage must not mint one failed ledger row per pending
			// PR on every poll cycle. If a future durable dispatcher replays a
			// poll-sourced row, preserve that already-existing run instead.
			existing, err := p.db.GetReviewRun(job.RunID)
			if err != nil {
				log.Printf("[REVIEWER] WARN: check poll-sourced run %s after provider init failure: %v", job.RunID, err)
			} else {
				persistFailure = existing != nil
			}
		}
		if persistFailure {
			p.rejectQueuedReviewJob(job, db.ReviewRunStatusFailed, "provider_init_failed", "dispatch", cause)
			if projected, err := p.db.SetPRErrorIfNoLiveReview(job.PR.Owner, job.PR.Repo, job.PR.Number, cause.Error()); err != nil {
				log.Printf("[REVIEWER] WARNING: failed to persist provider-init error for PR %d: %v", job.PR.Number, err)
			} else if projected {
				p.broadcastPRUpdate(job.PR.Owner, job.PR.Repo, job.PR.Number)
			}
		}
		p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
	}
}

func (p *Poller) providerAttemptObserver(exec *reviewExecution) service.ProviderAttemptObserver {
	ledger, ledgerOK := p.db.(db.ReviewRunLedger)
	return func(event service.ProviderAttemptEvent) error {
		exec.recordProviderAttempt(event)
		if !ledgerOK {
			return fmt.Errorf("%w: persist provider attempt for run %s: database does not support worker ledger writes", service.ErrProviderAttemptAborted, exec.Job.RunID)
		}
		attempt := db.ReviewStageAttempt{
			RunID: exec.Job.RunID, ExecutionAttempt: exec.ExecutionAttempt,
			Stage: event.Stage, InvocationNumber: event.InvocationNumber, AttemptNumber: event.AttemptNumber,
			Provider: event.Provider, Backend: event.Backend, RequestedModel: event.RequestedModel, ResolvedModel: event.ResolvedModel,
			ObservedServedModels: append([]string(nil), event.ObservedServedModels...), PrimaryServedModel: event.PrimaryServedModel,
			ServedModelSource: event.ServedModelSource, ServingModelVerified: event.ServingModelVerified,
			Fallback: event.Fallback, FallbackReason: event.FallbackReason, MatcherVersion: event.MatcherVersion,
			Effort: event.Effort, Status: event.Status, AssistantTurns: event.AssistantTurns,
			BudgetUnitsUsed: event.BudgetUnitsUsed,
			InputTokens:     event.InputTokens, OutputTokens: event.OutputTokens, TotalTokens: event.TotalTokens,
			StartedAt: event.StartedAt, CompletedAt: event.CompletedAt, DurationMS: event.DurationMS,
			StopReason: event.StopReason, ErrorCode: event.ErrorCode, ErrorSummary: event.ErrorSummary,
		}
		var persistErr error
		for retry := 1; retry <= reviewLedgerRetryAttempts; retry++ {
			accepted, err := ledger.UpsertReviewStageAttemptAsHolder(&attempt, exec.Holder, time.Now().UTC())
			if err == nil {
				if !accepted {
					// A definitive fence rejection means this worker must stop doing
					// provider work even though service-level telemetry observers are
					// otherwise best-effort. Cancelling the tracked context makes an
					// imminent HTTP call or spawned CLI abort at its own boundary.
					p.untrackReviewRun(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.RunID)
					return fmt.Errorf("%w: persist provider attempt for run %s: execution lease is no longer owned", service.ErrProviderAttemptAborted, exec.Job.RunID)
				}
				return nil
			}
			persistErr = err
			if retry < reviewLedgerRetryAttempts {
				time.Sleep(reviewLedgerRetryBaseDelay << (retry - 1))
			}
		}
		return fmt.Errorf("persist provider attempt for run %s after %d attempts: %w", exec.Job.RunID, reviewLedgerRetryAttempts, persistErr)
	}
}
