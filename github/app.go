package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gogithub "github.com/google/go-github/v57/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

// AppClient wraps the GitHub client authenticated as a GitHub App
type AppClient struct {
	appID          string
	installationID string
	privateKey     *rsa.PrivateKey
	httpClient     *http.Client

	// Installation token caching
	installationToken     string
	installationTokenExp  time.Time
	installationTokenLock sync.RWMutex

	// GitHub clients (recreated when token refreshes)
	gh     *gogithub.Client
	ghv4   *githubv4.Client
	ghLock sync.RWMutex
}

// InstallationToken represents a GitHub App installation token
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewAppClient creates a new GitHub App client
func NewAppClient(appID, privateKeyPath, installationID string) (*AppClient, error) {
	// Read private key file
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}

	// Parse PEM block
	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("failed to parse PEM block from private key")
	}

	// Parse private key
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 format
		keyInterface, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse private key (tried PKCS1 and PKCS8): %v / %v", err, err2)
		}
		var ok bool
		key, ok = keyInterface.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
	}

	client := &AppClient{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}

	// Get initial installation token and create GitHub clients
	if _, err := client.getInstallationToken(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to get initial installation token: %w", err)
	}

	return client, nil
}

// generateJWT creates a JWT for GitHub App authentication
func (c *AppClient) generateJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		// Issued 60 seconds in the past to allow for clock drift
		IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)),
		// JWT expires in 10 minutes (maximum allowed by GitHub)
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		// GitHub App ID as the issuer
		Issuer: c.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(c.privateKey)
}

// getInstallationToken gets or refreshes the installation access token
func (c *AppClient) getInstallationToken(ctx context.Context) (string, error) {
	c.installationTokenLock.RLock()
	// Check if we have a valid cached token (with 5 minute buffer)
	if c.installationToken != "" && time.Now().Add(5*time.Minute).Before(c.installationTokenExp) {
		token := c.installationToken
		c.installationTokenLock.RUnlock()
		return token, nil
	}
	c.installationTokenLock.RUnlock()

	// Need to refresh the token
	c.installationTokenLock.Lock()
	defer c.installationTokenLock.Unlock()

	// Double-check after acquiring write lock
	if c.installationToken != "" && time.Now().Add(5*time.Minute).Before(c.installationTokenExp) {
		return c.installationToken, nil
	}

	// Generate JWT for authentication
	jwtToken, err := c.generateJWT()
	if err != nil {
		return "", fmt.Errorf("failed to generate JWT: %w", err)
	}

	// Request installation token
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", c.installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to request installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to get installation token: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode token response: %w", err)
	}

	// Cache the token
	c.installationToken = tokenResp.Token
	c.installationTokenExp = tokenResp.ExpiresAt

	// Update GitHub clients with new token
	c.updateGitHubClients(tokenResp.Token)

	log.Printf("[GITHUB APP] Installation token refreshed, expires at %s", tokenResp.ExpiresAt.Format(time.RFC3339))

	return tokenResp.Token, nil
}

// updateGitHubClients creates new GitHub clients with the given token
func (c *AppClient) updateGitHubClients(token string) {
	c.ghLock.Lock()
	defer c.ghLock.Unlock()

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)

	c.gh = gogithub.NewClient(tc)
	c.ghv4 = githubv4.NewClient(tc)
}

// GetClient returns the go-github client, refreshing the token if needed
func (c *AppClient) GetClient(ctx context.Context) (*gogithub.Client, error) {
	// Ensure token is valid
	if _, err := c.getInstallationToken(ctx); err != nil {
		return nil, err
	}

	c.ghLock.RLock()
	defer c.ghLock.RUnlock()
	return c.gh, nil
}

// GetToken returns the current installation token, refreshing if needed
func (c *AppClient) GetToken(ctx context.Context) (string, error) {
	return c.getInstallationToken(ctx)
}

// SearchOpenPRsForOrg returns lightweight PR identifiers for all open PRs in the org.
// This uses the GitHub Search API and is fast (no per-PR detail fetches).
func (c *AppClient) SearchOpenPRsForOrg(ctx context.Context, orgName string) ([]PRInfo, error) {
	gh, err := c.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf("type:pr state:open org:%s", orgName)
	log.Printf("[GITHUB APP] Search query: %s", query)

	opts := &gogithub.SearchOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	var allPRs []PRInfo
	for {
		result, resp, err := gh.Search.Issues(ctx, query, opts)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}

		log.Printf("[GITHUB APP] Found %d PRs (page %d)", len(result.Issues), opts.Page)

		for _, issue := range result.Issues {
			if issue.PullRequestLinks == nil {
				continue
			}

			repoURL := issue.GetRepositoryURL()
			parts := splitRepoURL(repoURL)
			if len(parts) < 2 {
				log.Printf("[GITHUB APP] Invalid repository URL: %s", repoURL)
				continue
			}

			allPRs = append(allPRs, PRInfo{
				Owner:  parts[0],
				Repo:   parts[1],
				Number: issue.GetNumber(),
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allPRs, nil
}

// batchGetPRDetailsChunk fetches PR details for a batch of PRs via GraphQL.
// Max ~50 PRs per call to stay within GraphQL complexity limits.
func (c *AppClient) batchGetPRDetailsChunk(ctx context.Context, prs []PRInfo) (map[string]PullRequest, error) {
	if len(prs) == 0 {
		return make(map[string]PullRequest), nil
	}

	// Build GraphQL query with aliases
	var queryBuilder strings.Builder
	queryBuilder.WriteString("query {")
	aliasMap := make(map[string]PRInfo) // alias -> PRInfo

	for i, pr := range prs {
		alias := fmt.Sprintf("pr%d", i)
		aliasMap[alias] = pr
		queryBuilder.WriteString(fmt.Sprintf(`
			%s: repository(owner: %q, name: %q) {
				pullRequest(number: %d) {
					number
					title
					url
					author { login }
					createdAt
					isDraft
					headRefOid
				}
			}
		`, alias, pr.Owner, pr.Repo, pr.Number))
	}
	queryBuilder.WriteString("}")

	var graphqlResp GraphQLPRDetailsResponse
	if err := c.executeGraphQL(ctx, queryBuilder.String(), &graphqlResp); err != nil {
		return nil, err
	}

	results := make(map[string]PullRequest)
	for alias, info := range aliasMap {
		repoData, ok := graphqlResp.Data[alias]
		if !ok {
			log.Printf("[GITHUB APP] Warning: No data for alias %s (PR %s/%s#%d)", alias, info.Owner, info.Repo, info.Number)
			continue
		}

		pr := repoData.PullRequest
		var createdAt *time.Time
		if t, err := time.Parse(time.RFC3339, pr.CreatedAt); err == nil {
			createdAt = &t
		}

		key := prKey(info.Owner, info.Repo, pr.Number)
		results[key] = PullRequest{
			Owner:     info.Owner,
			Repo:      info.Repo,
			Number:    pr.Number,
			CommitSHA: pr.HeadRefOid,
			Title:     pr.Title,
			URL:       pr.URL,
			Author:    pr.Author.Login,
			CreatedAt: createdAt,
			Draft:     pr.IsDraft,
		}
	}

	return results, nil
}

// BatchGetPRDetails fetches full PR details for multiple PRs using GraphQL.
// PRs are batched in groups of 50 to stay within GraphQL complexity limits.
func (c *AppClient) BatchGetPRDetails(ctx context.Context, prs []PRInfo) (map[string]PullRequest, error) {
	const batchSize = 50
	results := make(map[string]PullRequest)

	for i := 0; i < len(prs); i += batchSize {
		end := i + batchSize
		if end > len(prs) {
			end = len(prs)
		}
		chunk := prs[i:end]

		log.Printf("[GITHUB APP] Fetching PR details batch %d-%d of %d via GraphQL", i+1, end, len(prs))
		chunkResults, err := c.batchGetPRDetailsChunk(ctx, chunk)
		if err != nil {
			log.Printf("[GITHUB APP] Warning: Failed to fetch PR details batch: %v", err)
			continue
		}

		for k, v := range chunkResults {
			results[k] = v
		}
	}

	return results, nil
}

// GetAllOpenPRsForOrg fetches all open PRs across all org repos.
// Uses search API for discovery + GraphQL for batch detail fetching.
func (c *AppClient) GetAllOpenPRsForOrg(ctx context.Context, orgName string) ([]PullRequest, error) {
	infos, err := c.SearchOpenPRsForOrg(ctx, orgName)
	if err != nil {
		return nil, err
	}

	detailsMap, err := c.BatchGetPRDetails(ctx, infos)
	if err != nil {
		return nil, err
	}

	allPRs := make([]PullRequest, 0, len(detailsMap))
	for _, pr := range detailsMap {
		allPRs = append(allPRs, pr)
	}

	return allPRs, nil
}

// GetPRReviewRequests fetches the requested reviewers for a PR
// Returns both individual users and teams
func (c *AppClient) GetPRReviewRequests(ctx context.Context, owner, repo string, prNumber int) ([]string, []string, error) {
	gh, err := c.GetClient(ctx)
	if err != nil {
		return nil, nil, err
	}

	reviewers, _, err := gh.PullRequests.ListReviewers(ctx, owner, repo, prNumber, nil)
	if err != nil {
		return nil, nil, err
	}

	var users []string
	for _, user := range reviewers.Users {
		users = append(users, user.GetLogin())
	}

	var teams []string
	for _, team := range reviewers.Teams {
		teams = append(teams, team.GetSlug())
	}

	return users, teams, nil
}

// GetOrgTeamMembers fetches all members of a team in the organization
func (c *AppClient) GetOrgTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error) {
	gh, err := c.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	opts := &gogithub.TeamListTeamMembersOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	var members []string
	for {
		teamMembers, resp, err := gh.Teams.ListTeamMembersBySlug(ctx, orgName, teamSlug, opts)
		if err != nil {
			return nil, err
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

// splitRepoURL parses a repository URL and returns [owner, repo]
func splitRepoURL(repoURL string) []string {
	// Format: https://api.github.com/repos/{owner}/{repo}
	parts := make([]string, 0, 2)
	trimmed := repoURL

	// Find the last two path segments
	lastSlash := -1
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] == '/' {
			if lastSlash == -1 {
				lastSlash = i
			} else {
				parts = append([]string{trimmed[i+1 : lastSlash]}, trimmed[lastSlash+1:])
				break
			}
		}
	}

	return parts
}
