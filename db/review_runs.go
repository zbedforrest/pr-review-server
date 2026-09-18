package db

import (
	"errors"
	"sort"
	"time"
)

// ErrReviewRunConflict is returned when a run insert conflicts with an
// existing run ID or caller-scoped idempotency key. Callers can re-fetch by
// idempotency key without parsing dialect-specific database errors, but a nil
// result means the conflict was the run ID and must be treated as a hard error.
var ErrReviewRunConflict = errors.New("review run already exists")

const (
	ReviewRunStatusQueued    = "queued"
	ReviewRunStatusRunning   = "running"
	ReviewRunStatusCompleted = "completed"
	ReviewRunStatusFailed    = "failed"
	ReviewRunStatusTimedOut  = "timed_out"
	ReviewRunStatusCancelled = "cancelled"
	MaxReviewRunListLimit    = 500
)

// ReviewRun is the durable identity, immutable configuration, and lifecycle
// record for one review execution. Target fields are intentionally duplicated
// instead of relying on a PR foreign key so closed-PR cleanup cannot erase
// review history.
type ReviewRun struct {
	RunID             string
	PRID              *int
	RepoOwner         string
	RepoName          string
	PRNumber          int
	CommitSHA         string
	RequestedByUserID *int
	TriggerSource     string
	Status            string

	RequestedConfigJSON string
	EffectiveConfigJSON string
	ConfigSourcesJSON   string
	ConfigHash          string
	ConfigSchemaVersion int

	Profile           string
	AgentBackend      string
	AgentModel        string
	AgentEffort       string
	AgentWallClockSec int
	AgentMaxTurns     int

	AcceptedAt  time.Time
	QueuedAt    time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	DurationMS  int64

	HTMLPath                 string
	JSONPath                 string
	CriticalCount            int
	MediumCount              int
	LowCount                 int
	Verdict                  string
	ModelFallback            bool
	ServingModelVerification string
	ActualModelsJSON         string
	PublicationStatus        string

	TerminalCode string
	FailureStage string
	ErrorSummary string

	ServiceRevision    string
	LeaseHolder        string
	LeaseExpiresAt     *time.Time
	ExecutionAttempt   int
	IdempotencyScope   string
	IdempotencyKeyHash string
	RequestHash        string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ReviewRunFilter struct {
	RepoOwner        string
	RepoName         string
	PRNumber         int
	CommitSHA        string
	Status           string
	BeforeAcceptedAt time.Time
	BeforeRunID      string
	Limit            int
}

// ReviewRunPatch updates mutable lifecycle/result fields. Pointer fields
// distinguish "do not update" from a deliberate zero/false/empty value.
// For LeaseExpiresAt only, a non-nil pointer to the zero time clears the
// nullable column. Worker result/lifecycle writes must use
// PatchReviewRunAsHolder; PatchReviewRun is reserved for administrative and
// reconciliation writes. Lease acquisition itself must use ClaimReviewRun.
type ReviewRunPatch struct {
	Status                   *string
	StartedAt                *time.Time
	CompletedAt              *time.Time
	DurationMS               *int64
	HTMLPath                 *string
	JSONPath                 *string
	CriticalCount            *int
	MediumCount              *int
	LowCount                 *int
	Verdict                  *string
	ModelFallback            *bool
	ServingModelVerification *string
	ActualModelsJSON         *string
	PublicationStatus        *string
	TerminalCode             *string
	FailureStage             *string
	ErrorSummary             *string
	LeaseHolder              *string
	LeaseExpiresAt           *time.Time
	ExecutionAttempt         *int
}

// ReviewRunSuccessFinalization is the complete, immutable input for publishing
// one successful review. CompletedAt and DurationMS are supplied by the caller
// so the database row, PR projection, and persisted sidecar can share one exact
// timing boundary. LeaseCheckedAt is a caller-supplied lower bound refreshed on
// each retry; the database also samples time after acquiring the run lock so a
// transaction that waited past lease expiry can never publish.
type ReviewRunSuccessFinalization struct {
	RunID            string
	Holder           string
	ExecutionAttempt int
	LeaseCheckedAt   time.Time
	CompletedAt      time.Time
	DurationMS       int64

	HTMLPath                 string
	JSONPath                 string
	CanonicalPath            string
	Critical                 int
	Medium                   int
	Low                      int
	Verdict                  string
	ModelFallback            bool
	ServingModelVerification string
	ActualModelsJSON         string
	ReviewRunJSON            string
}

// ReviewRunFinalizationResult distinguishes a holder that lost ownership from
// a successful immutable completion whose mutable PR projection was superseded.
// PublicationStatus is "published" or "superseded" whenever Finalized is true.
type ReviewRunFinalizationResult struct {
	Finalized         bool
	Published         bool
	PublicationStatus string
}

// ReviewRunLedger is the worker-only extension for lease-fenced result and
// provider-attempt writes. Keeping it separate from Database lets read-only
// consumers and lightweight test doubles avoid implementing worker mutation
// capabilities they never use.
type ReviewRunLedger interface {
	FinalizeReviewRunSuccess(input ReviewRunSuccessFinalization) (ReviewRunFinalizationResult, error)
	UpsertReviewStageAttemptAsHolder(attempt *ReviewStageAttempt, holder string, now time.Time) (bool, error)
}

// CompletedReviewPathLookup is the read-only capability used to recover a
// completed review from its immutable artifact when a canonical alias is
// missing. Implementations that do not expose it simply skip that fallback.
type CompletedReviewPathLookup interface {
	GetCompletedPRByReviewPath(reviewPath string) (*PR, error)
}

// ReviewStageAttempt records one actual provider invocation. Parallel
// first-pass draws use InvocationNumber; provider retries use AttemptNumber.
type ReviewStageAttempt struct {
	ID                   int
	RunID                string
	ExecutionAttempt     int
	Stage                string
	InvocationNumber     int
	AttemptNumber        int
	Provider             string
	Backend              string
	RequestedModel       string
	ResolvedModel        string
	ObservedServedModels []string
	PrimaryServedModel   string
	ServedModelSource    string
	ServingModelVerified bool
	Fallback             bool
	FallbackReason       string
	MatcherVersion       string
	Effort               string
	Status               string
	AssistantTurns       int
	BudgetUnitsUsed      int
	TurnBudgetUnit       string
	TurnBudgetVersion    int
	InputTokens          int64
	OutputTokens         int64
	TotalTokens          int64
	CostUSD              float64
	StartedAt            *time.Time
	CompletedAt          *time.Time
	DurationMS           int64
	StopReason           string
	ErrorCode            string
	ErrorSummary         string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ReviewProfileStats summarizes finished runs of one profile over a window:
// how many ran, their median wall time, mean provider cost, and how many hit
// the agent wall clock.
type ReviewProfileStats struct {
	Runs          int     `json:"runs"`
	P50DurationMS int64   `json:"p50_duration_ms"`
	MeanCostUSD   float64 `json:"mean_cost_usd"`
	Timeouts      int     `json:"timeouts"`
}

// ReviewProfileRunSample is one finished run as read for profile statistics.
type ReviewProfileRunSample struct {
	RunID      string
	Profile    string
	Status     string
	DurationMS int64
	CostUSD    float64
	TimedOut   bool
}

// SummarizeReviewProfiles folds run samples into per-profile statistics;
// runs recorded before profiles existed count under full.
func SummarizeReviewProfiles(samples []ReviewProfileRunSample) map[string]ReviewProfileStats {
	durations := map[string][]int64{}
	stats := map[string]ReviewProfileStats{}
	costs := map[string]float64{}
	for _, sample := range samples {
		profile := sample.Profile
		if profile == "" {
			profile = "full"
		}
		current := stats[profile]
		current.Runs++
		if sample.TimedOut {
			current.Timeouts++
		}
		costs[profile] += sample.CostUSD
		durations[profile] = append(durations[profile], sample.DurationMS)
		stats[profile] = current
	}
	for profile, current := range stats {
		sorted := durations[profile]
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		current.P50DurationMS = sorted[len(sorted)/2]
		current.MeanCostUSD = costs[profile] / float64(current.Runs)
		stats[profile] = current
	}
	return stats
}
