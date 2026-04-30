package db

import (
	"time"
)

// PR represents a pull request in the database
type PR struct {
	ID              int
	RepoOwner       string
	RepoName        string
	PRNumber        int
	LastCommitSHA   string
	LastReviewedAt  *time.Time
	ReviewHTMLPath  string
	Status          string // "pending", "generating", "completed", "error"
	GeneratingSince *time.Time
	Title           string     // PR title from GitHub
	Author          string     // PR author from GitHub
	ApprovalCount   int        // Number of current approvals
	MyReviewStatus  string     // Current user's review status: "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	CreatedAt       *time.Time // PR creation timestamp from GitHub
	Draft           bool       // true if PR is in draft mode
	CIState         string     // CI status: "success", "failure", "pending", "unknown"
	CIFailedChecks  string     // JSON array of failed check names
	// Review importance counts
	CriticalCount int // Number of CRITICAL importance comments
	MediumCount   int // Number of MEDIUM importance comments
	LowCount      int // Number of LOW importance comments
	// User notes (single-user mode)
	Notes string
	// Poll economy: last seen updated_at from GitHub search API
	GitHubUpdatedAt *time.Time
	// Populated when Status=="error"; surfaced to the UI.
	ErrorMessage string
}

// User represents a user in multi-user mode
type User struct {
	ID              int
	GitHubID        int64
	GitHubUsername  string
	GitHubAvatarURL string
	CreatedAt       time.Time
	LastLoginAt     *time.Time
}

// Session represents a user session
type Session struct {
	ID        string
	UserID    int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// UserPRAssignment represents the relationship between users and PRs
// Deprecated: Use UserPRView instead. This type is kept for backward compatibility.
type UserPRAssignment struct {
	ID             int
	UserID         int
	PRID           int
	IsAuthor       bool
	IsReviewer     bool
	ReviewerGroups string // JSON array of team names (deprecated, use ViaTeams)
	MyReviewStatus string // User's review status for this PR
	Notes          string // User's notes for this PR
}

// UserPRView represents the relationship between users and PRs (new name for UserPRAssignment)
// This is the preferred type for new code.
type UserPRView struct {
	ID           int
	UserID       int
	PRID         int
	IsAuthor     bool
	IsReviewer   bool
	ViaTeams     string // JSON array of team names (was ReviewerGroups)
	ReviewStatus string // User's review status for this PR (was MyReviewStatus)
	Notes        string // User's notes for this PR
	Hidden       bool   // Whether this PR is hidden from the user's view
}

// UserPRViewBatchItem represents a single row for batch upsert into user_pr_views.
// Pointer fields mean "update this column on conflict"; nil means "preserve existing value".
type UserPRViewBatchItem struct {
	UserID       int
	PRID         int
	IsAuthor     bool
	ReviewStatus *string   // nil = don't update
	ViaTeams     *[]string // nil = don't update
}

// PRWithUserView combines PR data with user-specific view data
type PRWithUserView struct {
	PR
	IsAuthor     bool     // From user_pr_views
	IsReviewer   bool     // From user_pr_views
	UserNotes    string   // Notes from user_pr_views (overrides PR.Notes)
	ReviewStatus string   // User's review status from user_pr_views
	ViaTeams     []string // Team names from user_pr_views
}

// TelemetryEvent represents a single telemetry event for creation
type TelemetryEvent struct {
	UserID   int
	Action   string
	Label    string
	PROwner  string
	PRRepo   string
	PRNumber int
}

// TelemetryStats represents aggregated telemetry statistics
type TelemetryStats struct {
	TotalEvents int                  `json:"total_events"`
	ActiveUsers int                  `json:"active_users"`
	ByAction    []ActionCount        `json:"by_action"`
	ByDay       []DayCount           `json:"by_day"`
	TopSearches []LabelCount         `json:"top_searches"`
	TopPRs      []PRInteractionCount `json:"top_prs"`
}

// ActionCount represents event count per action
type ActionCount struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

// DayCount represents event count per day
type DayCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// LabelCount represents count per label
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// PRInteractionCount represents interaction count per PR
type PRInteractionCount struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Count  int    `json:"count"`
}

// Database defines the interface that both SQLite and PostgreSQL implementations must satisfy
type Database interface {
	// PR operations
	GetPR(owner, repo string, prNumber int) (*PR, error)
	UpsertPR(pr *PR) error
	UpdatePRStatus(owner, repo string, prNumber int, status string) error
	ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error
	SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool) error
	SetPRAgentReviewing(owner, repo string, prNumber int) error
	SetPRError(owner, repo string, prNumber int, message string) error
	MarkPRCompleted(owner, repo string, prNumber int, commitSHA, reviewPath string, critical, medium, low int) error
	GetAllPRs() ([]PR, error)
	DeletePR(owner, repo string, prNumber int) error
	ResetStaleGeneratingPRs(timeoutMinutes int) (int, error)
	ResetErrorPRs(maxAgeMinutes int) (int, error)
	GetPRsWithMissingMetadata() ([]PR, error)
	UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error
	UpdatePRNotes(owner, repo string, prNumber int, notes string) error
	GetPRsWithMissingCreatedAt() ([]PR, error)
	UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error
	UpdatePRGitHubUpdatedAt(owner, repo string, prNumber int, updatedAt time.Time) error

	// Settings operations
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	GetAutoReviewRequestedPRs() (bool, error)
	SetAutoReviewRequestedPRs(enabled bool) error
	GetReviewNRequests() (int, error)
	SetReviewNRequests(n int) error
	GetGenerateHTML() (bool, error)
	SetGenerateHTML(enabled bool) error

	// User operations
	GetUserByGitHubID(githubID int64) (*User, error)
	GetUserByID(id int) (*User, error)
	GetUserByUsername(username string) (*User, error)
	GetAllUsers() ([]User, error)
	CreateUser(user *User) error
	UpdateUserLastLogin(userID int) error

	// Session operations (multi-user mode only)
	CreateSession(session *Session) error
	GetSession(id string) (*Session, error)
	DeleteSession(id string) error

	// User-PR view operations
	GetUserPRAssignment(userID, prID int) (*UserPRAssignment, error)
	UpsertUserPRAssignment(assignment *UserPRAssignment) error
	GetPRsForUser(userID int) ([]PR, error)
	GetPRsForUserWithNotes(userID int) ([]PRWithUserView, error)
	UpdateUserPRNotes(userID, prID int, notes string) error
	UpdateUserReviewStatus(userID, prID int, reviewStatus string) error
	UpdateUserViaTeams(userID, prID int, viaTeams []string) error
	DeleteAllUserPRViews(userID int) (int64, error)
	HidePRForUser(userID, prID int) error
	EnsureUserPRView(userID, prID int, isAuthor bool) error
	MigrateLegacyNotes(userID int) (int, error)

	// Batch operations (used by poller for efficiency)
	BatchUpsertPRs(prs []*PR) error
	BatchUpsertUserPRViews(views []UserPRViewBatchItem) error

	// Telemetry operations
	CreateTelemetryEvents(events []TelemetryEvent) error
	GetTelemetryStats(days int) (*TelemetryStats, error)

	// Lifecycle
	Close() error
}
