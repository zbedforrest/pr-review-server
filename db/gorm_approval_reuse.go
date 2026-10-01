package db

import (
	"time"

	"gorm.io/gorm/clause"
)

// approvalReuseEntry indexes immutable assessment rows by reuse key, so its
// writes and reads do not take the approval mutation gate.
type approvalReuseEntry struct {
	UserID    int    `gorm:"primaryKey"`
	ReuseKey  string `gorm:"primaryKey;size:64"`
	TargetID  string `gorm:"index;size:64"`
	CreatedAt time.Time
}

func (approvalReuseEntry) TableName() string { return "approval_reuse_index" }

func (g *GormDB) RecordApprovalReuse(user int, key, targetID string, now time.Time) error {
	entry := approvalReuseEntry{UserID: user, ReuseKey: key, TargetID: targetID, CreatedAt: now}
	return g.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "reuse_key"}}, DoUpdates: clause.AssignmentColumns([]string{"target_id", "created_at"})}).Create(&entry).Error
}

func (g *GormDB) FindApprovalReuse(user int, key string) (*ApprovalReuse, error) {
	var rows []ApprovalReuse
	err := g.db.Table("approval_reuse_index AS e").
		Select("e.target_id AS target_id, a.json AS assessment_json").
		Joins("JOIN approval_targets t ON t.id = e.target_id AND t.user_id = e.user_id AND t.execution_status = ?", "completed").
		Joins("JOIN approval_assessments a ON a.target_id = e.target_id").
		Where("e.user_id = ? AND e.reuse_key = ?", user, key).
		Limit(1).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrApprovalNotFound
	}
	return &rows[0], nil
}
