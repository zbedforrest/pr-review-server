package poller

import (
	"context"

	gh "github.com/google/go-github/v57/github"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/types"
)

// GitHubClient defines the interface for GitHub API operations used by the poller.
// This interface enables mocking GitHub calls in tests.
type GitHubClient interface {
	// GetPRsRequestingReview returns PRs where the user is a requested reviewer
	GetPRsRequestingReview(ctx context.Context) ([]github.PullRequest, error)

	// GetMyOpenPRs returns the user's own open PRs
	GetMyOpenPRs(ctx context.Context) ([]github.PullRequest, error)

	// IsPROpen checks if a PR is currently open (not closed or merged)
	IsPROpen(ctx context.Context, owner, repo string, prNumber int) (bool, error)

	// GetPRDetails fetches title and author for a specific PR
	GetPRDetails(ctx context.Context, owner, repo string, prNumber int) (title, author string, err error)

	// GetPRHeadSHA fetches the current HEAD commit SHA for a PR
	GetPRHeadSHA(ctx context.Context, owner, repo string, prNumber int) (string, error)

	// GetPR fetches a full PR object from GitHub
	GetPR(ctx context.Context, owner, repo string, prNumber int) (*gh.PullRequest, *gh.Response, error)

	// BatchGetPRReviewData fetches review data for multiple PRs efficiently using GraphQL
	BatchGetPRReviewData(ctx context.Context, prs []github.PullRequest) (map[string]*github.PRReviewData, error)

	// BatchGetCIStatus fetches CI check status for multiple PRs using GraphQL
	BatchGetCIStatus(ctx context.Context, prs []struct {
		Owner, Repo string
		Number      int
		CommitSHA   string
	}) (map[string]*github.CIStatus, error)

	// BatchGetReviewerGroups fetches reviewer group info for multiple PRs using GraphQL
	BatchGetReviewerGroups(ctx context.Context, prs []github.PullRequest) (map[string]*github.ReviewerGroupData, error)

	// GetAllOpenPRs returns all open PRs for the given org (used in multi-user mode)
	GetAllOpenPRs(ctx context.Context, orgName string) ([]github.PullRequest, error)

	// SearchOpenPRs returns lightweight PR identifiers for all open PRs in the org
	SearchOpenPRs(ctx context.Context, orgName string) ([]github.PRInfo, error)

	// BatchGetPRDetails fetches full details for a set of PRs via GraphQL
	BatchGetPRDetails(ctx context.Context, prs []github.PRInfo) (map[string]github.PullRequest, error)

	// BatchGetPRState fetches state (open/closed/merged) and HEAD SHA for multiple PRs using GraphQL
	BatchGetPRState(ctx context.Context, prs []github.PRInfo) (map[string]*github.PRState, error)

	// GetOrgTeamMembers fetches members of a GitHub team by org and team slug
	GetOrgTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error)
}

// Verify that github.Client implements GitHubClient interface at compile time
var _ GitHubClient = (*github.Client)(nil)

// ReviewStorage defines the interface for storing and checking reviews.
// This interface enables mocking storage operations in tests.
type ReviewStorage interface {
	// ReviewExists checks if a review already exists for the given PR+commit.
	ReviewExists(ctx context.Context, owner, repo string, prNumber int, commitSHA string) (bool, error)

	// SaveReview persists the review content and returns the filename/path.
	SaveReview(ctx context.Context, owner, repo string, prNumber int, commitSHA string, content []byte) (string, error)

	// SaveReviewSidecar persists an extra artifact (e.g. the structured
	// findings JSON) under the given filename. Returns nil on success or
	// when the storage backend silently no-ops (local-only deployments may
	// choose to skip sidecars). HTML is the source of truth — callers
	// should log + continue if this fails, not abort the review.
	SaveReviewSidecar(ctx context.Context, filename, contentType string, content []byte) error
}

// ReviewResult contains the output from generating a review.
//
// HTMLContent + counts feed the existing HTML pipeline.
// Comments / Diff / FileContents are the structured form used to build the
// JSON sidecar; they may be nil for mock generators in tests.
type ReviewResult struct {
	HTMLContent   []byte
	CriticalCount int
	MediumCount   int
	LowCount      int

	Comments     []types.LineComment
	Diff         string
	FileContents map[string]string
}

// ReviewGeneratorConfig contains configuration for generating a review
type ReviewGeneratorConfig struct {
	Token        string
	Owner        string
	RepoName     string
	PRNumber     int
	CommitSHA    string
	WithComments bool
	Verbose      bool
	Fast         bool
	NRequests    int
}

// ReviewGenerator defines the interface for generating AI reviews.
// This interface enables mocking the review generation in tests.
type ReviewGenerator interface {
	// GenerateReview generates an AI review for the given PR
	GenerateReview(ctx context.Context, cfg ReviewGeneratorConfig) (*ReviewResult, error)
}
