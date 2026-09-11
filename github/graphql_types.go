package github

import "time"

// GraphQL response types for review data queries

// ReviewAuthor represents the author of a review
type ReviewAuthor struct {
	Login string `json:"login"`
}

// ReviewCommit is the commit a review was submitted against
type ReviewCommit struct {
	OID string `json:"oid"`
}

// ReviewNode represents a single review in the GraphQL response
type ReviewNode struct {
	Author *ReviewAuthor `json:"author"`
	State  string        `json:"state"`
	Commit *ReviewCommit `json:"commit"`
}

// ReviewsPageInfo reports whether older reviews exist beyond the fetched window
type ReviewsPageInfo struct {
	HasPreviousPage bool `json:"hasPreviousPage"`
}

// ReviewsData holds the collection of review nodes
type ReviewsData struct {
	Nodes    []ReviewNode    `json:"nodes"`
	PageInfo ReviewsPageInfo `json:"pageInfo"`
}

// PRReviewGraphQL represents PR review data in GraphQL response
type PRReviewGraphQL struct {
	HeadRefOid string      `json:"headRefOid"`
	IsDraft    bool        `json:"isDraft"`
	Reviews    ReviewsData `json:"reviews"`
}

// RepoReviewData represents repository data containing PR review info
type RepoReviewData struct {
	PullRequest PRReviewGraphQL `json:"pullRequest"`
}

// GraphQLReviewResponse represents the full GraphQL response for review data queries
type GraphQLReviewResponse struct {
	Data map[string]RepoReviewData `json:"data"`
}

// GraphQL response types for reviewer groups queries (via timeline events)

// TimelineReviewRequested represents a REVIEW_REQUESTED_EVENT from the PR timeline.
// This captures ALL teams/users ever requested, not just currently-pending ones.
type TimelineReviewRequested struct {
	RequestedReviewer struct {
		TypeName     string `json:"__typename"` // "User" or "Team"
		Login        string `json:"login"`      // User only
		Name         string `json:"name"`       // Team only
		Slug         string `json:"slug"`       // Team only
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"` // Team only — the owning org
	} `json:"requestedReviewer"`
}

// TimelineItemsData holds the collection of timeline events
type TimelineItemsData struct {
	Nodes []TimelineReviewRequested `json:"nodes"`
}

// PRReviewerGroupsGraphQL represents PR reviewer groups data in GraphQL response
type PRReviewerGroupsGraphQL struct {
	Number        int               `json:"number"`
	TimelineItems TimelineItemsData `json:"timelineItems"`
}

// RepoReviewerGroupsData represents repository data containing PR reviewer groups info
type RepoReviewerGroupsData struct {
	PullRequest PRReviewerGroupsGraphQL `json:"pullRequest"`
}

// GraphQLReviewerGroupsResponse represents the full GraphQL response for reviewer groups queries
type GraphQLReviewerGroupsResponse struct {
	Data map[string]RepoReviewerGroupsData `json:"data"`
}

// GraphQL response types for CI status queries

// CheckNode represents a single CI check in the GraphQL response
type CheckNode struct {
	TypeName   string `json:"__typename"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

// ContextsData holds the collection of check nodes
type ContextsData struct {
	Nodes []CheckNode `json:"nodes"`
}

// StatusCheckRollup represents the overall CI status rollup
type StatusCheckRollup struct {
	State    string       `json:"state"`
	Contexts ContextsData `json:"contexts"`
}

// CommitObject represents a commit object with status check rollup
type CommitObject struct {
	StatusCheckRollup *StatusCheckRollup `json:"statusCheckRollup"`
}

// CICommitNode wraps the commit inside a PR's commits connection
type CICommitNode struct {
	Commit CommitObject `json:"commit"`
}

// CIPullRequestData holds the last commit of a PR for CI status queries.
// Querying commits(last: 1) instead of object(oid:) means the rollup is
// always for the PR's actual current head, even if our stored SHA is stale.
type CIPullRequestData struct {
	Commits struct {
		Nodes []CICommitNode `json:"nodes"`
	} `json:"commits"`
}

// RepoCIStatusData represents repository data containing CI status info
type RepoCIStatusData struct {
	PullRequest *CIPullRequestData `json:"pullRequest"`
}

// GraphQLCIStatusResponse represents the full GraphQL response for CI status queries
type GraphQLCIStatusResponse struct {
	Data map[string]RepoCIStatusData `json:"data"`
}

// GraphQL response types for PR state queries (open/closed + HEAD SHA)

// PRStateGraphQL represents PR state data in GraphQL response
type PRStateGraphQL struct {
	State      string `json:"state"`      // OPEN, CLOSED, MERGED
	HeadRefOid string `json:"headRefOid"` // current HEAD SHA
	IsDraft    bool   `json:"isDraft"`
}

// RepoStateData represents repository data containing PR state info
type RepoStateData struct {
	PullRequest PRStateGraphQL `json:"pullRequest"`
}

// GraphQLPRStateResponse represents the full GraphQL response for PR state queries
type GraphQLPRStateResponse struct {
	Data map[string]RepoStateData `json:"data"`
}

// PRInfo holds basic PR identification info for batch operations
type PRInfo struct {
	Owner     string
	Repo      string
	Number    int
	Title     string     // PR title (populated by search, "" otherwise)
	UpdatedAt *time.Time // GitHub updated_at (populated by search, nil otherwise)
}

// PRDetailsAuthor represents the author in a PR details GraphQL response
type PRDetailsAuthor struct {
	Login string `json:"login"`
}

// PRDetailsGraphQL represents PR details returned by a GraphQL query
type PRDetailsGraphQL struct {
	Number     int             `json:"number"`
	Title      string          `json:"title"`
	URL        string          `json:"url"`
	Author     PRDetailsAuthor `json:"author"`
	CreatedAt  string          `json:"createdAt"`
	IsDraft    bool            `json:"isDraft"`
	HeadRefOid string          `json:"headRefOid"`
}

// RepoPRDetailsData represents repository data containing PR details
type RepoPRDetailsData struct {
	PullRequest PRDetailsGraphQL `json:"pullRequest"`
}

// GraphQLPRDetailsResponse represents the full GraphQL response for PR detail queries
type GraphQLPRDetailsResponse struct {
	Data map[string]RepoPRDetailsData `json:"data"`
}
