package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	CreatedAt time.Time
	Draft     bool
}

// PRReviewData holds review information for a single PR
type PRReviewData struct {
	Owner          string
	Repo           string
	Number         int
	ApprovalCount  int
	MyReviewStatus string // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
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
	// Search for PRs where the user is a requested reviewer
	query := fmt.Sprintf("type:pr state:open review-requested:%s", c.username)
	log.Printf("GitHub search query: %s", query)

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	result, resp, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("GitHub search error: %v", err)
		return nil, err
	}

	log.Printf("GitHub search returned %d total results (rate limit: %d/%d remaining)",
		result.GetTotal(), resp.Rate.Remaining, resp.Rate.Limit)

	var prs []PullRequest
	for _, issue := range result.Issues {
		if issue.PullRequestLinks == nil {
			continue
		}

		// Extract owner and repo from repository URL
		// RepositoryURL format: https://api.github.com/repos/{owner}/{repo}
		repoURL := issue.GetRepositoryURL()
		parts := strings.Split(repoURL, "/")
		if len(parts) < 2 {
			log.Printf("Invalid repository URL: %s", repoURL)
			continue
		}
		repoOwner := parts[len(parts)-2]
		repoName := parts[len(parts)-1]
		prNumber := issue.GetNumber()

		log.Printf("Found PR: %s/%s#%d - %s", repoOwner, repoName, prNumber, issue.GetTitle())

		// Get the PR to fetch the HEAD commit SHA
		pr, _, err := c.gh.PullRequests.Get(ctx, repoOwner, repoName, prNumber)
		if err != nil {
			log.Printf("Error fetching PR details for %s/%s#%d: %v", repoOwner, repoName, prNumber, err)
			continue // Skip this PR if we can't fetch it
		}

		prs = append(prs, PullRequest{
			Owner:     repoOwner,
			Repo:      repoName,
			Number:    prNumber,
			CommitSHA: pr.GetHead().GetSHA(),
			Title:     pr.GetTitle(),
			URL:       pr.GetHTMLURL(),
			Author:    pr.GetUser().GetLogin(),
			CreatedAt: pr.GetCreatedAt().Time,
			Draft:     pr.GetDraft(),
		})
	}

	return prs, nil
}

func (c *Client) GetMyOpenPRs(ctx context.Context) ([]PullRequest, error) {
	// Search for PRs authored by the user that are open
	query := fmt.Sprintf("type:pr state:open author:%s", c.username)
	log.Printf("GitHub search query (my PRs): %s", query)

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	result, resp, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("GitHub search error (my PRs): %v", err)
		return nil, err
	}

	log.Printf("GitHub search returned %d of my open PRs (rate limit: %d/%d remaining)",
		result.GetTotal(), resp.Rate.Remaining, resp.Rate.Limit)

	var prs []PullRequest
	for _, issue := range result.Issues {
		if issue.PullRequestLinks == nil {
			continue
		}

		// Extract owner and repo from repository URL
		repoURL := issue.GetRepositoryURL()
		parts := strings.Split(repoURL, "/")
		if len(parts) < 2 {
			log.Printf("Invalid repository URL: %s", repoURL)
			continue
		}
		repoOwner := parts[len(parts)-2]
		repoName := parts[len(parts)-1]
		prNumber := issue.GetNumber()

		log.Printf("Found my PR: %s/%s#%d - %s", repoOwner, repoName, prNumber, issue.GetTitle())

		// Get the PR to fetch the HEAD commit SHA
		pr, _, err := c.gh.PullRequests.Get(ctx, repoOwner, repoName, prNumber)
		if err != nil {
			log.Printf("Error fetching my PR details for %s/%s#%d: %v", repoOwner, repoName, prNumber, err)
			continue
		}

		prs = append(prs, PullRequest{
			Owner:     repoOwner,
			Repo:      repoName,
			Number:    prNumber,
			CommitSHA: pr.GetHead().GetSHA(),
			Title:     pr.GetTitle(),
			URL:       pr.GetHTMLURL(),
			Author:    pr.GetUser().GetLogin(),
			CreatedAt: pr.GetCreatedAt().Time,
			Draft:     pr.GetDraft(),
		})
	}

	return prs, nil
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

	results := make(map[string]*PRReviewData)

	// Fetch review data for each repository
	for repoKey, repoPRs := range prsByRepo {
		log.Printf("[GRAPHQL] Fetching review data for %d PRs in %s", len(repoPRs), repoKey)

		repoData, err := c.fetchReviewDataForRepo(ctx, repoPRs)
		if err != nil {
			log.Printf("[GRAPHQL] Error fetching review data for %s: %v", repoKey, err)
			// Continue with other repos even if one fails
			continue
		}

		// Merge results
		for k, v := range repoData {
			results[k] = v
		}
	}

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

	// Build a single GraphQL query with aliases for all PRs in this repo
	// This reduces N queries to 1 query per repository
	var queryBuilder strings.Builder
	queryBuilder.WriteString("query {")

	// Create an alias for each PR (pr1, pr2, etc.)
	prAliases := make(map[string]int) // alias -> PR number
	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		prAliases[alias] = pr.Number
		// NOTE: reviews(last: 100) fetches the most recent 100 reviews.
		// For PRs with >100 review events, we might miss older review states.
		// This is acceptable since we only care about the most recent state per reviewer.
		// If a PR has >100 reviews from unique reviewers, approval counts may be incomplete,
		// but this is rare in practice. Full pagination would add significant complexity.
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

	// Execute the batched query using raw HTTP POST to GitHub GraphQL API
	graphqlQuery := map[string]string{"query": queryBuilder.String()}
	jsonData, err := json.Marshal(graphqlQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal GraphQL query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.github.com/graphql", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to build HTTP request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GraphQL query failed with status %d", resp.StatusCode)
	}

	// Define structs for type-safe GraphQL response parsing
	type ReviewAuthor struct {
		Login string `json:"login"`
	}
	type ReviewNode struct {
		Author *ReviewAuthor `json:"author"`
		State  string        `json:"state"`
	}
	type ReviewsData struct {
		Nodes []ReviewNode `json:"nodes"`
	}
	type PRData struct {
		Reviews ReviewsData `json:"reviews"`
	}
	type RepoData struct {
		PullRequest PRData `json:"pullRequest"`
	}
	type GraphQLResponse struct {
		Data map[string]RepoData `json:"data"`
	}

	// Parse response
	var graphqlResp GraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&graphqlResp); err != nil {
		return nil, fmt.Errorf("failed to decode GraphQL response: %w", err)
	}

	// Parse results
	results := make(map[string]*PRReviewData)
	for alias, prNumber := range prAliases {
		repoData, ok := graphqlResp.Data[alias]
		if !ok {
			log.Printf("[GRAPHQL] Warning: Failed to parse repo data for alias %s", alias)
			continue
		}

		// Process reviews to count approvals and find my review status
		userLatestReview := make(map[string]string)
		for _, reviewNode := range repoData.PullRequest.Reviews.Nodes {
			// Bot reviews or deleted users might have nil author
			if reviewNode.Author == nil {
				continue
			}

			username := reviewNode.Author.Login
			state := reviewNode.State

			// Track latest review per user (reviews are in chronological order)
			if state != "PENDING" && state != "DISMISSED" {
				userLatestReview[username] = state
			}
		}

		// Count approvals
		approvalCount := 0
		for _, state := range userLatestReview {
			if state == "APPROVED" {
				approvalCount++
			}
		}

		// Find my review status
		myReviewStatus := ""
		if status, exists := userLatestReview[c.username]; exists {
			myReviewStatus = status
		}

		key := fmt.Sprintf("%s/%s/%d", owner, repo, prNumber)
		results[key] = &PRReviewData{
			Owner:          owner,
			Repo:           repo,
			Number:         prNumber,
			ApprovalCount:  approvalCount,
			MyReviewStatus: myReviewStatus,
		}

		log.Printf("[GRAPHQL] PR %s/%s#%d: %d approvals, my status: %s", owner, repo, prNumber, approvalCount, myReviewStatus)
	}

	return results, nil
}
