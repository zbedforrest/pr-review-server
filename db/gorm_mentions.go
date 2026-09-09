package db

import "gorm.io/gorm/clause"

// MentionHandled reports whether a review request comment was already acted on.
func (g *GormDB) MentionHandled(commentID int64) (bool, error) {
	var n int64
	err := g.db.Model(&MentionTriggerModel{}).Where("comment_id = ?", commentID).Count(&n).Error
	return n > 0, err
}

// RecordMention stores a handled request; a repeat for the same comment is a
// no-op.
func (g *GormDB) RecordMention(m *MentionTrigger) error {
	return g.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&MentionTriggerModel{
		CommentID: m.CommentID, RepoOwner: m.RepoOwner, RepoName: m.RepoName, PRNumber: m.PRNumber,
		Author: m.Author, CommitSHA: m.CommitSHA, Publish: m.Publish, CreatedAt: m.CreatedAt, TriggeredAt: m.TriggeredAt,
	}).Error
}
