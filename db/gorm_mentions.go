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

// ReserveMention claims a request for one holder before its review is
// admitted, in a single statement: the insert takes a new comment, and the
// conflict update takes over only a reservation that is neither finalised
// nor live. Two scanners cannot both win.
func (g *GormDB) ReserveMention(m *MentionTrigger) (bool, error) {
	row := MentionTriggerModel{CommentID: m.CommentID, RepoOwner: m.RepoOwner, RepoName: m.RepoName, PRNumber: m.PRNumber,
		Author: m.Author, CommitSHA: m.CommitSHA, Publish: m.Publish, Queued: false, Holder: m.Holder, CreatedAt: m.CreatedAt, TriggeredAt: m.TriggeredAt}
	res := g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "comment_id"}},
		Where:     clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "NOT mention_triggers.queued AND mention_triggers.triggered_at < ?", Vars: []interface{}{m.TriggeredAt.Add(-mentionReservationTTL)}}}},
		DoUpdates: clause.AssignmentColumns([]string{"commit_sha", "publish", "holder", "triggered_at"}),
	}).Create(&row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// FinalizeMention marks the holder's reservation as admitted.
func (g *GormDB) FinalizeMention(commentID int64, holder string) error {
	return g.db.Model(&MentionTriggerModel{}).Where("comment_id = ? AND holder = ?", commentID, holder).Update("queued", true).Error
}

// ReleaseMention drops the holder's reservation whose review could not be
// admitted yet.
func (g *GormDB) ReleaseMention(commentID int64, holder string) error {
	return g.db.Where("comment_id = ? AND holder = ? AND NOT queued", commentID, holder).Delete(&MentionTriggerModel{}).Error
}
