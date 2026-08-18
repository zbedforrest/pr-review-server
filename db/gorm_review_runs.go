package db

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
		query = query.Where("repo_owner = ?", filter.RepoOwner)
	}
	if filter.RepoName != "" {
		query = query.Where("repo_name = ?", filter.RepoName)
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
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
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

func (g *GormDB) UpsertReviewStageAttempt(attempt *ReviewStageAttempt) error {
	if attempt == nil {
		return fmt.Errorf("upsert review stage attempt: attempt is nil")
	}
	if attempt.RunID == "" || attempt.ExecutionAttempt <= 0 || attempt.Stage == "" || attempt.InvocationNumber <= 0 || attempt.AttemptNumber <= 0 {
		return fmt.Errorf("upsert review stage attempt: run ID, execution attempt, stage, invocation number, and attempt number are required")
	}
	model := reviewStageAttemptToModel(*attempt)
	// The natural attempt key owns the upsert. Never send a previously
	// round-tripped auto-increment ID, which could conflict independently on
	// SQLite/Postgres before the natural-key conflict is resolved.
	model.ID = 0
	err := g.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}, {Name: "execution_attempt"}, {Name: "stage"}, {Name: "invocation_number"}, {Name: "attempt_number"}},
			DoUpdates: clause.AssignmentColumns(reviewStageAttemptMutableColumns),
		}).Create(&model).Error; err != nil {
			return fmt.Errorf("upsert: %w", err)
		}
		var persisted ReviewStageAttemptModel
		if err := tx.Where(
			"run_id = ? AND execution_attempt = ? AND stage = ? AND invocation_number = ? AND attempt_number = ?",
			attempt.RunID, attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber,
		).First(&persisted).Error; err != nil {
			return fmt.Errorf("reload: %w", err)
		}
		model = persisted
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert review stage attempt %s/%d/%s/%d/%d: %w", attempt.RunID, attempt.ExecutionAttempt, attempt.Stage, attempt.InvocationNumber, attempt.AttemptNumber, err)
	}
	*attempt = reviewStageAttemptFromModel(model)
	return nil
}

var reviewStageAttemptMutableColumns = []string{
	"provider", "backend", "requested_model", "resolved_model",
	"observed_served_models", "primary_served_model", "served_model_source",
	"serving_model_verified", "fallback", "fallback_reason", "matcher_version",
	"effort", "status", "assistant_turns", "input_tokens", "output_tokens",
	"total_tokens", "started_at", "completed_at", "duration_ms", "stop_reason",
	"error_code", "error_summary", "updated_at",
}

func (g *GormDB) ListReviewStageAttempts(runID string) ([]ReviewStageAttempt, error) {
	var models []ReviewStageAttemptModel
	err := g.db.Where("run_id = ?", runID).
		Order("execution_attempt ASC, stage ASC, invocation_number ASC, attempt_number ASC").
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
		ID: uint(attempt.ID), RunID: attempt.RunID, ExecutionAttempt: attempt.ExecutionAttempt, Stage: attempt.Stage,
		InvocationNumber: attempt.InvocationNumber, AttemptNumber: attempt.AttemptNumber,
		Provider: attempt.Provider, Backend: attempt.Backend, RequestedModel: attempt.RequestedModel,
		ResolvedModel: attempt.ResolvedModel, ObservedServedModels: JSONStringArray(attempt.ObservedServedModels),
		PrimaryServedModel: attempt.PrimaryServedModel, ServedModelSource: attempt.ServedModelSource,
		ServingModelVerified: attempt.ServingModelVerified, Fallback: attempt.Fallback,
		FallbackReason: attempt.FallbackReason, MatcherVersion: attempt.MatcherVersion,
		Effort: attempt.Effort, Status: attempt.Status, AssistantTurns: attempt.AssistantTurns,
		InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, TotalTokens: attempt.TotalTokens,
		StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt, DurationMS: attempt.DurationMS,
		StopReason: attempt.StopReason, ErrorCode: attempt.ErrorCode, ErrorSummary: attempt.ErrorSummary,
		CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt,
	}
}

func reviewStageAttemptFromModel(model ReviewStageAttemptModel) ReviewStageAttempt {
	return ReviewStageAttempt{
		ID: int(model.ID), RunID: model.RunID, ExecutionAttempt: model.ExecutionAttempt, Stage: model.Stage,
		InvocationNumber: model.InvocationNumber, AttemptNumber: model.AttemptNumber,
		Provider: model.Provider, Backend: model.Backend, RequestedModel: model.RequestedModel,
		ResolvedModel: model.ResolvedModel, ObservedServedModels: []string(model.ObservedServedModels),
		PrimaryServedModel: model.PrimaryServedModel, ServedModelSource: model.ServedModelSource,
		ServingModelVerified: model.ServingModelVerified, Fallback: model.Fallback,
		FallbackReason: model.FallbackReason, MatcherVersion: model.MatcherVersion,
		Effort: model.Effort, Status: model.Status, AssistantTurns: model.AssistantTurns,
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
