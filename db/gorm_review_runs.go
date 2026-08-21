package db

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const reviewRunAbandonBatchSize = 500

var _ CompletedReviewPathLookup = (*GormDB)(nil)
var _ ReviewRunLedger = (*GormDB)(nil)

// GetCompletedPRByReviewPath resolves the rare missing-alias fallback without
// loading every PR into the server process. review_path is indexed because the
// direct artifact route calls this only after both storage lookups miss.
func (g *GormDB) GetCompletedPRByReviewPath(reviewPath string) (*PR, error) {
	if reviewPath == "" {
		return nil, nil
	}
	var model PRModel
	err := g.db.Where("review_path = ? AND status = ?", reviewPath, "completed").First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get completed PR by review path %s: %w", reviewPath, err)
	}
	return prModelToPR(&model), nil
}

// SetPRGeneratingForReviewRun atomically claims the mutable PR projection for
// runID while moving it into the generating state. A later run may replace the
// claim; all subsequent projection writes below are conditional on ownership.
func (g *GormDB) SetPRGeneratingForReviewRun(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool, runID string) error {
	if owner == "" || repo == "" || prNumber <= 0 || commitSHA == "" || runID == "" {
		return fmt.Errorf("set PR generating for review run: complete PR target, commit, and run ID are required")
	}
	now := time.Now().UTC()
	model := &PRModel{
		RepoOwner: owner, RepoName: repo, PRNumber: prNumber, LastCommitSHA: commitSHA,
		Status: "generating", GeneratingSince: &now, Title: title, Author: author,
		CreatedAt: createdAt, Draft: draft, ProjectionRunID: runID,
	}
	updateColumns := []string{
		"last_commit_sha", "status", "generating_since", "title", "author", "draft",
		"projection_run_id", "error_message",
	}
	// A missing GitHub created_at is represented as nil. Preserve an existing
	// timestamp on conflict so temporary upstream omissions do not degrade PR
	// ordering or trigger avoidable metadata backfills.
	if createdAt != nil {
		updateColumns = append(updateColumns, "created_at")
	}
	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns(updateColumns),
	}).Create(model).Error
}

func (g *GormDB) SetPRAgentReviewingForReviewRun(owner, repo string, prNumber int, runID string) (bool, error) {
	result := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND projection_run_id = ?", owner, repo, prNumber, runID).
		Updates(map[string]any{"status": "agent_reviewing", "error_message": ""})
	if result.Error != nil {
		return false, fmt.Errorf("set PR agent reviewing for run %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (g *GormDB) SetPRErrorForReviewRun(owner, repo string, prNumber int, runID, message string) (bool, error) {
	now := time.Now().UTC()
	result := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND projection_run_id = ?", owner, repo, prNumber, runID).
		Updates(map[string]any{
			"status": "error", "error_message": message, "last_reviewed_at": now, "generating_since": nil,
		})
	if result.Error != nil {
		return false, fmt.Errorf("set PR error for run %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// SetPRErrorIfNoLiveReview records an admission/dispatch failure only when the
// target PR has no queued or live-leased run. The target-wide predicate covers
// newly accepted work before its run ID claims the mutable PR projection.
func (g *GormDB) SetPRErrorIfNoLiveReview(owner, repo string, prNumber int, message string) (bool, error) {
	now := time.Now().UTC()
	result := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Where("status <> ?", "completed").
		Where(`NOT EXISTS (
			SELECT 1 FROM review_runs
			WHERE review_runs.repo_owner = prs.repo_owner
			  AND review_runs.repo_name = prs.repo_name
			  AND review_runs.pr_number = prs.pr_number
			  AND (review_runs.status = ? OR
			       (review_runs.status = ? AND (review_runs.lease_expires_at IS NULL OR review_runs.lease_expires_at > ?)))
		)`, ReviewRunStatusQueued, ReviewRunStatusRunning, now).
		Updates(map[string]any{
			"status": "error", "error_message": message, "last_reviewed_at": now, "generating_since": nil,
		})
	if result.Error != nil {
		return false, fmt.Errorf("set PR error without live review: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (g *GormDB) MarkPRCompletedForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string) (bool, error) {
	now := time.Now().UTC()
	result := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND projection_run_id = ?", owner, repo, prNumber, projectionRunID).
		Updates(map[string]any{
			"status": "completed", "review_path": reviewPath, "last_commit_sha": commitSHA,
			"last_reviewed_at": now, "generating_since": nil, "critical_count": critical,
			"medium_count": medium, "low_count": low, "review_verdict": verdict,
			"model_fallback": modelFallback, "review_run_id": reviewRunID,
			"review_run_json": reviewRunJSON, "error_message": "", "error_retry_count": 0,
		})
	if result.Error != nil {
		return false, fmt.Errorf("mark PR completed for run %s: %w", projectionRunID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// FinalizeReviewRunSuccess atomically terminalizes a holder-owned run and, when
// that run still owns the matching PR+commit projection, publishes its result
// there. Losing the mutable projection is a successful immutable completion:
// the run is finalized as superseded while the newer PR projection is left
// untouched. The completed-success case is idempotent for retry after an
// ambiguous transaction response from the same execution attempt.
func (g *GormDB) FinalizeReviewRunSuccess(input ReviewRunSuccessFinalization) (ReviewRunFinalizationResult, error) {
	if input.RunID == "" || input.Holder == "" || input.ExecutionAttempt <= 0 || input.LeaseCheckedAt.IsZero() || input.CompletedAt.IsZero() {
		return ReviewRunFinalizationResult{}, fmt.Errorf("finalize successful review run: run ID, holder, execution attempt, lease-check time, and completion time are required")
	}
	if input.DurationMS < 0 || input.Critical < 0 || input.Medium < 0 || input.Low < 0 {
		return ReviewRunFinalizationResult{}, fmt.Errorf("finalize successful review run %s: duration and finding counts cannot be negative", input.RunID)
	}
	if input.HTMLPath == "" || input.JSONPath == "" || input.CanonicalPath == "" {
		return ReviewRunFinalizationResult{}, fmt.Errorf("finalize successful review run %s: immutable and canonical artifact paths are required", input.RunID)
	}

	var outcome ReviewRunFinalizationResult
	err := g.db.Transaction(func(tx *gorm.DB) error {
		// This no-op write is a portable row lock: PostgreSQL locks the selected
		// row, while SQLite acquires its transaction write lock. Terminalization
		// and attempt insertion use the same pattern, so exactly one side of the
		// lease boundary can commit first.
		locked := tx.Exec(`UPDATE review_runs SET updated_at = updated_at
			WHERE run_id = ? AND status = ? AND execution_attempt = ?
			  AND lease_holder = ?`,
			input.RunID, ReviewRunStatusRunning, input.ExecutionAttempt, input.Holder)
		if locked.Error != nil {
			return fmt.Errorf("lock successful review run: %w", locked.Error)
		}

		var run ReviewRunModel
		if locked.RowsAffected != 1 {
			if err := tx.Where("run_id = ?", input.RunID).First(&run).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return fmt.Errorf("inspect unowned successful review run: %w", err)
			}
			if run.Status == ReviewRunStatusCompleted && run.TerminalCode == "success" &&
				run.ExecutionAttempt == input.ExecutionAttempt &&
				(run.PublicationStatus == "published" || run.PublicationStatus == "superseded") {
				outcome = ReviewRunFinalizationResult{
					Finalized: true, Published: run.PublicationStatus == "published",
					PublicationStatus: run.PublicationStatus,
				}
			}
			return nil
		}

		if err := tx.Where("run_id = ?", input.RunID).First(&run).Error; err != nil {
			return fmt.Errorf("load locked successful review run: %w", err)
		}
		leaseCheckedAt := time.Now().UTC()
		if input.LeaseCheckedAt.After(leaseCheckedAt) {
			leaseCheckedAt = input.LeaseCheckedAt
		}
		if run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(leaseCheckedAt) {
			return nil
		}

		projected := tx.Model(&PRModel{}).
			Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND projection_run_id = ? AND last_commit_sha = ?",
				run.RepoOwner, run.RepoName, run.PRNumber, run.RunID, run.CommitSHA).
			Updates(map[string]any{
				"status": "completed", "review_path": input.CanonicalPath,
				"last_commit_sha": run.CommitSHA, "last_reviewed_at": input.CompletedAt,
				"generating_since": nil, "critical_count": input.Critical,
				"medium_count": input.Medium, "low_count": input.Low,
				"review_verdict": input.Verdict, "model_fallback": input.ModelFallback,
				"review_run_id": input.RunID, "review_run_json": input.ReviewRunJSON,
				"error_message": "", "error_retry_count": 0,
			})
		if projected.Error != nil {
			return fmt.Errorf("publish successful review run to PR: %w", projected.Error)
		}

		publicationStatus := "superseded"
		if projected.RowsAffected == 1 {
			publicationStatus = "published"
		}
		finished := tx.Model(&ReviewRunModel{}).
			Where("run_id = ? AND status = ? AND execution_attempt = ? AND lease_holder = ?",
				input.RunID, ReviewRunStatusRunning, input.ExecutionAttempt, input.Holder).
			Updates(map[string]any{
				"status": ReviewRunStatusCompleted, "completed_at": input.CompletedAt,
				"duration_ms": input.DurationMS, "html_path": input.HTMLPath, "json_path": input.JSONPath,
				"critical_count": input.Critical, "medium_count": input.Medium, "low_count": input.Low,
				"verdict": input.Verdict, "model_fallback": input.ModelFallback,
				"serving_model_verification": input.ServingModelVerification,
				"actual_models_json":         input.ActualModelsJSON, "publication_status": publicationStatus,
				"terminal_code": "success", "failure_stage": "", "error_summary": "",
				"lease_holder": "", "lease_expires_at": nil,
			})
		if finished.Error != nil {
			return fmt.Errorf("terminalize successful review run: %w", finished.Error)
		}
		if finished.RowsAffected != 1 {
			return fmt.Errorf("terminalize successful review run %s: lease ownership changed during transaction", input.RunID)
		}
		outcome = ReviewRunFinalizationResult{
			Finalized: true, Published: publicationStatus == "published", PublicationStatus: publicationStatus,
		}
		return nil
	})
	if err != nil {
		return ReviewRunFinalizationResult{}, fmt.Errorf("finalize successful review run %s: %w", input.RunID, err)
	}
	return outcome, nil
}

// RestorePRCompletedFromCacheForReviewRun projects an existing artifact only
// while no active run owns the PR and the row is not already completed. An
// in-flight-looking row is recoverable only when it has aged beyond the caller's
// admission window and no queued/live run exists for the PR. The target-wide
// live-run check also protects a newly accepted run before it claims the mutable
// projection, including when that projection still names an older terminal run.
func (g *GormDB) RestorePRCompletedFromCacheForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string, inFlightStaleBefore time.Time) (bool, error) {
	if owner == "" || repo == "" || prNumber <= 0 || projectionRunID == "" || commitSHA == "" || reviewPath == "" || inFlightStaleBefore.IsZero() {
		return false, fmt.Errorf("restore cached PR for review run: complete PR target, projection run, commit, path, and stale cutoff are required")
	}
	now := time.Now().UTC()
	result := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Where("status <> ?", "completed").
		Where("status NOT IN ? OR (COALESCE(projection_run_id, '') <> '' AND (generating_since IS NULL OR generating_since <= ?))",
			[]string{"generating", "agent_reviewing"}, inFlightStaleBefore).
		Where(`NOT EXISTS (
			SELECT 1 FROM review_runs
			WHERE review_runs.repo_owner = prs.repo_owner
			  AND review_runs.repo_name = prs.repo_name
			  AND review_runs.pr_number = prs.pr_number
			  AND review_runs.run_id <> ?
			  AND (review_runs.status = ? OR
			       (review_runs.status = ? AND (review_runs.lease_expires_at IS NULL OR review_runs.lease_expires_at > ?)))
		)`, projectionRunID, ReviewRunStatusQueued, ReviewRunStatusRunning, now).
		Updates(map[string]any{
			"status": "completed", "projection_run_id": projectionRunID,
			"review_path": reviewPath, "last_commit_sha": commitSHA, "last_reviewed_at": now,
			"generating_since": nil, "critical_count": critical, "medium_count": medium,
			"low_count": low, "review_verdict": verdict, "model_fallback": modelFallback,
			"review_run_id": reviewRunID, "review_run_json": reviewRunJSON,
			"error_message": "", "error_retry_count": 0,
		})
	if result.Error != nil {
		return false, fmt.Errorf("restore cached PR for run %s: %w", projectionRunID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (g *GormDB) CreateReviewRun(run *ReviewRun) error {
	if run == nil {
		return fmt.Errorf("create review run: run is nil")
	}
	if run.RunID == "" {
		return fmt.Errorf("create review run: run_id is required")
	}
	if run.RepoOwner == "" || run.RepoName == "" || run.PRNumber <= 0 || run.CommitSHA == "" {
		return fmt.Errorf("create review run %s: complete PR target is required", run.RunID)
	}
	if (run.PRID != nil && *run.PRID <= 0) || (run.RequestedByUserID != nil && *run.RequestedByUserID <= 0) {
		return fmt.Errorf("create review run %s: optional database IDs must be positive", run.RunID)
	}
	if run.TriggerSource == "" || run.Status == "" {
		return fmt.Errorf("create review run %s: trigger source and status are required", run.RunID)
	}
	if run.RequestedConfigJSON == "" || run.EffectiveConfigJSON == "" || run.ConfigSourcesJSON == "" {
		return fmt.Errorf("create review run %s: configuration snapshots are required", run.RunID)
	}
	if run.AcceptedAt.IsZero() || run.QueuedAt.IsZero() {
		return fmt.Errorf("create review run %s: accepted_at and queued_at are required", run.RunID)
	}
	if (run.IdempotencyScope == "") != (run.IdempotencyKeyHash == "") {
		return fmt.Errorf("create review run %s: idempotency scope and key hash must be set together", run.RunID)
	}
	model := reviewRunToModel(*run)
	if err := g.db.Create(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			var identityConflict ReviewRunModel
			if g.db.Where("run_id = ?", run.RunID).First(&identityConflict).Error == nil {
				return fmt.Errorf("%w: run_id=%s scope=%s (run ID or idempotency key conflict)", ErrReviewRunConflict, run.RunID, run.IdempotencyScope)
			}
			if run.IdempotencyScope != "" && run.IdempotencyKeyHash != "" &&
				g.db.Where("idempotency_scope = ? AND idempotency_key_hash = ?", run.IdempotencyScope, run.IdempotencyKeyHash).First(&identityConflict).Error == nil {
				return fmt.Errorf("%w: run_id=%s scope=%s (run ID or idempotency key conflict)", ErrReviewRunConflict, run.RunID, run.IdempotencyScope)
			}
			var live ReviewRunModel
			liveErr := g.db.Where(
				"LOWER(repo_owner) = LOWER(?) AND LOWER(repo_name) = LOWER(?) AND pr_number = ? AND status IN ?",
				run.RepoOwner, run.RepoName, run.PRNumber, []string{ReviewRunStatusQueued, ReviewRunStatusRunning},
			).First(&live).Error
			if liveErr == nil && live.RunID != run.RunID {
				return fmt.Errorf("%w: target=%s/%s#%d active_run_id=%s", ErrReviewRunActiveConflict, run.RepoOwner, run.RepoName, run.PRNumber, live.RunID)
			}
			return fmt.Errorf("%w: run_id=%s scope=%s (run ID or idempotency key conflict)", ErrReviewRunConflict, run.RunID, run.IdempotencyScope)
		}
		return fmt.Errorf("create review run %s: %w", run.RunID, err)
	}
	*run = reviewRunFromModel(model)
	return nil
}

func (g *GormDB) GetReviewRun(runID string) (*ReviewRun, error) {
	var model ReviewRunModel
	if err := g.db.Where("run_id = ?", runID).First(&model).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get review run %s: %w", runID, err)
	}
	run := reviewRunFromModel(model)
	return &run, nil
}

func (g *GormDB) GetReviewRunByIdempotency(scope, keyHash string) (*ReviewRun, error) {
	if scope == "" || keyHash == "" {
		return nil, nil
	}
	var model ReviewRunModel
	if err := g.db.Where("idempotency_scope = ? AND idempotency_key_hash = ?", scope, keyHash).First(&model).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get idempotent review run: %w", err)
	}
	run := reviewRunFromModel(model)
	return &run, nil
}

func (g *GormDB) ListReviewRuns(filter ReviewRunFilter) ([]ReviewRun, error) {
	query := g.db.Model(&ReviewRunModel{})
	if filter.RepoOwner != "" {
		query = query.Where("LOWER(repo_owner) = LOWER(?)", filter.RepoOwner)
	}
	if filter.RepoName != "" {
		query = query.Where("LOWER(repo_name) = LOWER(?)", filter.RepoName)
	}
	if filter.PRNumber > 0 {
		query = query.Where("pr_number = ?", filter.PRNumber)
	}
	if filter.CommitSHA != "" {
		query = query.Where("commit_sha = ?", filter.CommitSHA)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if !filter.BeforeAcceptedAt.IsZero() && filter.BeforeRunID != "" {
		query = query.Where(
			"(accepted_at < ?) OR (accepted_at = ? AND run_id < ?)",
			filter.BeforeAcceptedAt, filter.BeforeAcceptedAt, filter.BeforeRunID,
		)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxReviewRunListLimit {
		limit = MaxReviewRunListLimit
	}
	var models []ReviewRunModel
	if err := query.Order("accepted_at DESC, run_id DESC").Limit(limit).Find(&models).Error; err != nil {
		return nil, fmt.Errorf("list review runs: %w", err)
	}
	runs := make([]ReviewRun, len(models))
	for i, model := range models {
		runs[i] = reviewRunFromModel(model)
	}
	return runs, nil
}

func (g *GormDB) PatchReviewRun(runID string, patch ReviewRunPatch) error {
	updates := reviewRunPatchUpdates(patch)
	if len(updates) == 0 {
		return fmt.Errorf("patch review run %s: patch is empty", runID)
	}
	result := g.db.Model(&ReviewRunModel{}).Where("run_id = ?", runID).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("patch review run %s: %w", runID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("patch review run %s: not found", runID)
	}
	return nil
}

// PatchQueuedReviewRun applies a dispatch result only while the run remains
// queued. It races safely with ClaimReviewRun: exactly one transition wins.
func (g *GormDB) PatchQueuedReviewRun(runID string, patch ReviewRunPatch) (bool, error) {
	updates := reviewRunPatchUpdates(patch)
	if len(updates) == 0 {
		return false, fmt.Errorf("patch queued review run %s: patch is empty", runID)
	}
	result := g.db.Model(&ReviewRunModel{}).
		Where("run_id = ? AND status = ?", runID, ReviewRunStatusQueued).
		Updates(updates)
	if result.Error != nil {
		return false, fmt.Errorf("patch queued review run %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// ClaimOrRenewQueuedReviewRunLease records that an in-process dispatcher is
// still responsible for a durably accepted queued run. A different dispatcher
// may take over only after the previous lease expires.
func (g *GormDB) ClaimOrRenewQueuedReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	if runID == "" || holder == "" || now.IsZero() || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("claim queued review run lease: run ID, holder, and a future expiry are required")
	}
	result := g.db.Model(&ReviewRunModel{}).
		Where("run_id = ? AND status = ? AND (lease_holder = ? OR lease_holder = '' OR lease_expires_at IS NULL OR lease_expires_at <= ?)",
			runID, ReviewRunStatusQueued, holder, now).
		Updates(map[string]any{"lease_holder": holder, "lease_expires_at": leaseExpiresAt})
	if result.Error != nil {
		return false, fmt.Errorf("claim queued review run lease %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// PatchReviewRunAsHolder applies a worker lifecycle/result update only while
// holder still owns a live lease. The predicate fences out a stale worker
// after another worker has taken over the run.
func (g *GormDB) PatchReviewRunAsHolder(runID, holder string, now time.Time, patch ReviewRunPatch) (bool, error) {
	if runID == "" || holder == "" || now.IsZero() {
		return false, fmt.Errorf("patch review run as holder: run ID, holder, and current time are required")
	}
	updates := reviewRunPatchUpdates(patch)
	if len(updates) == 0 {
		return false, fmt.Errorf("patch review run as holder %s: patch is empty", runID)
	}
	result := g.db.Model(&ReviewRunModel{}).
		Where("run_id = ? AND status = ? AND lease_holder = ? AND lease_expires_at > ?",
			runID, ReviewRunStatusRunning, holder, now).
		Updates(updates)
	if result.Error != nil {
		return false, fmt.Errorf("patch review run as holder %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

func reviewRunPatchUpdates(patch ReviewRunPatch) map[string]any {
	updates := map[string]any{}
	if patch.Status != nil {
		updates["status"] = *patch.Status
	}
	if patch.StartedAt != nil {
		updates["started_at"] = *patch.StartedAt
	}
	if patch.CompletedAt != nil {
		updates["completed_at"] = *patch.CompletedAt
	}
	if patch.DurationMS != nil {
		updates["duration_ms"] = *patch.DurationMS
	}
	if patch.HTMLPath != nil {
		updates["html_path"] = *patch.HTMLPath
	}
	if patch.JSONPath != nil {
		updates["json_path"] = *patch.JSONPath
	}
	if patch.CriticalCount != nil {
		updates["critical_count"] = *patch.CriticalCount
	}
	if patch.MediumCount != nil {
		updates["medium_count"] = *patch.MediumCount
	}
	if patch.LowCount != nil {
		updates["low_count"] = *patch.LowCount
	}
	if patch.Verdict != nil {
		updates["verdict"] = *patch.Verdict
	}
	if patch.ModelFallback != nil {
		updates["model_fallback"] = *patch.ModelFallback
	}
	if patch.ServingModelVerification != nil {
		updates["serving_model_verification"] = *patch.ServingModelVerification
	}
	if patch.ActualModelsJSON != nil {
		updates["actual_models_json"] = *patch.ActualModelsJSON
	}
	if patch.PublicationStatus != nil {
		updates["publication_status"] = *patch.PublicationStatus
	}
	if patch.TerminalCode != nil {
		updates["terminal_code"] = *patch.TerminalCode
	}
	if patch.FailureStage != nil {
		updates["failure_stage"] = *patch.FailureStage
	}
	if patch.ErrorSummary != nil {
		updates["error_summary"] = *patch.ErrorSummary
	}
	if patch.LeaseHolder != nil {
		updates["lease_holder"] = *patch.LeaseHolder
	}
	if patch.LeaseExpiresAt != nil {
		if patch.LeaseExpiresAt.IsZero() {
			updates["lease_expires_at"] = nil
		} else {
			updates["lease_expires_at"] = *patch.LeaseExpiresAt
		}
	}
	if patch.ExecutionAttempt != nil {
		updates["execution_attempt"] = *patch.ExecutionAttempt
	}
	return updates
}

// ClaimReviewRun atomically acquires a queued run or takes over a running run
// whose lease expired. Exactly one concurrent worker can observe claimed=true.
func (g *GormDB) ClaimReviewRun(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	if runID == "" || holder == "" || now.IsZero() || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("claim review run: run ID, holder, and a future lease expiry are required")
	}
	result := g.db.Model(&ReviewRunModel{}).
		Where("run_id = ? AND (status = ? OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))",
			runID, ReviewRunStatusQueued, ReviewRunStatusRunning, now).
		Updates(map[string]any{
			"status":                     ReviewRunStatusRunning,
			"started_at":                 gorm.Expr("COALESCE(started_at, ?)", now),
			"completed_at":               nil,
			"duration_ms":                0,
			"html_path":                  "",
			"json_path":                  "",
			"critical_count":             0,
			"medium_count":               0,
			"low_count":                  0,
			"verdict":                    "",
			"model_fallback":             false,
			"serving_model_verification": "",
			"actual_models_json":         "",
			"publication_status":         "",
			"terminal_code":              "",
			"failure_stage":              "",
			"error_summary":              "",
			"lease_holder":               holder,
			"lease_expires_at":           leaseExpiresAt,
			"execution_attempt":          gorm.Expr("execution_attempt + 1"),
		})
	if result.Error != nil {
		return false, fmt.Errorf("claim review run %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// RenewReviewRunLease extends a live lease only for its current holder. An
// expired lease cannot be resurrected; the worker must claim the run again.
func (g *GormDB) RenewReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error) {
	if runID == "" || holder == "" || now.IsZero() || !leaseExpiresAt.After(now) {
		return false, fmt.Errorf("renew review run lease: run ID, holder, and a future lease expiry are required")
	}
	result := g.db.Model(&ReviewRunModel{}).
		Where("run_id = ? AND status = ? AND lease_holder = ? AND lease_expires_at > ?",
			runID, ReviewRunStatusRunning, holder, now).
		Update("lease_expires_at", leaseExpiresAt)
	if result.Error != nil {
		return false, fmt.Errorf("renew review run lease %s: %w", runID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// AbandonExpiredReviewRuns terminalizes running rows whose worker lease stayed
// expired through the grace period and queued rows left behind by a dispatcher
// crash. Both updates race safely with the atomic queued-to-running claim.
func (g *GormDB) AbandonExpiredReviewRuns(now time.Time, runningGrace, queuedMaxAge time.Duration) (int, error) {
	if now.IsZero() || runningGrace < 0 || queuedMaxAge <= 0 {
		return 0, fmt.Errorf("abandon expired review runs: current time, non-negative running grace, and positive queue max age are required")
	}
	runningCutoff := now.Add(-runningGrace)
	queuedCutoff := now.Add(-queuedMaxAge)
	abandoned := 0
	err := g.db.Transaction(func(tx *gorm.DB) error {
		var candidateIDs []string
		if err := tx.Model(&ReviewRunModel{}).
			Where("(status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?) OR (status = ? AND ((lease_expires_at IS NOT NULL AND lease_expires_at <= ?) OR (lease_expires_at IS NULL AND queued_at <= ?)))",
				ReviewRunStatusRunning, runningCutoff, ReviewRunStatusQueued, runningCutoff, queuedCutoff).
			Order("run_id ASC").
			Limit(reviewRunAbandonBatchSize).
			Pluck("run_id", &candidateIDs).Error; err != nil {
			return fmt.Errorf("list candidate rows: %w", err)
		}
		if len(candidateIDs) == 0 {
			return nil
		}
		running := tx.Model(&ReviewRunModel{}).
			Where("run_id IN ? AND status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?", candidateIDs, ReviewRunStatusRunning, runningCutoff).
			Updates(map[string]any{
				"status":           ReviewRunStatusTimedOut,
				"completed_at":     now,
				"terminal_code":    "lease_abandoned",
				"failure_stage":    "execution",
				"error_summary":    "review worker lease expired before terminal completion",
				"lease_holder":     "",
				"lease_expires_at": nil,
			})
		if running.Error != nil {
			return fmt.Errorf("running rows: %w", running.Error)
		}
		queued := tx.Model(&ReviewRunModel{}).
			Where("run_id IN ? AND status = ? AND ((lease_expires_at IS NOT NULL AND lease_expires_at <= ?) OR (lease_expires_at IS NULL AND queued_at <= ?))",
				candidateIDs, ReviewRunStatusQueued, runningCutoff, queuedCutoff).
			Updates(map[string]any{
				"status":           ReviewRunStatusTimedOut,
				"completed_at":     now,
				"terminal_code":    "queue_abandoned",
				"failure_stage":    "dispatch",
				"error_summary":    "review run remained queued beyond the dispatch recovery window",
				"lease_holder":     "",
				"lease_expires_at": nil,
			})
		if queued.Error != nil {
			return fmt.Errorf("queued rows: %w", queued.Error)
		}
		abandoned = int(running.RowsAffected + queued.RowsAffected)
		if abandoned == 0 {
			return nil
		}
		var runIDs []string
		if err := tx.Model(&ReviewRunModel{}).
			Where("run_id IN ? AND status = ? AND terminal_code IN ?", candidateIDs, ReviewRunStatusTimedOut, []string{"lease_abandoned", "queue_abandoned"}).
			Pluck("run_id", &runIDs).Error; err != nil {
			return fmt.Errorf("list abandoned rows: %w", err)
		}
		if len(runIDs) == 0 {
			return nil
		}
		result := tx.Model(&PRModel{}).
			Where("projection_run_id IN ?", runIDs).
			Where("status <> ?", "completed").
			Where(`NOT EXISTS (
				SELECT 1 FROM review_runs
				WHERE review_runs.repo_owner = prs.repo_owner
				  AND review_runs.repo_name = prs.repo_name
				  AND review_runs.pr_number = prs.pr_number
				  AND (review_runs.status = ? OR
				       (review_runs.status = ? AND (review_runs.lease_expires_at IS NULL OR review_runs.lease_expires_at > ?)))
			)`, ReviewRunStatusQueued, ReviewRunStatusRunning, now).
			Updates(map[string]any{
				"status": "error", "error_message": "review run abandoned after lease expiry",
				"last_reviewed_at": now, "generating_since": nil,
			})
		if result.Error != nil {
			return fmt.Errorf("project abandoned rows: %w", result.Error)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("abandon expired review runs: %w", err)
	}
	return abandoned, nil
}

func (g *GormDB) UpsertReviewStageAttempt(attempt *ReviewStageAttempt) error {
	if err := validateReviewStageAttempt(attempt); err != nil {
		return err
	}
	var model ReviewStageAttemptModel
	err := g.db.Transaction(func(tx *gorm.DB) error {
		var err error
		model, err = upsertReviewStageAttempt(tx, attempt)
		return err
	})
	if err != nil {
		return fmt.Errorf("upsert review stage attempt %s/%d/%s/%d/%d: %w", attempt.RunID, attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber, err)
	}
	*attempt = reviewStageAttemptFromModel(model)
	return nil
}

// UpsertReviewStageAttemptAsHolder records provider telemetry only while the
// originating execution attempt still owns a live lease. The parent-row lock
// shares the same serialization point as successful terminalization, so a
// stale worker cannot append or overwrite attempts after losing ownership.
func (g *GormDB) UpsertReviewStageAttemptAsHolder(attempt *ReviewStageAttempt, holder string, now time.Time) (bool, error) {
	if err := validateReviewStageAttempt(attempt); err != nil {
		return false, err
	}
	if holder == "" || now.IsZero() {
		return false, fmt.Errorf("upsert review stage attempt as holder: holder and current time are required")
	}

	accepted := false
	var model ReviewStageAttemptModel
	err := g.db.Transaction(func(tx *gorm.DB) error {
		locked := tx.Exec(`UPDATE review_runs SET updated_at = updated_at
			WHERE run_id = ? AND status = ? AND execution_attempt = ?
			  AND lease_holder = ?`,
			attempt.RunID, ReviewRunStatusRunning, attempt.ExecutionAttempt, holder)
		if locked.Error != nil {
			return fmt.Errorf("lock attempt parent run: %w", locked.Error)
		}
		if locked.RowsAffected != 1 {
			return nil
		}
		var run ReviewRunModel
		if err := tx.Where("run_id = ?", attempt.RunID).First(&run).Error; err != nil {
			return fmt.Errorf("load locked attempt parent run: %w", err)
		}
		leaseCheckedAt := time.Now().UTC()
		if now.After(leaseCheckedAt) {
			leaseCheckedAt = now
		}
		if run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(leaseCheckedAt) {
			return nil
		}
		var err error
		model, err = upsertReviewStageAttempt(tx, attempt)
		if err != nil {
			return err
		}
		accepted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("upsert review stage attempt as holder %s/%d/%s/%d/%d: %w",
			attempt.RunID, attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber, err)
	}
	if accepted {
		*attempt = reviewStageAttemptFromModel(model)
	}
	return accepted, nil
}

func validateReviewStageAttempt(attempt *ReviewStageAttempt) error {
	if attempt == nil {
		return fmt.Errorf("upsert review stage attempt: attempt is nil")
	}
	if attempt.RunID == "" || attempt.ExecutionAttempt <= 0 || attempt.Stage == "" || attempt.InvocationNumber <= 0 || attempt.AttemptNumber <= 0 {
		return fmt.Errorf("upsert review stage attempt: run ID, execution attempt, stage, invocation number, and attempt number are required")
	}
	return nil
}

func upsertReviewStageAttempt(tx *gorm.DB, attempt *ReviewStageAttempt) (ReviewStageAttemptModel, error) {
	model := reviewStageAttemptToModel(*attempt)
	// The natural attempt key owns the upsert. Never send a previously
	// round-tripped auto-increment ID, which could conflict independently on
	// SQLite/Postgres before the natural-key conflict is resolved.
	model.ID = 0
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "run_id"}, {Name: "execution_attempt"}, {Name: "stage"}, {Name: "invocation_number"}, {Name: "attempt_number"}},
		DoUpdates: clause.AssignmentColumns(reviewStageAttemptMutableColumns),
	}).Create(&model).Error; err != nil {
		return ReviewStageAttemptModel{}, fmt.Errorf("upsert: %w", err)
	}
	var persisted ReviewStageAttemptModel
	if err := tx.Where(
		"run_id = ? AND execution_attempt = ? AND stage = ? AND invocation_number = ? AND attempt_number = ?",
		attempt.RunID, attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber,
	).First(&persisted).Error; err != nil {
		return ReviewStageAttemptModel{}, fmt.Errorf("reload: %w", err)
	}
	return persisted, nil
}

var reviewStageAttemptMutableColumns = []string{
	"provider", "backend", "requested_model", "resolved_model",
	"observed_served_models", "primary_served_model", "served_model_source",
	"serving_model_verified", "fallback", "fallback_reason", "matcher_version",
	"effort", "status", "assistant_turns", "budget_units_used", "turn_budget_unit", "turn_budget_version",
	"input_tokens", "output_tokens",
	"total_tokens", "started_at", "completed_at", "duration_ms", "stop_reason",
	"error_code", "error_summary", "updated_at",
}

func (g *GormDB) ListReviewStageAttempts(runID string) ([]ReviewStageAttempt, error) {
	var models []ReviewStageAttemptModel
	err := g.db.Where("run_id = ?", runID).
		Order(`execution_attempt ASC,
			CASE stage
				WHEN 'first_pass' THEN 10
				WHEN 'classification' THEN 20
				WHEN 'classification_summary' THEN 20
				WHEN 'summary' THEN 30
				WHEN 'agent' THEN 40
				ELSE 90
			END ASC,
			invocation_number ASC, attempt_number ASC,
			CASE WHEN started_at IS NULL THEN 1 ELSE 0 END ASC,
			started_at ASC, id ASC`).
		Find(&models).Error
	if err != nil {
		return nil, fmt.Errorf("list review stage attempts for %s: %w", runID, err)
	}
	attempts := make([]ReviewStageAttempt, len(models))
	for i, model := range models {
		attempts[i] = reviewStageAttemptFromModel(model)
	}
	return attempts, nil
}

func reviewRunToModel(run ReviewRun) ReviewRunModel {
	return ReviewRunModel{
		RunID: run.RunID, PRID: intPtrToUint(run.PRID), RepoOwner: run.RepoOwner,
		RepoName: run.RepoName, PRNumber: run.PRNumber, CommitSHA: run.CommitSHA,
		RequestedByUserID: intPtrToUint(run.RequestedByUserID), TriggerSource: run.TriggerSource,
		Status: run.Status, RequestedConfigJSON: run.RequestedConfigJSON,
		EffectiveConfigJSON: run.EffectiveConfigJSON, ConfigSourcesJSON: run.ConfigSourcesJSON,
		ConfigHash: run.ConfigHash, ConfigSchemaVersion: run.ConfigSchemaVersion,
		AgentBackend: run.AgentBackend, AgentModel: run.AgentModel, AgentEffort: run.AgentEffort,
		AgentWallClockSec: run.AgentWallClockSec, AgentMaxTurns: run.AgentMaxTurns,
		AcceptedAt: run.AcceptedAt, QueuedAt: run.QueuedAt, StartedAt: run.StartedAt,
		CompletedAt: run.CompletedAt, DurationMS: run.DurationMS, HTMLPath: run.HTMLPath,
		JSONPath: run.JSONPath, CriticalCount: run.CriticalCount, MediumCount: run.MediumCount,
		LowCount: run.LowCount, Verdict: run.Verdict, ModelFallback: run.ModelFallback,
		ServingModelVerification: run.ServingModelVerification, ActualModelsJSON: run.ActualModelsJSON,
		PublicationStatus: run.PublicationStatus, TerminalCode: run.TerminalCode,
		FailureStage: run.FailureStage, ErrorSummary: run.ErrorSummary, ServiceRevision: run.ServiceRevision,
		LeaseHolder: run.LeaseHolder, LeaseExpiresAt: run.LeaseExpiresAt,
		ExecutionAttempt: run.ExecutionAttempt, IdempotencyScope: run.IdempotencyScope,
		IdempotencyKeyHash: run.IdempotencyKeyHash, RequestHash: run.RequestHash,
		CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
}

func reviewRunFromModel(model ReviewRunModel) ReviewRun {
	return ReviewRun{
		RunID: model.RunID, PRID: uintPtrToInt(model.PRID), RepoOwner: model.RepoOwner,
		RepoName: model.RepoName, PRNumber: model.PRNumber, CommitSHA: model.CommitSHA,
		RequestedByUserID: uintPtrToInt(model.RequestedByUserID), TriggerSource: model.TriggerSource,
		Status: model.Status, RequestedConfigJSON: model.RequestedConfigJSON,
		EffectiveConfigJSON: model.EffectiveConfigJSON, ConfigSourcesJSON: model.ConfigSourcesJSON,
		ConfigHash: model.ConfigHash, ConfigSchemaVersion: model.ConfigSchemaVersion,
		AgentBackend: model.AgentBackend, AgentModel: model.AgentModel, AgentEffort: model.AgentEffort,
		AgentWallClockSec: model.AgentWallClockSec, AgentMaxTurns: model.AgentMaxTurns,
		AcceptedAt: model.AcceptedAt, QueuedAt: model.QueuedAt, StartedAt: model.StartedAt,
		CompletedAt: model.CompletedAt, DurationMS: model.DurationMS, HTMLPath: model.HTMLPath,
		JSONPath: model.JSONPath, CriticalCount: model.CriticalCount, MediumCount: model.MediumCount,
		LowCount: model.LowCount, Verdict: model.Verdict, ModelFallback: model.ModelFallback,
		ServingModelVerification: model.ServingModelVerification, ActualModelsJSON: model.ActualModelsJSON,
		PublicationStatus: model.PublicationStatus, TerminalCode: model.TerminalCode,
		FailureStage: model.FailureStage, ErrorSummary: model.ErrorSummary, ServiceRevision: model.ServiceRevision,
		LeaseHolder: model.LeaseHolder, LeaseExpiresAt: model.LeaseExpiresAt,
		ExecutionAttempt: model.ExecutionAttempt, IdempotencyScope: model.IdempotencyScope,
		IdempotencyKeyHash: model.IdempotencyKeyHash, RequestHash: model.RequestHash,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func reviewStageAttemptToModel(attempt ReviewStageAttempt) ReviewStageAttemptModel {
	return ReviewStageAttemptModel{
		ID: uint(attempt.ID), ReviewRunID: attempt.RunID, ExecutionAttempt: attempt.ExecutionAttempt, Stage: attempt.Stage,
		InvocationNumber: attempt.InvocationNumber, AttemptNumber: attempt.AttemptNumber,
		Provider: attempt.Provider, Backend: attempt.Backend, RequestedModel: attempt.RequestedModel,
		ResolvedModel: attempt.ResolvedModel, ObservedServedModels: JSONStringArray(attempt.ObservedServedModels),
		PrimaryServedModel: attempt.PrimaryServedModel, ServedModelSource: attempt.ServedModelSource,
		ServingModelVerified: attempt.ServingModelVerified, Fallback: attempt.Fallback,
		FallbackReason: attempt.FallbackReason, MatcherVersion: attempt.MatcherVersion,
		Effort: attempt.Effort, Status: attempt.Status, AssistantTurns: attempt.AssistantTurns,
		BudgetUnitsUsed: attempt.BudgetUnitsUsed,
		TurnBudgetUnit:  attempt.TurnBudgetUnit, TurnBudgetVersion: attempt.TurnBudgetVersion,
		InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, TotalTokens: attempt.TotalTokens,
		StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt, DurationMS: attempt.DurationMS,
		StopReason: attempt.StopReason, ErrorCode: attempt.ErrorCode, ErrorSummary: attempt.ErrorSummary,
		CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt,
	}
}

func reviewStageAttemptFromModel(model ReviewStageAttemptModel) ReviewStageAttempt {
	return ReviewStageAttempt{
		ID: int(model.ID), RunID: model.ReviewRunID, ExecutionAttempt: model.ExecutionAttempt, Stage: model.Stage,
		InvocationNumber: model.InvocationNumber, AttemptNumber: model.AttemptNumber,
		Provider: model.Provider, Backend: model.Backend, RequestedModel: model.RequestedModel,
		ResolvedModel: model.ResolvedModel, ObservedServedModels: []string(model.ObservedServedModels),
		PrimaryServedModel: model.PrimaryServedModel, ServedModelSource: model.ServedModelSource,
		ServingModelVerified: model.ServingModelVerified, Fallback: model.Fallback,
		FallbackReason: model.FallbackReason, MatcherVersion: model.MatcherVersion,
		Effort: model.Effort, Status: model.Status, AssistantTurns: model.AssistantTurns,
		BudgetUnitsUsed: model.BudgetUnitsUsed,
		TurnBudgetUnit:  model.TurnBudgetUnit, TurnBudgetVersion: model.TurnBudgetVersion,
		InputTokens: model.InputTokens, OutputTokens: model.OutputTokens, TotalTokens: model.TotalTokens,
		StartedAt: model.StartedAt, CompletedAt: model.CompletedAt, DurationMS: model.DurationMS,
		StopReason: model.StopReason, ErrorCode: model.ErrorCode, ErrorSummary: model.ErrorSummary,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func intPtrToUint(value *int) *uint {
	if value == nil || *value <= 0 {
		return nil
	}
	converted := uint(*value)
	return &converted
}

func uintPtrToInt(value *uint) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}
