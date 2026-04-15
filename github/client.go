package github

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

type Client struct {
	gh         *github.Client
	ghv4       *githubv4.Client
	httpClient *http.Client
	token      string
	username   string
	appClient  *AppClient
}

// SetAppClient sets the AppClient for org-wide operations (multi-user mode).
// It also reconfigures the REST and GraphQL clients to use a refreshable token source
// so that installation tokens are automatically renewed when they expire.
func (c *Client) SetAppClient(ac *AppClient) {
	c.appClient = ac

	// Replace the static token source with one that refreshes via AppClient
	ts := &appTokenSource{appClient: ac}
	tc := oauth2.NewClient(context.Background(), ts)
	c.gh = github.NewClient(tc)
	c.ghv4 = githubv4.NewClient(tc)
	c.httpClient = tc
}

// appTokenSource implements oauth2.TokenSource using AppClient's refreshable installation token.
type appTokenSource struct {
	appClient *AppClient
}

func (s *appTokenSource) Token() (*oauth2.Token, error) {
	token, expiry, err := s.appClient.GetTokenWithExpiry(context.Background())
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: token, Expiry: expiry}, nil
}

// GetAllOpenPRs fetches all open PRs for the given org using the App installation token.
// Requires SetAppClient to have been called first.
func (c *Client) GetAllOpenPRs(ctx context.Context, orgName string) ([]PullRequest, error) {
	if c.appClient == nil {
		return nil, fmt.Errorf("appClient not set: GetAllOpenPRs requires a GitHub App client")
	}
	return c.appClient.GetAllOpenPRsForOrg(ctx, orgName)
}

// SearchOpenPRs returns lightweight PR identifiers for all open PRs in the org.
func (c *Client) SearchOpenPRs(ctx context.Context, orgName string) ([]PRInfo, error) {
	if c.appClient == nil {
		return c.searchOpenPRsWithREST(ctx, orgName)
	}
	return c.appClient.SearchOpenPRsForOrg(ctx, orgName)
}

// BatchGetPRDetails fetches full PR details for multiple PRs via GraphQL.
func (c *Client) BatchGetPRDetails(ctx context.Context, prs []PRInfo) (map[string]PullRequest, error) {
	if c.appClient == nil {
		return c.batchGetPRDetailsWithREST(ctx, prs)
	}
	return c.appClient.BatchGetPRDetails(ctx, prs)
}

func (c *Client) searchOpenPRsWithREST(ctx context.Context, orgName string) ([]PRInfo, error) {
	query := fmt.Sprintf("type:pr state:open org:%s", orgName)
	log.Printf("GitHub search query (org-wide): %s", query)

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var allPRs []PRInfo
	for {
		result, resp, err := c.gh.Search.Issues(ctx, query, opts)
		if err != nil {
			log.Printf("GitHub search error (org-wide): %v", err)
			return nil, err
		}

		for _, issue := range result.Issues {
			if issue.PullRequestLinks == nil {
				continue
			}

			repoOwner, repoName, err := parseRepoFromURL(issue.GetRepositoryURL())
			if err != nil {
				log.Printf("Invalid repository URL: %s", issue.GetRepositoryURL())
				continue
			}

			info := PRInfo{
				Owner:  repoOwner,
				Repo:   repoName,
				Number: issue.GetNumber(),
			}
			if updatedAt := issue.GetUpdatedAt(); !updatedAt.IsZero() {
				t := updatedAt.Time
				info.UpdatedAt = &t
			}
			allPRs = append(allPRs, info)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allPRs, nil
}

func pullRequestFromGitHubPR(owner, repo string, pr *github.PullRequest) PullRequest {
	var createdAt *time.Time
	if pr.GetCreatedAt().Time.Unix() != 0 {
		t := pr.GetCreatedAt().Time
		createdAt = &t
	}

	return PullRequest{
		Owner:     owner,
		Repo:      repo,
		Number:    pr.GetNumber(),
		CommitSHA: pr.GetHead().GetSHA(),
		Title:     pr.GetTitle(),
		URL:       pr.GetHTMLURL(),
		Author:    pr.GetUser().GetLogin(),
		CreatedAt: createdAt,
		Draft:     pr.GetDraft(),
	}
}

func (c *Client) batchGetPRDetailsWithREST(ctx context.Context, prs []PRInfo) (map[string]PullRequest, error) {
	results := make(map[string]PullRequest, len(prs))
	for _, info := range prs {
		pr, _, err := c.gh.PullRequests.Get(ctx, info.Owner, info.Repo, info.Number)
		if err != nil {
			log.Printf("Error fetching PR details for %s/%s#%d: %v", info.Owner, info.Repo, info.Number, err)
			continue
		}

		results[prKey(info.Owner, info.Repo, info.Number)] = pullRequestFromGitHubPR(info.Owner, info.Repo, pr)
	}
	return results, nil
}

// BatchGetPRState fetches state (open/closed/merged) and HEAD SHA for multiple PRs using batched GraphQL.
// Returns a map keyed by "owner/repo/number".
func (c *Client) BatchGetPRState(ctx context.Context, prs []PRInfo) (map[string]*PRState, error) {
	const batchSize = 100
	if len(prs) == 0 {
		return make(map[string]*PRState), nil
	}

	var (
		mu      sync.Mutex
		results = make(map[string]*PRState)
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 5)
	)

	for i := 0; i < len(prs); i += batchSize {
		end := i + batchSize
		if end > len(prs) {
			end = len(prs)
		}
		chunk := prs[i:end]
		chunkStart := i + 1

		wg.Add(1)
		sem <- struct{}{}
		go func(c2 []PRInfo, start, end2 int) {
			defer wg.Done()
			defer func() { <-sem }()

			query := buildPRStateQuery(c2)

			var graphqlResp GraphQLPRStateResponse
			if err := c.executeGraphQL(ctx, query, &graphqlResp); err != nil {
				log.Printf("[GRAPHQL] Warning: Failed to fetch PR state batch %d-%d: %v", start, end2, err)
				return
			}

			mu.Lock()
			for j, pr := range c2 {
				alias := fmt.Sprintf("pr%d", j)
				repoData, ok := graphqlResp.Data[alias]
				if !ok {
					continue
				}
				key := prKey(pr.Owner, pr.Repo, pr.Number)
				results[key] = &PRState{
					Owner:      pr.Owner,
					Repo:       pr.Repo,
					Number:     pr.Number,
					State:      repoData.PullRequest.State,
					HeadRefOid: repoData.PullRequest.HeadRefOid,
				}
			}
			mu.Unlock()
		}(chunk, chunkStart, end)
	}
	wg.Wait()

	log.Printf("[GRAPHQL] Fetched PR state for %d/%d PRs", len(results), len(prs))
	return results, nil
}

type RateLimitInfo struct {
	Limit     int
	Remaining int
	ResetTime time.Time
}

type PullRequest struct {
	Owner     string
	Repo      string
	Number    int
	CommitSHA string
	Title     string
	URL       string
	Author    string
	CreatedAt *time.Time
	Draft     bool
}

// Types for AI Reviewer
type User struct {
	Login string `json:"login"`
}

type PR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"html_url"`
	State     string    `json:"state"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	MergedAt  time.Time `json:"merged_at"`
	Files     []string  `json:"files"`
	Body      string    `json:"body"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Additions     int    `json:"additions"`
	Deletions     int    `json:"deletions"`
	ChangedFiles  int    `json:"changed_files"`
	Draft         bool   `json:"draft"`
	Approvals     int    `json:"-"`
	MyReviewState string `json:"-"`
}

type PRFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
}

type PRComment struct {
	ID               int       `json:"id"`
	Body             string    `json:"body"`
	User             User      `json:"user"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Path             string    `json:"path,omitempty"`              // Only for review comments
	Line             int       `json:"line,omitempty"`              // Only for review comments
	DiffHunk         string    `json:"diff_hunk,omitempty"`         // Only for review comments
	OriginalLine     int       `json:"original_line,omitempty"`     // Only for review comments
	OriginalPosition int       `json:"original_position,omitempty"` // Only for review comments
	Position         int       `json:"position,omitempty"`          // Only for review comments
	CommitID         string    `json:"commit_id,omitempty"`         // Only for review comments
	InReplyToID      int       `json:"in_reply_to_id,omitempty"`    // Only for review comments
}

type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	PreRelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

type Review struct {
	State       string    `json:"state"`
	User        User      `json:"user"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// PRReviewData holds review information for a single PR
type PRReviewData struct {
	Owner          string
	Repo           string
	Number         int
	ApprovalCount  int
	MyReviewStatus string            // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	UserReviews    map[string]string // Username -> latest review state (e.g. "APPROVED", "CHANGES_REQUESTED")
}

// ReviewerGroupData holds information about requested reviewer groups
type ReviewerGroupData struct {
	Owner          string
	Repo           string
	Number         int
	ReviewerGroups []string          // Team names (no longer includes __PERSONAL__)
	TeamSlugs      map[string]string // Team name -> slug (for API calls)
	OrgName        string            // GitHub org that owns the teams (from timeline data)
	RequestedUsers []string          // Individual users ever requested as reviewers
}

// PRState holds state and HEAD SHA for a PR (batch result)
type PRState struct {
	Owner      string
	Repo       string
	Number     int
	State      string // "OPEN", "CLOSED", "MERGED"
	HeadRefOid string // current HEAD commit SHA
}

// CIStatus holds CI check run status for a PR
type CIStatus struct {
	Owner        string
	Repo         string
	Number       int
	State        string   // "success", "failure", "pending", "unknown"
	FailedChecks []string // Names of failed checks
}

// NewTestClient creates a Client for testing with a custom base URL for the REST API.
func NewTestClient(baseURL, username string) *Client {
	ghClient := github.NewClient(nil)
	u, _ := url.Parse(baseURL + "/")
	ghClient.BaseURL = u
	return &Client{
		gh:       ghClient,
		username: username,
	}
}

func NewClient(token, username string) *Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	return &Client{
		gh:         github.NewClient(tc),
		ghv4:       githubv4.NewClient(tc),
		httpClient: tc,
		token:      token,
		username:   username,
	}
}

func (c *Client) GetPRsRequestingReview(ctx context.Context) ([]PullRequest, error) {
	query := fmt.Sprintf("type:pr state:open review-requested:%s", c.username)
	return c.searchAndFetchPRs(ctx, query, "review requests")
}

func (c *Client) GetMyOpenPRs(ctx context.Context) ([]PullRequest, error) {
	query := fmt.Sprintf("type:pr state:open author:%s", c.username)
	return c.searchAndFetchPRs(ctx, query, "my PRs")
}

// searchAndFetchPRs is a shared helper that searches for PRs and fetches their details.
// It handles the common logic for both GetPRsRequestingReview and GetMyOpenPRs.
func (c *Client) searchAndFetchPRs(ctx context.Context, query, queryType string) ([]PullRequest, error) {
	log.Printf("GitHub search query (%s): %s", queryType, query)

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	result, resp, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("GitHub search error (%s): %v", queryType, err)
		return nil, err
	}

	log.Printf("GitHub search returned %d %s (rate limit: %d/%d remaining)",
		result.GetTotal(), queryType, resp.Rate.Remaining, resp.Rate.Limit)

	var prs []PullRequest
	for _, issue := range result.Issues {
		if issue.PullRequestLinks == nil {
			continue
		}

		// Extract owner and repo from repository URL
		repoOwner, repoName, err := parseRepoFromURL(issue.GetRepositoryURL())
		if err != nil {
			log.Printf("Invalid repository URL: %s", issue.GetRepositoryURL())
			continue
		}
		prNumber := issue.GetNumber()

		log.Printf("Found %s: %s/%s#%d - %s", queryType, repoOwner, repoName, prNumber, issue.GetTitle())

		// Get the PR to fetch the HEAD commit SHA
		pr, _, err := c.gh.PullRequests.Get(ctx, repoOwner, repoName, prNumber)
		if err != nil {
			log.Printf("Error fetching PR details for %s/%s#%d: %v", repoOwner, repoName, prNumber, err)
			continue // Skip this PR if we can't fetch it
		}

		prs = append(prs, pullRequestFromGitHubPR(repoOwner, repoName, pr))
	}

	return prs, nil
}

// parseRepoFromURL extracts owner and repo from a GitHub API repository URL.
// URL format: https://api.github.com/repos/{owner}/{repo}
func parseRepoFromURL(repoURL string) (owner, repo string, err error) {
	parts := strings.Split(repoURL, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid repository URL: %s", repoURL)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

// IsPROpen checks if a PR is currently open (not closed or merged)
func (c *Client) IsPROpen(ctx context.Context, owner, repo string, prNumber int) (bool, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return false, err
	}

	// PR is open if state is "open"
	return pr.GetState() == "open", nil
}

// GetPRDetails fetches title and author for a specific PR
func (c *Client) GetPRDetails(ctx context.Context, owner, repo string, prNumber int) (title, author string, err error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return "", "", err
	}

	return pr.GetTitle(), pr.GetUser().GetLogin(), nil
}

// GetPRHeadSHA fetches the current HEAD commit SHA for a PR
func (c *Client) GetPRHeadSHA(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return "", err
	}

	return pr.GetHead().GetSHA(), nil
}

// GetPR fetches a full PR object from GitHub
func (c *Client) GetPR(ctx context.Context, owner, repo string, prNumber int) (*github.PullRequest, *github.Response, error) {
	return c.gh.PullRequests.Get(ctx, owner, repo, prNumber)
}

// ListReviews fetches all reviews for a PR
func (c *Client) ListReviews(ctx context.Context, owner, repo string, prNumber int) ([]*github.PullRequestReview, *github.Response, error) {
	opts := &github.ListOptions{PerPage: 100}
	return c.gh.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
}

// GetMyReviewStatus returns the current user's most recent review state on a PR
// Returns: (status, wasRateLimited, error)
// Status: "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "PENDING", or "" (no review)
func (c *Client) GetMyReviewStatus(ctx context.Context, owner, repo string, prNumber int) (string, bool, error) {
	opts := &github.ListOptions{PerPage: 100}
	reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
	if err != nil {
		// Check if this is a rate limit error
		if resp != nil && resp.Rate.Remaining == 0 {
			resetIn := time.Until(resp.Rate.Reset.Time)
			log.Printf("[RATE_LIMIT] API call BLOCKED by rate limit (resets in %v at %s)",
				resetIn.Round(time.Minute), resp.Rate.Reset.Time.Format("15:04:05 MST"))
			return "", true, fmt.Errorf("rate limited (resets at %s): %w", resp.Rate.Reset.Time.Format("15:04:05"), err)
		}
		return "", false, err
	}

	// Find the most recent review by the current user
	// Reviews are returned in chronological order, so we iterate backwards
	for i := len(reviews) - 1; i >= 0; i-- {
		review := reviews[i]
		if review.GetUser().GetLogin() == c.username {
			state := review.GetState()
			// Return the most recent non-DISMISSED, non-PENDING state
			if state != "DISMISSED" && state != "PENDING" {
				return state, false, nil
			}
		}
	}

	return "", false, nil // No review found
}

// GetRateLimitInfo returns the current rate limit status
func (c *Client) GetRateLimitInfo(ctx context.Context) (*RateLimitInfo, error) {
	limits, _, err := c.gh.RateLimit.Get(ctx)
	if err != nil {
		return nil, err
	}

	core := limits.GetCore()
	return &RateLimitInfo{
		Limit:     core.Limit,
		Remaining: core.Remaining,
		ResetTime: core.Reset.Time,
	}, nil
}

// IsRateLimited checks if we're currently rate limited (has few or no requests remaining)
func (c *Client) IsRateLimited(ctx context.Context) bool {
	info, err := c.GetRateLimitInfo(ctx)
	if err != nil {
		log.Printf("[RATE_LIMIT] Warning: Failed to check rate limit: %v", err)
		return false // Assume not rate limited if we can't check
	}

	// Consider rate limited if we have less than 10 requests remaining
	// or if we're completely out
	isLimited := info.Remaining < 10
	if isLimited {
		resetIn := time.Until(info.ResetTime)
		log.Printf("[RATE_LIMIT] WARNING: Rate limit low! %d/%d requests remaining (resets in %v at %s)",
			info.Remaining, info.Limit, resetIn.Round(time.Minute), info.ResetTime.Format("15:04:05 MST"))
	}
	return isLimited
}

// GetApprovalCount returns the number of current approvals on a PR
// This counts unique users whose most recent review is APPROVED
// Returns (approvalCount, wasRateLimited, error)
func (c *Client) GetApprovalCount(ctx context.Context, owner, repo string, prNumber int) (int, bool, error) {
	opts := &github.ListOptions{PerPage: 100}
	reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
	if err != nil {
		// Check if this is a rate limit error
		if resp != nil && resp.Rate.Remaining == 0 {
			resetIn := time.Until(resp.Rate.Reset.Time)
			log.Printf("[RATE_LIMIT] API call BLOCKED by rate limit (resets in %v at %s)",
				resetIn.Round(time.Minute), resp.Rate.Reset.Time.Format("15:04:05 MST"))
			return 0, true, fmt.Errorf("rate limited (resets at %s): %w", resp.Rate.Reset.Time.Format("15:04:05"), err)
		}
		return 0, false, err
	}

	// Track the most recent review state for each user
	userLatestReview := make(map[string]string)
	for _, review := range reviews {
		username := review.GetUser().GetLogin()
		state := review.GetState()
		// Only track non-PENDING, non-DISMISSED reviews
		if state != "PENDING" && state != "DISMISSED" {
			userLatestReview[username] = state
		}
	}

	// Count how many users have APPROVED as their latest review
	approvalCount := 0
	for _, state := range userLatestReview {
		if state == "APPROVED" {
			approvalCount++
		}
	}

	return approvalCount, false, nil
}

// BatchGetPRReviewData fetches review data for multiple PRs efficiently using GraphQL.
// Groups PRs by repository and makes one query per repository.
// Returns a map of "owner/repo/number" -> PRReviewData
func (c *Client) BatchGetPRReviewData(ctx context.Context, prs []PullRequest) (map[string]*PRReviewData, error) {
	if len(prs) == 0 {
		return make(map[string]*PRReviewData), nil
	}

	// Group PRs by repository
	prsByRepo := make(map[string][]PullRequest)
	for _, pr := range prs {
		key := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[key] = append(prsByRepo[key], pr)
	}

	var (
		mu      sync.Mutex
		results = make(map[string]*PRReviewData)
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 5) // limit to 10 concurrent GitHub API calls
	)

	for repoKey, repoPRs := range prsByRepo {
		wg.Add(1)
		sem <- struct{}{}
		go func(rk string, rps []PullRequest) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Printf("[GRAPHQL] Fetching review data for %d PRs in %s", len(rps), rk)

			repoData, err := c.fetchReviewDataForRepo(ctx, rps)
			if err != nil {
				log.Printf("[GRAPHQL] Error fetching review data for %s: %v", rk, err)
				return
			}

			mu.Lock()
			for k, v := range repoData {
				results[k] = v
			}
			mu.Unlock()
		}(repoKey, repoPRs)
	}
	wg.Wait()

	log.Printf("[GRAPHQL] Successfully fetched review data for %d/%d PRs", len(results), len(prs))
	return results, nil
}

// fetchReviewDataForRepo fetches review data for all PRs in a single repository using GraphQL
// Makes ONE batched query per repository using aliases for all PRs
func (c *Client) fetchReviewDataForRepo(ctx context.Context, prs []PullRequest) (map[string]*PRReviewData, error) {
	if len(prs) == 0 {
		return make(map[string]*PRReviewData), nil
	}

	owner := prs[0].Owner
	repo := prs[0].Repo

	// Build the GraphQL query
	query := c.buildReviewDataQuery(owner, repo, prs)
	prAliases := buildPRAliasMap(prs)

	// Execute the query using helper
	var graphqlResp GraphQLReviewResponse
	if err := c.executeGraphQL(ctx, query, &graphqlResp); err != nil {
		return nil, err
	}

	// Parse results
	results := make(map[string]*PRReviewData)
	for alias, prNumber := range prAliases {
		repoData, ok := graphqlResp.Data[alias]
		if !ok {
			log.Printf("[GRAPHQL] Warning: Failed to parse repo data for alias %s", alias)
			continue
		}

		// Process reviews using helper
		approvalCount, myReviewStatus, userReviews := c.countUserApprovals(repoData.PullRequest.Reviews)

		key := prKey(owner, repo, prNumber)
		results[key] = &PRReviewData{
			Owner:          owner,
			Repo:           repo,
			Number:         prNumber,
			ApprovalCount:  approvalCount,
			MyReviewStatus: myReviewStatus,
			UserReviews:    userReviews,
		}

		log.Printf("[GRAPHQL] PR %s/%s#%d: %d approvals, my status: %s", owner, repo, prNumber, approvalCount, myReviewStatus)
	}

	return results, nil
}

// buildReviewDataQuery builds a GraphQL query to fetch review data for PRs in a single repository.
func (c *Client) buildReviewDataQuery(owner, repo string, prs []PullRequest) string {
	var queryBuilder strings.Builder
	queryBuilder.WriteString("query {")

	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		// NOTE: reviews(last: 100) fetches the most recent 100 reviews.
		// For PRs with >100 review events, we might miss older review states.
		// This is acceptable since we only care about the most recent state per reviewer.
		queryBuilder.WriteString(fmt.Sprintf(`
			%s: repository(owner: "%s", name: "%s") {
				pullRequest(number: %d) {
					number
					reviews(last: 100) {
						nodes {
							author {
								login
							}
							state
						}
					}
				}
			}
		`, alias, owner, repo, pr.Number))
	}

	queryBuilder.WriteString("}")
	return queryBuilder.String()
}

// BatchGetReviewerGroups fetches reviewer group information for multiple PRs using GraphQL.
// Groups PRs by repository and makes one query per repository.
// Returns a map of "owner/repo/number" -> ReviewerGroupData
func (c *Client) BatchGetReviewerGroups(ctx context.Context, prs []PullRequest) (map[string]*ReviewerGroupData, error) {
	if len(prs) == 0 {
		return make(map[string]*ReviewerGroupData), nil
	}

	// Group PRs by repository
	prsByRepo := make(map[string][]PullRequest)
	for _, pr := range prs {
		key := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		prsByRepo[key] = append(prsByRepo[key], pr)
	}

	var (
		mu      sync.Mutex
		results = make(map[string]*ReviewerGroupData)
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 5) // limit to 10 concurrent GitHub API calls
	)

	for repoKey, repoPRs := range prsByRepo {
		wg.Add(1)
		sem <- struct{}{}
		go func(rk string, rps []PullRequest) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Printf("[GRAPHQL] Fetching reviewer groups for %d PRs in %s", len(rps), rk)

			repoData, err := c.fetchReviewerGroupsForRepo(ctx, rps)
			if err != nil {
				log.Printf("[GRAPHQL] Error fetching reviewer groups for %s: %v", rk, err)
				return
			}

			mu.Lock()
			for k, v := range repoData {
				results[k] = v
			}
			mu.Unlock()
		}(repoKey, repoPRs)
	}
	wg.Wait()

	log.Printf("[GRAPHQL] Successfully fetched reviewer groups for %d/%d PRs", len(results), len(prs))
	return results, nil
}

// fetchReviewerGroupsForRepo fetches reviewer groups for all PRs in a single repository using GraphQL
func (c *Client) fetchReviewerGroupsForRepo(ctx context.Context, prs []PullRequest) (map[string]*ReviewerGroupData, error) {
	if len(prs) == 0 {
		return make(map[string]*ReviewerGroupData), nil
	}

	owner := prs[0].Owner
	repo := prs[0].Repo

	// Build the GraphQL query
	query := c.buildReviewerGroupsQuery(owner, repo, prs)
	prAliases := buildPRAliasMap(prs)

	// Execute the query using helper
	var graphqlResp GraphQLReviewerGroupsResponse
	if err := c.executeGraphQL(ctx, query, &graphqlResp); err != nil {
		return nil, err
	}

	// Parse results
	results := make(map[string]*ReviewerGroupData)
	for alias, prNumber := range prAliases {
		repoData, ok := graphqlResp.Data[alias]
		if !ok {
			log.Printf("[GRAPHQL] Warning: No data for alias %s (PR %d)", alias, prNumber)
			continue
		}

		// Extract reviewer groups using helper
		reviewerGroupsDisplay, teamSlugs, orgName, requestedUsers := c.extractReviewerGroups(repoData.PullRequest.TimelineItems)

		key := prKey(owner, repo, prNumber)
		results[key] = &ReviewerGroupData{
			Owner:          owner,
			Repo:           repo,
			Number:         prNumber,
			ReviewerGroups: reviewerGroupsDisplay,
			TeamSlugs:      teamSlugs,
			OrgName:        orgName,
			RequestedUsers: requestedUsers,
		}

		log.Printf("[GRAPHQL] PR %s/%s#%d: reviewer groups:%v",
			owner, repo, prNumber, reviewerGroupsDisplay)
	}

	return results, nil
}

// buildReviewerGroupsQuery builds a GraphQL query to fetch all teams ever requested
// for PRs in a single repository, using timeline events instead of reviewRequests.
// This captures teams whose reviews were already satisfied (removed from reviewRequests by GitHub).
func (c *Client) buildReviewerGroupsQuery(owner, repo string, prs []PullRequest) string {
	var queryBuilder strings.Builder
	queryBuilder.WriteString("query {")

	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		queryBuilder.WriteString(fmt.Sprintf(`
			%s: repository(owner: "%s", name: "%s") {
				pullRequest(number: %d) {
					number
					timelineItems(first: 100, itemTypes: REVIEW_REQUESTED_EVENT) {
						nodes {
							... on ReviewRequestedEvent {
								requestedReviewer {
									__typename
									... on User {
										login
									}
									... on Team {
										name
										slug
										organization { login }
									}
								}
							}
						}
					}
				}
			}
		`, alias, owner, repo, pr.Number))
	}

	queryBuilder.WriteString("}")
	return queryBuilder.String()
}

// extractReviewerGroups processes timeline events (REVIEW_REQUESTED_EVENT) and returns
// all teams ever requested for the PR (deduplicated), along with a mapping of team name to slug,
// and all individually requested user logins.
// Uses timeline instead of reviewRequests to capture teams whose reviews were already satisfied.
// The __PERSONAL__ marker is no longer determined here — callers handle per-user logic.
func (c *Client) extractReviewerGroups(timelineItems TimelineItemsData) ([]string, map[string]string, string, []string) {
	teamSlugs := make(map[string]string)
	seenTeams := make(map[string]bool)
	seenUsers := make(map[string]bool)
	var reviewerGroups []string
	var requestedUsers []string
	var orgName string

	for _, event := range timelineItems.Nodes {
		if event.RequestedReviewer.TypeName == "User" && event.RequestedReviewer.Login != "" {
			login := event.RequestedReviewer.Login
			if !seenUsers[login] {
				seenUsers[login] = true
				requestedUsers = append(requestedUsers, login)
			}
		} else if event.RequestedReviewer.TypeName == "Team" && event.RequestedReviewer.Name != "" {
			name := event.RequestedReviewer.Name
			if !seenTeams[name] {
				seenTeams[name] = true
				reviewerGroups = append(reviewerGroups, name)
				if event.RequestedReviewer.Slug != "" {
					teamSlugs[name] = event.RequestedReviewer.Slug
				}
			}
			if orgName == "" && event.RequestedReviewer.Organization.Login != "" {
				orgName = event.RequestedReviewer.Organization.Login
			}
		}
	}

	return reviewerGroups, teamSlugs, orgName, requestedUsers
}

// BatchGetCIStatus fetches CI check status for multiple PRs using GraphQL
func (c *Client) BatchGetCIStatus(ctx context.Context, prs []struct {
	Owner, Repo string
	Number      int
	CommitSHA   string
}) (map[string]*CIStatus, error) {
	if len(prs) == 0 {
		return make(map[string]*CIStatus), nil
	}

	// Convert to PRInfoWithCommit slice for helper function
	prInfos := make([]PRInfoWithCommit, len(prs))
	for i, pr := range prs {
		prInfos[i] = PRInfoWithCommit{
			Owner:     pr.Owner,
			Repo:      pr.Repo,
			Number:    pr.Number,
			CommitSHA: pr.CommitSHA,
		}
	}

	// Build the GraphQL query
	query := buildCIStatusQuery(prInfos)
	prAliases := buildPRInfoAliasMap(prInfos)

	// Execute the query using helper
	var graphqlResp GraphQLCIStatusResponse
	if err := c.executeGraphQL(ctx, query, &graphqlResp); err != nil {
		return nil, err
	}

	// Parse results
	results := make(map[string]*CIStatus)
	for alias, prInfo := range prAliases {
		repoData, ok := graphqlResp.Data[alias]
		if !ok || repoData.Object == nil || repoData.Object.StatusCheckRollup == nil {
			// No CI checks found
			key := prKey(prInfo.Owner, prInfo.Repo, prInfo.Number)
			results[key] = &CIStatus{
				Owner:        prInfo.Owner,
				Repo:         prInfo.Repo,
				Number:       prInfo.Number,
				State:        "unknown",
				FailedChecks: []string{},
			}
			continue
		}

		// Parse CI status using helper
		state, failedChecks := parseCIStatusFromRollup(repoData.Object.StatusCheckRollup)

		key := prKey(prInfo.Owner, prInfo.Repo, prInfo.Number)
		results[key] = &CIStatus{
			Owner:        prInfo.Owner,
			Repo:         prInfo.Repo,
			Number:       prInfo.Number,
			State:        state,
			FailedChecks: failedChecks,
		}
	}

	return results, nil
}

// buildCIStatusQuery builds a GraphQL query to fetch CI status for multiple PRs.
func buildCIStatusQuery(prs []PRInfoWithCommit) string {
	var queryBuilder strings.Builder
	queryBuilder.WriteString("query {")

	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		queryBuilder.WriteString(fmt.Sprintf(`
			%s: repository(owner: "%s", name: "%s") {
				object(oid: "%s") {
					... on Commit {
						statusCheckRollup {
							state
							contexts(first: 100) {
								nodes {
									... on CheckRun {
										__typename
										name
										conclusion
										status
									}
									... on StatusContext {
										__typename
										context
										state
									}
								}
							}
						}
					}
				}
			}`,
			alias, pr.Owner, pr.Repo, pr.CommitSHA))
	}
	queryBuilder.WriteString("}")
	return queryBuilder.String()
}

// parseCIStatusFromRollup extracts CI state and failed checks from a status check rollup.
func parseCIStatusFromRollup(rollup *StatusCheckRollup) (state string, failedChecks []string) {
	state = strings.ToLower(rollup.State)

	for _, node := range rollup.Contexts.Nodes {
		if node.TypeName == "CheckRun" {
			// CheckRun conclusion: SUCCESS, FAILURE, NEUTRAL, CANCELLED, SKIPPED, TIMED_OUT, ACTION_REQUIRED
			// Status: QUEUED, IN_PROGRESS, COMPLETED
			if node.Status != "COMPLETED" {
				state = "pending"
			} else if node.Conclusion == "FAILURE" || node.Conclusion == "TIMED_OUT" || node.Conclusion == "ACTION_REQUIRED" {
				failedChecks = append(failedChecks, node.Name)
			}
		} else if node.TypeName == "StatusContext" {
			// StatusContext state: ERROR, EXPECTED, FAILURE, PENDING, SUCCESS
			if node.State == "PENDING" {
				state = "pending"
			} else if node.State == "ERROR" || node.State == "FAILURE" {
				failedChecks = append(failedChecks, node.Context)
			}
		}
	}

	// Override state based on failed checks
	if len(failedChecks) > 0 {
		state = "failure"
	}

	return state, failedChecks
}

// GetOrgTeamMembers fetches the members of a GitHub team by organization and team slug.
func (c *Client) GetOrgTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error) {
	opts := &github.TeamListTeamMembersOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var members []string
	for {
		teamMembers, resp, err := c.gh.Teams.ListTeamMembersBySlug(ctx, orgName, teamSlug, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list team members for %s/%s: %w", orgName, teamSlug, err)
		}
		for _, member := range teamMembers {
			members = append(members, member.GetLogin())
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return members, nil
}

func (c *Client) GetPRDiff(token, owner, repoName string, prNumber int) (string, error) {
	ctx := context.Background()
	// Use Raw format to get diff
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, repoName, prNumber)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	// Use stored token if available, otherwise fallback to passed token
	authToken := c.token
	if authToken == "" {
		authToken = token
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("error fetching diff: status %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String(), nil
}

// GetReviewPR matches the signature expected by the reviewer interface
func (c *Client) GetReviewPR(token, owner, repoName string, prNumber int) (PR, error) {
	ctx := context.Background()
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repoName, prNumber)
	if err != nil {
		return PR{}, err
	}

	localPR := PR{
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		URL:       pr.GetHTMLURL(),
		State:     pr.GetState(),
		User:      User{Login: pr.GetUser().GetLogin()},
		CreatedAt: pr.GetCreatedAt().Time,
		Body:      pr.GetBody(),
		Draft:     pr.GetDraft(),
	}
	if pr.MergedAt != nil {
		localPR.MergedAt = pr.MergedAt.Time
	}
	if pr.Base != nil {
		localPR.Base.Ref = pr.Base.GetRef()
	}
	if pr.Head != nil {
		localPR.Head.SHA = pr.Head.GetSHA()
	}
	// Additions/Deletions/ChangedFiles are available in full PR object
	localPR.Additions = pr.GetAdditions()
	localPR.Deletions = pr.GetDeletions()
	localPR.ChangedFiles = pr.GetChangedFiles()

	return localPR, nil
}

func (c *Client) GetPRFiles(token, owner, repoName string, prNumber int) ([]PRFile, error) {
	ctx := context.Background()
	opts := &github.ListOptions{PerPage: 100}
	var allFiles []PRFile

	for {
		files, resp, err := c.gh.PullRequests.ListFiles(ctx, owner, repoName, prNumber, opts)
		if err != nil {
			return nil, err
		}

		for _, f := range files {
			allFiles = append(allFiles, PRFile{
				Filename: f.GetFilename(),
				Status:   f.GetStatus(),
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allFiles, nil
}

func (c *Client) GetFileContent(token, owner, repoName, filePath, branch string) (string, error) {
	ctx := context.Background()
	opts := &github.RepositoryContentGetOptions{Ref: branch}
	fileContent, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repoName, filePath, opts)
	if err != nil {
		return "", err
	}
	if fileContent == nil {
		return "", fmt.Errorf("file content is nil")
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return "", fmt.Errorf("error decoding file content: %w", err)
	}
	return content, nil
}

func (c *Client) PostLineComment(token, owner, repoName string, prNumber int, commitID, comment, path string, line int) error {
	ctx := context.Background()
	cComment := &github.PullRequestComment{
		Body:     &comment,
		Path:     &path,
		Line:     &line,
		Side:     github.String("RIGHT"),
		CommitID: &commitID,
	}
	_, _, err := c.gh.PullRequests.CreateComment(ctx, owner, repoName, prNumber, cComment)
	return err
}

func (c *Client) PostPRComment(token, owner, repoName string, prNumber int, comment string) error {
	ctx := context.Background()
	issueComment := &github.IssueComment{
		Body: &comment,
	}
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repoName, prNumber, issueComment)
	return err
}

func (c *Client) GetPRApprovals(token, owner, repo string, prNumber int) (int, error) {
	result, _, err := c.GetApprovalCount(context.Background(), owner, repo, prNumber)
	return result, err
}

func (c *Client) GetPRReviews(token, owner, repo string, prNumber int) ([]Review, error) {
	ctx := context.Background()
	ghReviews, _, err := c.ListReviews(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, err
	}

	var reviews []Review
	for _, r := range ghReviews {
		reviews = append(reviews, Review{
			State:       r.GetState(),
			User:        User{Login: r.GetUser().GetLogin()},
			SubmittedAt: r.GetSubmittedAt().Time,
		})
	}
	return reviews, nil
}

func (c *Client) GetPRComments(token, owner, repoName string, prNumber int) ([]PRComment, error) {
	ctx := context.Background()

	// Issue comments
	issueComments, _, err := c.gh.Issues.ListComments(ctx, owner, repoName, prNumber, nil)
	if err != nil {
		return nil, err
	}

	// Review comments
	reviewComments, _, err := c.gh.PullRequests.ListComments(ctx, owner, repoName, prNumber, nil)
	if err != nil {
		return nil, err
	}

	var allComments []PRComment
	for _, ic := range issueComments {
		allComments = append(allComments, PRComment{
			ID:        int(ic.GetID()),
			Body:      ic.GetBody(),
			User:      User{Login: ic.GetUser().GetLogin()},
			CreatedAt: ic.GetCreatedAt().Time,
			UpdatedAt: ic.GetUpdatedAt().Time,
		})
	}
	for _, rc := range reviewComments {
		allComments = append(allComments, PRComment{
			ID:           int(rc.GetID()),
			Body:         rc.GetBody(),
			User:         User{Login: rc.GetUser().GetLogin()},
			CreatedAt:    rc.GetCreatedAt().Time,
			UpdatedAt:    rc.GetUpdatedAt().Time,
			Path:         rc.GetPath(),
			Line:         rc.GetLine(),
			DiffHunk:     rc.GetDiffHunk(),
			OriginalLine: rc.GetOriginalLine(),
			CommitID:     rc.GetCommitID(),
		})
	}
	return allComments, nil
}

func (c *Client) GetCurrentUser(token string) (User, error) {
	// If username is already set in client, return it
	if c.username != "" {
		return User{Login: c.username}, nil
	}
	// Otherwise fetch
	ctx := context.Background()
	u, _, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		return User{}, err
	}
	return User{Login: u.GetLogin()}, nil
}
