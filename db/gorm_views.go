package db

import (
	"encoding/json"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// userPRViewModelToUserPRAssignment converts a UserPRViewModel to a UserPRAssignment interface type
func userPRViewModelToUserPRAssignment(m *UserPRViewModel) *UserPRAssignment {
	if m == nil {
		return nil
	}

	viaTeams := "[]"
	if m.ViaTeams != nil {
		if bytes, err := json.Marshal(m.ViaTeams); err == nil {
			viaTeams = string(bytes)
		}
	}

	return &UserPRAssignment{
		ID:             int(m.ID),
		UserID:         int(m.UserID),
		PRID:           int(m.PRID),
		IsAuthor:       m.IsAuthor,
		IsReviewer:     m.IsReviewer,
		ReviewerGroups: viaTeams, // Note: renamed to ViaTeams in model, but interface still uses ReviewerGroups
		MyReviewStatus: m.ReviewStatus,
		Notes:          m.Notes,
	}
}

// userPRAssignmentToUserPRViewModel converts a UserPRAssignment interface type to a UserPRViewModel
func userPRAssignmentToUserPRViewModel(a *UserPRAssignment) *UserPRViewModel {
	if a == nil {
		return nil
	}

	var viaTeams JSONStringArray
	if a.ReviewerGroups != "" {
		_ = json.Unmarshal([]byte(a.ReviewerGroups), &viaTeams)
	}

	return &UserPRViewModel{
		ID:           uint(a.ID),
		UserID:       uint(a.UserID),
		PRID:         uint(a.PRID),
		IsAuthor:     a.IsAuthor,
		IsReviewer:   a.IsReviewer,
		ViaTeams:     viaTeams,
		ReviewStatus: a.MyReviewStatus,
		Notes:        a.Notes,
	}
}

// GetUserPRAssignment retrieves a user's PR view by user ID and PR ID
func (g *GormDB) GetUserPRAssignment(userID, prID int) (*UserPRAssignment, error) {
	var model UserPRViewModel
	result := g.db.Where("user_id = ? AND pr_id = ?", userID, prID).First(&model)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return userPRViewModelToUserPRAssignment(&model), nil
}

// UpsertUserPRAssignment inserts or updates a user's PR view.
// NOTE: via_teams is intentionally excluded from DoUpdates. The ONLY code path
// that should write via_teams is UpdateUserViaTeams, guarded by shouldUpdateViaTeams
// in the poller. This prevents accidental overwrites with empty/null values.
func (g *GormDB) UpsertUserPRAssignment(assignment *UserPRAssignment) error {
	model := userPRAssignmentToUserPRViewModel(assignment)

	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "pr_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"is_author",
			"is_reviewer",
			"review_status",
			"notes",
		}),
	}).Create(model).Error
}

// GetPRsForUser returns all PRs for a specific user, joining prs with user_pr_views
func (g *GormDB) GetPRsForUser(userID int) ([]PR, error) {
	var models []PRModel

	err := g.db.Table("prs").
		Select("prs.*").
		Joins("INNER JOIN user_pr_views ON prs.id = user_pr_views.pr_id").
		Where("user_pr_views.user_id = ?", userID).
		Where("user_pr_views.hidden = ?", false).
		Order(`
			user_pr_views.is_author ASC,
			prs.created_at DESC NULLS LAST,
			CASE prs.status
				WHEN 'generating' THEN 1
				WHEN 'pending' THEN 2
				WHEN 'completed' THEN 3
				ELSE 4
			END
		`).
		Find(&models).Error

	if err != nil {
		return nil, err
	}

	prs := make([]PR, len(models))
	for i, m := range models {
		prs[i] = *prModelToPR(&m)
	}

	return prs, nil
}

// UpdateUserPRNotes updates the notes field for a user's PR view
func (g *GormDB) UpdateUserPRNotes(userID, prID int, notes string) error {
	// Truncate to 15 chars as defensive measure
	if len(notes) > 15 {
		notes = notes[:15]
	}

	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("notes", notes).Error
}

// UpdateUserReviewStatus updates the review_status field for a user's PR view
func (g *GormDB) UpdateUserReviewStatus(userID, prID int, reviewStatus string) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("review_status", reviewStatus).Error
}

// UpdateUserViaTeams updates the via_teams field for a user's PR view.
// This is the ONLY function that should write via_teams. Callers MUST guard
// with shouldUpdateViaTeams() to prevent empty/nil values from overwriting good data.
func (g *GormDB) UpdateUserViaTeams(userID, prID int, viaTeams []string) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("via_teams", JSONStringArray(viaTeams)).Error
}

// HidePRForUser hides a PR from a user's view (soft delete)
func (g *GormDB) HidePRForUser(userID, prID int) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("hidden", true).Error
}

// EnsureUserPRView creates a user_pr_view record if it doesn't exist,
// or un-hides it if it was previously hidden.
// NOTE: On conflict, ONLY updates "hidden". All other fields (including via_teams)
// are preserved. New records start with via_teams=NULL, which is populated later
// by UpdateUserViaTeams in the poller's reviewer-groups phase.
func (g *GormDB) EnsureUserPRView(userID, prID int, isAuthor bool) error {
	view := &UserPRViewModel{
		UserID:   uint(userID),
		PRID:     uint(prID),
		IsAuthor: isAuthor,
	}

	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "pr_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"hidden"}),
	}).Create(view).Error
}

// GetPRsForUserWithNotes returns all PRs for a user, with user-specific view data merged
func (g *GormDB) GetPRsForUserWithNotes(userID int) ([]PRWithUserView, error) {
	var results []struct {
		PRModel
		IsAuthor     bool            `gorm:"column:is_author"`
		IsReviewer   bool            `gorm:"column:is_reviewer"`
		UserNotes    string          `gorm:"column:user_notes"`
		ReviewStatus string          `gorm:"column:review_status"`
		ViaTeams     JSONStringArray `gorm:"column:via_teams"`
	}

	err := g.db.Table("prs").
		Select(`prs.*,
			user_pr_views.is_author,
			user_pr_views.is_reviewer,
			user_pr_views.notes as user_notes,
			user_pr_views.review_status,
			user_pr_views.via_teams`).
		Joins("INNER JOIN user_pr_views ON prs.id = user_pr_views.pr_id").
		Where("user_pr_views.user_id = ?", userID).
		Where("user_pr_views.hidden = ?", false).
		Order(`
			user_pr_views.is_author ASC,
			prs.created_at DESC NULLS LAST,
			CASE prs.status
				WHEN 'generating' THEN 1
				WHEN 'pending' THEN 2
				WHEN 'completed' THEN 3
				ELSE 4
			END
		`).
		Find(&results).Error

	if err != nil {
		return nil, err
	}

	prsWithViews := make([]PRWithUserView, len(results))
	for i, r := range results {
		pr := prModelToPR(&r.PRModel)
		prsWithViews[i] = PRWithUserView{
			PR:           *pr,
			IsAuthor:     r.IsAuthor,
			IsReviewer:   r.IsReviewer,
			UserNotes:    r.UserNotes,
			ReviewStatus: r.ReviewStatus,
			ViaTeams:     r.ViaTeams,
		}
	}

	return prsWithViews, nil
}

// MigrateLegacyNotes copies notes from prs.notes to user_pr_views.notes for a given user
// This is used during the migration from single-user to unified mode
func (g *GormDB) MigrateLegacyNotes(userID int) (int, error) {
	// Find all PRs with notes that the user has a view for
	result := g.db.Exec(`
		UPDATE user_pr_views
		SET notes = prs.notes
		FROM prs
		WHERE user_pr_views.pr_id = prs.id
			AND user_pr_views.user_id = ?
			AND prs.notes IS NOT NULL
			AND prs.notes != ''
			AND (user_pr_views.notes IS NULL OR user_pr_views.notes = '')
	`, userID)

	if result.Error != nil {
		return 0, result.Error
	}

	return int(result.RowsAffected), nil
}
