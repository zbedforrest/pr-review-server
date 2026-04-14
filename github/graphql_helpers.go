package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const graphQLEndpoint = "https://api.github.com/graphql"

// executeGraphQL executes a GraphQL query and decodes the response into the provided result struct.
// This consolidates the common GraphQL execution boilerplate used across multiple functions.
func (c *Client) executeGraphQL(ctx context.Context, query string, result interface{}) error {
	graphqlQuery := map[string]string{"query": query}
	jsonData, err := json.Marshal(graphqlQuery)
	if err != nil {
		return fmt.Errorf("failed to marshal GraphQL query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", graphQLEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to build HTTP request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GraphQL query failed with status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to decode GraphQL response: %w", err)
	}

	return nil
}

// executeGraphQL on AppClient executes a GraphQL query using the App installation token.
func (c *AppClient) executeGraphQL(ctx context.Context, query string, result interface{}) error {
	token, err := c.getInstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get installation token: %w", err)
	}

	graphqlQuery := map[string]string{"query": query}
	jsonData, err := json.Marshal(graphqlQuery)
	if err != nil {
		return fmt.Errorf("failed to marshal GraphQL query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", graphQLEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to build HTTP request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GraphQL query failed with status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to decode GraphQL response: %w", err)
	}

	return nil
}

// buildPRAliasMap creates a mapping from PR aliases (pr0, pr1, etc.) to PR numbers.
// This is used when building batched GraphQL queries with aliases.
func buildPRAliasMap(prs []PullRequest) map[string]int {
	aliases := make(map[string]int)
	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		aliases[alias] = pr.Number
	}
	return aliases
}

// buildPRInfoAliasMap creates a mapping from PR aliases to full PR info structs.
// This is used for CI status queries that need owner/repo/number info.
func buildPRInfoAliasMap(prs []PRInfoWithCommit) map[string]PRInfo {
	aliases := make(map[string]PRInfo)
	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		aliases[alias] = PRInfo{
			Owner:  pr.Owner,
			Repo:   pr.Repo,
			Number: pr.Number,
		}
	}
	return aliases
}

// isValidReviewState checks if a review state should be considered for approval counting.
// PENDING and DISMISSED states are excluded.
func isValidReviewState(state string) bool {
	return state != "PENDING" && state != "DISMISSED"
}

// countUserApprovals processes review nodes and counts approvals, tracking each user's latest review.
// Returns the approval count, the current user's review status, and a map of all users' latest review states.
func (c *Client) countUserApprovals(reviews ReviewsData) (approvalCount int, myReviewStatus string, userReviews map[string]string) {
	userLatestReview := make(map[string]string)

	for _, reviewNode := range reviews.Nodes {
		// Bot reviews or deleted users might have nil author
		if reviewNode.Author == nil {
			continue
		}

		username := reviewNode.Author.Login
		state := reviewNode.State

		// Track latest review per user (reviews are in chronological order)
		if state == "DISMISSED" {
			// Dismissal invalidates the user's previous review state.
			// e.g. CHANGES_REQUESTED -> APPROVED -> DISMISSED means the user
			// has no active review and needs to re-review.
			delete(userLatestReview, username)
		} else if isValidReviewState(state) {
			userLatestReview[username] = state
		}
	}

	// Count approvals
	for _, state := range userLatestReview {
		if state == "APPROVED" {
			approvalCount++
		}
	}

	// Find current user's review status
	if status, exists := userLatestReview[c.username]; exists {
		myReviewStatus = status
	}

	return approvalCount, myReviewStatus, userLatestReview
}

// prKey generates a unique key for a PR in the format "owner/repo/number".
func prKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s/%d", owner, repo, number)
}

// buildPRStateQuery builds a GraphQL query to fetch state + HEAD SHA for multiple PRs.
// Each PR gets its own alias with a full repository lookup (cross-repo pattern).
func buildPRStateQuery(prs []PRInfo) string {
	var qb strings.Builder
	qb.WriteString("query {")
	for i, pr := range prs {
		fmt.Fprintf(&qb, ` pr%d: repository(owner: %q, name: %q) { pullRequest(number: %d) { state headRefOid } }`, i, pr.Owner, pr.Repo, pr.Number)
	}
	qb.WriteString(" }")
	return qb.String()
}
