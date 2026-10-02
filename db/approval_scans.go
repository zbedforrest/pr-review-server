package db

import (
	"errors"
	"time"
)

const MaxApprovalTargetsPerScan = 50

var (
	ErrApprovalNotFound    = errors.New("approval record not found")
	ErrApprovalLeaseLost   = errors.New("approval lease lost")
	ErrApprovalBudget      = errors.New("approval budget exhausted")
	ErrApprovalIdempotency = errors.New("approval idempotency conflict")
)

type ApprovalActiveConflict struct{ ScanID string }

func (e *ApprovalActiveConflict) Error() string { return "approval scan already active: " + e.ScanID }

type ApprovalScan struct {
	ID              string     `gorm:"primaryKey;size:64" json:"scan_id"`
	UserID          int        `gorm:"index;uniqueIndex:idx_approval_idempotency" json:"-"`
	Kind            string     `json:"kind"`
	Status          string     `gorm:"index" json:"status"`
	IdempotencyKey  string     `gorm:"size:200;uniqueIndex:idx_approval_idempotency" json:"-"`
	RequestHash     string     `json:"-"`
	ScopeJSON       string     `gorm:"type:text" json:"-"`
	LimitsJSON      string     `gorm:"type:text" json:"-"`
	CreatedAt       time.Time  `json:"created_at"`
	Deadline        time.Time  `json:"deadline"`
	CompletedAt     *time.Time `json:"completed_at"`
	CancelRequested bool       `json:"cancel_requested"`
	Total           int        `json:"total"`
}

type ApprovalTarget struct {
	ID               string     `gorm:"primaryKey;size:64" json:"target_id"`
	ScanID           string     `gorm:"index;size:64" json:"scan_id"`
	UserID           int        `gorm:"index" json:"-"`
	Owner            string     `json:"owner"`
	Repo             string     `json:"repo"`
	Number           int        `json:"number"`
	RepositoryID     int64      `json:"repository_id"`
	AccessPartition  string     `json:"-"`
	ExpectedHeadSHA  string     `json:"revision"`
	Generation       int64      `json:"generation"`
	ExecutionStatus  string     `gorm:"index" json:"execution_status"`
	Decision         string     `json:"decision"`
	Freshness        string     `json:"freshness_state"`
	ReasonCodesJSON  string     `gorm:"type:text" json:"-"`
	Summary          string     `gorm:"type:text" json:"summary"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	Deadline         *time.Time `json:"deadline"`
	LeaseToken       string     `json:"-"`
	LeaseUntil       *time.Time `gorm:"index" json:"-"`
	Attempts         int        `json:"attempts"`
	InputTokens      int64      `json:"input_tokens"`
	OutputTokens     int64      `json:"output_tokens"`
	Rounds           int        `json:"rounds"`
	ToolCalls        int        `json:"tool_calls"`
	ToolBytes        int64      `json:"tool_bytes"`
	PendingCallID    string     `json:"-"`
	PendingInput     int64      `json:"-"`
	PendingOutput    int64      `json:"-"`
	BudgetDay        string     `json:"-"`
	ReservedInput    int64      `json:"-"`
	ReservedOutput   int64      `json:"-"`
	BudgetDeferred   bool       `json:"-"`
	ObservedChangeAt *time.Time `json:"-"`
	ValidatedAt      *time.Time `json:"validated_at"`
	ValidUntil       *time.Time `json:"valid_until"`
	SnapshotJSON     string     `gorm:"-" json:"-"`
	AssessmentJSON   string     `gorm:"-" json:"-"`
}

type ApprovalAdmission struct {
	Scan                                ApprovalScan
	Targets                             []ApprovalTarget
	Now                                 time.Time
	DailyInputLimit, DailyOutputLimit   int64
	TargetInputLimit, TargetOutputLimit int64
}

type ApprovalClaim struct {
	Worker                        string
	Now                           time.Time
	LeaseDuration, TargetDuration time.Duration
	MaxSlots                      int
	// MaxPerUser caps one user's concurrent targets; zero means one.
	MaxPerUser int
	// MaxClaims caps the targets one claim takes; zero means one.
	MaxClaims int
}

type ApprovalFinalization struct {
	ExecutionStatus, Decision, Freshness, ReasonCodesJSON, Summary string
	AssessmentJSON                                                 string
	ValidatedAt, ValidUntil                                        *time.Time
}

type ApprovalCallReservation struct {
	CallID                          string
	InputTokens, OutputTokens       int64
	MaxInputTokens, MaxOutputTokens int64
	MaxRounds, MaxToolCalls         int
	ToolCalls                       int
}

type ApprovalUsage struct {
	InputTokens, OutputTokens, ToolBytes int64
	Rounds, ToolCalls                    int
}

type ApprovalUsageReservation struct {
	ID     string
	Usage  ApprovalUsage
	Limits ApprovalUsage
	// Carry is tool and citation usage settled in process since the last
	// durable write; it is added to the target before the limits are checked.
	Carry ApprovalUsage
}

type ApprovalValidation struct {
	TargetID   string `gorm:"primaryKey;size:64"`
	UserID     int    `gorm:"index"`
	Generation int64
	Token      string
	LeaseUntil time.Time `gorm:"index"`
}

type ApprovalStore interface {
	AdmitApprovalScan(ApprovalAdmission) (*ApprovalScan, bool, error)
	GetApprovalScan(int, string) (*ApprovalScan, error)
	GetApprovalScanByIdempotency(int, string) (*ApprovalScan, error)
	ReleaseApprovalTargetSlot(string, string) error
	ListApprovalScans(int, string, int, string) ([]ApprovalScan, error)
	GetApprovalTarget(int, string, string) (*ApprovalTarget, error)
	ListApprovalTargets(int, string, int, string) ([]ApprovalTarget, error)
	ListCurrentApprovalTargets(int, int, string) ([]ApprovalTarget, error)
	ClaimApprovalTarget(ApprovalClaim) (*ApprovalTarget, error)
	ClaimApprovalTargets(ApprovalClaim) ([]ApprovalTarget, error)
	FlushApprovalUsage(string, string, time.Time, ApprovalUsage) error
	HeartbeatApprovalTarget(string, string, time.Time, time.Duration) error
	SetApprovalTargetStage(string, string, string, time.Time) error
	SetApprovalTargetProgress(string, string, string, time.Time) error
	SaveApprovalSnapshot(string, string, string, time.Time) error
	ReserveApprovalBudget(string, string, time.Time, int64, int64, int64, int64) error
	ReserveApprovalCall(string, string, time.Time, ApprovalCallReservation) error
	CompleteApprovalCall(string, string, string, time.Time, int64, int64) error
	ReserveApprovalUsage(string, string, time.Time, ApprovalUsageReservation) error
	SettleApprovalUsage(string, string, string, time.Time, ApprovalUsage) error
	FinalizeApprovalTarget(string, string, time.Time, ApprovalFinalization) error
	CancelApprovalScan(int, string, time.Time) error
	CancelAllApprovalScans(time.Time) error
	ClaimApprovalValidation(int, string, time.Time, time.Duration) (*ApprovalValidation, error)
	FinishApprovalValidation(int, string, string, time.Time, string, string) error
	InvalidateApprovalTargets(string, string, int, string, time.Time) error
	InvalidateUserApprovalTargets(int, string, string, int, string, time.Time) error
	InvalidateMatchingApprovalTargets(int, string, string, int, ApprovalInvalidation, string, time.Time) error
	PruneApprovalScans(time.Time) (int64, error)
	RecordApprovalReuse(int, string, string, time.Time) error
	FindApprovalReuse(int, string) (*ApprovalReuse, error)
}

type ApprovalReuse struct {
	TargetID, AssessmentJSON string
}
