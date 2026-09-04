package db

import (
	"errors"
	"time"
)

// ErrUserPRViewNotFound is returned by per-user PR view mutations when the
// user has no user_pr_views row for the PR, so callers can surface the miss
// (e.g. as a 404) instead of reporting a no-op success.
var ErrUserPRViewNotFound = errors.New("user PR view not found")

// ErrReviewRunActiveConflict is returned when another queued/running run
// already owns the same PR target. Terminal history remains unrestricted.
var ErrReviewRunActiveConflict = errors.New("another review run is active for this PR")

// PR represents a pull request in the database
type PR struct {
	ID              int
	RepoOwner       string
	RepoName        string
	PRNumber        int
	LastCommitSHA   string
	LastReviewedAt  *time.Time
	ReviewHTMLPath  string
	Status          string // "pending", "generating", "agent_reviewing", "completed", "error"
	GeneratingSince *time.Time
	Title           string     // PR title from GitHub
	Author          string     // PR author from GitHub
	ApprovalCount   int        // Number of current approvals
	MyReviewStatus  string     // Current user's review status: "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	CreatedAt       *time.Time // PR creation timestamp from GitHub
	Draft           bool       // true if PR is in draft mode
	CIState         string     // CI status: "success", "failure", "pending", "unknown"
	CIFailedChecks  string     // JSON array of failed check names
	PRState         string     // GitHub PR state: "open", "closed", "merged"
	ModelFallback   bool       // latest review ran on a fallback model, not the requested one
	ReviewRunID     string     // opaque ID for the latest review execution
	ReviewRunJSON   string     // structured model/run metadata for the latest review
	// Review importance counts
	CriticalCount int // Number of CRITICAL importance comments
	MediumCount   int // Number of MEDIUM importance comments
	LowCount      int // Number of LOW importance comments
	// Overall review verdict parsed from the SUMMARY entry:
	// "request_changes", "approve_suggestions", "approve", or "" (unknown)
	ReviewVerdict string
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
	UserHidden     bool   // User moved this PR to the Hidden section
	ViaManual      bool   // User manually requested a review for this PR
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
	Hidden       bool   // Whether this PR is hidden from the user's view (poller soft delete)
	UserHidden   bool   // User moved this PR to the Hidden section
	ViaManual    bool   // User manually requested a review for this PR
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

// ViaTeamsPrune identifies a stale user_pr_views row whose via_teams should be
// cleared because the user is no longer on any of the PR's reviewer teams and
// is not personally requested. Hide additionally hides the row when nothing
// else (authorship, review activity, user notes) keeps the PR on the user's
// dashboard.
type ViaTeamsPrune struct {
	UserID int
	PRID   int
	Hide   bool
}

// PRWithUserView combines PR data with user-specific view data
type PRWithUserView struct {
	PR
	IsAuthor     bool     // From user_pr_views
	IsReviewer   bool     // From user_pr_views
	UserNotes    string   // Notes from user_pr_views (overrides PR.Notes)
	ReviewStatus string   // User's review status from user_pr_views
	ViaTeams     []string // Team names from user_pr_views
	UserHidden   bool     // User moved this PR to the Hidden section
	ViaManual    bool     // User manually requested a review for this PR
}

// FindingOutcome is a recorded human triage decision on a single review
// finding: dismissed-with-reason, acknowledged risk, or explicitly
// unresolved. Keyed by (owner, repo, pr, reviewed sha, fingerprint) —
// re-deciding the same finding overwrites the prior decision.
//
// The persistence methods (UpsertFindingOutcome / GetFindingOutcomesForPR)
// live on *GormDB only and are deliberately NOT part of the Database
// interface: the feature is optional (env-gated at the HTTP layer), and
// callers type-assert for the narrow capability instead of forcing every
// Database implementation to grow with it.
type FindingOutcome struct {
	ID          int
	RepoOwner   string
	RepoName    string
	PRNumber    int
	ReviewedSHA string
	Fingerprint string
	Provenance  string
	Severity    string
	Outcome     string // "dismissed", "acknowledged", "unresolved"
	Reason      string
	DecidedBy   string
	DecidedAt   time.Time
}

// Published-finding kinds and states (GitHub publication ledger).
const (
	PublishedKindSummary = "summary" // the sticky summary comment; Fingerprint == kind
	PublishedKindFinding = "finding" // an inline review comment for one finding
	// PublishedKindAnnotation tracks a finding that was summarized but not
	// posted inline (below the severity floor, over the cap, or outside a
	// hunk), so round diffs stay stable.
	PublishedKindAnnotation = "annotation"

	PublishedStateOpen      = "open"
	PublishedStateResolved  = "resolved"  // thread resolved after the finding disappeared
	PublishedStateDismissed = "dismissed" // conceded in conversation or by reaction
)

// PublishedFinding records what PRism has posted to a PR on GitHub, one row
// per (owner, repo, pr, fingerprint) for the life of the PR. ReviewedSHA is the
// head at first publication and LastSeenSHA the most recent review that still
// produced the finding; the gap between them drives "still open" reporting.
type PublishedFinding struct {
	ID           int
	RepoOwner    string
	RepoName     string
	PRNumber     int
	Kind         string
	Fingerprint  string
	SourceTag    string // prism-only | greptile-only | both
	Severity     string
	ReviewedSHA  string
	LastSeenSHA  string
	CommentID    int64
	ThreadNodeID string
	ReviewID     int64
	CheckRunID   int64
	State        string
	// Rounds counts publication rounds; meaningful on the summary row only.
	Rounds      int
	PublishedAt time.Time
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
	// Review-run operations. review_runs is the historical source of truth;
	// the PR row remains only a latest-success projection.
	CreateReviewRun(run *ReviewRun) error
	GetReviewRun(runID string) (*ReviewRun, error)
	GetReviewRunByIdempotency(scope, keyHash string) (*ReviewRun, error)
	ListReviewRuns(filter ReviewRunFilter) ([]ReviewRun, error)
	PatchReviewRun(runID string, patch ReviewRunPatch) error
	PatchQueuedReviewRun(runID string, patch ReviewRunPatch) (bool, error)
	PatchReviewRunAsHolder(runID, holder string, now time.Time, patch ReviewRunPatch) (bool, error)
	ClaimOrRenewQueuedReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error)
	ClaimReviewRun(runID, holder string, now, leaseExpiresAt time.Time) (bool, error)
	RenewReviewRunLease(runID, holder string, now, leaseExpiresAt time.Time) (bool, error)
	AbandonExpiredReviewRuns(now time.Time, runningGrace, queuedMaxAge time.Duration) (int, error)
	UpsertReviewStageAttempt(attempt *ReviewStageAttempt) error
	ListReviewStageAttempts(runID string) ([]ReviewStageAttempt, error)

	// PR operations
	GetPR(owner, repo string, prNumber int) (*PR, error)
	UpsertPR(pr *PR) error
	UpdatePRStatus(owner, repo string, prNumber int, status string) error
	ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error
	SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool) error
	SetPRAgentReviewing(owner, repo string, prNumber int) error
	SetPRError(owner, repo string, prNumber int, message string) error
	MarkPRCompleted(owner, repo string, prNumber int, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRun ...string) error
	SetPRGeneratingForReviewRun(owner, repo string, prNumber int, commitSHA, title, author string, createdAt *time.Time, draft bool, runID string) error
	SetPRAgentReviewingForReviewRun(owner, repo string, prNumber int, runID string) (bool, error)
	SetPRErrorForReviewRun(owner, repo string, prNumber int, runID, message string) (bool, error)
	SetPRErrorIfNoLiveReview(owner, repo string, prNumber int, message string) (bool, error)
	MarkPRCompletedForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string) (bool, error)
	RestorePRCompletedFromCacheForReviewRun(owner, repo string, prNumber int, projectionRunID, reviewRunID, commitSHA, reviewPath string, critical, medium, low int, verdict string, modelFallback bool, reviewRunJSON string, inFlightStaleBefore time.Time) (bool, error)
	GetAllPRs() ([]PR, error)
	DeletePR(owner, repo string, prNumber int) error
	ResetStaleGeneratingPRs(timeoutMinutes int) (int, error)
	ResetErrorPRs(maxAgeMinutes int, maxRetries int) (int, error)
	GetPRsWithMissingMetadata() ([]PR, error)
	UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error
	UpdatePRNotes(owner, repo string, prNumber int, notes string) error
	GetPRsWithMissingCreatedAt() ([]PR, error)
	UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error
	UpdatePRGitHubUpdatedAt(owner, repo string, prNumber int, updatedAt time.Time) error
	UpdatePRDraft(owner, repo string, prNumber int, draft bool) error

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
	SetUserHiddenForPR(userID, prID int, hidden bool) error
	EnsureUserPRView(userID, prID int, isAuthor bool) error
	EnsureManualPRView(userID, prID int, isAuthor bool) error
	GetPRIDsWithManualClaims() (map[int]bool, error)
	HideNonManualViewsForPR(prID int) error
	SetPRState(owner, repo string, prNumber int, state string) error
	MigrateLegacyNotes(userID int) (int, error)

	// Leader election: only the lease holder runs the automatic poll cycle, so
	// multiple instances never poll concurrently. Returns true iff holderID holds
	// the lease after the call.
	TryAcquireOrRenewLeadership(holderID string, ttl time.Duration) (bool, error)

	// Batch operations (used by poller for efficiency)
	BatchUpsertPRs(prs []*PR) error
	BatchUpsertUserPRViews(views []UserPRViewBatchItem) error
	GetUserPRViewsWithViaTeams(prIDs []int) ([]UserPRView, error)
	BatchPruneViaTeams(prunes []ViaTeamsPrune) error

	// Telemetry operations
	CreateTelemetryEvents(events []TelemetryEvent) error
	GetTelemetryStats(days int) (*TelemetryStats, error)

	// Lifecycle
	Close() error
}
