package github

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh       *github.Client
	username string
}

type PullRequest struct {
	Owner     string
	Repo      string
	Number    int
	CommitSHA string
	Title     string
	URL       string
	Author    string
}

func NewClient(token, username string) *Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	return &Client{
		gh:       github.NewClient(tc),
		username: username,
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
