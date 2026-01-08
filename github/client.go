package github

import (
	"context"
	"fmt"

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

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	result, _, err := c.gh.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, err
	}

	var prs []PullRequest
	for _, issue := range result.Issues {
		if issue.PullRequestLinks == nil {
			continue
		}

		// Extract owner and repo from repository URL
		repoOwner := issue.Repository.GetOwner().GetLogin()
		repoName := issue.Repository.GetName()
		prNumber := issue.GetNumber()

		// Get the PR to fetch the HEAD commit SHA
		pr, _, err := c.gh.PullRequests.Get(ctx, repoOwner, repoName, prNumber)
		if err != nil {
			continue // Skip this PR if we can't fetch it
		}

		prs = append(prs, PullRequest{
			Owner:     repoOwner,
			Repo:      repoName,
			Number:    prNumber,
			CommitSHA: pr.GetHead().GetSHA(),
			Title:     pr.GetTitle(),
			URL:       pr.GetHTMLURL(),
		})
	}

	return prs, nil
}
