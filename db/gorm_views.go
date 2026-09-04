package db

import (
	"encoding/json"
	"log"

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
		UserHidden:     m.UserHidden,
		ViaManual:      m.ViaManual,
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

	// UserHidden and ViaManual are intentionally NOT mapped back: this
	// conversion feeds UpsertUserPRAssignment, and those columns must only
	// ever be written by SetUserHiddenForPR / EnsureManualPRView — never by
	// an upsert or poller path.
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
// NOTE: via_teams is intentionally excluded from DoUpdates, so this path can
// never overwrite it with empty/null values. Production writes via_teams
// through BatchUpsertUserPRViews and BatchPruneViaTeams, which the poller
// guards with shouldUpdateViaTeams.
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
// Single-row helper with no production callers: it survives for tests only,
// since the poller writes via_teams through BatchUpsertUserPRViews and clears
// it through BatchPruneViaTeams. An unguarded call with an empty slice would
// erase good data.
func (g *GormDB) UpdateUserViaTeams(userID, prID int, viaTeams []string) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("via_teams", JSONStringArray(viaTeams)).Error
}

// filterItemsWithExistingPRs returns the subset of items whose PRID exists in
// the prs table, plus the count of dropped items. A single SELECT covers all
// items rather than per-item existence checks. Used to defend against the
// poll-cycle TOCTOU where dbPRMap was snapshotted before a parallel
// PR delete (manual or cleanup-driven) removed the parent row.
func (g *GormDB) filterItemsWithExistingPRs(items []UserPRViewBatchItem) ([]UserPRViewBatchItem, int) {
	if len(items) == 0 {
		return items, 0
	}

	// Collect distinct PR IDs to query.
	idSet := make(map[int]struct{}, len(items))
	for _, it := range items {
		idSet[it.PRID] = struct{}{}
	}
	ids := make([]int, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	var existing []int
	if err := g.db.Model(&PRModel{}).Where("id IN ?", ids).Pluck("id", &existing).Error; err != nil {
		// On query failure, skip the filter rather than dropping the whole
		// batch. The original FK constraint will still catch any bad rows;
		// we'd just lose the warning-level diagnostic.
		log.Printf("[DB] WARN: filterItemsWithExistingPRs query failed; skipping pre-filter: %v", err)
		return items, 0
	}

	existingSet := make(map[int]struct{}, len(existing))
	for _, id := range existing {
		existingSet[id] = struct{}{}
	}

	kept := items[:0]
	dropped := 0
	for _, it := range items {
		if _, ok := existingSet[it.PRID]; ok {
			kept = append(kept, it)
		} else {
			dropped++
		}
	}
	return kept, dropped
}

// BatchUpsertUserPRViews batch-inserts or updates user_pr_view records.
// Items are grouped by which optional fields are set, and each group gets a single
// INSERT ... ON CONFLICT DO UPDATE with the appropriate columns.
//
// Defensively drops items whose pr_id no longer exists in `prs`. The poll
// cycle snapshots dbPRMap early and the views flush happens later; if a PR
// is deleted in between (manual delete via the dashboard, or closed-PR
// cleanup in another goroutine), the user_pr_views FK to prs(id) would
// fail and abort the entire batch. The pre-filter keeps the rest of the
// batch from taking collateral damage and logs a warning so it's still
// observable.
func (g *GormDB) BatchUpsertUserPRViews(items []UserPRViewBatchItem) error {
	if len(items) == 0 {
		return nil
	}

	items, dropped := g.filterItemsWithExistingPRs(items)
	if dropped > 0 {
		log.Printf("[DB] BatchUpsertUserPRViews: dropped %d item(s) referencing deleted PRs", dropped)
	}
	if len(items) == 0 {
		return nil
	}

	// Group items by which optional fields are set
	type group struct {
		models    []UserPRViewModel
		doUpdates []string
	}

	groups := map[string]*group{
		"ensure":       {doUpdates: []string{"hidden", "is_author"}},
		"review":       {doUpdates: []string{"hidden", "is_author", "review_status"}},
		"teams":        {doUpdates: []string{"hidden", "is_author", "via_teams"}},
		"review+teams": {doUpdates: []string{"hidden", "is_author", "review_status", "via_teams"}},
	}

	for _, item := range items {
		model := UserPRViewModel{
			UserID:   uint(item.UserID),
			PRID:     uint(item.PRID),
			IsAuthor: item.IsAuthor,
			Hidden:   false,
		}

		var groupKey string
		hasReview := item.ReviewStatus != nil
		hasTeams := item.ViaTeams != nil

		if hasReview && hasTeams {
			groupKey = "review+teams"
			model.ReviewStatus = *item.ReviewStatus
			model.ViaTeams = JSONStringArray(*item.ViaTeams)
		} else if hasReview {
			groupKey = "review"
			model.ReviewStatus = *item.ReviewStatus
		} else if hasTeams {
			groupKey = "teams"
			model.ViaTeams = JSONStringArray(*item.ViaTeams)
		} else {
			groupKey = "ensure"
		}

		groups[groupKey].models = append(groups[groupKey].models, model)
	}

	// Flush each group
	for _, grp := range groups {
		if len(grp.models) == 0 {
			continue
		}
		err := g.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "pr_id"}},
			DoUpdates: clause.AssignmentColumns(grp.doUpdates),
		}).CreateInBatches(&grp.models, 500).Error
		if err != nil {
			return err
		}
	}

	return nil
}

// GetUserPRViewsWithViaTeams returns all user_pr_views rows whose pr_id is in
// prIDs and whose via_teams holds at least one entry. Used by the poller's
// via_teams reconciliation pass (rows with empty via_teams have nothing to prune).
func (g *GormDB) GetUserPRViewsWithViaTeams(prIDs []int) ([]UserPRView, error) {
	if len(prIDs) == 0 {
		return nil, nil
	}

	var models []UserPRViewModel
	err := g.db.
		Where("pr_id IN ?", prIDs).
		Where("via_teams IS NOT NULL AND via_teams NOT IN ('', '[]', 'null')").
		Find(&models).Error
	if err != nil {
		return nil, err
	}

	views := make([]UserPRView, len(models))
	for i, m := range models {
		viaTeams := "[]"
		if m.ViaTeams != nil {
			if bytes, err := json.Marshal(m.ViaTeams); err == nil {
				viaTeams = string(bytes)
			}
		}
		views[i] = UserPRView{
			ID:           int(m.ID),
			UserID:       int(m.UserID),
			PRID:         int(m.PRID),
			IsAuthor:     m.IsAuthor,
			IsReviewer:   m.IsReviewer,
			ViaTeams:     viaTeams,
			ReviewStatus: m.ReviewStatus,
			Notes:        m.Notes,
			Hidden:       m.Hidden,
			UserHidden:   m.UserHidden,
			ViaManual:    m.ViaManual,
		}
	}
	return views, nil
}

// BatchPruneViaTeams clears via_teams on the given rows, additionally hiding
// the ones marked Hide. This is the deliberate counterpart to the
// shouldUpdateViaTeams guard: the poller calls it only for rows it has
// positively verified as stale (user off all reviewer teams, not personally
// requested), so writing an empty via_teams here is correct, not accidental.
func (g *GormDB) BatchPruneViaTeams(prunes []ViaTeamsPrune) error {
	if len(prunes) == 0 {
		return nil
	}

	return g.db.Transaction(func(tx *gorm.DB) error {
		for _, p := range prunes {
			updates := map[string]interface{}{"via_teams": JSONStringArray{}}
			if p.Hide {
				updates["hidden"] = true
			}
			err := tx.Model(&UserPRViewModel{}).
				Where("user_id = ? AND pr_id = ?", p.UserID, p.PRID).
				Updates(updates).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteAllUserPRViews removes all user_pr_view records for a user.
// Used in dev mode to reset stale views on startup.
func (g *GormDB) DeleteAllUserPRViews(userID int) (int64, error) {
	result := g.db.Where("user_id = ?", userID).Delete(&UserPRViewModel{})
	return result.RowsAffected, result.Error
}

// HidePRForUser hides a PR from a user's view (soft delete)
func (g *GormDB) HidePRForUser(userID, prID int) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("hidden", true).Error
}

// SetUserHiddenForPR sets the user-initiated hidden toggle for a user's PR
// view. Unlike HidePRForUser (poller-managed soft delete), user_hidden is
// only ever written here, so it survives poll cycles until the user flips
// it back. Returns ErrUserPRViewNotFound when the user has no view row for
// the PR (both SQLite and Postgres count rows matched, so an idempotent
// re-set of the same value still reports 1).
func (g *GormDB) SetUserHiddenForPR(userID, prID int, hidden bool) error {
	res := g.db.Model(&UserPRViewModel{}).
		Where("user_id = ? AND pr_id = ?", userID, prID).
		Update("user_hidden", hidden)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrUserPRViewNotFound
	}
	return nil
}

// EnsureUserPRView creates a user_pr_view record if it doesn't exist,
// or un-hides it if it was previously hidden.
// NOTE: On conflict, ONLY updates "hidden". All other fields (including via_teams)
// are preserved. New records start with via_teams=NULL, which is populated later
// by BatchUpsertUserPRViews in the poller's reviewer-groups phase.
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

// EnsureManualPRView records that the user manually requested a review for
// this PR: creates the view row if missing, else sets via_manual=true and
// clears both hidden flags — a paste-a-URL request is explicit, so it fully
// resurfaces a previously deleted or user-hidden entry. Notes and via_teams
// are preserved.
func (g *GormDB) EnsureManualPRView(userID, prID int, isAuthor bool) error {
	view := &UserPRViewModel{
		UserID:    uint(userID),
		PRID:      uint(prID),
		IsAuthor:  isAuthor,
		ViaManual: true,
	}

	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "pr_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"via_manual", "hidden", "user_hidden"}),
	}).Create(view).Error
}

// HideNonManualViewsForPR soft-hides every view of a PR except live manual
// claims. Used when closed-PR cleanup retains a PR for its manual claimants:
// everyone else's dashboard should behave as if the row had been cleaned up.
func (g *GormDB) HideNonManualViewsForPR(prID int) error {
	return g.db.Model(&UserPRViewModel{}).
		Where("pr_id = ? AND via_manual = ? AND hidden = ?", prID, false, false).
		Update("hidden", true).Error
}

// GetPRIDsWithManualClaims returns the set of pr_ids that at least one user
// manually requested and has not since deleted (via_manual=true, hidden=false).
// The closed-PR cleanup skips these so a manually requested review stays on
// the requester's dashboard until they delete it.
func (g *GormDB) GetPRIDsWithManualClaims() (map[int]bool, error) {
	var prIDs []int
	err := g.db.Model(&UserPRViewModel{}).
		Where("via_manual = ? AND hidden = ?", true, false).
		Distinct().
		Pluck("pr_id", &prIDs).Error
	if err != nil {
		return nil, err
	}
	claims := make(map[int]bool, len(prIDs))
	for _, id := range prIDs {
		claims[id] = true
	}
	return claims, nil
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
		UserHidden   bool            `gorm:"column:user_hidden"`
		ViaManual    bool            `gorm:"column:via_manual"`
	}

	err := g.db.Table("prs").
		Select(`prs.*,
			user_pr_views.is_author,
			user_pr_views.is_reviewer,
			user_pr_views.notes as user_notes,
			user_pr_views.review_status,
			user_pr_views.via_teams,
			user_pr_views.user_hidden,
			user_pr_views.via_manual`).
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
			UserHidden:   r.UserHidden,
			ViaManual:    r.ViaManual,
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
