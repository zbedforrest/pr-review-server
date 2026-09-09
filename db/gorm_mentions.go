package db

import (
	"time"

	"gorm.io/gorm/clause"
)

// mentionReservationTTL bounds how long a reserved but never finalised
// request (the process died between reserving and admitting) blocks a retry.
const mentionReservationTTL = 10 * time.Minute

// MentionHandled reports whether a review request comment was acted on: a
// finalised row, or a live reservation another pass is still working on.
func (g *GormDB) MentionHandled(commentID int64, now time.Time) (bool, error) {
	var m MentionTriggerModel
	res := g.db.Where("comment_id = ?", commentID).Limit(1).Find(&m)
	if res.Error != nil || res.RowsAffected == 0 {
		return false, res.Error
	}
	return m.Queued || m.TriggeredAt.After(now.Add(-mentionReservationTTL)), nil
}

// ReserveMention claims a request before its review is admitted so a crash
// in between cannot admit it twice. It reports false when a live or
// finalised row already exists.
func (g *GormDB) ReserveMention(m *MentionTrigger) (bool, error) {
	var existing MentionTriggerModel
	res := g.db.Where("comment_id = ?", m.CommentID).Limit(1).Find(&existing)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 && (existing.Queued || existing.TriggeredAt.After(m.TriggeredAt.Add(-mentionReservationTTL))) {
		return false, nil
	}
	row := MentionTriggerModel{CommentID: m.CommentID, RepoOwner: m.RepoOwner, RepoName: m.RepoName, PRNumber: m.PRNumber,
		Author: m.Author, CommitSHA: m.CommitSHA, Publish: m.Publish, Queued: false, CreatedAt: m.CreatedAt, TriggeredAt: m.TriggeredAt}
	return true, g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "comment_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"commit_sha", "publish", "queued", "triggered_at"}),
	}).Create(&row).Error
}

// FinalizeMention marks a reserved request as admitted.
func (g *GormDB) FinalizeMention(commentID int64) error {
	return g.db.Model(&MentionTriggerModel{}).Where("comment_id = ?", commentID).Update("queued", true).Error
}

// ReleaseMention drops a reservation whose review could not be admitted yet.
func (g *GormDB) ReleaseMention(commentID int64) error {
	return g.db.Where("comment_id = ? AND NOT queued", commentID).Delete(&MentionTriggerModel{}).Error
}
