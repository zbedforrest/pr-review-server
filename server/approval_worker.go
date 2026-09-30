package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
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

func (s *Server) openApprovalRepository(ctx context.Context, snapshot approval.Snapshot) (approval.Repository, func(), error) {
	if s.approvalExecution != nil && s.approvalExecution.repository != nil {
		return s.approvalExecution.repository(ctx, snapshot)
	}
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
	return approval.NativeInvestigator{Config: approval.ModelConfig{Provider: cfg.Provider, Model: cfg.Model, APIKey: cfg.APIKey}, InitialUsage: approval.Usage{InputTokens: int(target.InputTokens), OutputTokens: int(target.OutputTokens), Rounds: target.Rounds, ToolCalls: target.ToolCalls, ToolBytes: int(target.ToolBytes)}}
}

type approvalBudget struct {
	store       db.ApprovalStore
	target      db.ApprovalTarget
	reservation string
}

func approvalUsage(u approval.Usage) db.ApprovalUsage {
	return db.ApprovalUsage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), Rounds: u.Rounds, ToolCalls: u.ToolCalls, ToolBytes: int64(u.ToolBytes)}
}
func (b *approvalBudget) Reserve(ctx context.Context, u approval.Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := approvalID()
	err := b.store.ReserveApprovalUsage(b.target.ID, b.target.LeaseToken, time.Now(), db.ApprovalUsageReservation{ID: id, Usage: approvalUsage(u), Limits: db.ApprovalUsage{InputTokens: 600000, OutputTokens: 12000, Rounds: 16, ToolCalls: 40, ToolBytes: 1024 * 1024}})
	if err == nil {
		b.reservation = id
	}
	return err
}
func (b *approvalBudget) Settle(ctx context.Context, reserved, actual approval.Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.store.SettleApprovalUsage(b.target.ID, b.target.LeaseToken, b.reservation, time.Now(), approvalUsage(actual))
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
	snapshot, err := s.approvalCollector().Collect(ctx, target, viewer)
	if err != nil {
		return snapshot, err
	}
	inventory, err := s.approvalInventory(viewer.ID)
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
// evidence collection to the final answer; approvalWorkerSlots is how many
// run at once across the deployment.
const (
	approvalTargetDuration = 7 * time.Minute
	approvalWorkerSlots    = 4
)

func (s *Server) runApprovalWorkers(ctx context.Context) {
	store := s.approvalStore()
	if store == nil {
		return
	}
	if s.approvalAvailable() != "" {
		_ = store.CancelAllApprovalScans(time.Now())
	}
	for range approvalWorkerSlots {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			worker := approvalID()
			available := s.approvalAvailable() == ""
			nextMaintenance := time.Now().Add(time.Minute)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if s.approvalAvailable() != "" {
						if available || !time.Now().Before(nextMaintenance) {
							_ = store.CancelAllApprovalScans(time.Now())
							available = false
							nextMaintenance = time.Now().Add(time.Minute)
						}
						continue
					}
					available = true
					if time.Now().Before(s.approvalRate.pausedUntil()) {
						continue
					}
					target, err := store.ClaimApprovalTarget(db.ApprovalClaim{Worker: worker, Now: time.Now(), LeaseDuration: 60 * time.Second, TargetDuration: approvalTargetDuration, MaxSlots: approvalWorkerSlots})
					if err == nil && target != nil {
						s.investigateApproval(ctx, *target)
					}
				}
			}
		}()
	}
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
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		renewed := time.Now()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scan, err := store.GetApprovalScan(target.UserID, target.ScanID)
				if err != nil {
					failures++
					if failures < 3 {
						continue
					}
					cancel()
					return
				}
				if scan.CancelRequested || s.approvalAvailable() != "" {
					cancel()
					return
				}
				if time.Since(renewed) >= 15*time.Second {
					if err := store.HeartbeatApprovalTarget(target.ID, target.LeaseToken, time.Now(), 60*time.Second); err != nil {
						failures++
						if failures < 3 && !errors.Is(err, db.ErrApprovalLeaseLost) {
							continue
						}
						cancel()
						return
					}
					renewed = time.Now()
					failures = 0
				}
			}
		}
	}()
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
		_ = store.FinalizeApprovalTarget(target.ID, target.LeaseToken, time.Now(), f)
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
		finish("completed", a.Decision, "current", "excluded", "This pull request is outside the eligible scope", &a)
		return
	}
	if !s.approvalConfigurationMatches(*scan, nil) {
		configurationChanged()
		return
	}
	cfg := s.cfg.ApprovalCandidates()
	if err := store.ReserveApprovalBudget(target.ID, target.LeaseToken, time.Now(), cfg.DailyInput, cfg.DailyOutput, 600000, 12000); err != nil {
		fail("daily_budget", err)
		return
	}
	if err := store.SetApprovalTargetStage(target.ID, target.LeaseToken, "investigating", time.Now()); err != nil {
		fail("lease_lost", err)
		return
	}
	report(approval.Activity{Stage: "repository"})
	repo, closeRepository, err := s.openApprovalRepository(ctx, snapshot)
	if err != nil {
		fail("code_unavailable", err)
		return
	}
	defer closeRepository()
	investigator := s.approvalInvestigator(target)
	assessment, err := investigator.Investigate(ctx, snapshot, repo, &approvalBudget{store: store, target: target})
	if err != nil {
		if errors.Is(err, approval.ErrInvestigationLimit) {
			limit := approval.LimitCode(err)
			log.Printf("[APPROVAL] target %s (%s/%s#%d) stopped at %s: %v", target.ID, target.Owner, target.Repo, target.Number, limit, err)
			limited := approval.Evaluate(snapshot, approval.Assessment{SnapshotID: snapshot.ID, SnapshotDigest: snapshot.Digest, Summary: "Investigation reached its resource limit", CoverageGaps: []string{"Investigation stopped at its " + approval.LimitDescription(limit) + " before inspecting all required evidence"}})
			// The fallback assessment is synthesized, not the model's, so its
			// validation failure would only restate the limit.
			limited.ReasonCodes = approvalWithout(limited.ReasonCodes, "invalid_assessment")
			finish("completed", limited.Decision, "expired", limit, limited.Summary, &limited)
			return
		}
		fail(approvalFailure(err, "investigation_failed"), err)
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
