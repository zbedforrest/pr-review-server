package db

import (
	"encoding/json"
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

// UpsertPR inserts or updates a PR
func (g *GormDB) UpsertPR(pr *PR) error {
	model := prToPRModel(pr)

	// Use GORM's Clauses for upsert behavior
	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"last_commit_sha",
			"review_path",
			"status",
			"title",
			"author",
			"approval_count",
			"my_review_status",
			"draft",
			"ci_state",
			"ci_failed_checks",
		}),
	}).Create(model).Error
}

// BatchUpsertPRs inserts or updates multiple PRs in a single batch operation.
func (g *GormDB) BatchUpsertPRs(prs []*PR) error {
	if len(prs) == 0 {
		return nil
	}

	models := make([]PRModel, len(prs))
	for i, p := range prs {
		models[i] = *prToPRModel(p)
	}

	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"last_commit_sha",
			"review_path",
			"status",
			"title",
			"author",
			"approval_count",
			"my_review_status",
			"draft",
			"ci_state",
			"ci_failed_checks",
		}),
	}).CreateInBatches(&models, 500).Error
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

	return g.db.Clauses(clause.OnConflict{
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
}

// GetAllPRs returns all PRs ordered by status priority
func (g *GormDB) GetAllPRs() ([]PR, error) {
	var models []PRModel
	result := g.db.Order(`
		created_at DESC NULLS LAST,
		CASE status
			WHEN 'generating' THEN 1
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
