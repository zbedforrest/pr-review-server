package poller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

// Timeout constants for review process management.
// These values are coordinated to ensure consistent behavior:
//   - reviewProcessTimeout(): max time a review may run before the monitor
//     treats it as hung and the stale-reset reclaims it (derived, not constant)
//   - ReviewProcessWarningThreshold: Time after which to warn about long-running reviews
//   - ErrorPRRetryTimeout: Time after which an "error" PR is retried
const (
	// ReviewPipelineMargin covers everything in a review besides the agent
	// subprocess: the first-pass LLM stage (~4 min on large PRs), clone/fetch,
	// and artifact save. Added to the configured agent wall-clock budget by
	// reviewProcessTimeout() to derive the monitor and stale-reset timeouts.
	//
	// The previous fixed 5-minute ReviewProcessTimeout was SHORTER than the
	// agent wall-clock budget alone (6 min in prod), so healthy long-running
	// reviews were untracked mid-flight; under concurrency their results were
	// then lost to stale-reset/retrigger races instead of being saved.
	ReviewPipelineMargin = 8 * time.Minute
	// ReviewQueueAbandonAfter bounds durable queued rows after a worker crash.
	// It is deliberately much larger than normal concurrency waits so a live
	// process is never mistaken for an abandoned dispatcher.
	ReviewQueueAbandonAfter = 24 * time.Hour

	// ReviewProcessWarningThreshold is when to start warning about long-running reviews.
	ReviewProcessWarningThreshold = 2 * time.Minute

	// ErrorPRRetryTimeout is how long to wait before retrying a PR in "error" state.
	ErrorPRRetryTimeout = 5 * time.Minute

	// ErrorPRMaxAutoRetries caps how many times the poll cycle will reset
	// an error-state PR back to pending. After this many auto-retries the
	// PR stays in error state until a manual trigger resets the counter
	// via SetPRGenerating. Prevents deterministic failures (auth misconfig,
	// model outage, persistent bad-prompt state) from burning quota in a
	// 5-minute loop.
	ErrorPRMaxAutoRetries = 1
)

var errReviewRunBudgetExceeded = errors.New("review run wall-clock budget exceeded")

const reviewBudgetExceededMessage = "review exceeded its execution budget"

type ProcessInfo struct {
	PID       int
	TrackedAt time.Time
	StartTime time.Time
	Timeout   time.Duration
	RunID     string
	// Ctx is the per-review cancellable context. The goroutine MUST use this
	// for all downstream work (LLM calls, agent subprocess) so killReview
	// can abort it mid-flight by calling Cancel.
	Ctx    context.Context
	Cancel context.CancelFunc
}

type Poller struct {
	cfg              *config.Config
	db               db.Database
	ghClient         GitHubClient
	ghClientConcrete *github.Client // Concrete client for reviewer service (which uses a different interface)
	gcsClient        *gcs.Client
	bugMemory        *service.BugMemoryLibrary // nil = feature off
	storage          ReviewStorage             // Optional: for testing. If nil, uses gcsClient/local storage
	reviewGenerator  ReviewGenerator           // Optional: for testing. If nil, uses real reviewer service
	reviewDir        string                    // Local storage path (used when GCS is not configured)
	cacheUpdateFunc  func([]github.PullRequest)
	EventFunc        func(eventType string, payload interface{})
	StatusEventFunc  func()
	triggerChan      chan struct{}
	polling          bool
	pollMutex        sync.Mutex
	// Track active review processes for cancellation and monitoring
	activeReviews map[string]ProcessInfo // prKey (owner/repo/number) -> ProcessInfo
	reviewsMutex  sync.Mutex
	// Track last poll time for countdown display
	lastPollTime  time.Time
	pollTimeMutex sync.RWMutex
	// Track ticker start time for accurate countdown
	tickerStartTime time.Time
	// Dev user for syncing user_pr_views in dev mode
	devUser *db.User
	// Team member cache for resolving team review statuses
	teamMemberCache map[string][]string // team slug -> member logins
	teamCacheMutex  sync.RWMutex
	teamCacheExpiry time.Time
	// Poll economy: track cycles for periodic full refresh
	pollCount int
	// Agent-review subprocess spawner (nil-safe: defaults to the configured CLI).
	agentSpawner service.Spawner
	// compareFilesFn resolves the files changed between two commits of a repo
	// for the carry-forward staleness filter (nil-safe: defaults to the GitHub
	// compare API; tests inject a stub). ok=false means the comparison is
	// unreliable (diverged history, truncated file list, API error) and the
	// caller must not carry anything forward.
	compareFilesFn func(ctx context.Context, owner, repo, base, head, token string) (files []string, ok bool)
	// agentSlots caps concurrent agent reviews per process. Each agent run
	// holds ~1 GB of /tmp (clone) + agent memory; without a cap, two PRs
	// triggered close together can exhaust the instance's memory budget.
	// Buffered to AgentMaxConcurrent; nil if AgentMaxConcurrent <= 0
	// (unlimited, used by tests).
	agentSlots chan struct{}
	// reviewPipelineMargin overrides ReviewPipelineMargin in focused tests so
	// the organic run-timeout path can be exercised without an eight-minute
	// test. Zero retains the production constant.
	reviewPipelineMargin time.Duration
	// Leader election: only the instance holding the DB lease runs the automatic
	// poll cycle, so multiple instances (e.g. a deploy overlap) never poll
	// concurrently. holderID is unique per instance; isLeaderFlag is kept fresh
	// by runLeaderElection and read by the poll loop. Empty holderID disables
	// election (single-process tests that call poll() directly are unaffected,
	// since election only gates Start()'s loop).
	holderID     string
	isLeaderFlag atomic.Bool
}

// Leader-election lease timing. The lease must outlive a poll cycle by a
// comfortable margin (a leader mid-poll must not lose the lease), and renewal
// must beat expiry several times over so a single missed renew doesn't trigger
// a spurious failover.
const (
	leaderLeaseTTL      = 90 * time.Second
	leaderRenewInterval = 30 * time.Second
)

// newHolderID returns a per-instance leader-election identity: the Cloud Run
// revision (shared by a revision's instances) plus random bytes (unique per
// instance), so two instances of the same revision still contend correctly.
func newHolderID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// rand failure is effectively impossible; fall back to a time-based id.
		return fmt.Sprintf("%s-%d", revisionName(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s", revisionName(), hex.EncodeToString(buf))
}

// newReviewRunID returns an opaque, globally unique execution identifier.
// It intentionally does not contain model names: those are mutable metadata,
// while this value is safe to index and use for telemetry correlation.
func newReviewRunID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return reviewRunIDFromTime(time.Now())
	}
	return "run-" + hex.EncodeToString(buf)
}

// reviewRunIDFromTime preserves the run-{32 lowercase hex} contract even in
// the effectively impossible event that the system random source fails.
func reviewRunIDFromTime(t time.Time) string {
	return fmt.Sprintf("run-%032x", t.UnixNano())
}

func geminiModelUses() []payload.ModelUse {
	return []payload.ModelUse{
		{
			Stage:          "first_pass",
			Provider:       "google",
			Backend:        "gemini_api",
			RequestedModel: llm.ProModelName(),
		},
		{
			Stage:          "classification_summary",
			Provider:       "google",
			Backend:        "gemini_api",
			RequestedModel: llm.FlashModelName(),
		},
	}
}

func agentModelUse(review *service.AgentReview) payload.ModelUse {
	provider := "anthropic"
	if review.Backend == service.AgentBackendOpenRouter {
		provider = "openrouter"
	}
	return payload.ModelUse{
		Stage:                "agent",
		Provider:             provider,
		Backend:              review.Backend,
		RequestedModel:       review.RequestedModel,
		ServedModel:          review.ServedModel,
		ServingModelVerified: review.ServingModelVerified,
		Effort:               review.Effort,
		Fallback:             review.ModelFallback,
	}
}

func revisionName() string {
	if rev := os.Getenv("K_REVISION"); rev != "" {
		return rev
	}
	return "local"
}

// isLeader reports whether this instance currently holds the poller lease.
// When election is disabled (empty holderID, e.g. direct unit-test calls), it
// reports true so callers behave as a lone poller.
func (p *Poller) isLeader() bool {
	if p.holderID == "" {
		return true
	}
	return p.isLeaderFlag.Load()
}

// updateLeadership renews/acquires the lease once and records the result.
// On a lease-query error it fails OPEN (assume leadership) so a transient DB
// hiccup degrades to the old always-poll behavior rather than halting polling
// across the whole fleet.
func (p *Poller) updateLeadership(ctx context.Context) bool {
	if p.holderID == "" {
		return true
	}
	leader, err := p.db.TryAcquireOrRenewLeadership(p.holderID, leaderLeaseTTL)
	if err != nil {
		log.Printf("[LEADER] lease query failed, assuming leadership to avoid stalling polls: %v", err)
		leader = true
	}
	if prev := p.isLeaderFlag.Swap(leader); prev != leader {
		if leader {
			log.Printf("[LEADER] acquired poller leadership (holder=%s)", p.holderID)
		} else {
			log.Printf("[LEADER] lost poller leadership (holder=%s); another instance is polling", p.holderID)
		}
	}
	return leader
}

// runLeaderElection renews the lease on a steady cadence independent of the
// poll cycle, so lease validity never depends on how long a poll takes.
func (p *Poller) runLeaderElection(ctx context.Context) {
	ticker := time.NewTicker(leaderRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.updateLeadership(ctx)
		}
	}
}

func New(cfg *config.Config, database db.Database, ghClient *github.Client, gcsClient *gcs.Client) *Poller {
	p := &Poller{
		cfg:              cfg,
		db:               database,
		ghClient:         ghClient,
		ghClientConcrete: ghClient,
		gcsClient:        gcsClient,
		reviewDir:        cfg.ReviewsDir,
		triggerChan:      make(chan struct{}, 1), // Buffered to prevent blocking
		activeReviews:    make(map[string]ProcessInfo),
		agentSpawner:     service.DefaultSpawner{},
		holderID:         newHolderID(),
	}
	if cfg.AgentMaxConcurrent > 0 {
		p.agentSlots = make(chan struct{}, cfg.AgentMaxConcurrent)
	}
	p.loadBugMemory()
	return p
}

// loadBugMemory loads the optional pattern library at startup. Fail-open by
// design: a missing env is feature-off with an info log; a set-but-broken
// source is an error log and feature-off — never a failed boot or review.
// Benchmark arms that REQUIRE memory verify the startup log line instead.
func (p *Poller) loadBugMemory() {
	var data []byte
	var src string
	switch {
	case p.cfg.BugMemoryPath != "":
		src = p.cfg.BugMemoryPath
		b, err := os.ReadFile(p.cfg.BugMemoryPath)
		if err != nil {
			log.Printf("[BUGMEM] ERROR: read %s: %v — bug memory OFF", src, err)
			return
		}
		data = b
	case p.cfg.BugMemoryObject != "" && p.gcsClient != nil:
		src = "gs://" + p.gcsClient.BucketName() + "/" + p.cfg.BugMemoryObject
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b, err := p.gcsClient.GetReviewContent(ctx, p.cfg.BugMemoryObject)
		if err != nil {
			log.Printf("[BUGMEM] ERROR: fetch %s: %v — bug memory OFF", src, err)
			return
		}
		data = b
	default:
		return // feature off
	}
	lib, dropped, err := service.LoadBugMemory(data)
	if err != nil {
		log.Printf("[BUGMEM] ERROR: parse %s: %v — bug memory OFF", src, err)
		return
	}
	for _, d := range dropped {
		log.Printf("[BUGMEM] WARN: dropped entry: %s", d)
	}
	p.bugMemory = lib
	log.Printf("[BUGMEM] loaded %d entries (version %s) from %s", len(lib.Entries), lib.Version, src)
}

// SetAgentSpawner overrides the subprocess spawner used for agent reviews.
// Tests inject a stub to avoid actually invoking an agent CLI.
func (p *Poller) SetAgentSpawner(s service.Spawner) { p.agentSpawner = s }

// persistAgentFailureLog uploads a failed agent run's raw stream-json log to
// GCS under agent-logs/ — the only durable record of what the agent CLI
// reported. Best-effort: an upload failure must never mask the review error.
func (p *Poller) persistAgentFailureLog(logPath string) {
	if p.gcsClient == nil {
		return
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		log.Printf("[AGENT] WARN: read failure log %s: %v", logPath, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objectName := "agent-logs/" + filepath.Base(logPath)
	if err := p.gcsClient.UploadReviewSidecar(ctx, objectName, "application/x-ndjson", data); err != nil {
		log.Printf("[AGENT] WARN: persist failure log to gs://%s/%s: %v", p.gcsClient.BucketName(), objectName, err)
		return
	}
	log.Printf("[AGENT] persisted failure log: gs://%s/%s (%d bytes)", p.gcsClient.BucketName(), objectName, len(data))
}

// systemTelemetryUser is the reserved user that owns server-emitted
// telemetry events (telemetry_events.user_id is NOT NULL with an FK).
// The underscore makes it structurally invalid as a GitHub username, so a
// real login can never collide with (and out-sort) the system user in
// GetUserByUsername.
const systemTelemetryUser = "prism_system"

// recordModelFallback writes an agent_model_fallback telemetry event so
// fallback frequency is queryable from the telemetry dashboard alongside
// user events. Events are attributed to the reserved user named by
// systemTelemetryUser (telemetry_events.user_id is NOT NULL with an FK).
// Best-effort.
func (p *Poller) recordModelFallback(pr github.PullRequest, requested, served string) {
	user, err := p.db.GetUserByUsername(systemTelemetryUser)
	if err == nil && user == nil {
		user = &db.User{GitHubID: -1, GitHubUsername: systemTelemetryUser}
		if err = p.db.CreateUser(user); err != nil {
			// Concurrent fallbacks can race the first create (github_id is
			// unique); the loser re-fetches the row the winner made.
			user, err = p.db.GetUserByUsername(systemTelemetryUser)
		}
	}
	if err != nil || user == nil {
		log.Printf("[REVIEWER] WARN: could not resolve %s user for fallback telemetry: %v", systemTelemetryUser, err)
		return
	}
	event := db.TelemetryEvent{
		UserID:   user.ID,
		Action:   "agent_model_fallback",
		Label:    fmt.Sprintf("requested=%s served=%s", requested, served),
		PROwner:  pr.Owner,
		PRRepo:   pr.Repo,
		PRNumber: pr.Number,
	}
	if err := p.db.CreateTelemetryEvents([]db.TelemetryEvent{event}); err != nil {
		log.Printf("[REVIEWER] WARN: could not record fallback telemetry: %v", err)
	}
}

// isReviewInFlight reports whether a PR's status indicates an in-progress
// review (Gemini "generating" or Claude "agent_reviewing"). Used by the
// outdated-detection paths to decide whether to cancel the active review.
func isReviewInFlight(status string) bool {
	return status == "generating" || status == "agent_reviewing"
}

// runAgentStage runs the configured agent pass on a Gemini ReviewResult: flips
// the PR status to agent_reviewing, spawns the agent against a clone of the
// PR head, replaces the comment set with the agent's refined output, and
// re-renders via the same HTML pipeline so the inline-comment UI is intact.
//
// If a concurrency cap is configured (AGENT_MAX_CONCURRENT), dispatch must
// reserve a slot before the execution budget begins and release it after this
// stage returns.
func (p *Poller) runAgentStage(ctx context.Context, execution *reviewExecution, result *service.ReviewResult) (*ReviewResult, error) {
	pr := execution.Job.PR
	agentSettings := execution.Job.Config.Effective.Agent
	if p.agentSlots != nil && !execution.AgentSlotReserved {
		return nil, fmt.Errorf("agent review: concurrency slot was not reserved before execution budget started")
	}
	agentStartedAt := time.Now().UTC()
	provider := "anthropic"
	if agentSettings.Backend == service.AgentBackendOpenRouter {
		provider = "openrouter"
	}
	recordAgentFailure := func(cause error) {
		completedAt := time.Now().UTC()
		errorSummary := cause.Error()
		p.recordStageAttempt(execution, db.ReviewStageAttempt{
			Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Provider: provider,
			Backend: agentSettings.Backend, RequestedModel: agentSettings.Model, ResolvedModel: agentSettings.Model,
			Effort: agentSettings.Effort, Status: "failed", StartedAt: &agentStartedAt, CompletedAt: &completedAt,
			DurationMS: completedAt.Sub(agentStartedAt).Milliseconds(), ErrorCode: "agent_failed", ErrorSummary: errorSummary,
		})
	}

	log.Printf("[REVIEWER] PR %d: Gemini pass done (comments=%d), entering agent stage",
		pr.Number, len(result.Comments))
	if setErr := p.db.SetPRAgentReviewing(pr.Owner, pr.Repo, pr.Number); setErr != nil {
		log.Printf("[REVIEWER] WARNING: could not set agent_reviewing status for PR %d: %v", pr.Number, setErr)
	}
	p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)

	// Fetch a fresh token at agent-stage start. In prod (GitHub App auth)
	// p.cfg.GitHubToken is empty and we need to mint an installation token
	// from the App; the agent's git subprocess can't share the REST client's
	// oauth2.TokenSource. CurrentToken transparently returns the static PAT
	// in single-user/dev mode and the App installation token in multi-user/
	// prod mode.
	gitToken, tokenErr := p.ghClientConcrete.CurrentToken(ctx)
	if tokenErr != nil {
		log.Printf("[REVIEWER] ERROR: PR %d failed to get GitHub token for clone: %v", pr.Number, tokenErr)
		recordAgentFailure(tokenErr)
		return nil, fmt.Errorf("agent review: get GitHub token: %w", tokenErr)
	}
	agentCfg := p.agentConfigForExecution(execution, gitToken)
	// Pass the PR's true base branch so the clone and the deterministic-layer
	// diff (gates, bug memory, required checks) are computed against it. With
	// "" the diff falls back to origin/HEAD, which inflates the changed-line
	// set for PRs stacked on non-default branches — misattributing gate alerts
	// to parent-branch code or tripping the pathological-diff guard entirely.
	agentOut, agentErr := service.RunAgentReview(ctx, agentCfg, p.agentSpawner,
		pr.Owner, pr.Repo, result.BaseRef, pr.Number, pr.CommitSHA, result.Comments)
	if agentErr != nil {
		log.Printf("[REVIEWER] ERROR: agent review failed for PR %d: %v", pr.Number, agentErr)
		recordAgentFailure(agentErr)
		return nil, fmt.Errorf("agent review: %w", agentErr)
	}
	agentCompletedAt := time.Now().UTC()
	servedModelSource := "pinned_request"
	if agentOut.ServingModelVerified {
		servedModelSource = "stream"
	}
	fallbackReason := ""
	if agentOut.ModelFallback {
		fallbackReason = "observed served model did not match requested model"
	}
	p.recordStageAttempt(execution, db.ReviewStageAttempt{
		Stage: "agent", InvocationNumber: 1, AttemptNumber: 1, Provider: provider,
		Backend: agentOut.Backend, RequestedModel: agentSettings.Model, ResolvedModel: agentOut.RequestedModel,
		ObservedServedModels: append([]string(nil), agentOut.ObservedServedModels...), PrimaryServedModel: agentOut.ServedModel,
		ServedModelSource: servedModelSource, ServingModelVerified: agentOut.ServingModelVerified,
		Fallback: agentOut.ModelFallback, FallbackReason: fallbackReason, MatcherVersion: "v1",
		Effort: agentOut.Effort, Status: "completed", AssistantTurns: agentOut.AssistantTurns,
		StartedAt: &agentStartedAt, CompletedAt: &agentCompletedAt,
		DurationMS: agentCompletedAt.Sub(agentStartedAt).Milliseconds(), StopReason: "completed",
	})

	if agentOut.ModelFallback {
		log.Printf("[REVIEWER] ERROR: MODEL FALLBACK for %s/%s#%d: requested=%s served=%s — review published with fallback badge",
			pr.Owner, pr.Repo, pr.Number, agentOut.RequestedModel, agentOut.ServedModel)
		p.recordModelFallback(pr, agentOut.RequestedModel, agentOut.ServedModel)
	}

	// Reconcile instead of replace: the agent's output used to fully overwrite
	// the first-pass findings, and evaluation against real release blockers
	// showed the agent deleting correct first-pass catches it had argued itself
	// out of. The merge keeps the agent as the canonical voice (its phrasing
	// and SUMMARY win, and duplicates collapse into it) but re-admits
	// first-pass CRITICALs the agent dropped, provenance-tagged. CRITICAL-only
	// is deliberate noise control — loosen only with benchmark evidence.
	firstPassCriticals := make([]types.LineComment, 0, len(result.Comments))
	for _, c := range result.Comments {
		if strings.EqualFold(strings.TrimSpace(c.Importance), "CRITICAL") && c.FilePath != "SUMMARY" {
			firstPassCriticals = append(firstPassCriticals, c)
		}
	}
	sets := []service.FindingSet{
		{Provenance: "agent", Comments: agentOut.Comments},
		{Provenance: "first-pass", Comments: firstPassCriticals},
		// Required-check escalations (empty unless REQUIRED_CHECKS is on):
		// synthesized VIOLATED findings and unanswered memory re-admissions.
		// Merging as a lower-priority set reuses the provenance note and the
		// MEDIUM severity cap. This set must precede "mechanical": a
		// gate-derived VIOLATED synthesis anchors to the same (file, line 0)
		// as the gate alert that spawned it, and the earlier set's phrasing
		// wins the dedup — the confirmed-defect body must absorb the generic
		// advisory, not vanish into it.
		{Provenance: "required-check", Comments: agentOut.CheckFindings},
		{Provenance: "mechanical", Comments: agentOut.Gates},
	}
	// Cross-review carry-forward (CARRY_FORWARD_FINDINGS, default off): a PR
	// re-reviewed at a new head has already paid for k independent review
	// draws, but the dashboard points only at the latest — which can
	// stochastically LOSE a true finding an earlier draw surfaced. Re-admit
	// the previous review's findings whose cited file is untouched between
	// the previously reviewed SHA and this head, as the lowest-priority merge
	// set: duplicates collapse into this run's phrasing, unique carries get
	// the provenance note (naming the source SHA) and the MEDIUM cap.
	carriedSet, carriedInfo := p.carryForwardSet(ctx, pr, gitToken)
	if len(carriedSet.Comments) > 0 {
		sets = append(sets, carriedSet)
	}
	if carriedInfo != nil {
		log.Printf("[REVIEWER] PR %d: carry-forward: from=%s carried_in=%d carried_dropped=%d",
			pr.Number, carriedInfo.FromSHA, carriedInfo.CarriedIn, carriedInfo.CarriedDropped)
	}
	merged := service.MergeFindings(sets...)
	readmitted := len(merged) - len(agentOut.Comments)
	result.Comments = merged
	result.ComputeImportanceCounts()
	log.Printf("[REVIEWER] PR %d: agent stage ok (clone=%s, log=%s, agent_comments=%d, readmitted_first_pass=%d, critical=%d, medium=%d, low=%d)",
		pr.Number, agentOut.CloneDir, agentOut.LogPath, len(agentOut.Comments), readmitted,
		result.CriticalCount, result.MediumCount, result.LowCount)
	if agentOut.Checks.ChecksIssued > 0 {
		log.Printf("[REVIEWER] PR %d: required checks: issued=%d answered=%d violated=%d evidence_ok=%d",
			pr.Number, agentOut.Checks.ChecksIssued, agentOut.Checks.ChecksAnswered,
			agentOut.Checks.ChecksViolated, agentOut.Checks.ChecksEvidenceOK)
	}

	htmlContent := service.GenerateHTMLReportContent(result, pr.Number, pr.Owner, pr.Repo, pr.CommitSHA, llm.ProModelName())
	if htmlContent == nil {
		return nil, fmt.Errorf("failed to generate HTML content from agent comments")
	}
	return &ReviewResult{
		HTMLContent:   htmlContent,
		CriticalCount: result.CriticalCount,
		MediumCount:   result.MediumCount,
		LowCount:      result.LowCount,
		Comments:      result.Comments,
		Diff:          result.Diff,
		FileContents:  result.FileContents,
		BugMemory:     agentOut.BugMemory,
		Checks:        agentOut.Checks,
		ModelFallback: agentOut.ModelFallback,
		ReviewRun: &payload.ReviewRunInfo{
			Models: append(geminiModelUses(), agentModelUse(agentOut)),
		},
		// Copied (not aliased) so the no-swallow check reads the pre-merge
		// alert set even if a later stage mutates the agent output.
		GateAlerts: append(append([]types.LineComment{}, agentOut.Gates...), agentOut.CheckFindings...),
		Carried:    carriedInfo,
	}, nil
}

// ---- Cross-review carry-forward (CARRY_FORWARD_FINDINGS) -------------------
//
// Review artifacts are per-SHA in GCS, so every reviewed push of a PR leaves
// a findings sidecar behind. carryForwardSet turns that history into a union:
// it loads the most recent prior review of the PR, drops findings whose cited
// file the new push touched (assume addressed; zero-noise rule), and hands
// the survivors to MergeFindings as the lowest-priority set. Strictly
// best-effort — any failure logs and degrades to a carry-less review.

// carryForwardEnabled gates the feature. Read per call (not cached at init)
// so tests can toggle it with t.Setenv; unset keeps reviews byte-identical
// to a build without the feature.
func carryForwardEnabled() bool {
	return os.Getenv("CARRY_FORWARD_FINDINGS") == "true"
}

// carryForwardSet builds the carried FindingSet for a review of pr at its
// current head. Returns a zero FindingSet and nil telemetry when the feature
// is off, no prior review with a sidecar exists, or loading fails.
func (p *Poller) carryForwardSet(ctx context.Context, pr github.PullRequest, gitToken string) (service.FindingSet, *payload.CarryForwardInfo) {
	if !carryForwardEnabled() {
		return service.FindingSet{}, nil
	}
	loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	prior, err := p.loadPriorReviewPayload(loadCtx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
	if err != nil {
		log.Printf("[REVIEWER] WARN: PR %d: carry-forward: load prior review: %v — carrying nothing", pr.Number, err)
		return service.FindingSet{}, nil
	}
	if prior == nil {
		return service.FindingSet{}, nil // first review of this PR
	}
	fromSHA := prior.CommitSHA
	if fromSHA == "" {
		// Pre-schema sidecar without a commit pointer: no way to compute the
		// inter-push diff, so nothing can be proven untouched.
		return service.FindingSet{}, nil
	}
	candidates := prior.ToLineComments()

	fn := p.compareFilesFn
	if fn == nil {
		fn = githubCompareChangedFiles
	}
	touched, ok := fn(loadCtx, pr.Owner, pr.Repo, fromSHA, pr.CommitSHA, gitToken)
	if !ok {
		// Unreliable inter-push diff (force-push/rebase divergence, truncated
		// file list, API failure): we cannot prove any file untouched, so
		// carry nothing. Telemetry still records the attempt; the dropped
		// count uses the same filter as a real carry (SUMMARY never counts).
		log.Printf("[REVIEWER] WARN: PR %d: carry-forward: %s..%s comparison unreliable — carrying nothing",
			pr.Number, fromSHA, pr.CommitSHA)
		carriable, _ := service.CarryForwardFindings(candidates, nil)
		return service.FindingSet{}, &payload.CarryForwardInfo{
			FromSHA:        fromSHA,
			CarriedDropped: len(carriable),
		}
	}

	carried, dropped := service.CarryForwardFindings(candidates, touched)
	info := &payload.CarryForwardInfo{
		FromSHA:        fromSHA,
		CarriedIn:      len(carried),
		CarriedDropped: dropped,
	}
	return service.FindingSet{
		// The label embeds the source SHA, so the merge's provenance note on
		// each carried finding deterministically names the review it came from.
		Provenance: service.CarriedProvenance(fromSHA),
		Comments:   carried,
	}, info
}

// loadPriorReviewPayload returns the findings sidecar of the most recent
// review of this PR at a commit other than currentSHA, or (nil, nil) when no
// such review exists (first review, or history predates sidecars).
func (p *Poller) loadPriorReviewPayload(ctx context.Context, owner, repo string, prNumber int, currentSHA string) (*payload.Payload, error) {
	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		reviews, err := p.gcsClient.ListReviewsForPR(ctx, owner, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("list reviews: %w", err)
		}
		// Newest first; try a few in case the latest prior review predates
		// sidecars or its sidecar write failed.
		sort.SliceStable(reviews, func(i, j int) bool { return reviews[i].CreatedAt.After(reviews[j].CreatedAt) })
		tried := 0
		for _, r := range reviews {
			if isSameCommit(currentSHA, r.CommitSHA) {
				continue
			}
			if tried >= 3 {
				break
			}
			tried++
			body, err := p.gcsClient.GetReviewContent(ctx, gcs.ReviewJSONFileName(r.Filename))
			if err != nil {
				log.Printf("[REVIEWER] carry-forward: no sidecar for prior review %s: %v", r.Filename, err)
				continue
			}
			pl, err := parseSidecarPayload(body)
			if err != nil {
				log.Printf("[REVIEWER] carry-forward: bad sidecar for prior review %s: %v", r.Filename, err)
				continue
			}
			return pl, nil
		}
		return nil, nil
	}

	// Local storage: sidecars live next to the HTML in reviewDir under the
	// same per-SHA naming.
	entries, err := os.ReadDir(p.reviewDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read reviews dir: %w", err)
	}
	prefix := fmt.Sprintf("%s_%s_%d_", owner, repo, prNumber)
	var newestName string
	var newestMod time.Time
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		sha := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
		if isSameCommit(currentSHA, sha) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if newestName == "" || fi.ModTime().After(newestMod) {
			newestName, newestMod = name, fi.ModTime()
		}
	}
	if newestName == "" {
		return nil, nil
	}
	body, err := os.ReadFile(filepath.Join(p.reviewDir, newestName))
	if err != nil {
		return nil, fmt.Errorf("read sidecar %s: %w", newestName, err)
	}
	return parseSidecarPayload(body)
}

// parseSidecarPayload unmarshals a findings sidecar. A payload with zero
// findings is valid — a clean prior review is still the PR's most recent
// assessment, and correctly yields a carry-less run rather than falling
// through to an older, superseded review.
func parseSidecarPayload(body []byte) (*payload.Payload, error) {
	var pl payload.Payload
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

// isSameCommit compares two commit identifiers that may be truncated to
// different lengths (review object names carry 7-char SHAs, sidecar payloads
// full SHAs): they refer to the same commit when one is a prefix of the other.
func isSameCommit(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// githubAPIBaseURL is the GitHub REST endpoint used by the carry-forward
// compare call. Package variable so tests can point it at a stub server.
var githubAPIBaseURL = "https://api.github.com"

// githubCompareChangedFiles resolves the files changed between two commits
// via the GitHub compare API. This is the default compareFilesFn: one
// authenticated GET, no worktree needed (the agent's worktree is already
// cleaned up by merge time).
//
// ok=false when the answer cannot be trusted as a complete inter-push diff:
//   - status "diverged"/"behind" (force-push or rebase rewrote history —
//     the compare's merge-base semantics no longer equal old..new)
//   - 300 files reported (GitHub truncates the list at 300, so absence of a
//     file no longer proves it untouched)
//   - any transport/HTTP/decode error
func githubCompareChangedFiles(ctx context.Context, owner, repo, base, head, token string) ([]string, bool) {
	url := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", githubAPIBaseURL, owner, repo, base, head)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("[REVIEWER] carry-forward: build compare request: %v", err)
		return nil, false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[REVIEWER] carry-forward: compare %s..%s: %v", base, head, err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[REVIEWER] carry-forward: compare %s..%s: HTTP %d", base, head, resp.StatusCode)
		return nil, false
	}
	var body struct {
		Status string `json:"status"` // ahead | behind | diverged | identical
		Files  []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("[REVIEWER] carry-forward: decode compare %s..%s: %v", base, head, err)
		return nil, false
	}
	// "ahead" = base is an ancestor of head (a normal push): the compare's
	// merge-base diff IS the inter-push diff. "identical" = same tree
	// (re-review), trivially reliable with zero files.
	if body.Status != "ahead" && body.Status != "identical" {
		return nil, false
	}
	if len(body.Files) >= 300 {
		return nil, false
	}
	files := make([]string, 0, len(body.Files))
	for _, f := range body.Files {
		files = append(files, f.Filename)
		if f.PreviousFilename != "" {
			// A rename touches both names; a finding citing either is stale.
			files = append(files, f.PreviousFilename)
		}
	}
	return files, true
}

// broadcastPRUpdate helper to send update events
func (p *Poller) broadcastPRUpdate(owner, repo string, number int) {
	if p.EventFunc != nil {
		p.EventFunc("pr_updated", map[string]interface{}{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		})
	}
}

func (p *Poller) SetCacheUpdateFunc(f func([]github.PullRequest)) {
	p.cacheUpdateFunc = f
}

// SetDevUser sets the dev user for syncing user_pr_views in dev mode
func (p *Poller) SetDevUser(user *db.User) {
	p.devUser = user
}

// getTeamMembers returns team members with a 5-minute cache TTL.
func (p *Poller) getTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error) {
	p.teamCacheMutex.RLock()
	if time.Now().Before(p.teamCacheExpiry) {
		if members, ok := p.teamMemberCache[teamSlug]; ok {
			p.teamCacheMutex.RUnlock()
			return members, nil
		}
	}
	p.teamCacheMutex.RUnlock()

	members, err := p.ghClient.GetOrgTeamMembers(ctx, orgName, teamSlug)
	if err != nil {
		return nil, err
	}

	p.teamCacheMutex.Lock()
	if p.teamMemberCache == nil {
		p.teamMemberCache = make(map[string][]string)
	}
	// If cache is expired, clear it and reset expiry
	if time.Now().After(p.teamCacheExpiry) {
		p.teamMemberCache = make(map[string][]string)
		p.teamCacheExpiry = time.Now().Add(5 * time.Minute)
	}
	p.teamMemberCache[teamSlug] = members
	p.teamCacheMutex.Unlock()

	return members, nil
}

func (p *Poller) Trigger() {
	// Non-blocking send to trigger channel
	select {
	case p.triggerChan <- struct{}{}:
		log.Println("Manual poll trigger requested")
	default:
		// Channel already has a pending trigger, skip
	}
}

func (p *Poller) Start(ctx context.Context) {
	tickerStartTime := time.Now()
	ticker := time.NewTicker(p.cfg.PollingInterval)
	defer ticker.Stop()

	// Store ticker start time for accurate countdown
	p.pollTimeMutex.Lock()
	p.tickerStartTime = tickerStartTime
	p.pollTimeMutex.Unlock()

	// Start reviewer process monitor
	monitorTicker := time.NewTicker(30 * time.Second)
	defer monitorTicker.Stop()
	go p.monitorReviewerProcesses(ctx, monitorTicker)

	log.Println("Starting poller...")
	log.Printf("Ticker created at %s, will fire every %v", tickerStartTime.Format("15:04:05.000"), p.cfg.PollingInterval)
	if p.holderID != "" {
		log.Printf("[LEADER] poller leadership enabled (holder=%s, lease=%v, renew=%v)", p.holderID, leaderLeaseTTL, leaderRenewInterval)
	}

	// Acquire/renew leadership once synchronously so the initial poll reflects it,
	// then keep the lease fresh in the background.
	p.updateLeadership(ctx)
	go p.runLeaderElection(ctx)

	// Run immediately on start (leader only) — unless polling is disabled
	// outright. Benchmark and on-demand deployments set DISABLE_POLLING to
	// stop the boot-time and scheduled polls from ingesting real open PRs
	// (burning tokens and writing review artifacts that shadow the primary
	// deployment's for the same commits). DISABLE_POLLING takes precedence
	// over leadership; manual triggers and on-demand reviews still work.
	if p.cfg.DisablePolling {
		log.Println("DISABLE_POLLING set — skipping initial and scheduled polls (manual trigger + on-demand reviews still available)")
	} else if p.isLeader() {
		p.startPoll(ctx, "initial")
	} else {
		log.Printf("[LEADER] not leader at startup, skipping initial poll")
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Poller stopped")
			return
		case tickTime := <-ticker.C:
			if p.cfg.DisablePolling {
				continue
			}
			elapsed := tickTime.Sub(tickerStartTime)
			log.Printf("Ticker fired at %s (%.3fs since ticker start)", tickTime.Format("15:04:05.000"), elapsed.Seconds())
			// Only the lease holder runs the automatic cycle, so concurrent
			// instances never poll (and prune) at the same time. Manual triggers
			// below are deliberately exempt — they're explicit user actions.
			if p.isLeader() {
				p.startPoll(ctx, "scheduled")
			} else {
				log.Printf("[LEADER] not leader, skipping scheduled poll")
			}
		case <-p.triggerChan:
			p.startPoll(ctx, "manual")
		}
	}
}

// reviewProcessTimeout is the maximum legitimate duration of one review:
// the configured agent wall-clock budget plus ReviewPipelineMargin for the
// first-pass stage, clone, and save. The monitor and the stale-generating
// reset both derive from it so they can never fire on a healthy review.
func (p *Poller) reviewProcessTimeout() time.Duration {
	t := ReviewPipelineMargin
	if p.cfg != nil && p.cfg.AgentWallClockSec > 0 {
		t += time.Duration(p.cfg.AgentWallClockSec) * time.Second
	}
	return t
}

func (p *Poller) monitorReviewerProcesses(ctx context.Context, ticker *time.Ticker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p.isLeader() {
				if abandoned, err := p.db.AbandonExpiredReviewRuns(time.Now().UTC(), ReviewLeaseCompletionGrace, ReviewQueueAbandonAfter); err != nil {
					log.Printf("[MONITOR] WARNING: failed to abandon expired review runs: %v", err)
				} else if abandoned > 0 {
					log.Printf("[MONITOR] marked %d expired review runs timed out", abandoned)
				}
			}
			p.reviewsMutex.Lock()
			for key, info := range p.activeReviews {
				if info.StartTime.IsZero() {
					if !info.TrackedAt.IsZero() && time.Since(info.TrackedAt) > ReviewQueueAbandonAfter {
						log.Printf("[MONITOR] WARNING: queued review for %s exceeded %v, removing from tracking", key, ReviewQueueAbandonAfter)
						if info.Cancel != nil {
							info.Cancel()
						}
						delete(p.activeReviews, key)
					}
					continue
				}
				timeout := info.Timeout
				if timeout <= 0 {
					timeout = p.reviewProcessTimeout()
				}
				elapsed := time.Since(info.StartTime)
				if elapsed > timeout {
					log.Printf("[MONITOR] WARNING: review for %s has been running for %v (timeout), removing from tracking", key, elapsed)
					if info.Cancel != nil {
						info.Cancel()
					}
					delete(p.activeReviews, key)
				} else if elapsed > ReviewProcessWarningThreshold {
					log.Printf("[MONITOR] WARNING: review for %s has been running for %v (threshold: 2m)", key, elapsed)
				} else {
					log.Printf("[MONITOR] review for %s running normally (%v elapsed)", key, elapsed)
				}
			}
			p.reviewsMutex.Unlock()
		}
	}
}

func (p *Poller) GetReviewerStatus() (running bool, duration time.Duration) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()

	if len(p.activeReviews) == 0 {
		return false, 0
	}

	// Verify processes are actually still running and find the longest duration
	count := 0
	var maxDuration time.Duration

	// Iterate safely over copy of keys if we need to modify, or just check
	// Since we might delete, it's safer to just check existence here, or rely on monitor to clean up
	// For status check, we'll just read.
	for _, info := range p.activeReviews {
		// Queued jobs are owned but are not yet consuming execution capacity.
		if info.StartTime.IsZero() {
			continue
		}
		// Reviews run as in-process goroutines, just count executing ones.
		count++
		d := time.Since(info.StartTime)
		if d > maxDuration {
			maxDuration = d
		}
	}

	return count > 0, maxDuration
}

func (p *Poller) GetLastPollTime() time.Time {
	p.pollTimeMutex.RLock()
	defer p.pollTimeMutex.RUnlock()
	return p.lastPollTime
}

func (p *Poller) GetPollingInterval() time.Duration {
	return p.cfg.PollingInterval
}

// GetSecondsUntilNextPoll calculates accurate countdown based on ticker timing
func (p *Poller) GetSecondsUntilNextPoll() int {
	p.pollTimeMutex.RLock()
	tickerStart := p.tickerStartTime
	p.pollTimeMutex.RUnlock()

	if tickerStart.IsZero() {
		return 0
	}

	now := time.Now()
	interval := p.cfg.PollingInterval

	// Calculate how long since ticker started
	elapsed := now.Sub(tickerStart)

	// Calculate which tick number we're waiting for
	// Add 1 because we want the NEXT tick
	tickNumber := int(elapsed/interval) + 1

	// Calculate when that tick will fire
	nextTickTime := tickerStart.Add(time.Duration(tickNumber) * interval)

	// Calculate remaining time
	remaining := nextTickTime.Sub(now)

	if remaining < 0 {
		return 0
	}

	seconds := int(remaining.Seconds())

	return seconds
}

// prKey creates a unique key for tracking a PR
func prKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// trackReview is retained as a test/compatibility helper for callers that do
// not yet have a durable ReviewJob. Production dispatch uses the run-aware
// tracking methods below.
func (p *Poller) trackReview(parent context.Context, owner, repo string, number, pid int) context.Context {
	return p.trackReviewWithTimeout(parent, owner, repo, number, pid, "", p.reviewProcessTimeout())
}

func (p *Poller) trackReviewWithTimeout(parent context.Context, owner, repo string, number, pid int, runID string, timeout time.Duration) context.Context {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	if existing, ok := p.activeReviews[key]; ok && existing.Ctx != nil {
		log.Printf("[TRACK] Re-tracking %s (re-using existing ctx)", key)
		return existing.Ctx
	}
	if timeout <= 0 {
		timeout = p.reviewProcessTimeout()
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(parent, timeout)
	p.activeReviews[key] = ProcessInfo{
		PID:       pid,
		TrackedAt: now,
		StartTime: now,
		Timeout:   timeout,
		RunID:     runID,
		Ctx:       ctx,
		Cancel:    cancel,
	}
	log.Printf("[TRACK] Tracking compatibility review for %s", key)
	return ctx
}

func (p *Poller) tryTrackReviewJob(parent context.Context, job ReviewJob) (context.Context, bool) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)
	if _, exists := p.activeReviews[key]; exists {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	p.activeReviews[key] = ProcessInfo{
		TrackedAt: time.Now(), Timeout: p.reviewTimeout(job.Config.Effective), RunID: job.RunID, Ctx: ctx, Cancel: cancel,
	}
	log.Printf("[TRACK] Tracking queued review job %s for %s", job.RunID, key)
	return ctx, true
}

// trackOrAdoptReviewJob creates the worker tracking entry or adopts the entry
// synchronously created by ProcessReviewJob. It never shares a context across
// different run IDs for the same PR.
func (p *Poller) trackOrAdoptReviewJob(parent context.Context, job ReviewJob) (context.Context, bool) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)
	if existing, exists := p.activeReviews[key]; exists {
		if existing.RunID == job.RunID && existing.Ctx != nil {
			return existing.Ctx, true
		}
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	p.activeReviews[key] = ProcessInfo{
		TrackedAt: time.Now(), Timeout: p.reviewTimeout(job.Config.Effective), RunID: job.RunID, Ctx: ctx, Cancel: cancel,
	}
	log.Printf("[TRACK] Tracking queued review job %s for %s", job.RunID, key)
	return ctx, true
}

// startTrackedReviewJob starts the configured execution budget only after the
// batch semaphore grants a worker slot. Ownership is established earlier, but
// time spent queued must not reduce a per-review wall-clock allowance.
func (p *Poller) startTrackedReviewJob(job ReviewJob) (context.Context, bool) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)
	info, exists := p.activeReviews[key]
	if !exists || info.RunID != job.RunID || info.Ctx == nil || !info.StartTime.IsZero() {
		return nil, false
	}
	timeout := p.reviewTimeout(job.Config.Effective)
	runCtx, runCancel := context.WithTimeoutCause(info.Ctx, timeout, errReviewRunBudgetExceeded)
	queuedCancel := info.Cancel
	info.StartTime = time.Now()
	info.Timeout = timeout
	info.Ctx = runCtx
	info.Cancel = func() {
		runCancel()
		if queuedCancel != nil {
			queuedCancel()
		}
	}
	p.activeReviews[key] = info
	log.Printf("[TRACK] Started review job %s for %s (timeout=%s)", job.RunID, key, timeout)
	return runCtx, true
}

// untrackReviewRun removes a PR's review process from the active reviews map
// only when runID still owns it, and invokes its stored Cancel func. Both
// happen under one mutex hold so stale workers cannot cancel a replacement.
//
// Calling Cancel on the happy path (review completed normally) is required:
// the tracking entry owns a context.WithCancel, and `go vet`'s lostcancel rule
// (rightly) complains if we drop the cancel func without calling it. After
// successful completion the cancel is a no-op for the work, but it releases
// the bookkeeping the WithCancel goroutine holds.
func (p *Poller) untrackReviewRun(owner, repo string, number int, runID string) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	if info, ok := p.activeReviews[key]; ok {
		if runID == "" || info.RunID != runID {
			log.Printf("[TRACK] Refusing to untrack %s: owner run=%s, stale run=%s", key, info.RunID, runID)
			return
		}
		if info.Cancel != nil {
			info.Cancel()
		}
		delete(p.activeReviews, key)
		log.Printf("[TRACK] Untracked review for %s", key)
	}
}

// isTracked checks if a PR is currently being processed
func (p *Poller) isTracked(owner, repo string, number int) bool {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	key := prKey(owner, repo, number)
	_, exists := p.activeReviews[key]
	return exists
}

// setPRErrorUnlessReplaced serializes the timeout projection with local run
// admission. A missing entry means the monitor evicted this run and no
// successor exists yet, so projecting is safe; a different run ID means a
// successor already owns the PR and must not be clobbered.
func (p *Poller) setPRErrorUnlessReplaced(job ReviewJob, message string) (bool, error) {
	p.reviewsMutex.Lock()
	defer p.reviewsMutex.Unlock()
	if info, exists := p.activeReviews[prKey(job.PR.Owner, job.PR.Repo, job.PR.Number)]; exists && info.RunID != job.RunID {
		return false, nil
	}
	return true, p.db.SetPRError(job.PR.Owner, job.PR.Repo, job.PR.Number, message)
}

// killReview cancels an active review's context (which propagates to the
// agent subprocess via DefaultSpawner's ctx-watcher) and removes the entry
// from tracking, atomically. Returns false if no review was tracked. The
// goroutine detects the cancel via ctx.Err() in its current LLM call or
// agent subprocess, exits, and skips the SetPRError write so a concurrent
// ResetPRToOutdated isn't clobbered.
func (p *Poller) killReview(owner, repo string, number int) bool {
	p.reviewsMutex.Lock()
	key := prKey(owner, repo, number)
	info, exists := p.activeReviews[key]
	if exists {
		// Cancel + delete inside the same critical section so no concurrent
		// caller sees the entry as tracked-but-cancelled.
		if info.Cancel != nil {
			info.Cancel()
		}
		delete(p.activeReviews, key)
	}
	p.reviewsMutex.Unlock()

	if !exists {
		return false
	}
	log.Printf("[TRACK] Cancelled and untracked review for %s", key)
	return true
}

// ProcessReviewImmediate adapts the legacy immediate-review entry point into an
// immutable ReviewJob. ProcessReviewJob durably creates and synchronously tracks
// the queued run before launching its worker, so poll-cycle guards see it.
//
// If force is true, the existing-review cache check is skipped — useful for
// the manual "Review" button so a click always regenerates (overwriting the
// previous review for the same commit).
func (p *Poller) ProcessReviewImmediate(ctx context.Context, owner, repo string, number int, commitSHA, title, author string, createdAt *time.Time, draft bool, force bool) {
	pr := github.PullRequest{
		Owner: owner, Repo: repo, Number: number, CommitSHA: commitSHA,
		Title: title, Author: author, CreatedAt: createdAt, Draft: draft,
	}
	job, err := p.defaultReviewJob(pr, force, "legacy_api")
	if err == nil {
		err = p.ProcessReviewJob(ctx, job)
	}
	if err != nil {
		log.Printf("[IMMEDIATE] ERROR: Could not queue immediate review for %s/%s#%d: %v", owner, repo, number, err)
		if !errors.Is(err, ErrReviewAlreadyTracked) {
			if setErr := p.db.SetPRError(owner, repo, number, err.Error()); setErr != nil {
				log.Printf("[IMMEDIATE] WARNING: failed to persist error status: %v", setErr)
			}
			p.broadcastPRUpdate(owner, repo, number)
		}
	}
}

// IsReviewTracked returns whether a PR is currently being actively reviewed.
// Exported for use by the stale-reset guard in the poll cycle.
func (p *Poller) IsReviewTracked(owner, repo string, number int) bool {
	return p.isTracked(owner, repo, number)
}

func (p *Poller) startPoll(ctx context.Context, trigger string) {
	p.pollMutex.Lock()
	if p.polling {
		log.Printf("Poll already in progress, skipping %s trigger", trigger)
		p.pollMutex.Unlock()
		return
	}
	p.polling = true
	p.pollMutex.Unlock()

	log.Printf("Starting %s poll", trigger)
	if p.StatusEventFunc != nil {
		p.StatusEventFunc()
	}

	go func() {
		defer func() {
			p.pollMutex.Lock()
			p.polling = false
			p.pollMutex.Unlock()
			log.Printf("Completed %s poll", trigger)
			if p.StatusEventFunc != nil {
				p.StatusEventFunc()
			}
		}()
		p.poll(ctx)
	}()
}

// manualClaimPRIDs returns the set of PR ids exempt from closed-PR cleanup
// because a user manually requested them and hasn't deleted them. ok=false
// means the claims are unknown (DB error) — callers must skip deletion for
// the cycle rather than delete a row a user asked to keep.
func (p *Poller) manualClaimPRIDs() (map[int]bool, bool) {
	claims, err := p.db.GetPRIDsWithManualClaims()
	if err != nil {
		log.Printf("[CLEANUP] ERROR: could not load manual claims, skipping closed-PR cleanup this cycle: %v", err)
		return nil, false
	}
	return claims, true
}

// retainForManualClaim handles a closed PR kept alive by a manual claim:
// non-claimants' views are soft-hidden so their dashboards behave as if the
// row had been cleaned up (they get no pr_deleted broadcast; the row drops
// out on their next fetch), and the PR's GitHub state is persisted so the
// dashboard can render it as merged/closed. Best-effort — the retained row
// is re-processed every cycle, so failed writes self-heal.
func (p *Poller) retainForManualClaim(pr db.PR, state string) {
	key := fmt.Sprintf("%s/%s#%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
	log.Printf("[CLEANUP] PR %s is closed but manually requested — keeping until the requester deletes it", key)
	if err := p.db.HideNonManualViewsForPR(pr.ID); err != nil {
		log.Printf("[CLEANUP] WARN: could not hide non-manual views for retained PR %s: %v", key, err)
	}
	if state = strings.ToLower(state); state != "" && state != pr.PRState {
		if err := p.db.SetPRState(pr.RepoOwner, pr.RepoName, pr.PRNumber, state); err != nil {
			log.Printf("[CLEANUP] WARN: could not persist state %q for retained PR %s: %v", state, key, err)
		} else {
			p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		}
	}
}

// cleanupClosedPRs removes PRs from the database and filesystem if they're closed on GitHub
func (p *Poller) cleanupClosedPRs(ctx context.Context) (int, error) {
	// Get all PRs from database
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}

	manualClaims, claimsOK := p.manualClaimPRIDs()
	if !claimsOK {
		return 0, fmt.Errorf("skipping closed-PR cleanup: manual claims unavailable")
	}

	removed := 0
	for _, pr := range allPRs {
		// Check if PR is still open on GitHub
		isOpen, err := p.ghClient.IsPROpen(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			// If we can't fetch the PR, it might be deleted or we don't have access
			// Log but continue - we'll handle it on next poll
			log.Printf("[CLEANUP] Warning: Could not check status of PR %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		if !isOpen && manualClaims[pr.ID] {
			// IsPROpen can't distinguish merged from closed — leave the state
			// to the batched cleanup path (cleanupAndDetectOutdated).
			p.retainForManualClaim(pr, "")
			continue
		}

		// If PR is closed, remove it from the database
		// Note: Reviews are kept in GCS permanently for historical reference
		if !isOpen {
			log.Printf("[CLEANUP] PR %s/%s#%d is closed, removing from tracking (reviews kept in GCS)",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)

			// Delete from database
			if err := p.db.DeletePR(pr.RepoOwner, pr.RepoName, pr.PRNumber); err != nil {
				log.Printf("[CLEANUP] ERROR: Failed to delete PR %s/%s#%d from database: %v",
					pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
				continue
			}

			// Notify clients
			if p.EventFunc != nil {
				p.EventFunc("pr_deleted", map[string]interface{}{
					"owner":  pr.RepoOwner,
					"repo":   pr.RepoName,
					"number": pr.PRNumber,
				})
			}

			log.Printf("[CLEANUP] Successfully removed closed PR %s/%s#%d",
				pr.RepoOwner, pr.RepoName, pr.PRNumber)
			removed++
		}
	}

	return removed, nil
}

// cleanupAndDetectOutdated combines closed-PR cleanup and outdated-review detection
// into a single pass using batched GraphQL, replacing the sequential REST calls.
func (p *Poller) cleanupAndDetectOutdated(ctx context.Context) (removed int, outdated int, err error) {
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}
	if len(allPRs) == 0 {
		return 0, 0, nil
	}

	// Build PRInfo slice for batch query
	prInfos := make([]github.PRInfo, len(allPRs))
	for i, pr := range allPRs {
		prInfos[i] = github.PRInfo{
			Owner:  pr.RepoOwner,
			Repo:   pr.RepoName,
			Number: pr.PRNumber,
		}
	}

	// Single batched GraphQL call for all PRs
	log.Printf("[POLL] Batch-fetching state for %d PRs...", len(prInfos))
	stateMap, err := p.ghClient.BatchGetPRState(ctx, prInfos)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to batch-fetch PR state: %w", err)
	}

	manualClaims, claimsOK := p.manualClaimPRIDs()

	// Single pass: handle closed PRs and outdated reviews
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.RepoOwner, pr.RepoName, pr.PRNumber)
		state, ok := stateMap[key]
		if !ok {
			log.Printf("[POLL] Warning: No state data for PR %s, skipping", key)
			continue
		}

		// --- Closed PR cleanup ---
		if state.State != "OPEN" {
			if !claimsOK || manualClaims[pr.ID] {
				if claimsOK {
					p.retainForManualClaim(pr, state.State)
				}
				continue
			}
			log.Printf("[CLEANUP] PR %s is %s, removing from tracking (reviews kept in GCS)", key, state.State)
			if err := p.db.DeletePR(pr.RepoOwner, pr.RepoName, pr.PRNumber); err != nil {
				log.Printf("[CLEANUP] ERROR: Failed to delete PR %s: %v", key, err)
				continue
			}
			if p.EventFunc != nil {
				p.EventFunc("pr_deleted", map[string]interface{}{
					"owner":  pr.RepoOwner,
					"repo":   pr.RepoName,
					"number": pr.PRNumber,
				})
			}
			log.Printf("[CLEANUP] Successfully removed closed PR %s", key)
			removed++
			continue
		}

		// --- PR-state sync (re-opened retained PRs) ---
		// A PR retained past close (manual claim) had pr_state persisted as
		// closed/merged; if it's re-opened on GitHub, restore it or the UI
		// keeps showing the merged/closed dot instead of live CI.
		if pr.PRState != "" && pr.PRState != "open" {
			log.Printf("[STATE-SYNC] PR %s re-opened on GitHub, restoring pr_state to open", key)
			if err := p.db.SetPRState(pr.RepoOwner, pr.RepoName, pr.PRNumber, "open"); err != nil {
				log.Printf("[STATE-SYNC] ERROR: failed to restore state for PR %s: %v", key, err)
			} else {
				p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)
			}
		}

		// --- Draft-state sync ---
		// The metadata phases rebuild known PRs from the DB, so a draft↔ready
		// flip on GitHub would otherwise never reach us. This state query is
		// the one place we get fresh per-PR data every cycle — sync it here.
		if state.IsDraft != pr.Draft {
			log.Printf("[DRAFT-SYNC] PR %s draft state changed on GitHub: %t -> %t", key, pr.Draft, state.IsDraft)
			if err := p.db.UpdatePRDraft(pr.RepoOwner, pr.RepoName, pr.PRNumber, state.IsDraft); err != nil {
				log.Printf("[DRAFT-SYNC] ERROR: Failed to update draft state for PR %s: %v", key, err)
			} else {
				p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)
			}
		}

		// --- Outdated review detection ---
		if state.HeadRefOid != pr.LastCommitSHA {
			wasInFlight := isReviewInFlight(pr.Status)
			oldSHA, newSHA := pr.LastCommitSHA, state.HeadRefOid
			if len(oldSHA) > 7 {
				oldSHA = oldSHA[:7]
			}
			if len(newSHA) > 7 {
				newSHA = newSHA[:7]
			}
			log.Printf("[OUTDATED] PR %s has new commits (old: %s, new: %s), resetting to pending",
				key, oldSHA, newSHA)

			// Delete old HTML file if it exists
			if pr.ReviewHTMLPath != "" {
				oldHTMLPath := filepath.Join(p.reviewDir, pr.ReviewHTMLPath)
				if err := os.Remove(oldHTMLPath); err != nil && !os.IsNotExist(err) {
					log.Printf("[OUTDATED] Warning: Failed to delete old HTML file %s: %v", oldHTMLPath, err)
				}
			}

			// If the PR had an active Gemini or agent review, kill the process.
			if wasInFlight {
				if p.killReview(pr.RepoOwner, pr.RepoName, pr.PRNumber) {
					log.Printf("[OUTDATED] Killed active review process for %s", key)
				}
			}

			// Reset PR to pending with new commit SHA
			if err := p.db.ResetPRToOutdated(pr.RepoOwner, pr.RepoName, pr.PRNumber, state.HeadRefOid); err != nil {
				log.Printf("[OUTDATED] ERROR: Failed to reset PR %s: %v", key, err)
				continue
			}

			p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)
			outdated++
		}
	}

	return removed, outdated, nil
}

// reviewExists checks if a review already exists for the given PR+commit.
// If a storage interface is set (for testing), it uses that.
// Otherwise, checks GCS if bucket is configured, or local file storage.
func (p *Poller) reviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error) {
	if p.storage != nil {
		return p.storage.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.ReviewExists(ctx, owner, repo, prNumber, commitSHA)
	}

	filename := gcs.ReviewFileName(owner, repo, prNumber, commitSHA)
	localPath := filepath.Join(p.reviewDir, filename)
	_, err := os.Stat(localPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// saveReview persists review content to storage (GCS or local disk).
func (p *Poller) saveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, reviewRun *payload.ReviewRunInfo, content []byte) (string, error) {
	if reviewRun == nil || reviewRun.HTMLPath == "" {
		return "", fmt.Errorf("save review: immutable review-run path is required")
	}
	if err := p.saveImmutableReviewArtifact(ctx, reviewRun.HTMLPath, "text/html; charset=utf-8", content); err != nil {
		return "", fmt.Errorf("save immutable review %s: %w", reviewRun.RunID, err)
	}

	if p.storage != nil {
		return p.storage.SaveReview(ctx, owner, repo, prNumber, commitSHA, content)
	}

	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.UploadReview(ctx, owner, repo, prNumber, commitSHA, content)
	}

	filename := gcs.ReviewFileName(owner, repo, prNumber, commitSHA)
	localPath := filepath.Join(p.reviewDir, filename)

	if err := os.MkdirAll(p.reviewDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create reviews directory: %w", err)
	}

	if err := os.WriteFile(localPath, content, 0644); err != nil {
		return "", fmt.Errorf("failed to write review file: %w", err)
	}

	log.Printf("[LOCAL] Saved review to: %s", localPath)
	return filename, nil
}

// saveImmutableReviewArtifact persists an object that is uniquely keyed by a
// run ID. Unlike canonical sidecars it never performs archive-on-overwrite.
func (p *Poller) saveImmutableReviewArtifact(ctx context.Context, filename, contentType string, content []byte) error {
	if p.storage != nil {
		// ReviewStorage's auxiliary-artifact method accepts arbitrary names and
		// content types, which keeps existing test/custom backends compatible.
		return p.storage.SaveReviewSidecar(ctx, filename, contentType, content)
	}
	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.UploadImmutableReviewArtifact(ctx, filename, contentType, content)
	}

	localPath := filepath.Join(p.reviewDir, filename)
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("create immutable review directory: %w", err)
	}
	if err := os.WriteFile(localPath, content, 0644); err != nil {
		return fmt.Errorf("write immutable review artifact: %w", err)
	}
	log.Printf("[LOCAL] Saved immutable review artifact to: %s", localPath)
	return nil
}

// saveReviewSidecar persists an auxiliary review artifact (currently the
// structured findings JSON) alongside the rendered HTML. Best-effort: HTML
// is the source of truth, so callers log and continue on failure rather
// than failing the whole review.
func (p *Poller) saveReviewSidecar(ctx context.Context, filename, contentType string, content []byte) error {
	if p.storage != nil {
		return p.storage.SaveReviewSidecar(ctx, filename, contentType, content)
	}
	if p.gcsClient != nil && p.gcsClient.BucketName() != "" {
		return p.gcsClient.UploadReviewSidecar(ctx, filename, contentType, content)
	}

	localPath := filepath.Join(p.reviewDir, filename)
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create reviews directory: %w", err)
	}
	if err := os.WriteFile(localPath, content, 0644); err != nil {
		return fmt.Errorf("failed to write sidecar file: %w", err)
	}
	log.Printf("[LOCAL] Saved sidecar to: %s", localPath)
	return nil
}

// writeSidecarBestEffort builds the structured findings payload and uploads it
// to the same backend the HTML lives in. Errors are logged but swallowed —
// the HTML review is the canonical artifact.
func (p *Poller) writeSidecarBestEffort(ctx context.Context, owner, repo string, prNumber int, commitSHA, htmlFilename string, rr *ReviewResult) {
	// GateAlerts arms Build's no-swallow assertion: a fired deterministic
	// alert that is no longer traceable in the merged findings logs an ERROR
	// naming the alert (nil/empty when no deterministic signal fired).
	pl := payload.Build(owner, repo, prNumber, commitSHA, rr.Comments, rr.Diff, rr.FileContents,
		payload.WithGateAlerts(rr.GateAlerts))
	if len(rr.BugMemory.Matched) > 0 || len(rr.BugMemory.Excluded) > 0 {
		pl.BugMemory = &payload.BugMemoryInfo{
			Version:         rr.BugMemory.Version,
			Matched:         rr.BugMemory.Matched,
			ExcludedLeakage: rr.BugMemory.Excluded,
		}
	}
	// Persist the required-check funnel alongside the findings: the sidecar
	// is the artifact benchmark attribution reads (Cloud Run logs rotate),
	// so the telemetry must survive there, mirroring bug_memory above.
	if rr.Checks.ChecksIssued > 0 {
		pl.RequiredChecks = &payload.RequiredChecksInfo{
			Issued:     rr.Checks.ChecksIssued,
			Answered:   rr.Checks.ChecksAnswered,
			Violated:   rr.Checks.ChecksViolated,
			EvidenceOK: rr.Checks.ChecksEvidenceOK,
		}
	}
	// Carry-forward telemetry (carried_in / carried_dropped): persisted for
	// the same reason as the funnels above. Nil (field omitted) when the
	// feature is off, keeping legacy sidecars byte-identical.
	pl.CarriedFindings = rr.Carried
	pl.ReviewRun = rr.ReviewRun
	body, err := json.Marshal(pl)
	if err != nil {
		log.Printf("[REVIEWER] WARN: marshal findings sidecar for %s/%s#%d: %v", owner, repo, prNumber, err)
		return
	}
	if rr.ReviewRun != nil && rr.ReviewRun.JSONPath != "" {
		if err := p.saveImmutableReviewArtifact(ctx, rr.ReviewRun.JSONPath, "application/json", body); err != nil {
			log.Printf("[REVIEWER] WARN: save immutable findings sidecar %s: %v", rr.ReviewRun.JSONPath, err)
		} else {
			log.Printf("[REVIEWER] Saved immutable findings sidecar: %s", rr.ReviewRun.JSONPath)
		}
	}
	sidecarName := gcs.ReviewJSONFileName(htmlFilename)
	if err := p.saveReviewSidecar(ctx, sidecarName, "application/json", body); err != nil {
		log.Printf("[REVIEWER] WARN: save findings sidecar %s: %v", sidecarName, err)
		return
	}
	log.Printf("[REVIEWER] Saved findings sidecar: %s (%d findings)", sidecarName, len(pl.Findings))
}

// backfillPRMetadata fills in missing title/author for existing PRs by fetching from GitHub
func (p *Poller) backfillPRMetadata(ctx context.Context) (int, error) {
	// Get PRs with missing metadata
	prs, err := p.db.GetPRsWithMissingMetadata()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs with missing metadata: %w", err)
	}

	if len(prs) == 0 {
		return 0, nil
	}

	updated := 0
	for _, pr := range prs {
		// Fetch PR details from GitHub
		title, author, err := p.ghClient.GetPRDetails(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[BACKFILL] Warning: Could not fetch PR details for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		// Update database with metadata
		if err := p.db.UpdatePRMetadata(pr.RepoOwner, pr.RepoName, pr.PRNumber, title, author); err != nil {
			log.Printf("[BACKFILL] ERROR: Failed to update metadata for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

		log.Printf("[BACKFILL] Updated metadata for PR %s/%s#%d: %s by %s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, title, author)
		updated++
	}

	return updated, nil
}

// backfillPRCreatedAt fills in missing created_at timestamps for existing PRs by fetching from GitHub
func (p *Poller) backfillPRCreatedAt(ctx context.Context) (int, error) {
	// Get PRs with missing created_at
	prs, err := p.db.GetPRsWithMissingCreatedAt()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs with missing created_at: %w", err)
	}

	if len(prs) == 0 {
		return 0, nil
	}

	updated := 0
	for _, pr := range prs {
		// Fetch PR details from GitHub
		ghPR, _, err := p.ghClient.GetPR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[BACKFILL] Warning: Could not fetch PR for created_at %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		createdAt := ghPR.GetCreatedAt().Time

		// Update database with created_at
		if err := p.db.UpdatePRCreatedAt(pr.RepoOwner, pr.RepoName, pr.PRNumber, createdAt); err != nil {
			log.Printf("[BACKFILL] ERROR: Failed to update created_at for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

		log.Printf("[BACKFILL] Updated created_at for PR %s/%s#%d: %s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, createdAt.Format("2006-01-02 15:04:05"))
		updated++
	}

	return updated, nil
}

// checkForOutdatedReviews detects PRs with new commits and resets them to pending
func (p *Poller) checkForOutdatedReviews(ctx context.Context) (int, error) {
	// Get all PRs from database
	allPRs, err := p.db.GetAllPRs()
	if err != nil {
		return 0, fmt.Errorf("failed to get PRs from database: %w", err)
	}

	outdated := 0
	checkedCount := 0
	for _, pr := range allPRs {
		// Check ALL PRs regardless of status - pending/error PRs can also get new commits
		// and we want to ensure we always process the latest commit, not stale ones
		checkedCount++

		// Fetch current HEAD SHA from GitHub
		currentSHA, err := p.ghClient.GetPRHeadSHA(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			log.Printf("[OUTDATED] Warning: Could not fetch current HEAD SHA for %s/%s#%d: %v",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
			continue
		}

		log.Printf("[OUTDATED] Checking %s/%s#%d: stored=%s current=%s status=%s",
			pr.RepoOwner, pr.RepoName, pr.PRNumber, pr.LastCommitSHA[:7], currentSHA[:7], pr.Status)

		// Compare commit SHAs
		if currentSHA != pr.LastCommitSHA {
			wasInFlight := isReviewInFlight(pr.Status)
			statusMsg := pr.Status
			if wasInFlight {
				statusMsg = pr.Status + " (cancelling)"
			}
			log.Printf("[OUTDATED] PR %s/%s#%d (%s) has new commits (old: %s, new: %s), resetting to pending",
				pr.RepoOwner, pr.RepoName, pr.PRNumber, statusMsg, pr.LastCommitSHA[:7], currentSHA[:7])

			// Delete old HTML file if it exists
			if pr.ReviewHTMLPath != "" {
				oldHTMLPath := filepath.Join(p.reviewDir, pr.ReviewHTMLPath)
				if err := os.Remove(oldHTMLPath); err != nil && !os.IsNotExist(err) {
					log.Printf("[OUTDATED] Warning: Failed to delete old HTML file %s: %v", oldHTMLPath, err)
				} else if err == nil {
					log.Printf("[OUTDATED] Deleted old HTML file: %s", pr.ReviewHTMLPath)
				}
			}

			// If the PR had an active Gemini or agent review, kill the process.
			if wasInFlight {
				if p.killReview(pr.RepoOwner, pr.RepoName, pr.PRNumber) {
					log.Printf("[OUTDATED] Killed active review process for %s/%s#%d",
						pr.RepoOwner, pr.RepoName, pr.PRNumber)
				}
			}

			// Reset PR to pending with new commit SHA and clear old review data
			if err := p.db.ResetPRToOutdated(pr.RepoOwner, pr.RepoName, pr.PRNumber, currentSHA); err != nil {
				log.Printf("[OUTDATED] ERROR: Failed to reset PR %s/%s#%d: %v",
					pr.RepoOwner, pr.RepoName, pr.PRNumber, err)
				continue
			}

			p.broadcastPRUpdate(pr.RepoOwner, pr.RepoName, pr.PRNumber)

			if wasInFlight {
				log.Printf("[OUTDATED] PR %d had new commit while in-flight (%s). Cancelled old review.", pr.PRNumber, pr.Status)
			} else if pr.Status == "completed" {
				log.Printf("[OUTDATED] PR %d has a new commit. Removed stale review.", pr.PRNumber)
			} else {
				log.Printf("[OUTDATED] Updated commit SHA for PR %d from %s to %s", pr.PRNumber, pr.LastCommitSHA[:7], currentSHA[:7])
			}

			outdated++
		}
	}

	if checkedCount > 0 {
		log.Printf("[OUTDATED] Checked %d PRs (out of %d total)", checkedCount, len(allPRs))
	}

	return outdated, nil
}

// syncUserPRViews creates user_pr_view records for PRs the user authored.
// Reviewer views are created separately in the reviewer groups phase.
// Uses the pre-built dbPRMap to avoid N+1 GetPR queries.
func (p *Poller) syncUserPRViews(allPRs []github.PullRequest, dbPRMap map[string]*db.PR, users []db.User) {
	batch := newViewBatch()
	for _, user := range users {
		for _, pr := range allPRs {
			if !strings.EqualFold(pr.Author, user.GitHubUsername) {
				continue
			}

			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			dbPR, exists := dbPRMap[key]
			if !exists {
				continue
			}

			batch.EnsureView(user.ID, dbPR.ID, true)
		}
	}
	if batch.Len() > 0 {
		if err := batch.Flush(p.db); err != nil {
			log.Printf("[POLL] Warning: Failed to batch-upsert author views: %v", err)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	startTime := time.Now()

	// Update last poll time for countdown display
	p.pollTimeMutex.Lock()
	p.lastPollTime = startTime
	p.pollTimeMutex.Unlock()

	log.Printf("[POLL] Starting poll at %s", startTime.Format("15:04:05"))

	// --- Phase 1: Fast DB self-healing ---

	// Reset any PRs stuck in "generating" for too long
	log.Printf("[POLL] Checking for stale PRs...")
	resetCount, err := p.db.ResetStaleGeneratingPRs(int(p.reviewProcessTimeout().Minutes()))
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset stale PRs: %v", err)
	} else if resetCount > 0 {
		log.Printf("[POLL] Reset %d stale PRs from 'generating' to 'pending'", resetCount)
		// Guard: if any actively-tracked reviews were reset (e.g. a long-running
		// immediate review), restore them to "generating" so the goroutine's
		// eventual DB write doesn't collide with a re-queued pending review.
		p.reviewsMutex.Lock()
		trackedKeys := make(map[string]bool, len(p.activeReviews))
		for k := range p.activeReviews {
			trackedKeys[k] = true
		}
		p.reviewsMutex.Unlock()
		if len(trackedKeys) > 0 {
			allPRsForCheck, checkErr := p.db.GetAllPRs()
			if checkErr == nil {
				for _, dbPR := range allPRsForCheck {
					key := prKey(dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber)
					if trackedKeys[key] && dbPR.Status == "pending" {
						log.Printf("[POLL] Restoring actively-tracked PR %s from 'pending' back to 'generating'", key)
						_ = p.db.SetPRGenerating(dbPR.RepoOwner, dbPR.RepoName, dbPR.PRNumber, dbPR.LastCommitSHA, dbPR.Title, dbPR.Author, dbPR.CreatedAt, dbPR.Draft)
					}
				}
			}
		}
	} else {
		log.Printf("[POLL] No stale PRs found")
	}

	// Reset PRs in error state after timeout (self-healing)
	log.Printf("[POLL] Checking for error PRs to retry...")
	errorResetCount, err := p.db.ResetErrorPRs(int(ErrorPRRetryTimeout.Minutes()), ErrorPRMaxAutoRetries)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to reset error PRs: %v", err)
	} else if errorResetCount > 0 {
		log.Printf("[POLL] SELF-HEALING: Reset %d error PRs to 'pending' (cap: %d auto-retries before requiring manual trigger)", errorResetCount, ErrorPRMaxAutoRetries)
	} else {
		log.Printf("[POLL] No error PRs to retry")
	}

	// --- Phase 2: Fast metadata refresh (critical path for ≤60s dashboard staleness) ---
	// GitHub search + batched GraphQL metadata runs before slow self-healing operations
	// to minimize time between poll start and fresh dashboard data.

	// Check auto-review setting to determine if we should process new PRs
	autoReviewEnabled, autoReviewErr := p.db.GetAutoReviewRequestedPRs()
	if autoReviewErr != nil {
		log.Printf("[POLL] Warning: Failed to get auto-review setting: %v", autoReviewErr)
		autoReviewEnabled = true // Default to enabled on error
	}

	// Build the canonical DB state map ONCE, early. Used for:
	// 1. Skipping known PRs during fetch (multi-user optimization)
	// 2. Pre-insertion check for new PRs
	// 3. All downstream metadata phases (review data, CI, reviewer groups)
	dbPRMap := make(map[string]*db.PR)
	dbPRsAll, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] WARNING: Failed to get database PRs: %v", err)
		dbPRsAll = nil
	} else {
		for i := range dbPRsAll {
			key := fmt.Sprintf("%s/%s/%d", dbPRsAll[i].RepoOwner, dbPRsAll[i].RepoName, dbPRsAll[i].PRNumber)
			dbPRMap[key] = &dbPRsAll[i]
		}
	}

	// Fetch PRs from GitHub.
	// If an org is configured, always use the org-wide search path so dev mode
	// sees the same universe of PRs as prod (including team-requested PRs).
	// In dev mode we still only sync user_pr_views for the dev user, so this
	// does not turn local development into a multi-user dashboard.
	var allPRs []github.PullRequest
	var prInfoMap map[string]*time.Time // PR key -> search updatedAt (for poll economy, multi-user only)
	if p.cfg.GitHubOrgName != "" {
		// Org-wide mode: fast search + skip known PRs + batch GraphQL for unknowns
		log.Printf("[POLL] Searching open PRs for org %s...", p.cfg.GitHubOrgName)
		prInfos, err := p.ghClient.SearchOpenPRs(ctx, p.cfg.GitHubOrgName)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to search org PRs: %v", err)
			prInfos = []github.PRInfo{}
		}
		log.Printf("[POLL] Found %d open PRs for org %s", len(prInfos), p.cfg.GitHubOrgName)

		// Build updatedAt map for poll economy change detection
		prInfoMap = make(map[string]*time.Time, len(prInfos))
		for _, info := range prInfos {
			key := fmt.Sprintf("%s/%s/%d", info.Owner, info.Repo, info.Number)
			prInfoMap[key] = info.UpdatedAt
		}

		// Partition into known (in DB) vs unknown PRs
		var unknownPRs []github.PRInfo
		for _, info := range prInfos {
			key := fmt.Sprintf("%s/%s/%d", info.Owner, info.Repo, info.Number)
			if dbPR, exists := dbPRMap[key]; exists {
				// Known PRs are hydrated from the DB, not GitHub, so a renamed
				// PR would keep its stale title forever. The search response
				// already carries the current title — write it through.
				if info.Title != "" && info.Title != dbPR.Title {
					if err := p.db.UpdatePRMetadata(info.Owner, info.Repo, info.Number, info.Title, dbPR.Author); err != nil {
						log.Printf("[POLL] Warning: Failed to update title for %s: %v", key, err)
					} else {
						log.Printf("[POLL] Updated title for %s: %q", key, info.Title)
						dbPR.Title = info.Title
						p.broadcastPRUpdate(info.Owner, info.Repo, info.Number)
					}
				}
				allPRs = append(allPRs, buildPRFromDB(*dbPR))
			} else {
				unknownPRs = append(unknownPRs, info)
			}
		}

		// Batch fetch details only for unknown PRs via GraphQL
		if len(unknownPRs) > 0 {
			detailsMap, err := p.ghClient.BatchGetPRDetails(ctx, unknownPRs)
			if err != nil {
				log.Printf("[POLL] ERROR: Failed to batch fetch PR details: %v", err)
			} else {
				for _, pr := range detailsMap {
					allPRs = append(allPRs, pr)
				}
			}
		}
		log.Printf("[POLL] %d from DB, %d fetched via GraphQL", len(prInfos)-len(unknownPRs), len(unknownPRs))
	} else {
		// Dev mode: user-specific fetches
		log.Printf("[POLL] Fetching PRs requesting review from GitHub...")
		reviewPRs, err := p.ghClient.GetPRsRequestingReview(ctx)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to fetch PRs requesting review: %v", err)
			reviewPRs = []github.PullRequest{}
		} else {
			log.Printf("[POLL] Found %d PRs requesting review", len(reviewPRs))
		}

		log.Printf("[POLL] Fetching my own open PRs from GitHub...")
		myPRs, err := p.ghClient.GetMyOpenPRs(ctx)
		if err != nil {
			log.Printf("[POLL] ERROR: Failed to fetch my open PRs: %v", err)
			myPRs = []github.PullRequest{}
		}
		log.Printf("[POLL] Found %d of my own open PRs", len(myPRs))

		allPRs = append(reviewPRs, myPRs...)
	}

	// Build the users list once for all per-user operations
	var allUsers []db.User
	if p.devUser != nil {
		allUsers = []db.User{*p.devUser}
	} else {
		allUsers, err = p.db.GetAllUsers()
		if err != nil {
			log.Printf("[POLL] WARNING: Failed to get all users: %v", err)
			allUsers = nil
		}
		if len(allUsers) > 0 {
			log.Printf("[POLL] Syncing PR views for %d users", len(allUsers))
		}
	}

	// Update cache for fast dashboard loading
	if p.cacheUpdateFunc != nil {
		p.cacheUpdateFunc(allPRs)
	}

	// Ensure all GitHub-fetched PRs exist in the database BEFORE syncUserPRViews.
	// Without this, new PRs discovered from GitHub won't have a prs row yet,
	// so syncUserPRViews can't create the user_pr_views record (FK constraint),
	// and the PR stays invisible on the dashboard until the NEXT poll cycle.
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		if dbPR, exists := dbPRMap[key]; exists {
			// Dev mode fetches full PRs from GitHub, so allPRs carries fresh
			// titles here — write a rename through, mirroring the org-mode
			// search-phase sync above (where this is a no-op: known PRs were
			// built from the already-synced dbPR).
			if pr.Title != "" && pr.Title != dbPR.Title {
				if err := p.db.UpdatePRMetadata(pr.Owner, pr.Repo, pr.Number, pr.Title, dbPR.Author); err != nil {
					log.Printf("[POLL] Warning: Failed to update title for %s: %v", key, err)
				} else {
					log.Printf("[POLL] Updated title for %s: %q", key, pr.Title)
					dbPR.Title = pr.Title
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				}
			}
		} else {
			newPR := &db.PR{
				RepoOwner:     pr.Owner,
				RepoName:      pr.Repo,
				PRNumber:      pr.Number,
				LastCommitSHA: pr.CommitSHA,
				Title:         pr.Title,
				Author:        pr.Author,
				CreatedAt:     pr.CreatedAt,
				Draft:         pr.Draft,
				Status:        "pending",
			}
			if err := p.db.UpsertPR(newPR); err != nil {
				log.Printf("[POLL] Warning: Failed to pre-insert PR %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			} else {
				log.Printf("[POLL] Pre-inserted new PR %s/%s#%d into database", pr.Owner, pr.Repo, pr.Number)
				p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				// Fetch the new PR's database ID and add to the map so downstream
				// phases (reviewer groups, CI, etc.) can find it immediately.
				if inserted, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number); err == nil && inserted != nil {
					dbPRMap[key] = inserted
				}
			}
		}
	}

	// Sync user_pr_views for all users (ensure all PRs have user-PR relationship records)
	p.syncUserPRViews(allPRs, dbPRMap, allUsers)

	// Add database PRs not in the GitHub fetch to allPRs so metadata still refreshes
	// for already-tracked PRs (for example, approval counts after a user reviews a PR).
	//
	// This happens AFTER syncUserPRViews so dev mode does not create spurious user_pr_views
	// for unrelated PRs from a shared database, while still keeping metadata current.
	ghKeys := make(map[string]bool, len(allPRs))
	for _, pr := range allPRs {
		ghKeys[fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)] = true
	}
	added := 0
	for key, dbPR := range dbPRMap {
		if !ghKeys[key] {
			allPRs = append(allPRs, buildPRFromDB(*dbPR))
			added++
		}
	}
	if added > 0 {
		log.Printf("[POLL] Added %d database PRs to metadata update list (total: %d PRs)", added, len(allPRs))
	}

	// Track which PRs changed across all metadata phases, then broadcast once at the end.
	// This ensures the frontend receives a consistent snapshot (approval count + via_teams + CI status
	// all current) rather than partial updates from individual phases.
	changedPRs := make(map[string]bool)

	// --- Poll economy: determine which PRs need metadata refresh ---
	// Compare GitHub's updated_at from search with stored values to skip unchanged PRs.
	p.pollCount++
	isFullRefresh := p.devUser != nil || prInfoMap == nil || p.pollCount%10 == 0
	if isFullRefresh && p.pollCount%10 == 0 {
		log.Printf("[POLL] Full refresh (cycle %d)", p.pollCount)
	}

	var metadataPRs []github.PullRequest
	if !isFullRefresh {
		changedPRKeys := make(map[string]bool)
		for key, searchUpdatedAt := range prInfoMap {
			dbPR, exists := dbPRMap[key]
			if !exists || dbPR.GitHubUpdatedAt == nil || searchUpdatedAt == nil ||
				!searchUpdatedAt.Truncate(time.Second).Equal(dbPR.GitHubUpdatedAt.Truncate(time.Second)) {
				changedPRKeys[key] = true
			}
		}
		// DB-only PRs (not in search) always included — unknown state
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if _, inSearch := prInfoMap[key]; !inSearch {
				changedPRKeys[key] = true
			}
		}
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if changedPRKeys[key] {
				metadataPRs = append(metadataPRs, pr)
			}
		}
		log.Printf("[POLL] %d/%d PRs changed, skipping %d unchanged", len(metadataPRs), len(allPRs), len(allPRs)-len(metadataPRs))
	} else {
		metadataPRs = allPRs
	}

	// --- Parallel GitHub API fetches ---
	// All three data sources (review data, reviewer groups, CI status) are independent.
	// Fetch them concurrently, then process results sequentially.
	var reviewDataMap map[string]*github.PRReviewData
	var reviewerGroupsMap map[string]*github.ReviewerGroupData
	var ciStatusMap map[string]*github.CIStatus

	if len(allPRs) > 0 {
		log.Printf("[POLL] Parallel-fetching review data + reviewer groups for %d changed PRs, CI status for all %d PRs...", len(metadataPRs), len(allPRs))

		var fetchWg sync.WaitGroup

		// Fetch review data (only changed PRs)
		if len(metadataPRs) > 0 {
			fetchWg.Add(1)
			go func() {
				defer fetchWg.Done()
				var err error
				reviewDataMap, err = p.ghClient.BatchGetPRReviewData(ctx, metadataPRs)
				if err != nil {
					log.Printf("[POLL] WARNING: Failed to batch fetch review data: %v", err)
					reviewDataMap = nil
				}
			}()
		}

		// Fetch reviewer groups (only changed PRs)
		if len(allUsers) > 0 && len(metadataPRs) > 0 {
			fetchWg.Add(1)
			go func() {
				defer fetchWg.Done()
				var err error
				reviewerGroupsMap, err = p.ghClient.BatchGetReviewerGroups(ctx, metadataPRs)
				if err != nil {
					log.Printf("[POLL] WARNING: Failed to batch fetch reviewer groups: %v", err)
					reviewerGroupsMap = nil
				}
			}()
		}

		// Fetch CI status for ALL PRs every cycle — CI check completions
		// don't update GitHub's PR updated_at, so we can't rely on change detection.
		// The query targets each PR's current head commit on GitHub, so the result
		// is correct even when our stored SHA is behind (new push / force-push).
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			ciPRs := make([]github.PRInfo, 0, len(allPRs))
			for _, pr := range allPRs {
				ciPRs = append(ciPRs, github.PRInfo{
					Owner:  pr.Owner,
					Repo:   pr.Repo,
					Number: pr.Number,
				})
			}
			var err error
			ciStatusMap, err = p.ghClient.BatchGetCIStatus(ctx, ciPRs)
			if err != nil {
				log.Printf("[POLL] WARNING: Failed to batch fetch CI status: %v", err)
				ciStatusMap = nil
			}
		}()

		fetchWg.Wait()
		log.Printf("[POLL] All parallel fetches complete")
	}

	// --- Process review data ---
	if reviewDataMap != nil {
		reviewViewBatch := newViewBatch()
		reviewPRBatch := newPRBatch()
		updateCount := 0
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			if reviewData, exists := reviewDataMap[key]; exists {
				existingPR, existsInDB := dbPRMap[key]
				if !existsInDB {
					continue
				}

				for _, user := range allUsers {
					userStatus := ""
					for login, status := range reviewData.UserReviews {
						if strings.EqualFold(login, user.GitHubUsername) {
							userStatus = status
							break
						}
					}
					if userStatus != "" {
						isAuthor := strings.EqualFold(existingPR.Author, user.GitHubUsername)
						reviewViewBatch.EnsureView(user.ID, existingPR.ID, isAuthor)
						reviewViewBatch.SetReviewStatus(user.ID, existingPR.ID, userStatus)
					}
				}

				approvalChanged := existingPR.ApprovalCount != reviewData.ApprovalCount
				reviewStatusChanged := existingPR.MyReviewStatus != reviewData.MyReviewStatus
				draftChanged := existingPR.Draft != pr.Draft

				if !approvalChanged && !reviewStatusChanged && !draftChanged {
					continue
				}

				existingPR.ApprovalCount = reviewData.ApprovalCount
				existingPR.MyReviewStatus = reviewData.MyReviewStatus
				existingPR.Draft = pr.Draft
				if pr.CreatedAt != nil {
					existingPR.CreatedAt = pr.CreatedAt
				}

				reviewPRBatch.Upsert(existingPR)
				changedPRs[key] = true
				updateCount++
			}
		}
		if err := reviewViewBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert review user views: %v", err)
		}
		if err := reviewPRBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert review PR data: %v", err)
		}
		log.Printf("[POLL] Updated review data for %d PRs (only those with changes)", updateCount)
	}

	// --- Process reviewer groups ---
	if reviewerGroupsMap != nil && len(allUsers) > 0 {
		orgName := p.cfg.GitHubOrgName
		if orgName == "" {
			for _, data := range reviewerGroupsMap {
				if data.OrgName != "" {
					orgName = data.OrgName
					break
				}
			}
		}

		teamMembersMap := map[string][]string{}
		failedSlugs := map[string]bool{}
		if orgName != "" {
			uniqueSlugs := collectUniqueSlugs(reviewerGroupsMap)
			for slug := range uniqueSlugs {
				members, err := p.getTeamMembers(ctx, orgName, slug)
				if err != nil {
					log.Printf("[POLL] Warning: Failed to get members for team %s: %v", slug, err)
					failedSlugs[slug] = true
					continue
				}
				teamMembersMap[slug] = members
			}
		}

		// Tracks (user, PR) pairs verified this cycle as still having a reason
		// to see the PR via teams. The reconciliation pass below prunes rows
		// with fresh reviewer-group data that are NOT in this set.
		entitled := make(map[userPRViewKey]bool)

		groupsViewBatch := newViewBatch()
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			groupData, exists := reviewerGroupsMap[key]
			if !exists {
				continue
			}
			existingPR, existsInDB := dbPRMap[key]
			if !existsInDB {
				continue
			}

			var userReviews map[string]string
			if reviewDataMap != nil {
				if reviewData, exists := reviewDataMap[key]; exists {
					userReviews = reviewData.UserReviews
				}
			}

			for _, user := range allUsers {
				isOnTeam := false
				for _, team := range groupData.ReviewerGroups {
					slug, hasSlug := groupData.TeamSlugs[team]
					if !hasSlug {
						slug = slugifyTeamName(team)
					}
					for _, member := range teamMembersMap[slug] {
						if strings.EqualFold(member, user.GitHubUsername) {
							isOnTeam = true
							break
						}
					}
					if isOnTeam {
						break
					}
				}
				isPersonallyRequested := false
				for _, requestedUser := range groupData.RequestedUsers {
					if strings.EqualFold(requestedUser, user.GitHubUsername) {
						isPersonallyRequested = true
						break
					}
				}

				if !isOnTeam && !isPersonallyRequested {
					continue
				}
				entitled[userPRViewKey{UserID: user.ID, PRID: existingPR.ID}] = true

				teamStatuses := resolveTeamReviewStatuses(
					groupData.ReviewerGroups,
					nil,
					teamMembersMap,
					groupData.TeamSlugs,
					userReviews,
					user.GitHubUsername,
				)
				var viaTeams []string
				for _, team := range groupData.ReviewerGroups {
					if status, ok := teamStatuses[team]; ok {
						viaTeams = append(viaTeams, team+":"+status)
					}
				}

				if isPersonallyRequested {
					viaTeams = append(viaTeams, "__PERSONAL__")
				}

				if shouldUpdateViaTeams(viaTeams) {
					isAuthor := strings.EqualFold(pr.Author, user.GitHubUsername)
					groupsViewBatch.EnsureView(user.ID, existingPR.ID, isAuthor)
					groupsViewBatch.SetViaTeams(user.ID, existingPR.ID, viaTeams)
					changedPRs[key] = true
				}
			}
		}
		if err := groupsViewBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert reviewer group views: %v", err)
		}
		log.Printf("[POLL] Updated reviewer groups for %d PRs", len(reviewerGroupsMap))

		p.pruneStaleViaTeams(ctx, orgName, allPRs, dbPRMap, reviewerGroupsMap, entitled, failedSlugs, allUsers, changedPRs)
	}

	// --- Process CI status ---
	if ciStatusMap != nil {
		ciPRBatch := newPRBatch()
		updateCount := 0
		for _, pr := range allPRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
			ciStatus, hasCIData := ciStatusMap[key]
			if !hasCIData {
				continue
			}
			existingPR, existsInDB := dbPRMap[key]
			if !existsInDB {
				continue
			}

			failedChecksJSON := "[]"
			if len(ciStatus.FailedChecks) > 0 {
				if jsonBytes, err := json.Marshal(ciStatus.FailedChecks); err == nil {
					failedChecksJSON = string(jsonBytes)
				}
			}

			if existingPR.CIState == ciStatus.State && existingPR.CIFailedChecks == failedChecksJSON {
				continue
			}

			existingPR.CIState = ciStatus.State
			existingPR.CIFailedChecks = failedChecksJSON
			ciPRBatch.Upsert(existingPR)
			changedPRs[key] = true
			updateCount++
		}
		if err := ciPRBatch.Flush(p.db); err != nil {
			log.Printf("[POLL] ERROR: Failed to batch-upsert CI status: %v", err)
		}
		log.Printf("[POLL] Updated CI status for %d PRs (only those with changes)", updateCount)
	}

	// Broadcast all changed PRs once, after all metadata phases are complete.
	// This ensures the frontend receives a consistent snapshot (approval count +
	// via_teams + CI status all current) rather than partial updates.
	for _, pr := range allPRs {
		key := fmt.Sprintf("%s/%s/%d", pr.Owner, pr.Repo, pr.Number)
		if changedPRs[key] {
			p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
		}
	}

	// --- Poll economy: persist GitHub updatedAt for change detection next cycle ---
	if len(prInfoMap) > 0 {
		updated := 0
		for key, ts := range prInfoMap {
			if ts == nil {
				continue
			}
			dbPR, exists := dbPRMap[key]
			if !exists || dbPR.GitHubUpdatedAt == nil || !ts.Equal(*dbPR.GitHubUpdatedAt) {
				parts := strings.SplitN(key, "/", 3)
				if len(parts) == 3 {
					num, _ := strconv.Atoi(parts[2])
					if err := p.db.UpdatePRGitHubUpdatedAt(parts[0], parts[1], num, *ts); err != nil {
						log.Printf("[POLL] Warning: Failed to update GitHubUpdatedAt for %s: %v", key, err)
					} else {
						updated++
					}
				}
			}
		}
		if updated > 0 {
			log.Printf("[POLL] Updated GitHubUpdatedAt for %d PRs", updated)
		}
	}

	// --- Phase 3: Slow self-healing (runs after metadata is already fresh) ---

	// Batch cleanup + outdated detection (single GraphQL pass replaces 2N REST calls)
	log.Printf("[POLL] Batch-checking PR state (cleanup + outdated detection)...")
	removedCount, outdatedCount, err := p.cleanupAndDetectOutdated(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed batch PR state check: %v", err)
	} else {
		if removedCount > 0 {
			log.Printf("[POLL] CLEANUP: Removed %d closed PRs from system", removedCount)
		} else {
			log.Printf("[POLL] No closed PRs to remove")
		}
		if outdatedCount > 0 {
			log.Printf("[POLL] OUTDATED: Reset %d PRs with new commits to pending", outdatedCount)
		} else {
			log.Printf("[POLL] No outdated reviews found")
		}
	}

	// Backfill missing PR metadata (self-healing)
	log.Printf("[POLL] Checking for PRs with missing metadata...")
	backfilledCount, err := p.backfillPRMetadata(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to backfill metadata: %v", err)
	} else if backfilledCount > 0 {
		log.Printf("[POLL] BACKFILL: Updated metadata for %d PRs", backfilledCount)
	} else {
		log.Printf("[POLL] No PRs need metadata backfill")
	}

	// Backfill missing created_at timestamps (self-healing)
	log.Printf("[POLL] Checking for PRs with missing created_at...")
	timestampBackfilledCount, err := p.backfillPRCreatedAt(ctx)
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to backfill created_at: %v", err)
	} else if timestampBackfilledCount > 0 {
		log.Printf("[POLL] BACKFILL: Updated created_at for %d PRs", timestampBackfilledCount)
	} else {
		log.Printf("[POLL] No PRs need created_at backfill")
	}

	// --- Phase 4: Review generation ---

	// These are used to split PRs into "mine" (skip self-review) vs "to review"
	var myPRs []github.PullRequest
	var reviewPRs []github.PullRequest

	// Re-query pending/generating PRs from database. We need fresh status data here
	// because Phase 3 (outdated review detection) may have reset PRs to "pending".
	log.Printf("[POLL] Checking database for pending PRs...")

	freshDBPRs, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[POLL] ERROR: Failed to get PRs from database: %v", err)
	} else {
		pendingCount := 0
		skippedCount := 0
		for _, dbPR := range freshDBPRs {
			if dbPR.Status == "pending" || dbPR.Status == "generating" {
				// Skip pending PRs if auto-review is disabled
				// BUT: Always process 'generating' PRs (manual triggers)
				if dbPR.Status == "pending" && !autoReviewEnabled {
					skippedCount++
					continue
				}

				ghPR := buildPRFromDB(dbPR)

				// In single-user mode, determine if the PR is "mine" to avoid self-review
				isMine := p.cfg.GitHubUsername != "" && strings.EqualFold(dbPR.Author, p.cfg.GitHubUsername)

				if isMine {
					myPRs = append(myPRs, ghPR)
				} else {
					reviewPRs = append(reviewPRs, ghPR)
				}
				pendingCount++
			}
		}
		if pendingCount > 0 {
			log.Printf("[POLL] Found %d pending PRs in database to process", pendingCount)
		}
		if skippedCount > 0 {
			log.Printf("[POLL] Skipped %d pending PRs (auto-review disabled)", skippedCount)
		}
	}

	// Combine myPRs and reviewPRs for processing - both get AI reviews
	allPRsToProcess := append(reviewPRs, myPRs...)

	// Group all PRs by repository for batch processing
	prsByRepo := make(map[string][]github.PullRequest)
	for _, pr := range allPRsToProcess {
		repoKey := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[repoKey] = append(prsByRepo[repoKey], pr)
	}

	// Process PRs in smaller batches
	log.Printf("[POLL] Processing %d repositories for PRs", len(prsByRepo))
	for repoKey, repoPRs := range prsByRepo {
		log.Printf("[POLL] Processing PRs for repository %s with %d PRs", repoKey, len(repoPRs))
		// Split into smaller batches of 5 PRs to avoid timeout
		p.processInBatches(ctx, repoPRs, 5, autoReviewEnabled)
	}

	duration := time.Since(startTime)
	log.Printf("[POLL] Poll completed in %v", duration)
}

func (p *Poller) processInBatches(ctx context.Context, prs []github.PullRequest, batchSize int, autoReviewEnabled bool) {
	for i := 0; i < len(prs); i += batchSize {
		end := i + batchSize
		if end > len(prs) {
			end = len(prs)
		}
		batch := prs[i:end]
		log.Printf("[POLL] Processing batch %d-%d of %d PRs", i+1, end, len(prs))
		if err := p.processPRBatch(ctx, batch, autoReviewEnabled); err != nil {
			log.Printf("[POLL] ERROR: Batch %d-%d failed: %v", i+1, end, err)
		} else {
			log.Printf("[POLL] Successfully processed batch %d-%d", i+1, end)
		}
	}
}

func (p *Poller) processPRBatch(ctx context.Context, prs []github.PullRequest, autoReviewEnabled bool) error {
	if len(prs) == 0 {
		return nil
	}

	log.Printf("[BATCH] Processing %d PRs", len(prs))

	// Auto-review setting is now passed from poll() - no duplicate query needed

	var prsToReview []github.PullRequest

	for _, pr := range prs {
		existingPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			log.Printf("[BATCH] WARNING: Could not get existing PR for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			// Continue to try and process if we can
		}

		isNew := existingPR == nil
		if isNew {
			existingPR = &db.PR{Status: "pending"}
		}

		// Determine if we should generate a review
		shouldGenerate := shouldReview(pr, existingPR, p.isTracked(pr.Owner, pr.Repo, pr.Number), autoReviewEnabled)

		if shouldGenerate && existingPR.Status == "generating" && !p.isTracked(pr.Owner, pr.Repo, pr.Number) {
			log.Printf("[BATCH] Manual trigger detected for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		}

		// Always update metadata first
		existingPR.RepoOwner = pr.Owner
		existingPR.RepoName = pr.Repo
		existingPR.PRNumber = pr.Number
		existingPR.LastCommitSHA = pr.CommitSHA
		existingPR.Title = pr.Title
		existingPR.Author = pr.Author
		existingPR.CreatedAt = pr.CreatedAt
		existingPR.Draft = pr.Draft

		if err := p.db.UpsertPR(existingPR); err != nil {
			log.Printf("[BATCH] ERROR: Failed to upsert PR metadata for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		} else {
			if isNew {
				if p.EventFunc != nil {
					p.EventFunc("pr_created", map[string]interface{}{
						"owner":  pr.Owner,
						"repo":   pr.Repo,
						"number": pr.Number,
					})
				}
			} else {
				// Broadcast update to ensure metadata (title, author, etc) is fresh in UI
				p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
			}
		}

		if shouldGenerate {
			// Add to queue
			prsToReview = append(prsToReview, pr)
		}
	}

	if len(prsToReview) == 0 {
		return nil
	}

	// Mark all PRs as generating (if not already)
	log.Printf("[BATCH] Queuing %d PRs for generation", len(prsToReview))

	owner := prsToReview[0].Owner
	repo := prsToReview[0].Repo
	prNumbers := getPRNumbers(prsToReview)
	log.Printf("[BATCH] Starting reviewer batch for %s/%s PRs: %v", owner, repo, prNumbers)

	startTime := time.Now()
	// Generate reviews using native reviewer (batch). force=false on the
	// auto-poll path: respect the per-commit cache so we don't burn cycles
	// regenerating reviews for already-reviewed commits.
	batchErr := p.generateReviewsBatch(ctx, prsToReview, false)
	duration := time.Since(startTime)

	if batchErr != nil {
		log.Printf("[BATCH] ERROR: reviewer batch failed after %v: %v", duration, batchErr)
	} else {
		log.Printf("[BATCH] reviewer batch completed in %v", duration)
	}

	return nil
}

func getPRNumbers(prs []github.PullRequest) []int {
	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums
}

func (p *Poller) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	if err := p.db.UpdatePRStatus(owner, repo, prNumber, status); err != nil {
		return err
	}
	p.broadcastPRUpdate(owner, repo, prNumber)
	return nil
}

// generateReviewsBatch runs Gemini (and optionally agent) review generation
// for one or more PRs. When force is true, the existing-review cache check
// is skipped — the caller wants a fresh review even if one already exists
// for the same commit. The auto-poll path uses force=false to avoid
// re-reviewing already-reviewed commits; the manual API trigger uses
// force=true so a click always regenerates.
func (p *Poller) generateReviewsBatch(ctx context.Context, prs []github.PullRequest, force bool) error {
	if len(prs) == 0 {
		return nil
	}
	jobs := make([]ReviewJob, 0, len(prs))
	var dispatchErrors []error
	for _, pr := range prs {
		job, err := p.defaultReviewJob(pr, force, "poller")
		if err != nil {
			configErr := fmt.Errorf("invalid deployment review defaults for %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
			log.Printf("[REVIEWER] ERROR: %v", configErr)
			if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, configErr.Error()); setErr != nil {
				dispatchErrors = append(dispatchErrors, fmt.Errorf("%w (also failed to persist PR error: %v)", configErr, setErr))
			} else {
				dispatchErrors = append(dispatchErrors, configErr)
			}
			p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
			continue
		}
		jobs = append(jobs, job)
	}
	if err := p.generateReviewJobs(ctx, jobs); err != nil {
		dispatchErrors = append(dispatchErrors, err)
	}
	return errors.Join(dispatchErrors...)
}

func (p *Poller) generateReviewJobs(ctx context.Context, jobs []ReviewJob) error {
	if len(jobs) == 0 {
		return nil
	}
	for _, job := range jobs {
		if err := job.Validate(); err != nil {
			return err
		}
	}

	// Establish ownership before provider initialization, cache reads, or PR
	// state writes. This closes the entire poll-vs-immediate race window, not
	// just the portion inside each worker goroutine.
	ownedJobs := make([]ReviewJob, 0, len(jobs))
	jobContexts := make(map[string]context.Context, len(jobs))
	for _, job := range jobs {
		jobCtx, owned := p.trackOrAdoptReviewJob(ctx, job)
		if !owned {
			log.Printf("[REVIEWER] PR %d is tracked by another run; rejecting %s", job.PR.Number, job.RunID)
			p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "pr_already_claimed", "dispatch", ErrReviewAlreadyTracked)
			continue
		}
		ownedJobs = append(ownedJobs, job)
		jobContexts[job.RunID] = jobCtx
	}
	if len(ownedJobs) == 0 {
		return nil
	}
	jobs = ownedJobs

	// If using mock generator (for testing), skip LLM client initialization
	var reviewSvc *service.Service
	if p.reviewGenerator == nil {
		// Initialize reviewer clients
		smartLlmClient := llm.NewClient(llm.ProviderGemini, p.cfg.GeminiAPIKey, false, false)
		fastLlmClient := llm.NewClient(llm.ProviderGemini, p.cfg.GeminiAPIKey, true, false)

		// Validate API Key once
		if err := smartLlmClient.ValidateAPIKey(); err != nil {
			wrappedErr := fmt.Errorf("Gemini API key validation failed: %w", err)
			for _, job := range jobs {
				p.rejectQueuedReviewJob(job, db.ReviewRunStatusFailed, "gemini_validation_failed", "first_pass", err)
				if jobContexts[job.RunID].Err() == nil {
					if setErr := p.db.SetPRError(job.PR.Owner, job.PR.Repo, job.PR.Number, wrappedErr.Error()); setErr != nil {
						log.Printf("[REVIEWER] WARNING: failed to persist API validation error for PR %d: %v", job.PR.Number, setErr)
					}
					p.broadcastPRUpdate(job.PR.Owner, job.PR.Repo, job.PR.Number)
				}
				p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
			}
			return wrappedErr
		}

		reviewSvc = service.NewService(p.ghClientConcrete, smartLlmClient, fastLlmClient)
	}

	// Batch tokens are held across the agent stage, so a batch limit below
	// AgentMaxConcurrent would silently cap agent concurrency under the
	// configured value. Agent slots are intentionally reserved for the full
	// first-pass + agent pipeline below: a lease/deadline cannot safely pause
	// during an agent-slot wait. This trades some first-pass pipelining for the
	// invariant that queue time never consumes a caller's execution budget.
	concurrencyLimit := 5
	if p.cfg.AgentMaxConcurrent > concurrencyLimit {
		concurrencyLimit = p.cfg.AgentMaxConcurrent
	}
	sem := make(chan struct{}, concurrencyLimit)
	var wg sync.WaitGroup

	// Process each PR concurrently
	for _, job := range jobs {
		queuedCtx := jobContexts[job.RunID]
		select {
		case sem <- struct{}{}: // Acquire token
			wg.Add(1)
		case <-queuedCtx.Done():
			p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "cancelled", "dispatch", queuedCtx.Err())
			p.untrackReviewRun(job.PR.Owner, job.PR.Repo, job.PR.Number, job.RunID)
			continue
		}

		go func(job ReviewJob) {
			defer wg.Done()
			defer func() { <-sem }() // Release token
			pr := job.PR
			queuedCtx := jobContexts[job.RunID]

			log.Printf("[REVIEWER] Processing PR: %s/%s#%d (commit: %s)", pr.Owner, pr.Repo, pr.Number, pr.CommitSHA[:7])

			// Check if review already exists (in GCS if configured, otherwise locally).
			// Skipped when force=true so the manual trigger always regenerates.
			exists := false
			var existsErr error
			if !job.Force {
				exists, existsErr = p.reviewExists(queuedCtx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
			} else {
				log.Printf("[REVIEWER] PR %d: force=true, skipping existing-review cache check", pr.Number)
			}
			if existsErr != nil {
				log.Printf("[REVIEWER] Warning: Failed to check for existing review: %v", existsErr)
				// Continue anyway - will regenerate if needed
			} else if exists {
				log.Printf("[REVIEWER] Review already exists for PR %d commit %s, skipping generation", pr.Number, pr.CommitSHA[:7])
				// Update database to point to existing review, preserving importance counts
				filename := gcs.ReviewFileName(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA)
				// Get existing importance counts + verdict from database
				existingPR, _ := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
				criticalCount, mediumCount, lowCount, verdict, modelFallback := 0, 0, 0, "", false
				reviewRunID, reviewRunJSON := "", ""
				if existingPR != nil {
					criticalCount = existingPR.CriticalCount
					mediumCount = existingPR.MediumCount
					lowCount = existingPR.LowCount
					verdict = existingPR.ReviewVerdict
					modelFallback = existingPR.ModelFallback
					reviewRunID = existingPR.ReviewRunID
					reviewRunJSON = existingPR.ReviewRunJSON
				}
				if err := p.db.MarkPRCompleted(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, criticalCount, mediumCount, lowCount, verdict, modelFallback, reviewRunID, reviewRunJSON); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for existing review: %v", err)
				} else {
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				}
				p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "review_cached", "dispatch", fmt.Errorf("review artifact already exists"))
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}

			agentSlotReserved := false
			if job.Config.Effective.Agent.Enabled && p.agentSlots != nil {
				// Reserve scarce agent capacity while the job still has its
				// un-deadlined queued context. Neither the configured execution
				// budget nor the worker lease burns down waiting for this slot.
				select {
				case p.agentSlots <- struct{}{}:
					agentSlotReserved = true
					defer func() { <-p.agentSlots }()
				case <-queuedCtx.Done():
					p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "cancelled", "dispatch", queuedCtx.Err())
					p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
					return
				}
			}
			prCtx, started := p.startTrackedReviewJob(job)
			if !started {
				log.Printf("[REVIEWER] PR %d lost queued ownership before execution; rejecting %s", pr.Number, job.RunID)
				if queuedCtx.Err() != nil {
					p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "cancelled", "dispatch", queuedCtx.Err())
				} else {
					p.rejectQueuedReviewJob(job, db.ReviewRunStatusCancelled, "pr_already_claimed", "dispatch", ErrReviewAlreadyTracked)
				}
				return
			}

			// Set status to generating
			if err := p.db.SetPRGenerating(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, pr.Title, pr.Author, pr.CreatedAt, pr.Draft); err != nil {
				log.Printf("[BATCH] ERROR: Failed to set generating status for %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
				p.rejectQueuedReviewJob(job, db.ReviewRunStatusFailed, "pr_state_failed", "dispatch", err)
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}
			p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)

			execution, beginErr := p.beginReviewExecution(job)
			if beginErr != nil {
				log.Printf("[REVIEWER] ERROR: Could not begin review run %s: %v", job.RunID, beginErr)
				rejectedQueued := p.rejectQueuedReviewJob(job, db.ReviewRunStatusFailed, "claim_failed", "dispatch", beginErr)
				// A not-claimed run may be executing on another instance; never
				// overwrite its PR projection. Other failures belong to this worker.
				if rejectedQueued || !errors.Is(beginErr, ErrReviewRunNotClaimed) {
					if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, beginErr.Error()); setErr != nil {
						log.Printf("[REVIEWER] WARNING: failed to persist begin error for PR %d: %v", pr.Number, setErr)
					}
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
				}
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}
			execution.AgentSlotReserved = agentSlotReserved
			execStart := execution.AttemptStartedAt
			nRequests := job.Config.Effective.FirstPass.Samples

			// Generate review using mock interface (testing) or real service
			var err error
			var reviewResult *ReviewResult
			if p.reviewGenerator != nil {
				// Use mock generator for testing
				genCfg := ReviewGeneratorConfig{
					RunID:        job.RunID,
					Config:       job.Config,
					Token:        "",
					Owner:        pr.Owner,
					RepoName:     pr.Repo,
					PRNumber:     pr.Number,
					CommitSHA:    pr.CommitSHA,
					WithComments: false,
					Verbose:      false,
					Fast:         false,
					NRequests:    nRequests,
				}
				reviewResult, err = p.reviewGenerator.GenerateReview(prCtx, genCfg)
			} else {
				// Use real reviewer service
				reviewCfg := service.PerformReviewConfig{
					Token:        p.cfg.GitHubToken,
					Owner:        pr.Owner,
					RepoName:     pr.Repo,
					PRNumber:     pr.Number,
					WithComments: false,
					Verbose:      false,
					Fast:         false,
					NRequests:    nRequests,
				}

				firstPassStarted := time.Now().UTC()
				result, svcErr := reviewSvc.PerformReviewWithContext(prCtx, reviewCfg)
				firstPassCompleted := time.Now().UTC()
				if svcErr != nil {
					p.recordGeminiAttempts(execution, firstPassStarted, firstPassCompleted, "failed", svcErr.Error())
					err = svcErr
				} else if job.Config.Effective.Agent.Enabled {
					p.recordGeminiAttempts(execution, firstPassStarted, firstPassCompleted, "completed", "")
					reviewResult, err = p.runAgentStage(prCtx, execution, result)
				} else {
					p.recordGeminiAttempts(execution, firstPassStarted, firstPassCompleted, "completed", "")
					// Legacy HTML report path.
					htmlContent := service.GenerateHTMLReportContent(result, pr.Number, pr.Owner, pr.Repo, pr.CommitSHA, llm.ProModelName())
					if htmlContent == nil {
						err = fmt.Errorf("failed to generate HTML content")
					} else {
						reviewResult = &ReviewResult{
							HTMLContent:   htmlContent,
							CriticalCount: result.CriticalCount,
							MediumCount:   result.MediumCount,
							LowCount:      result.LowCount,
							Comments:      result.Comments,
							Diff:          result.Diff,
							FileContents:  result.FileContents,
						}
					}
				}
			}
			execCompleted := time.Now().UTC()
			execDuration := execCompleted.Sub(execStart)

			if err != nil {
				log.Printf("[REVIEWER] ERROR: Review failed for PR %d after %v: %v", pr.Number, execDuration, err)

				// External cancellation leaves the PR projection to its caller;
				// an organic execution-budget timeout is projected as an error by
				// finishInterruptedReviewExecution so retries remain bounded.
				if prCtx.Err() != nil {
					log.Printf("[REVIEWER] PR %d review was interrupted (ctx=%v)", pr.Number, prCtx.Err())
					p.finishInterruptedReviewExecution(execution, prCtx, "execution", err)
					p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
					return
				}

				// Check if outdated
				currentPR, dbErr := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
				if dbErr == nil && currentPR != nil && currentPR.Status == "pending" && currentPR.LastCommitSHA != pr.CommitSHA {
					log.Printf("[REVIEWER] Review for PR %d was cancelled because it became outdated.", pr.Number)
					status := db.ReviewRunStatusCancelled
					terminalCode := "commit_outdated"
					failureStage := "publication"
					errorSummary := "PR head changed while the review was running"
					p.finishReviewExecution(execution, db.ReviewRunPatch{Status: &status, TerminalCode: &terminalCode, FailureStage: &failureStage, ErrorSummary: &errorSummary})
				} else {
					status := db.ReviewRunStatusFailed
					terminalCode := "review_failed"
					failureStage := "generation"
					errorSummary := err.Error()
					if p.finishReviewExecution(execution, db.ReviewRunPatch{Status: &status, TerminalCode: &terminalCode, FailureStage: &failureStage, ErrorSummary: &errorSummary}) {
						if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, err.Error()); setErr != nil {
							log.Printf("[REVIEWER] WARNING: failed to persist error status for PR %d: %v", pr.Number, setErr)
						}
						p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
					} else {
						log.Printf("[REVIEWER] STALE WORKER: run %s no longer owns its lease; skipping generation error projection", job.RunID)
					}
				}
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}

			log.Printf("[REVIEWER] Review completed successfully for PR %d in %v", pr.Number, execDuration)
			if reviewResult.ReviewRun == nil {
				reviewResult.ReviewRun = &payload.ReviewRunInfo{}
			}
			if p.reviewGenerator == nil && len(reviewResult.ReviewRun.Models) == 0 {
				reviewResult.ReviewRun.Models = geminiModelUses()
			}
			if !p.renewReviewExecutionForPublication(execution) {
				log.Printf("[REVIEWER] STALE WORKER: run %s no longer owns its lease; skipping artifact publication", job.RunID)
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}
			runInfo := p.reviewRunInfo(execution, execCompleted)
			runInfo.Models = reviewResult.ReviewRun.Models
			reviewResult.ReviewRun = runInfo
			log.Printf("[REVIEWER] PR %d review run: %s", pr.Number, job.RunID)

			// Save review (to GCS if configured, otherwise locally)
			filename, err := p.saveReview(prCtx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, reviewResult.ReviewRun, reviewResult.HTMLContent)
			if err != nil {
				log.Printf("[REVIEWER] ERROR: Failed to save review for PR %d: %v", pr.Number, err)
				if prCtx.Err() != nil {
					log.Printf("[REVIEWER] PR %d artifact save was interrupted (ctx=%v)", pr.Number, prCtx.Err())
					p.finishInterruptedReviewExecution(execution, prCtx, "artifact_save", err)
				} else {
					status := db.ReviewRunStatusFailed
					terminalCode := "artifact_save_failed"
					failureStage := "artifact_save"
					errorSummary := err.Error()
					finished := p.finishReviewExecution(execution, db.ReviewRunPatch{Status: &status, TerminalCode: &terminalCode, FailureStage: &failureStage, ErrorSummary: &errorSummary})
					if finished {
						if setErr := p.db.SetPRError(pr.Owner, pr.Repo, pr.Number, err.Error()); setErr != nil {
							log.Printf("[REVIEWER] WARNING: failed to persist error status for PR %d: %v", pr.Number, setErr)
						}
						p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
					} else {
						log.Printf("[REVIEWER] STALE WORKER: run %s no longer owns its lease; skipping artifact-save error projection", job.RunID)
					}
				}
				p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
				return
			}

			log.Printf("[REVIEWER] Saved review: %s", filename)

			// Best-effort: write the structured findings sidecar so /api/review
			// can serve a parseable payload without scraping HTML. Failure here
			// is logged but does NOT abort the review — HTML remains the source
			// of truth and the API endpoint falls back gracefully.
			if len(reviewResult.Comments) > 0 || reviewResult.Diff != "" {
				p.writeSidecarBestEffort(prCtx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, reviewResult)
			}

			// Verify commit SHA matches (hasn't changed during generation)
			currentPR, err := p.db.GetPR(pr.Owner, pr.Repo, pr.Number)
			if err != nil {
				log.Printf("[REVIEWER] ERROR: Failed to fetch PR from DB: %v", err)
				p.finishCompletedReviewExecution(execution, reviewResult, "artifact_saved")
			} else if currentPR != nil && currentPR.LastCommitSHA != pr.CommitSHA {
				log.Printf("[REVIEWER] STALE REVIEW: PR %d commit changed during generation, but keeping in GCS for history", pr.Number)
				// Don't update DB - the next poll will generate a new review for the new commit
				p.finishCompletedReviewExecution(execution, reviewResult, "superseded")
			} else {
				// Parse the overall verdict from the SUMMARY entry; ""
				// (unknown) when the mock generator supplies no comments or
				// the SUMMARY has no recognizable verdict phrasing.
				verdict := service.VerdictFromComments(reviewResult.Comments)
				reviewRunJSON, marshalErr := json.Marshal(reviewResult.ReviewRun)
				if marshalErr != nil {
					log.Printf("[REVIEWER] WARNING: failed to marshal review-run metadata for PR %d: %v", pr.Number, marshalErr)
					reviewRunJSON = nil
				}
				if !p.finishCompletedReviewExecution(execution, reviewResult, "artifact_saved") {
					log.Printf("[REVIEWER] STALE WORKER: run %s lost its lease before projection; skipping latest-review update", job.RunID)
					p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
					return
				}
				if err := p.db.MarkPRCompleted(pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, filename, reviewResult.CriticalCount, reviewResult.MediumCount, reviewResult.LowCount, verdict, reviewResult.ModelFallback, reviewResult.ReviewRun.RunID, string(reviewRunJSON)); err != nil {
					log.Printf("[REVIEWER] ERROR: Failed to update DB for PR %d: %v", pr.Number, err)
				} else {
					p.setReviewRunPublication(job.RunID, "published")
					p.broadcastPRUpdate(pr.Owner, pr.Repo, pr.Number)
					log.Printf("[REVIEWER] Marked PR %d as 'completed' (critical=%d, medium=%d, low=%d, verdict=%q)", pr.Number, reviewResult.CriticalCount, reviewResult.MediumCount, reviewResult.LowCount, verdict)
				}
			}

			p.untrackReviewRun(pr.Owner, pr.Repo, pr.Number, job.RunID)
		}(job)
	}

	wg.Wait()
	return nil
}

// shouldReview determines if a PR should be processed for review generation
// based on its current state, database state, tracking status, and global settings.
func shouldReview(pr github.PullRequest, dbPR *db.PR, isTracked bool, autoReviewEnabled bool) bool {
	if dbPR == nil {
		return false // New PRs are not reviewed until they are persisted
	}

	// Condition 1: Manual Trigger
	// The PR is marked as 'generating' in DB (by user action), but we aren't tracking a process for it yet.
	isManualTrigger := dbPR.Status == "generating" && !isTracked

	if isManualTrigger {
		return true
	}

	// Condition 2: Auto Candidate
	// The PR is 'pending' AND auto-review is globally enabled.
	isAutoCandidate := dbPR.Status == "pending" && autoReviewEnabled

	if isAutoCandidate {
		// Additional check: Don't auto-generate if already completed for this commit
		if dbPR.LastCommitSHA == pr.CommitSHA && dbPR.Status == "completed" {
			return false
		}
		return true
	}

	return false
}
