package poller

import (
	"context"
	"errors"
	"testing"

	gh "github.com/google/go-github/v57/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/tickets"
)

type fakeTicketFetcher struct {
	tickets map[string]tickets.Ticket
	calls   []string
}

func (f *fakeTicketFetcher) Fetch(ctx context.Context, key string) (tickets.Ticket, error) {
	f.calls = append(f.calls, key)
	if t, ok := f.tickets[key]; ok {
		return t, nil
	}
	return tickets.Ticket{}, errors.New("boom")
}

func jiraTestPoller(fetcher tickets.Fetcher, mockGH *MockGitHubClient) *Poller {
	p := newTestPoller(mockGH, NewMockDatabase())
	p.cfg.JiraBaseURL = "https://jira.acme.example"
	p.cfg.JiraEmail = "bot@acme.example"
	p.cfg.JiraAPIToken = "tok"
	p.cfg.JiraProjectKeys = []string{"XO"}
	p.ticketFetcher = fetcher
	return p
}

func TestLinkedTicketContextFetchesKeysFromTitleBodyAndBranch(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockGH.GetPRResults["acme/example/7"] = struct {
		PR  *gh.PullRequest
		Err error
	}{PR: &gh.PullRequest{
		Title: gh.String("XO-370 tighten retries"),
		Body:  gh.String("Body from GitHub"),
		Head:  &gh.PullRequestBranch{Ref: gh.String("feature/XO-371-jitter")},
	}}
	fetcher := &fakeTicketFetcher{tickets: map[string]tickets.Ticket{
		"XO-370": {Key: "XO-370", Summary: "Retry policy"},
		"XO-371": {Key: "XO-371", Summary: "Jitter"},
	}}
	p := jiraTestPoller(fetcher, mockGH)
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7, Title: "XO-370 tighten retries"}

	got := p.linkedTicketContext(context.Background(), pr, "Fixes XO-372 and AB-1.")

	assert.Equal(t, "XO-370 tighten retries", got.Title)
	assert.Equal(t, "Fixes XO-372 and AB-1.", got.Body, "first-pass body wins over a GitHub refetch")
	assert.Equal(t, []string{"XO-370", "XO-372", "XO-371"}, fetcher.calls, "title, body, then branch; AB-1 filtered by project allowlist")
	require.Len(t, got.Tickets, 2)
	assert.Equal(t, []string{"XO-370", "XO-371"}, got.keys(), "only fetched tickets are recorded")

	var cfg service.AgentConfig
	got.applyTo(&cfg)
	assert.Equal(t, "XO-370 tighten retries", cfg.PRTitle)
	assert.Equal(t, "Fixes XO-372 and AB-1.", cfg.PRBody)
	assert.Equal(t, got.Tickets, cfg.LinkedTickets)

	runInfo := &payload.ReviewRunInfo{}
	runInfo.LinkedTickets = got.keys()
	assert.Equal(t, []string{"XO-370", "XO-371"}, runInfo.LinkedTickets)
}

func TestLinkedTicketContextFallsBackToGitHubBodyWhenFirstPassHasNone(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockGH.GetPRResults["acme/example/8"] = struct {
		PR  *gh.PullRequest
		Err error
	}{PR: &gh.PullRequest{Body: gh.String("Refs XO-9"), Head: &gh.PullRequestBranch{Ref: gh.String("main")}}}
	fetcher := &fakeTicketFetcher{tickets: map[string]tickets.Ticket{"XO-9": {Key: "XO-9"}}}
	p := jiraTestPoller(fetcher, mockGH)

	got := p.linkedTicketContext(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 8, Title: "T"}, "")

	assert.Equal(t, "Refs XO-9", got.Body)
	assert.Equal(t, []string{"XO-9"}, got.keys())
}

func TestLinkedTicketContextWithJiraOffStillCarriesTitleAndBody(t *testing.T) {
	mockGH := NewMockGitHubClient()
	fetcher := &fakeTicketFetcher{tickets: map[string]tickets.Ticket{"XO-1": {Key: "XO-1"}}}
	p := newTestPoller(mockGH, NewMockDatabase())
	p.ticketFetcher = fetcher

	got := p.linkedTicketContext(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 9, Title: "XO-1 thing"}, "body")

	assert.Equal(t, "XO-1 thing", got.Title)
	assert.Equal(t, "body", got.Body)
	assert.Empty(t, got.Tickets)
	assert.Empty(t, got.keys())
	assert.Empty(t, fetcher.calls, "no fetch without Jira configured")
	assert.Empty(t, mockGH.GetPRResults, "no GitHub refetch without Jira configured")
}

func TestLinkedTicketContextSurvivesGitHubAndFetchFailures(t *testing.T) {
	mockGH := NewMockGitHubClient()
	fetcher := &fakeTicketFetcher{}
	p := jiraTestPoller(fetcher, mockGH)

	got := p.linkedTicketContext(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 10, Title: "XO-5 broken"}, "")

	assert.Equal(t, "XO-5 broken", got.Title)
	assert.Equal(t, []string{"XO-5"}, fetcher.calls, "keys from the title still get tried when GetPR fails")
	assert.Empty(t, got.Tickets)
	assert.Empty(t, got.keys())
}

func TestLinkedTicketContextSkipsFetchWhenNoKeys(t *testing.T) {
	mockGH := NewMockGitHubClient()
	mockGH.GetPRResults["acme/example/11"] = struct {
		PR  *gh.PullRequest
		Err error
	}{PR: &gh.PullRequest{Head: &gh.PullRequestBranch{Ref: gh.String("fix/typo")}}}
	fetcher := &fakeTicketFetcher{}
	p := jiraTestPoller(fetcher, mockGH)

	got := p.linkedTicketContext(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 11, Title: "fix typo"}, "no keys here")

	assert.Empty(t, fetcher.calls)
	assert.Empty(t, got.Tickets)
}

func TestDefaultTicketFetcherIsJiraWhenConfigured(t *testing.T) {
	p := jiraTestPoller(nil, NewMockGitHubClient())
	f, ok := p.ticketFetcherOrDefault().(*tickets.JiraFetcher)
	require.True(t, ok)
	assert.Equal(t, "https://jira.acme.example", f.BaseURL)
	assert.Equal(t, "bot@acme.example", f.Email)
	assert.Equal(t, "tok", f.APIToken)
}
