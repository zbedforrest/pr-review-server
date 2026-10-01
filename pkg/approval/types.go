package approval

import (
	"context"
	"time"
)

const PolicyVersion = "approval-v2"
const PromptVersion = "approval-v1"
const RuntimeVersion = "native-tools-v1"

type Target struct {
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Number          int    `json:"number"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
}
type Viewer struct {
	ID       int    `json:"id"`
	Login    string `json:"login"`
	GitHubID int64  `json:"github_id"`
}
type Revision struct {
	Head      string `json:"head"`
	Base      string `json:"base"`
	MergeBase string `json:"merge_base"`
}
type Source struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ActorID          int64  `json:"actor_id"`
	Login            string `json:"login"`
	ActorType        string `json:"actor_type"`
	AppID            int64  `json:"app_id,omitempty"`
	Verified         bool   `json:"verified"`
	Verification     string `json:"verification"`
	AdapterVersion   string `json:"adapter_version"`
	Presence         string `json:"presence"`
	Completion       string `json:"completion"`
	ReviewedSHA      string `json:"reviewed_sha"`
	RevisionRelation string `json:"revision_relation"`
	FileCoverage     string `json:"file_coverage"`
	Incomplete       bool   `json:"incomplete"`
	Verdict          string `json:"verdict"`
}
type Evidence struct {
	ID          string    `json:"id"`
	SourceID    string    `json:"source_id"`
	Kind        string    `json:"kind"`
	RemoteID    string    `json:"remote_id"`
	ParentID    string    `json:"parent_id,omitempty"`
	Body        string    `json:"body"`
	BodyDigest  string    `json:"body_digest"`
	URL         string    `json:"url"`
	ReviewedSHA string    `json:"reviewed_sha"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Resolved    bool      `json:"resolved"`
	ConcernIDs  []string  `json:"concern_ids"`
	Path        string    `json:"path,omitempty"`
	StartLine   int       `json:"start_line,omitempty"`
	EndLine     int       `json:"end_line,omitempty"`
}
type Citation struct {
	EvidenceID string `json:"evidence_id,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Path       string `json:"path,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
	Excerpt    string `json:"excerpt"`
	Validated  bool   `json:"validated"`
}
type Concern struct {
	ID               string     `json:"id"`
	EvidenceIDs      []string   `json:"evidence_ids"`
	OriginalSeverity string     `json:"original_severity"`
	Impact           string     `json:"impact"`
	Claim            string     `json:"claim"`
	OriginalRevision string     `json:"original_revision"`
	Path             string     `json:"path,omitempty"`
	StartLine        int        `json:"start_line,omitempty"`
	EndLine          int        `json:"end_line,omitempty"`
	Disposition      string     `json:"disposition"`
	Rationale        string     `json:"rationale"`
	Citations        []Citation `json:"citations"`
}
type ArtifactDisposition struct {
	EvidenceID     string   `json:"evidence_id"`
	Classification string   `json:"classification"`
	Rationale      string   `json:"rationale"`
	ConcernIDs     []string `json:"concern_ids"`
}
type Endpoint struct {
	Name      string   `json:"name"`
	Complete  bool     `json:"complete"`
	Pages     int      `json:"pages"`
	Errors    []string `json:"errors"`
	Truncated bool     `json:"truncated"`
}
type Manifest struct {
	Complete        bool              `json:"complete"`
	Endpoints       []Endpoint        `json:"endpoints"`
	Errors          []string          `json:"errors"`
	TotalBytes      int64             `json:"total_bytes"`
	AdapterVersions map[string]string `json:"adapter_versions"`
}
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
	SHA   string `json:"sha"`
}
type Snapshot struct {
	ID                       string     `json:"id"`
	Target                   Target     `json:"target"`
	Viewer                   Viewer     `json:"viewer"`
	RepositoryID             int64      `json:"repository_id"`
	AccessPartition          string     `json:"access_partition"`
	Revision                 Revision   `json:"revision"`
	CapturedAt               time.Time  `json:"captured_at"`
	Digest                   string     `json:"digest"`
	Eligible                 bool       `json:"eligible"`
	ExclusionReasons         []string   `json:"exclusion_reasons"`
	HumanChangesRequested    bool       `json:"human_changes_requested"`
	ProviderChangesRequested bool       `json:"provider_changes_requested"`
	ReviewInProgress         bool       `json:"review_in_progress"`
	Draft                    bool       `json:"draft"`
	Sources                  []Source   `json:"sources"`
	Evidence                 []Evidence `json:"evidence"`
	Concerns                 []Concern  `json:"concerns"`
	Manifest                 Manifest   `json:"manifest"`
	Checks                   []Check    `json:"checks"`
	AllowedRevisions         []string   `json:"allowed_revisions"`
}
type Assessment struct {
	SchemaVersion  string                `json:"schema_version"`
	PolicyVersion  string                `json:"policy_version"`
	PromptVersion  string                `json:"prompt_version"`
	RuntimeVersion string                `json:"runtime_version"`
	SnapshotID     string                `json:"snapshot_id"`
	SnapshotDigest string                `json:"snapshot_digest"`
	Decision       string                `json:"decision"`
	Summary        string                `json:"summary"`
	ReasonCodes    []string              `json:"reason_codes"`
	Sources        []Source              `json:"sources"`
	Concerns       []Concern             `json:"concerns"`
	Artifacts      []ArtifactDisposition `json:"artifacts"`
	CoverageGaps   []string              `json:"coverage_gaps"`
	Citations      []Citation            `json:"citations"`
	Model          string                `json:"model"`
	AssessedAt     time.Time             `json:"assessed_at"`
	Usage          Usage                 `json:"usage"`
	Origin         string                `json:"origin,omitempty"`
	ReusedFrom     string                `json:"reused_from,omitempty"`
}
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Rounds       int `json:"rounds"`
	ToolCalls    int `json:"tool_calls"`
	ToolBytes    int `json:"tool_bytes"`
}
type ReadRequest struct {
	Revision      string `json:"revision"`
	OtherRevision string `json:"other_revision,omitempty"`
	Path          string `json:"path,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	Query         string `json:"query,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}
type ReadResult struct {
	Text       string `json:"text"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type Repository interface {
	Read(context.Context, string, ReadRequest) (ReadResult, error)
}
type Budget interface {
	Reserve(context.Context, Usage) error
	Settle(context.Context, Usage, Usage) error
}
type Investigator interface {
	Investigate(context.Context, Snapshot, Repository, Budget) (Assessment, error)
}
