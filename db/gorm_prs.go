package db

import (
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// prModelToPR converts a PRModel to a PR interface type
func prModelToPR(m *PRModel) *PR {
	if m == nil {
		return nil
	}

	ciFailedChecks := "[]"
	if m.CIFailedChecks != nil {
		if bytes, err := json.Marshal(m.CIFailedChecks); err == nil {
			ciFailedChecks = string(bytes)
		}
	}

	return &PR{
		ID:              int(m.ID),
		RepoOwner:       m.RepoOwner,
		RepoName:        m.RepoName,
		PRNumber:        m.PRNumber,
		LastCommitSHA:   m.LastCommitSHA,
		LastReviewedAt:  m.LastReviewedAt,
		ReviewHTMLPath:  m.ReviewPath,
		Status:          m.Status,
		GeneratingSince: m.GeneratingSince,
		Title:           m.Title,
		Author:          m.Author,
		ApprovalCount:   m.ApprovalCount,
		MyReviewStatus:  m.MyReviewStatus,
		CreatedAt:       m.CreatedAt,
		Draft:           m.Draft,
		CIState:         m.CIState,
		CIFailedChecks:  ciFailedChecks,
		CriticalCount:   m.CriticalCount,
		MediumCount:     m.MediumCount,
		LowCount:        m.LowCount,
		Notes:           m.Notes,
		GitHubUpdatedAt: m.GitHubUpdatedAt,
		ErrorMessage:    m.ErrorMessage,
	}
}

// prToPRModel converts a PR interface type to a PRModel
func prToPRModel(p *PR) *PRModel {
	if p == nil {
		return nil
	}

	var ciFailedChecks JSONStringArray
	if p.CIFailedChecks != "" {
		_ = json.Unmarshal([]byte(p.CIFailedChecks), &ciFailedChecks)
	}

	return &PRModel{
		ID:              uint(p.ID),
		RepoOwner:       p.RepoOwner,
		RepoName:        p.RepoName,
		PRNumber:        p.PRNumber,
		Title:           p.Title,
		Author:          p.Author,
		LastCommitSHA:   p.LastCommitSHA,
		Status:          p.Status,
		ReviewPath:      p.ReviewHTMLPath,
		LastReviewedAt:  p.LastReviewedAt,
		GeneratingSince: p.GeneratingSince,
		CreatedAt:       p.CreatedAt,
		Draft:           p.Draft,
		ApprovalCount:   p.ApprovalCount,
		MyReviewStatus:  p.MyReviewStatus,
		CIState:         p.CIState,
		CIFailedChecks:  ciFailedChecks,
		CriticalCount:   p.CriticalCount,
		MediumCount:     p.MediumCount,
		LowCount:        p.LowCount,
		Notes:           p.Notes,
		GitHubUpdatedAt: p.GitHubUpdatedAt,
		ErrorMessage:    p.ErrorMessage,
	}
}

// GetPR retrieves a PR by owner, repo, and PR number
func (g *GormDB) GetPR(owner, repo string, prNumber int) (*PR, error) {
	var model PRModel
	result := g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).First(&model)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return prModelToPR(&model), nil
}

// upsertMetadataColumns is the set of columns UpsertPR / BatchUpsertPRs are
// allowed to touch on conflict. Excludes status / review_path / importance
// counts so a stale read from the polling cycle can't clobber a fresh write
// from the orchestration goroutine — those have dedicated setters
// (SetPRGenerating, SetPRAgentReviewing, SetPRError, MarkPRCompleted).
//
// approval_count, my_review_status, ci_state, ci_failed_checks ARE included:
// the poller's reviewPRBatch / ciPRBatch flushes in poller.go are the only
// writers for those columns, so the stale-clobber concern doesn't apply.
var upsertMetadataColumns = []string{
	"last_commit_sha",
	"title",
	"author",
	"draft",
	"created_at",
	"github_updated_at",
	"approval_count",
	"my_review_status",
	"ci_state",
	"ci_failed_checks",
}

// UpsertPR inserts or updates a PR's metadata. Status / review_path /
// importance counts are NOT touched on conflict — use the dedicated setters
// (SetPRGenerating, SetPRAgentReviewing, SetPRError, MarkPRCompleted) to
// transition those fields safely.
func (g *GormDB) UpsertPR(pr *PR) error {
	model := prToPRModel(pr)

	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns(upsertMetadataColumns),
	}).Create(model).Error
}

// BatchUpsertPRs is the batch equivalent of UpsertPR. Same narrow column set.
func (g *GormDB) BatchUpsertPRs(prs []*PR) error {
	if len(prs) == 0 {
		return nil
	}

	models := make([]PRModel, len(prs))
	for i, p := range prs {
		models[i] = *prToPRModel(p)
	}

	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns(upsertMetadataColumns),
	}).CreateInBatches(&models, 500).Error
}

// MarkPRCompleted atomically transitions a PR to "completed" and records the
// review's persisted location + importance counts. Uses an explicit UPDATE
// (not an upsert) so it can't be defeated by a concurrent stale-read from
// the polling cycle.
func (g *GormDB) MarkPRCompleted(owner, repo string, prNumber int, commitSHA, reviewPath string, critical, medium, low int) error {
	now := time.Now().UTC()

	// Diagnostic: read status before the UPDATE so we know what we were
	// transitioning from.
	var before PRModel
	_ = g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).First(&before).Error

	res := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(map[string]interface{}{
			"status":           "completed",
			"review_path":      reviewPath,
			"last_commit_sha":  commitSHA,
			"last_reviewed_at": now,
			"critical_count":   critical,
			"medium_count":     medium,
			"low_count":        low,
			"error_message":    "",
		})
	if res.Error != nil {
		return res.Error
	}

	// Diagnostic: read status after the UPDATE to confirm the write took.
	var after PRModel
	_ = g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).First(&after).Error
	beforeReviewedAt := ""
	if before.LastReviewedAt != nil {
		beforeReviewedAt = before.LastReviewedAt.Format("15:04:05.000")
	}
	afterReviewedAt := ""
	if after.LastReviewedAt != nil {
		afterReviewedAt = after.LastReviewedAt.Format("15:04:05.000")
	}
	log.Printf("[DB] MarkPRCompleted %s/%s#%d: rows=%d status_before=%q status_after=%q lr_before=%s lr_after=%s now=%s",
		owner, repo, prNumber, res.RowsAffected, before.Status, after.Status, beforeReviewedAt, afterReviewedAt, now.Format("15:04:05.000"))
	return nil
}

// UpdatePRStatus updates the status of a PR
func (g *GormDB) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	updates := map[string]interface{}{"status": status}

	// When marking as error, set last_reviewed_at to track when the error occurred
	if status == "error" {
		now := time.Now().UTC()
		updates["last_reviewed_at"] = now
	}

	// When marking as generating, also set the generating_since timestamp
	if status == "generating" {
		now := time.Now().UTC()
		updates["generating_since"] = now
	}

	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(updates).Error
}

// ResetPRToOutdated resets a PR to pending status with new commit SHA and clears old review data
func (g *GormDB) ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error {
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(map[string]interface{}{
			"status":           "pending",
			"last_commit_sha":  newCommitSHA,
			"review_path":      nil,
			"last_reviewed_at": nil,
			"generating_since": nil,
		}).Error
}

// SetPRAgentReviewing moves an existing PR to the agent_reviewing status.
// Unlike SetPRGenerating it does not create a row — the PR must already exist
// (the Gemini stage creates it). Clears any stored error_message from an
// earlier failed run of this commit.
func (g *GormDB) SetPRAgentReviewing(owner, repo string, prNumber int) error {
	res := g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(map[string]interface{}{
			"status":        "agent_reviewing",
			"error_message": "",
		})
	if res.Error == nil {
		var after PRModel
		_ = g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).First(&after).Error
		log.Printf("[DB] SetPRAgentReviewing %s/%s#%d: rows=%d status_after=%q", owner, repo, prNumber, res.RowsAffected, after.Status)
	}
	return res.Error
}

// SetPRError marks a PR as error and stores a human-readable message.
// Replaces UpdatePRStatus(..., "error") so the message isn't lost.
func (g *GormDB) SetPRError(owner, repo string, prNumber int, message string) error {
	now := time.Now().UTC()
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(map[string]interface{}{
			"status":           "error",
			"error_message":    message,
			"last_reviewed_at": now,
		}).Error
}

// SetPRGenerating creates or updates a PR and sets it to generating status
func (g *GormDB) SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool) error {
	now := time.Now().UTC()

	model := &PRModel{
		RepoOwner:       owner,
		RepoName:        repo,
		PRNumber:        prNumber,
		LastCommitSHA:   commitSHA,
		Status:          "generating",
		GeneratingSince: &now,
		Title:           title,
		Author:          author,
		CreatedAt:       createdAt,
		Draft:           draft,
	}

	err := g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"last_commit_sha",
			"status",
			"generating_since",
			"title",
			"author",
			"created_at",
			"draft",
		}),
	}).Create(model).Error
	if err != nil {
		return err
	}
	// A fresh generation attempt supersedes any prior error for this PR. This
	// is a best-effort write; a missing error_message column (e.g. if the
	// migration was skipped) shouldn't block the trigger.
	_ = g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Update("error_message", "").Error
	return nil
}

// GetAllPRs returns all PRs ordered by status priority
func (g *GormDB) GetAllPRs() ([]PR, error) {
	var models []PRModel
	result := g.db.Order(`
		created_at DESC NULLS LAST,
		CASE status
			WHEN 'generating' THEN 1
			WHEN 'agent_reviewing' THEN 1
			WHEN 'pending' THEN 2
			WHEN 'completed' THEN 3
			ELSE 4
		END
	`).Find(&models)

	if result.Error != nil {
		return nil, result.Error
	}

	prs := make([]PR, len(models))
	for i, m := range models {
		prs[i] = *prModelToPR(&m)
	}

	return prs, nil
}

// DeletePR removes a PR from the database
func (g *GormDB) DeletePR(owner, repo string, prNumber int) error {
	return g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Delete(&PRModel{}).Error
}

// ResetStaleGeneratingPRs resets PRs that have been in "generating" status for too long
func (g *GormDB) ResetStaleGeneratingPRs(timeoutMinutes int) (int, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(timeoutMinutes) * time.Minute)
	result := g.db.Model(&PRModel{}).
		Where("status = ?", "generating").
		Where("generating_since IS NULL OR generating_since < ?", cutoff).
		Updates(map[string]interface{}{
			"status":           "pending",
			"generating_since": nil,
		})

	if result.Error != nil {
		return 0, result.Error
	}

	return int(result.RowsAffected), nil
}

// ResetErrorPRs resets PRs that have been in "error" status for too long
func (g *GormDB) ResetErrorPRs(maxAgeMinutes int) (int, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(maxAgeMinutes) * time.Minute)
	result := g.db.Model(&PRModel{}).
		Where("status = ?", "error").
		Where("last_reviewed_at IS NULL OR last_reviewed_at < ?", cutoff).
		Update("status", "pending")

	if result.Error != nil {
		return 0, result.Error
	}

	return int(result.RowsAffected), nil
}

// GetPRsWithMissingMetadata returns PRs that don't have title or author set
func (g *GormDB) GetPRsWithMissingMetadata() ([]PR, error) {
	var models []PRModel
	result := g.db.Where("(title IS NULL OR title = '') OR (author IS NULL OR author = '')").Find(&models)

	if result.Error != nil {
		return nil, result.Error
	}

	prs := make([]PR, len(models))
	for i, m := range models {
		prs[i] = *prModelToPR(&m)
	}

	return prs, nil
}

// UpdatePRMetadata updates only the title and author for a PR
func (g *GormDB) UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error {
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Updates(map[string]interface{}{
			"title":  title,
			"author": author,
		}).Error
}

// UpdatePRNotes updates only the notes field for a PR
// Note: In multi-user mode with GORM, notes are stored in user_pr_views
// This method is kept for backward compatibility in single-user mode
func (g *GormDB) UpdatePRNotes(owner, repo string, prNumber int, notes string) error {
	// Truncate to 15 chars as defensive measure
	if len(notes) > 15 {
		notes = notes[:15]
	}
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Update("notes", notes).Error
}

// GetPRsWithMissingCreatedAt returns PRs that don't have created_at set
func (g *GormDB) GetPRsWithMissingCreatedAt() ([]PR, error) {
	var models []PRModel
	result := g.db.Where("created_at IS NULL").Find(&models)

	if result.Error != nil {
		return nil, result.Error
	}

	prs := make([]PR, len(models))
	for i, m := range models {
		prs[i] = *prModelToPR(&m)
	}

	return prs, nil
}

// UpdatePRCreatedAt updates only the created_at field for a PR
func (g *GormDB) UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error {
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Update("created_at", createdAt).Error
}

// UpdatePRGitHubUpdatedAt updates the github_updated_at field for poll economy change detection
func (g *GormDB) UpdatePRGitHubUpdatedAt(owner, repo string, prNumber int, updatedAt time.Time) error {
	return g.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Update("github_updated_at", updatedAt).Error
}
