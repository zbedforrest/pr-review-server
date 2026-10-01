package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/approval"
	"pr-review-server/pkg/reviewer/service"
)

type approvalEvidenceCollector interface {
	Collect(context.Context, approval.Target, approval.Viewer) (approval.Snapshot, error)
}

type approvalExecution struct {
	collector    approvalEvidenceCollector
	repository   func(context.Context, approval.Snapshot) (approval.Repository, func(), error)
	investigator func(db.ApprovalTarget) approval.Investigator
	summarize    func(context.Context, []approval.Activity) (string, error)
}

func (s *Server) approvalConfigurationMatches(scan db.ApprovalScan, assessment *approval.Assessment) bool {
	var admitted struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Policy   string `json:"policy_version"`
		Prompt   string `json:"prompt_version"`
		Runtime  string `json:"runtime_version"`
	}
	if s.cfg == nil || json.Unmarshal([]byte(scan.LimitsJSON), &admitted) != nil {
		return false
	}
	current := s.cfg.ApprovalCandidates()
	if admitted.Provider != current.Provider || admitted.Model != current.Model || admitted.Policy != approval.PolicyVersion || admitted.Prompt != approval.PromptVersion || admitted.Runtime != approval.RuntimeVersion {
		return false
	}
	return assessment == nil || (assessment.SchemaVersion == "1" && assessment.Model == admitted.Model && assessment.PolicyVersion == admitted.Policy && assessment.PromptVersion == admitted.Prompt && assessment.RuntimeVersion == admitted.Runtime)
}

// approvalRepositorySetups bounds concurrent repository preparation per
// process: each one runs git fetches against the shared clone caches.
var approvalRepositorySetups = make(chan struct{}, 16)

func (s *Server) openApprovalRepository(ctx context.Context, snapshot approval.Snapshot) (approval.Repository, func(), error) {
	if s.approvalExecution != nil && s.approvalExecution.repository != nil {
		return s.approvalExecution.repository(ctx, snapshot)
	}
	select {
	case approvalRepositorySetups <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-approvalRepositorySetups }()
	reference, release := service.BorrowCloneCache(s.cfg.AgentCloneRootDir, snapshot.Target.Owner, snapshot.Target.Repo)
	repo, err := approval.NewGitRepository(ctx, s.cfg.ApprovalCandidates().CacheRoot, snapshot.Target, snapshot.AllowedRevisions, s.ghClient.ApprovalRepositoryToken, reference)
	if err != nil {
		release()
		return nil, nil, err
	}
	if err := repo.ValidateDiff(ctx, snapshot.Revision.MergeBase, snapshot.Revision.Head); err != nil {
		_ = repo.Close()
		release()
		return nil, nil, err
	}
	return repo, func() { _ = repo.Close(); release() }, nil
}

func (s *Server) approvalInvestigator(target db.ApprovalTarget) approval.Investigator {
	if s.approvalExecution != nil && s.approvalExecution.investigator != nil {
		return s.approvalExecution.investigator(target)
	}
	cfg := s.cfg.ApprovalCandidates()
	return approval.NativeInvestigator{Config: approval.ModelConfig{Provider: cfg.Provider, Model: cfg.Model, APIKey: cfg.APIKey, ReasoningTokens: cfg.ReasoningTokens}, InitialUsage: approval.Usage{InputTokens: int(target.InputTokens), OutputTokens: int(target.OutputTokens), Rounds: target.Rounds, ToolCalls: target.ToolCalls, ToolBytes: int(target.ToolBytes)}}
}

// approvalBudget enforces a target's allowance. Token-bearing reservations
// are durable; tool and citation reservations carry no tokens, so they are
// checked against the same limits in memory and their settled usage rides
// along with the next durable write (Carry) or the final flush.
type approvalBudget struct {
	store       db.ApprovalStore
	target      db.ApprovalTarget
	mu          sync.Mutex
	reservation string
	durable     bool
	used        db.ApprovalUsage
	local       *db.ApprovalUsage
	carry       db.ApprovalUsage
}

func newApprovalBudget(store db.ApprovalStore, target db.ApprovalTarget) *approvalBudget {
	return &approvalBudget{store: store, target: target, used: db.ApprovalUsage{InputTokens: target.InputTokens, OutputTokens: target.OutputTokens, Rounds: target.Rounds, ToolCalls: target.ToolCalls, ToolBytes: target.ToolBytes}}
}

func approvalUsage(u approval.Usage) db.ApprovalUsage {
	return db.ApprovalUsage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), Rounds: u.Rounds, ToolCalls: u.ToolCalls, ToolBytes: int64(u.ToolBytes)}
}

func approvalUsageLimits() db.ApprovalUsage {
	return db.ApprovalUsage{InputTokens: approval.TargetInputTokens, OutputTokens: approval.TargetOutputTokens, Rounds: approval.MaxRounds, ToolCalls: approval.MaxToolCalls + approval.MaxCitationReads, ToolBytes: approval.MaxToolBytes + approval.MaxCitationBytes}
}

// abandonLocal keeps the conservative charge of a local reservation that was
// never settled.
func (b *approvalBudget) abandonLocal() {
	if b.local != nil {
		b.carry.Rounds += b.local.Rounds
		b.carry.ToolCalls += b.local.ToolCalls
		b.carry.ToolBytes += b.local.ToolBytes
		b.local = nil
	}
}

func (b *approvalBudget) Reserve(ctx context.Context, u approval.Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.abandonLocal()
	b.reservation, b.durable = "", false
	r, l := approvalUsage(u), approvalUsageLimits()
	if r.InputTokens < 0 || r.OutputTokens < 0 || r.Rounds < 0 || r.ToolCalls < 0 || r.ToolBytes < 0 {
		return errors.New("invalid usage reservation")
	}
	if r.InputTokens == 0 && r.OutputTokens == 0 {
		if b.used.Rounds+r.Rounds > l.Rounds || b.used.ToolCalls+r.ToolCalls > l.ToolCalls || b.used.ToolBytes+r.ToolBytes > l.ToolBytes {
			return db.ErrApprovalBudget
		}
		b.used.Rounds += r.Rounds
		b.used.ToolCalls += r.ToolCalls
		b.used.ToolBytes += r.ToolBytes
		b.local = &r
		return nil
	}
	id := approvalID()
	if err := b.store.ReserveApprovalUsage(b.target.ID, b.target.LeaseToken, time.Now(), db.ApprovalUsageReservation{ID: id, Usage: r, Limits: l, Carry: b.carry}); err != nil {
		return err
	}
	b.carry = db.ApprovalUsage{}
	b.used.InputTokens += r.InputTokens
	b.used.OutputTokens += r.OutputTokens
	b.used.Rounds += r.Rounds
	b.used.ToolCalls += r.ToolCalls
	b.used.ToolBytes += r.ToolBytes
	b.reservation, b.durable = id, true
	return nil
}

func (b *approvalBudget) Settle(ctx context.Context, reserved, actual approval.Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := approvalUsage(actual)
	if b.durable {
		r := approvalUsage(reserved)
		if err := b.store.SettleApprovalUsage(b.target.ID, b.target.LeaseToken, b.reservation, time.Now(), a); err != nil {
			return err
		}
		b.used.InputTokens += a.InputTokens - r.InputTokens
		b.used.OutputTokens += a.OutputTokens - r.OutputTokens
		b.used.ToolBytes += a.ToolBytes - r.ToolBytes
		b.reservation, b.durable = "", false
		return nil
	}
	r := b.local
	if r == nil {
		return db.ErrApprovalLeaseLost
	}
	if a.InputTokens != 0 || a.OutputTokens != 0 || a.ToolBytes < 0 || a.Rounds < 0 || a.ToolCalls < 0 || a.ToolBytes > r.ToolBytes || a.Rounds > r.Rounds || a.ToolCalls > r.ToolCalls {
		return db.ErrApprovalBudget
	}
	b.used.ToolBytes -= r.ToolBytes - a.ToolBytes
	b.carry.Rounds += r.Rounds
	b.carry.ToolCalls += r.ToolCalls
	b.carry.ToolBytes += a.ToolBytes
	b.local = nil
	return nil
}

// flush writes the locally accounted usage; a failure loses only tool usage,
// never tokens.
func (b *approvalBudget) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.abandonLocal()
	if b.carry == (db.ApprovalUsage{}) {
		return
	}
	if err := b.store.FlushApprovalUsage(b.target.ID, b.target.LeaseToken, time.Now(), b.carry); err == nil {
		b.carry = db.ApprovalUsage{}
	}
}

func (s *Server) approvalCollector() approvalEvidenceCollector {
	if s.approvalExecution != nil && s.approvalExecution.collector != nil {
		return s.approvalExecution.collector
	}
	c := &approval.Collector{GitHub: s.ghClient, PRism: s.collectApprovalPRism}
	for _, p := range s.cfg.ApprovalCandidates().Identities {
		c.Providers = append(c.Providers, approval.ProviderIdentity{Provider: p.Provider, ActorID: p.ActorID, AppID: p.AppID})
	}
	return c
}

func (s *Server) collectApprovalSnapshot(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
	started := time.Now()
	snapshot, err := s.approvalCollector().Collect(ctx, target, viewer)
	log.Printf("[APPROVAL] collected %s/%s#%d in %s: %d evidence, %d bytes, err=%v", target.Owner, target.Repo, target.Number, time.Since(started).Round(time.Millisecond), len(snapshot.Evidence), snapshot.Manifest.TotalBytes, err)
	if err != nil {
		return snapshot, err
	}
	inventory, err := s.approvalInventoryCached(viewer.ID)
	if err != nil {
		return snapshot, err
	}
	row, exists := inventory[approvalKey(target.Owner, target.Repo, target.Number)]
	if !exists || row.UserHidden {
		snapshot.Eligible = false
		snapshot.ExclusionReasons = append(snapshot.ExclusionReasons, "outside_visible_scope")
		approval.CanonicalizeSnapshot(&snapshot)
	}
	return snapshot, nil
}

func (s *Server) approvalValidationAvailable() bool {
	return s.cfg != nil && s.cfg.ApprovalCandidates().Enabled && s.approvalStore() != nil && s.ghClient != nil
}

// approvalTargetDuration bounds one pull request's investigation, from
// evidence collection to the final answer.
const (
	approvalTargetDuration = 5 * time.Minute
	// approvalIdleClaimEvery spaces claims when there is no work: every
	// claim serializes on the approval mutation gate.
	approvalIdleClaimEvery = 5 * time.Second
)

// approvalSlots is how many targets investigate at once, both across the
// deployment and for one user.
func approvalSlots() int {
	return approvalEnvCount("APPROVAL_CANDIDATES_SLOTS", 50)
}

// approvalModelConcurrency bounds this process's in-flight model requests.
func approvalModelConcurrency() int {
	return approvalEnvCount("APPROVAL_CANDIDATES_MODEL_CONCURRENCY", approvalSlots())
}

func approvalEnvCount(name string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return min(max(n, 1), 100)
}

// approvalWorkerRevisionKey records the deployment revision whose workers
// may claim targets. Cloud Run keeps a replaced revision's instances (and
// their background goroutines) alive for a while after a deploy, so the
// newest revision claims the role at startup and older ones stand down.
const approvalWorkerRevisionKey = "approval_worker_revision"

func (s *Server) runApprovalWorkers(ctx context.Context) {
	store := s.approvalStore()
	if store == nil {
		return
	}
	revision := os.Getenv("K_REVISION")
	if revision != "" && s.db != nil {
		if err := s.db.SetSetting(approvalWorkerRevisionKey, revision); err != nil {
			log.Printf("[APPROVAL] record worker revision: %v", err)
		}
	}
	var currentMu sync.Mutex
	current, checked := true, time.Time{}
	isCurrent := func() bool {
		if revision == "" || s.db == nil {
			return true
		}
		currentMu.Lock()
		defer currentMu.Unlock()
		if time.Since(checked) >= 15*time.Second {
			checked = time.Now()
			if owner, err := s.db.GetSetting(approvalWorkerRevisionKey); err == nil && owner != "" {
				if next := owner == revision; next != current {
					current = next
					log.Printf("[APPROVAL] worker revision %s is current=%t (owner %s)", revision, current, owner)
				}
			}
		}
		return current
	}
	if s.approvalAvailable() != "" {
		_ = store.CancelAllApprovalScans(time.Now())
	}
	go func() {
		s.waitForCloneCaches(ctx)
		s.dispatchApprovalTargets(ctx, store, isCurrent)
	}()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			_, _ = store.PruneApprovalScans(time.Now().Add(-30 * 24 * time.Hour))
			if s.cfg != nil {
				_, _ = approval.PruneRepositoryCache(ctx, s.cfg.ApprovalCandidates().CacheRoot)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// dispatchApprovalTargets claims every free local slot in one claim, on a
// one-second tick or as soon as a scan is admitted. A finished target claims
// at once only when the last claim filled every free slot, so queued work may
// remain; otherwise the idle spacing holds and finishes do not each run an
// empty claim on the gate.
func (s *Server) dispatchApprovalTargets(ctx context.Context, store db.ApprovalStore, isCurrent func() bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	slots := approvalSlots()
	var running atomic.Int32
	worker := approvalID()
	available := s.approvalAvailable() == ""
	nextMaintenance := time.Now().Add(time.Minute)
	var nextClaim time.Time
	finished := make(chan struct{}, 1)
	backlog := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.approvalWake.channel():
			nextClaim = time.Time{}
		case <-finished:
			if backlog {
				nextClaim = time.Time{}
			}
		}
		if s.approvalAvailable() != "" {
			if available || !time.Now().Before(nextMaintenance) {
				_ = store.CancelAllApprovalScans(time.Now())
				available = false
				nextMaintenance = time.Now().Add(time.Minute)
			}
			continue
		}
		available = true
		free := slots - int(running.Load())
		if free <= 0 || time.Now().Before(s.approvalRate.pausedUntil()) || time.Now().Before(nextClaim) || !isCurrent() {
			continue
		}
		targets, err := store.ClaimApprovalTargets(db.ApprovalClaim{Worker: worker, Now: time.Now(), LeaseDuration: approvalLeaseDuration, TargetDuration: approvalTargetDuration, MaxSlots: slots, MaxPerUser: slots, MaxClaims: free})
		backlog = err == nil && len(targets) >= free
		if err != nil || len(targets) == 0 {
			if err != nil {
				log.Printf("[APPROVAL] claim targets: %v", err)
			}
			nextClaim = time.Now().Add(approvalIdleClaimEvery)
			continue
		}
		if !backlog {
			nextClaim = time.Now().Add(approvalIdleClaimEvery)
		}
		for _, target := range targets {
			running.Add(1)
			go func() {
				defer func() {
					select {
					case finished <- struct{}{}:
					default:
					}
				}()
				defer running.Add(-1)
				s.investigateApproval(ctx, target)
			}()
		}
	}
}

func (s *Server) investigateApproval(parent context.Context, target db.ApprovalTarget) {
	store := s.approvalStore()
	defer store.ReleaseApprovalTargetSlot(target.ID, target.LeaseToken)
	deadline := time.Now().Add(approvalTargetDuration)
	if target.Deadline != nil {
		deadline = *target.Deadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	report, stopProgress := s.startApprovalProgress(ctx, target)
	defer stopProgress()
	ctx = approval.WithActivityObserver(ctx, report)
	ctx = approval.WithModelLimiter(ctx, s.approvalModelLimiter())
	go s.watchApprovalTarget(ctx, cancel, target)
	finish := func(status, decision, freshness, reason, summary string, a *approval.Assessment) {
		stopProgress()
		f := db.ApprovalFinalization{ExecutionStatus: status, Decision: decision, Freshness: freshness, ReasonCodesJSON: approvalJSON([]string{reason}), Summary: summary}
		if a != nil {
			f.AssessmentJSON = approvalJSON(a)
			reasons := append([]string(nil), a.ReasonCodes...)
			if reason != "" {
				reasons = append(reasons, reason)
			}
			f.ReasonCodesJSON = approvalJSON(reasons)
		}
		if freshness == "current" {
			now := time.Now()
			until := now.Add(5 * time.Minute)
			f.ValidatedAt = &now
			f.ValidUntil = &until
		}
		key := s.approvalReuseKey(a)
		if err := store.FinalizeApprovalTarget(target.ID, target.LeaseToken, time.Now(), f); err == nil && key != "" {
			_ = store.RecordApprovalReuse(target.UserID, key, target.ID, time.Now())
		}
	}
	defer func() {
		if recover() != nil {
			finish("failed", "insufficient_evidence", "expired", "runtime_failed", "Investigation could not complete", nil)
		}
	}()
	fail := func(reason string, cause error) {
		if cause != nil {
			log.Printf("[APPROVAL] target %s (%s/%s#%d) %s: %v", target.ID, target.Owner, target.Repo, target.Number, reason, cause)
		}
		status := "failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = "timed_out"
			reason = "deadline_exceeded"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			if parent.Err() != nil {
				return
			}
			scan, err := store.GetApprovalScan(target.UserID, target.ScanID)
			if err == nil && scan.CancelRequested {
				return
			}
			if s.approvalAvailable() != "" {
				_ = store.CancelApprovalScan(target.UserID, target.ScanID, time.Now())
				return
			}
			status = "failed"
			reason = "control_unavailable"
		}
		finish(status, "insufficient_evidence", "expired", reason, approvalFailureSummary(cause), nil)
	}
	scan, err := store.GetApprovalScan(target.UserID, target.ScanID)
	if err != nil {
		fail("configuration_unavailable", err)
		return
	}
	configurationChanged := func() {
		finish("completed", "insufficient_evidence", "stale", "evidence_changed", "Investigation configuration changed; start a new scan", nil)
	}
	if !s.approvalConfigurationMatches(*scan, nil) {
		configurationChanged()
		return
	}
	user, err := s.db.GetUserByID(target.UserID)
	if err != nil || user == nil {
		fail("access_unavailable", err)
		return
	}
	if err := s.approvalRetryAfterRateLimit(ctx, func() error { return s.approvalAccess(ctx, user.ID, target) }); err != nil {
		fail(approvalFailure(err, "access_unavailable"), err)
		return
	}
	requested := approval.Target{Owner: target.Owner, Repo: target.Repo, Number: target.Number, ExpectedHeadSHA: target.ExpectedHeadSHA}
	var snapshot approval.Snapshot
	err = s.approvalRetryAfterRateLimit(ctx, func() error {
		var err error
		snapshot, err = s.collectApprovalSnapshot(ctx, requested, approval.Viewer{ID: user.ID, Login: user.GitHubUsername, GitHubID: user.GitHubID})
		return err
	})
	if err != nil {
		fail(approvalFailure(err, "collection_failed"), err)
		return
	}
	if snapshot.RepositoryID != target.RepositoryID {
		fail("repository_changed", nil)
		return
	}
	if snapshot.Revision.Head != target.ExpectedHeadSHA {
		finish("completed", "insufficient_evidence", "stale", "head_changed", "The pull request changed; recheck its current revision", nil)
		return
	}
	if target.Attempts > 1 {
		stored, err := store.GetApprovalTarget(user.ID, target.ScanID, target.ID)
		if err != nil {
			fail("snapshot_unavailable", err)
			return
		}
		if stored.SnapshotJSON != "" {
			var previous approval.Snapshot
			if json.Unmarshal([]byte(stored.SnapshotJSON), &previous) != nil || previous.Digest != snapshot.Digest {
				finish("completed", "insufficient_evidence", "stale", "evidence_changed", "Review evidence changed during recovery", nil)
				return
			}
			snapshot = previous
		}
	}
	if err := store.SaveApprovalSnapshot(target.ID, target.LeaseToken, approvalJSON(snapshot), time.Now()); err != nil {
		fail("snapshot_unavailable", err)
		return
	}
	if !snapshot.Eligible {
		a := approval.Evaluate(snapshot, approval.Assessment{})
		a.Origin = approval.OriginGate
		finish("completed", a.Decision, "current", "excluded", "This pull request is outside the eligible scope", &a)
		return
	}
	if a, ok := s.approvalPrejudged(*scan, user.ID, snapshot); ok {
		if !s.approvalConfigurationMatches(*scan, a) {
			configurationChanged()
			return
		}
		finish("completed", a.Decision, "current", "", a.Summary, a)
		return
	}
	if !s.approvalConfigurationMatches(*scan, nil) {
		configurationChanged()
		return
	}
	cfg := s.cfg.ApprovalCandidates()
	if err := store.ReserveApprovalBudget(target.ID, target.LeaseToken, time.Now(), cfg.DailyInput, cfg.DailyOutput, approval.TargetInputTokens, approval.TargetOutputTokens); err != nil {
		fail("daily_budget", err)
		return
	}
	if err := store.SetApprovalTargetStage(target.ID, target.LeaseToken, "investigating", time.Now()); err != nil {
		fail("lease_lost", err)
		return
	}
	report(approval.Activity{Stage: "repository"})
	repoStarted := time.Now()
	repo, closeRepository, err := s.openApprovalRepository(ctx, snapshot)
	log.Printf("[APPROVAL] repository for %s/%s#%d ready in %s, err=%v", target.Owner, target.Repo, target.Number, time.Since(repoStarted).Round(time.Millisecond), err)
	if err != nil {
		fail("code_unavailable", err)
		return
	}
	defer closeRepository()
	investigator := s.approvalInvestigator(target)
	budget := newApprovalBudget(store, target)
	assessment, err := investigator.Investigate(ctx, snapshot, repo, budget)
	budget.flush()
	if err != nil {
		if errors.Is(err, approval.ErrInvestigationLimit) {
			limit := approval.LimitCode(err)
			log.Printf("[APPROVAL] target %s (%s/%s#%d) stopped at %s: %v", target.ID, target.Owner, target.Repo, target.Number, limit, err)
			limited := approval.Evaluate(snapshot, approval.Assessment{SnapshotID: snapshot.ID, SnapshotDigest: snapshot.Digest, Summary: "Investigation reached its resource limit", CoverageGaps: []string{"Investigation stopped at its " + approval.LimitDescription(limit) + " before inspecting all required evidence"}})
			limited.Origin = approval.OriginLimit
			// The fallback assessment is synthesized, not the model's, so its
			// validation failure would only restate the limit.
			limited.ReasonCodes = approvalWithout(limited.ReasonCodes, "invalid_assessment")
			finish("completed", limited.Decision, "expired", limit, limited.Summary, &limited)
			return
		}
		fail(approvalFailure(err, "investigation_failed"), err)
		return
	}
	assessment.Origin = approval.OriginInvestigator
	// Only a candidate needs its evidence re-collected; nothing else can approve.
	if assessment.Decision != "candidate" {
		if err := s.approvalRetryAfterRateLimit(ctx, func() error { return s.approvalAccess(ctx, user.ID, target) }); err != nil {
			fail(approvalFailure(err, "access_unavailable"), err)
			return
		}
		if !s.approvalConfigurationMatches(*scan, &assessment) {
			configurationChanged()
			return
		}
		finish("completed", assessment.Decision, "current", "", assessment.Summary, &assessment)
		return
	}
	if err := store.SetApprovalTargetStage(target.ID, target.LeaseToken, "validating", time.Now()); err != nil {
		fail("lease_lost", err)
		return
	}
	report(approval.Activity{Stage: "validating"})
	if err := s.approvalRetryAfterRateLimit(ctx, func() error { return s.approvalAccess(ctx, user.ID, target) }); err != nil {
		fail(approvalFailure(err, "access_unavailable"), err)
		return
	}
	var fresh approval.Snapshot
	err = s.approvalRetryAfterRateLimit(ctx, func() error {
		var err error
		fresh, err = s.collectApprovalSnapshot(ctx, requested, approval.Viewer{ID: user.ID, Login: user.GitHubUsername, GitHubID: user.GitHubID})
		return err
	})
	if err != nil || !fresh.Manifest.Complete {
		if err != nil {
			log.Printf("[APPROVAL] target %s (%s/%s#%d) revalidation: %v", target.ID, target.Owner, target.Repo, target.Number, err)
		}
		finish("completed", assessment.Decision, "expired", "validation_failed", "Evidence could not be revalidated", &assessment)
		return
	}
	if !s.approvalConfigurationMatches(*scan, &assessment) {
		configurationChanged()
		return
	}
	if fresh.Digest != snapshot.Digest {
		finish("completed", assessment.Decision, "stale", "evidence_changed", "Evidence changed; recheck before relying on this result", &assessment)
		return
	}
	finish("completed", assessment.Decision, "current", "", assessment.Summary, &assessment)
}

func (s *Server) handleApprovalRevalidate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.approvalRequest(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeV1Error(w, 405, "method_not_allowed", "Use POST")
		return
	}
	if !s.approvalValidationAvailable() {
		writeV1Error(w, 503, "unavailable", "Evidence validation unavailable")
		return
	}
	var request struct {
		TargetIDs []string `json:"target_ids"`
	}
	if !decodeApprovalRequest(w, r, &request) {
		return
	}
	if len(request.TargetIDs) == 0 || len(request.TargetIDs) > 25 {
		writeV1Error(w, 400, "invalid_request", "Provide 1 through 25 target IDs")
		return
	}
	wanted := map[string]bool{}
	for _, id := range request.TargetIDs {
		wanted[id] = true
	}
	targets := []db.ApprovalTarget{}
	cursor := ""
	for {
		rows, err := s.approvalStore().ListCurrentApprovalTargets(user.ID, 100, cursor)
		if err != nil {
			approvalError(w, err)
			return
		}
		for _, row := range rows {
			if wanted[row.ID] {
				targets = append(targets, row)
				delete(wanted, row.ID)
			}
		}
		if len(rows) < 100 || len(wanted) == 0 {
			break
		}
		cursor = rows[len(rows)-1].ID
	}
	if len(wanted) > 0 {
		writeV1Error(w, 404, "not_found", "A target is not current or accessible")
		return
	}
	for _, target := range targets {
		if !s.approvalCanRead(r.Context(), user.ID, target) {
			writeV1Error(w, 404, "not_found", "Target is not accessible")
			return
		}
	}
	accepted := []string{}
	states := map[string]string{}
	for _, target := range targets {
		lease, err := s.approvalStore().ClaimApprovalValidation(user.ID, target.ID, time.Now(), 90*time.Second)
		if err != nil {
			states[target.ID] = "not_eligible"
			continue
		}
		if lease == nil {
			states[target.ID] = "busy_or_already_running"
			continue
		}
		states[target.ID] = "validating"
		accepted = append(accepted, target.ID)
		go s.revalidateApproval(target, *lease, approval.Viewer{ID: user.ID, Login: user.GitHubUsername, GitHubID: user.GitHubID})
	}
	writeV1JSON(w, 202, map[string]any{"target_ids": accepted, "states": states})
}

func (s *Server) revalidateApproval(target db.ApprovalTarget, lease db.ApprovalValidation, viewer approval.Viewer) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := s.approvalStore()
	freshness, reason := "expired", "validation_failed"
	defer func() {
		if recover() != nil {
			freshness = "expired"
			reason = "validation_failed"
		}
		_ = store.FinishApprovalValidation(viewer.ID, target.ID, lease.Token, time.Now(), freshness, reason)
	}()
	if !s.approvalValidationAvailable() || !s.approvalCanRead(ctx, viewer.ID, target) {
		reason = "access_unavailable"
		return
	}
	stored, err := store.GetApprovalTarget(viewer.ID, target.ScanID, target.ID)
	if err != nil {
		return
	}
	scan, err := store.GetApprovalScan(viewer.ID, target.ScanID)
	if err != nil {
		return
	}
	var assessment approval.Assessment
	if json.Unmarshal([]byte(stored.AssessmentJSON), &assessment) != nil || !s.approvalConfigurationMatches(*scan, &assessment) {
		freshness = "stale"
		reason = "evidence_changed"
		return
	}
	var previous approval.Snapshot
	if json.Unmarshal([]byte(stored.SnapshotJSON), &previous) != nil {
		return
	}
	current, err := s.collectApprovalSnapshot(ctx, previous.Target, viewer)
	if err != nil || !current.Manifest.Complete {
		return
	}
	if current.Digest != previous.Digest {
		freshness = "stale"
		reason = "evidence_changed"
		return
	}
	if !s.approvalConfigurationMatches(*scan, &assessment) {
		freshness = "stale"
		reason = "evidence_changed"
		return
	}
	freshness = "current"
	reason = ""
}

const approvalFailureDetailMax = 240

// approvalFailureSummary is the stored summary of a failed target: the cause
// is kept, bounded, so a failure can be diagnosed after the fact.
func approvalFailureSummary(cause error) string {
	if cause == nil {
		return "Investigation could not complete"
	}
	detail := cause.Error()
	if len(detail) > approvalFailureDetailMax {
		detail = detail[:approvalFailureDetailMax] + "..."
	}
	return "Investigation could not complete: " + detail
}

func approvalWithout(reasons []string, drop string) []string {
	out := reasons[:0:0]
	for _, r := range reasons {
		if r != drop {
			out = append(out, r)
		}
	}
	return out
}

// waitForCloneCaches holds approval workers until the startup clone-cache
// warm-up finishes: a target claimed against a cold cache spends most of its
// time budget fetching history the warm-up is already cloning.
func (s *Server) waitForCloneCaches(ctx context.Context) {
	warm, ok := s.poller.(interface{ PrewarmDone() <-chan struct{} })
	if !ok {
		return
	}
	select {
	case <-warm.PrewarmDone():
	case <-time.After(5 * time.Minute):
	case <-ctx.Done():
	}
}
