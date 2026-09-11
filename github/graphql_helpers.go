package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const graphQLEndpoint = "https://api.github.com/graphql"

// ErrGraphQLRateLimited is returned when a GraphQL response carries a
// rate-limit error. GitHub still returns HTTP 200 with whatever partial data
// it managed, but that data is unreliable (aliases come back null), so callers
// must NOT treat it as authoritative — in particular the poller must not prune
// via_teams off rows that merely appear unentitled because the fetch was
// throttled. Distinct error type so callers can tell throttling apart from a
// genuine failure or a genuinely-empty result.
var ErrGraphQLRateLimited = errors.New("github: GraphQL rate limit exceeded")

// decodeGraphQLResponse decodes a GraphQL HTTP response body into result, after
// surfacing any GraphQL-level errors. GitHub returns HTTP 200 even for PARTIAL
// failures: the affected fields come back null while an `errors` array explains
// why (e.g. a field the token can't read). Without logging these, a partial
// failure is indistinguishable from a legitimately-empty result — which is how
// reviewer-group detection silently returned [] for some PRs. The label
// identifies the calling query; the error `path` pinpoints the alias (e.g.
// pr5 -> pullRequest -> timelineItems) so the failing PR can be cross-referenced.
func decodeGraphQLResponse(label string, body io.Reader, result interface{}) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("failed to read GraphQL response: %w", err)
	}

	var envelope struct {
		Errors []struct {
			Type    string        `json:"type"`
			Message string        `json:"message"`
			Path    []interface{} `json:"path"`
		} `json:"errors"`
	}
	rateLimited := false
	if json.Unmarshal(raw, &envelope) == nil {
		for _, e := range envelope.Errors {
			log.Printf("[GRAPHQL] %s partial error: type=%s path=%v message=%s",
				label, e.Type, e.Path, e.Message)
			if e.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(e.Message), "rate limit") {
				rateLimited = true
			}
		}
	}

	errDecode := json.Unmarshal(raw, result)
	// Surface rate limiting BEFORE any decode error: a throttled response can
	// come back with null fields that fail to unmarshal, and the rate-limit
	// signal is the one callers must act on (skip pruning) rather than a generic
	// decode failure. Decoding still ran first, so callers that want partial data
	// can read whatever populated.
	if rateLimited {
		return ErrGraphQLRateLimited
	}
	if errDecode != nil {
		return fmt.Errorf("failed to decode GraphQL response: %w", errDecode)
	}
	return nil
}

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

	// Use fresh installation token from AppClient if available (tokens expire after 1 hour).
	// Fall back to static token for single-user/dev mode.
	authToken := c.token
	if c.appClient != nil {
		if freshToken, err := c.appClient.GetToken(ctx); err == nil {
			authToken = freshToken
		}
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GraphQL query failed with status %d", resp.StatusCode)
	}

	return decodeGraphQLResponse("client", resp.Body, result)
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

	return decodeGraphQLResponse("appclient", resp.Body, result)
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

// attentionByUser reports, per reviewer, whether their standing decision is CHANGES_REQUESTED
// and no review of theirs targets the current head. Unknown users (no head, or no commit on
// the deciding review) are absent rather than false.
func attentionByUser(reviews ReviewsData, headOID string) map[string]bool {
	result := make(map[string]bool)
	if headOID == "" {
		return result
	}

	decisions := make(map[string]*ReviewNode)
	reviewedHead := make(map[string]bool)
	for i := range reviews.Nodes {
		node := &reviews.Nodes[i]
		if node.Author == nil || node.State == "PENDING" {
			continue
		}
		username := node.Author.Login
		result[username] = false
		if node.Commit != nil && node.Commit.OID == headOID {
			reviewedHead[username] = true
		}
		switch node.State {
		case "DISMISSED":
			delete(decisions, username)
		case "APPROVED", "CHANGES_REQUESTED":
			decisions[username] = node
		}
	}

	for username, decision := range decisions {
		if decision.State != "CHANGES_REQUESTED" {
			continue
		}
		if decision.Commit == nil {
			delete(result, username)
			continue
		}
		result[username] = !reviewedHead[username]
	}
	return result
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
		fmt.Fprintf(&qb, ` pr%d: repository(owner: %q, name: %q) { pullRequest(number: %d) { state headRefOid isDraft } }`, i, pr.Owner, pr.Repo, pr.Number)
	}
	qb.WriteString(" }")
	return qb.String()
}
