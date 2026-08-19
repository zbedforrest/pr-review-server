package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
)

const ReviewLeaseCompletionGrace = 2 * time.Minute

var ErrReviewRunNotClaimed = errors.New("review run was not claimed")
var ErrReviewAlreadyTracked = errors.New("a review is already active for this PR")

// ReviewJob is the immutable execution handoff. Config must already be fully
// resolved and policy-validated by the caller; Validate below enforces only
// structural completeness and hash integrity. Workers never consult mutable
// deployment defaults for caller-customizable review behavior.
type ReviewJob struct {
	PR                github.PullRequest
	RunID             string
	Config            runconfig.Snapshot
	TriggerSource     string
	RequestedByUserID *int
	Force             bool
}

type reviewExecution struct {
	Job               ReviewJob
	Holder            string
	ExecutionAttempt  int
	AttemptStartedAt  time.Time
	RunStartedAt      time.Time
	Timeout           time.Duration
	AgentSlotReserved bool
}

func (j ReviewJob) Validate() error {
	if j.RunID == "" || j.PR.Owner == "" || j.PR.Repo == "" || j.PR.Number <= 0 || j.PR.CommitSHA == "" {
		return fmt.Errorf("review job: run ID and complete PR target are required")
	}
	if j.TriggerSource == "" {
		return fmt.Errorf("review job %s: trigger source is required", j.RunID)
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
	// The stale-generating recovery window is deployment-wide. Until the API
	// introduces a separate operator cap, no admitted run may outlive it.
	if p.cfg != nil && p.cfg.AgentWallClockSec > 0 && job.Config.Effective.Agent.Enabled &&
		job.Config.Effective.Agent.WallClockSeconds > p.cfg.AgentWallClockSec {
		return fmt.Errorf("review job %s: agent wall clock %ds exceeds deployment maximum %ds",
			job.RunID, job.Config.Effective.Agent.WallClockSeconds, p.cfg.AgentWallClockSec)
	}
	return nil
}

func (p *Poller) defaultReviewJob(pr github.PullRequest, force bool, triggerSource string) (ReviewJob, error) {
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
	policy := runconfig.Policy{
		Backends: map[string]runconfig.BackendPolicy{
			backend: {
				Available: backend != service.AgentBackendOpenRouter || strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) != "",
				Models:    []string{model},
				Efforts:   []string{effort},
			},
		},
		MaxWallClockSeconds: defaults.Agent.WallClockSeconds,
		MaxTurns:            defaults.Agent.MaxTurns,
		MaxFirstPassSamples: defaults.FirstPass.Samples,
	}
	snapshot, err := runconfig.Resolve(runconfig.Overrides{}, defaults, policy)
	if err != nil {
		return ReviewJob{}, fmt.Errorf("resolve deployment review defaults: %w", err)
	}
	return ReviewJob{
		PR:            pr,
		RunID:         newReviewRunID(),
		Config:        snapshot,
		TriggerSource: triggerSource,
		Force:         force,
	}, nil
}

// ProcessReviewJob durably accepts one job and starts it asynchronously. The
// review-run row and active-review entry exist before this method returns, so
// callers can immediately poll by run ID without racing worker startup.
func (p *Poller) ProcessReviewJob(ctx context.Context, job ReviewJob) error {
	if err := p.validateReviewJob(job); err != nil {
		return err
	}
	// Acceptance is durable and execution is asynchronous, so an HTTP request
	// ending must not cancel the accepted run. Preserve context values while
	// replacing the caller's cancellation/deadline with the run's own timeout.
	reviewCtx, tracked := p.tryTrackReviewJob(context.WithoutCancel(ctx), job)
	if !tracked {
		return fmt.Errorf("%w: %s/%s#%d", ErrReviewAlreadyTracked, job.PR.Owner, job.PR.Repo, job.PR.Number)
	}
	queueHolder := newHolderID()
	now := time.Now().UTC()
	queueLeaseExpiresAt := now.Add(ReviewQueueLeaseTTL)
	if err := p.ensureReviewRunWithQueueLease(job, queueHolder, queueLeaseExpiresAt); err != nil {
		p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
		return err
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
		p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
		return fmt.Errorf("track accepted review run %s queue lease", job.RunID)
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
		OpenRouterBaseURL: p.cfg.OpenRouterBaseURL, BugMemory: p.bugMemory,
		RequiredChecks: exec.Job.Config.Effective.RequiredChecks, FailureLogSink: p.persistAgentFailureLog,
	}
}

func (p *Poller) ensureReviewRun(job ReviewJob) error {
	return p.ensureReviewRunWithQueueLease(job, "", time.Time{})
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
			!equalOptionalInt(existing.RequestedByUserID, job.RequestedByUserID) {
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

func (p *Poller) reviewRunInfo(exec *reviewExecution, completedAt time.Time) *payload.ReviewRunInfo {
	return &payload.ReviewRunInfo{
		RunID: exec.Job.RunID, ExecutionAttempt: exec.ExecutionAttempt,
		HTMLPath:  gcs.ReviewRunFileName(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.PR.CommitSHA, exec.Job.RunID),
		JSONPath:  gcs.ReviewRunJSONFileName(exec.Job.PR.Owner, exec.Job.PR.Repo, exec.Job.PR.Number, exec.Job.PR.CommitSHA, exec.Job.RunID),
		StartedAt: exec.RunStartedAt, CompletedAt: completedAt,
		DurationMS: completedAt.Sub(exec.RunStartedAt).Milliseconds(), Config: &exec.Job.Config,
	}
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
	updated, err := p.db.PatchReviewRunAsHolder(exec.Job.RunID, exec.Holder, now, patch)
	if err != nil {
		log.Printf("[REVIEWER] WARN: finalize review run %s: %v", exec.Job.RunID, err)
		return false
	}
	return updated
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
		errorSummary = contextCause.Error()
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
	now := time.Now().UTC()
	leaseExpiresAt := now.Add(ReviewLeaseCompletionGrace)
	if run, err := p.db.GetReviewRun(exec.Job.RunID); err == nil && run != nil && run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(leaseExpiresAt) {
		leaseExpiresAt = *run.LeaseExpiresAt
	}
	renewed, err := p.db.RenewReviewRunLease(exec.Job.RunID, exec.Holder, now, leaseExpiresAt)
	if err != nil {
		log.Printf("[REVIEWER] WARN: renew review run %s before publication: %v", exec.Job.RunID, err)
		return false
	}
	return renewed
}

func (p *Poller) setReviewRunPublication(runID, publicationStatus string) {
	if err := p.db.PatchReviewRun(runID, db.ReviewRunPatch{PublicationStatus: &publicationStatus}); err != nil {
		log.Printf("[REVIEWER] WARN: update publication status for run %s: %v", runID, err)
	}
}

func (p *Poller) finishCompletedReviewExecution(exec *reviewExecution, result *ReviewResult, publicationStatus string) bool {
	status := db.ReviewRunStatusCompleted
	terminalCode := "success"
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
	patch := db.ReviewRunPatch{
		Status: &status, HTMLPath: &result.ReviewRun.HTMLPath, JSONPath: &result.ReviewRun.JSONPath,
		CriticalCount: &result.CriticalCount, MediumCount: &result.MediumCount, LowCount: &result.LowCount,
		Verdict: &verdict, ModelFallback: &result.ModelFallback, ServingModelVerification: &verification,
		ActualModelsJSON: ptrString(string(modelsJSON)), PublicationStatus: &publicationStatus, TerminalCode: &terminalCode,
	}
	return p.finishReviewExecution(exec, patch)
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

func ptrString(value string) *string { return &value }

func (p *Poller) recordStageAttempt(exec *reviewExecution, attempt db.ReviewStageAttempt) {
	attempt.RunID = exec.Job.RunID
	attempt.ExecutionAttempt = exec.ExecutionAttempt
	if err := p.db.UpsertReviewStageAttempt(&attempt); err != nil {
		log.Printf("[REVIEWER] WARN: persist stage attempt for run %s: %v", exec.Job.RunID, err)
	}
}

func (p *Poller) recordGeminiAttempts(exec *reviewExecution, startedAt, completedAt time.Time, status, errorSummary string) {
	duration := completedAt.Sub(startedAt).Milliseconds()
	// The reviewer service exposes only one aggregate window for all parallel
	// first-pass draws, not per-draw timings. Record exactly one aggregate row
	// so consumers cannot accidentally sum fabricated invocation durations.
	p.recordStageAttempt(exec, db.ReviewStageAttempt{
		Stage: "first_pass", InvocationNumber: 1, AttemptNumber: 1,
		Provider: "google", Backend: "gemini_api", RequestedModel: llm.ProModelName(),
		ResolvedModel: llm.ProModelName(), Status: status, StartedAt: &startedAt,
		CompletedAt: &completedAt, DurationMS: duration, StopReason: "aggregate_window", ErrorSummary: errorSummary,
	})
	if status == "completed" {
		p.recordStageAttempt(exec, db.ReviewStageAttempt{
			Stage: "classification_summary", InvocationNumber: 1, AttemptNumber: 1,
			Provider: "google", Backend: "gemini_api", RequestedModel: llm.FlashModelName(),
			ResolvedModel: llm.FlashModelName(), Status: status, StartedAt: &startedAt,
			CompletedAt: &completedAt, StopReason: "timing_included_in_first_pass_aggregate",
		})
	}
}
