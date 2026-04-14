package db

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// JSONStringArray is a custom type for JSON array storage (SQLite compatible)
type JSONStringArray []string

// Scan implements the sql.Scanner interface
func (j *JSONStringArray) Scan(value interface{}) error {
	if value == nil {
		*j = []string{}
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		str, ok := value.(string)
		if !ok {
			return errors.New("JSONStringArray: invalid type")
		}
		bytes = []byte(str)
	}
	if len(bytes) == 0 {
		*j = []string{}
		return nil
	}
	return json.Unmarshal(bytes, j)
}

// Value implements the driver.Valuer interface
func (j JSONStringArray) Value() (driver.Value, error) {
	if j == nil {
		return "[]", nil
	}
	bytes, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	return string(bytes), nil
}

// UserModel represents a user in the database (GORM model)
type UserModel struct {
	ID             uint       `gorm:"primaryKey;autoIncrement"`
	GitHubID       int64      `gorm:"column:github_id;uniqueIndex;not null"`
	GitHubUsername string     `gorm:"column:github_username;size:255;not null"`
	GitHubAvatar   string     `gorm:"column:github_avatar"`
	CreatedAt      time.Time  `gorm:"autoCreateTime"`
	LastLoginAt    *time.Time `gorm:"column:last_login_at;index"`
}

// TableName specifies the table name for UserModel
func (UserModel) TableName() string {
	return "users"
}

// SessionModel represents a user session (GORM model)
type SessionModel struct {
	ID        string    `gorm:"primaryKey;size:64"`
	UserID    uint      `gorm:"index;not null"`
	User      UserModel `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	ExpiresAt time.Time `gorm:"index;not null"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

// TableName specifies the table name for SessionModel
func (SessionModel) TableName() string {
	return "sessions"
}

// PRModel represents a pull request in the database (GORM model)
type PRModel struct {
	ID              uint            `gorm:"primaryKey;autoIncrement"`
	RepoOwner       string          `gorm:"size:255;not null;uniqueIndex:idx_prs_unique"`
	RepoName        string          `gorm:"size:255;not null;uniqueIndex:idx_prs_unique"`
	PRNumber        int             `gorm:"not null;uniqueIndex:idx_prs_unique"`
	Title           string          `gorm:"type:text"`
	Author          string          `gorm:"size:255;index"`
	LastCommitSHA   string          `gorm:"size:40;not null"`
	Status          string          `gorm:"size:20;default:pending;index"`
	ReviewPath      string          `gorm:"column:review_path;type:text"`
	LastReviewedAt  *time.Time      `gorm:"index"`
	GeneratingSince *time.Time      `gorm:"index"`
	CreatedAt       *time.Time      `gorm:"index"`
	Draft           bool            `gorm:"default:false"`
	ApprovalCount   int             `gorm:"default:0"`
	MyReviewStatus  string          `gorm:"size:20"` // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	CIState         string          `gorm:"size:20;default:unknown"`
	CIFailedChecks  JSONStringArray `gorm:"type:text"`
	// Review importance counts (populated after review generation)
	CriticalCount int `gorm:"default:0"`
	MediumCount   int `gorm:"default:0"`
	LowCount      int `gorm:"default:0"`
	// User notes (single-user mode)
	Notes string `gorm:"size:15"`
	// Poll economy: last seen updated_at from GitHub search API
	GitHubUpdatedAt *time.Time `gorm:"column:github_updated_at"`
}

// TableName specifies the table name for PRModel
func (PRModel) TableName() string {
	return "prs"
}

// UserPRViewModel represents the relationship between users and PRs (GORM model)
type UserPRViewModel struct {
	ID           uint            `gorm:"primaryKey;autoIncrement"`
	UserID       uint            `gorm:"uniqueIndex:idx_user_pr_unique;not null"`
	User         UserModel       `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	PRID         uint            `gorm:"column:pr_id;uniqueIndex:idx_user_pr_unique;not null"`
	PR           PRModel         `gorm:"foreignKey:PRID;constraint:OnDelete:CASCADE"`
	IsAuthor     bool            `gorm:"default:false"`
	IsReviewer   bool            `gorm:"default:false"`
	ViaTeams     JSONStringArray `gorm:"type:text"` // JSON array of team names
	ReviewStatus string          `gorm:"size:20"`   // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	Notes        string          `gorm:"size:15"`
	Hidden       bool            `gorm:"default:false"`
}

// TableName specifies the table name for UserPRViewModel
func (UserPRViewModel) TableName() string {
	return "user_pr_views"
}

// TelemetryEventModel represents a user interaction event (GORM model)
type TelemetryEventModel struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	UserID    uint      `gorm:"index;not null"`
	User      UserModel `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	Action    string    `gorm:"size:50;index;not null"`
	Label     string    `gorm:"size:255"`
	PROwner   string    `gorm:"size:255"`
	PRRepo    string    `gorm:"size:255"`
	PRNumber  int       `gorm:"default:0"`
	CreatedAt time.Time `gorm:"autoCreateTime;index"`
}

// TableName specifies the table name for TelemetryEventModel
func (TelemetryEventModel) TableName() string {
	return "telemetry_events"
}

// SettingModel represents a key-value setting (GORM model)
type SettingModel struct {
	Key   string `gorm:"primaryKey;size:255"`
	Value string `gorm:"type:text;not null"`
}

// TableName specifies the table name for SettingModel
func (SettingModel) TableName() string {
	return "settings"
}
