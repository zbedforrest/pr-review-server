package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxComments         = 10
	maxCommentRunes     = 600
	maxDescriptionRunes = 3000
	defaultHTTPTimeout  = 8 * time.Second
)

// ErrNotFound is returned by Fetch when the issue does not exist or is not
// visible to the configured account.
var ErrNotFound = errors.New("ticket not found")

// Ticket is the subset of a Jira issue the review agent gets to see.
type Ticket struct {
	Key         string
	Summary     string
	Status      string
	Type        string
	URL         string
	Description string
	Comments    []Comment
}

// Comment is one issue comment, oldest first within Ticket.Comments.
type Comment struct {
	Author  string
	Created string
	Body    string
}

// Fetcher loads one ticket by key.
type Fetcher interface {
	Fetch(ctx context.Context, key string) (Ticket, error)
}

// JiraFetcher reads issues from Jira Cloud's REST API v3 with basic auth
// (account email + API token).
type JiraFetcher struct {
	BaseURL  string
	Email    string
	APIToken string
	HTTP     *http.Client
}

type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string                `json:"summary"`
		Status      struct{ Name string } `json:"status"`
		IssueType   struct{ Name string } `json:"issuetype"`
		Description json.RawMessage       `json:"description"`
		Comment     struct {
			Comments []jiraComment `json:"comments"`
		} `json:"comment"`
	} `json:"fields"`
	RenderedFields struct {
		Description string `json:"description"`
		Comment     struct {
			Comments []jiraComment `json:"comments"`
		} `json:"comment"`
	} `json:"renderedFields"`
}

type jiraComment struct {
	Author  struct{ DisplayName string } `json:"author"`
	Created string                       `json:"created"`
	Body    json.RawMessage              `json:"body"`
}

func (f *JiraFetcher) Fetch(ctx context.Context, key string) (Ticket, error) {
	base := strings.TrimRight(f.BaseURL, "/")
	endpoint := base + "/rest/api/3/issue/" + url.PathEscape(key) +
		"?fields=" + url.QueryEscape("summary,status,issuetype,description,comment") + "&expand=renderedFields"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Ticket{}, fmt.Errorf("jira %s: %w", key, err)
	}
	req.SetBasicAuth(f.Email, f.APIToken)
	req.Header.Set("Accept", "application/json")

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Ticket{}, fmt.Errorf("jira %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Ticket{}, fmt.Errorf("jira %s: %w", key, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Ticket{}, fmt.Errorf("jira %s: unexpected status %d", key, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Ticket{}, fmt.Errorf("jira %s: read body: %w", key, err)
	}
	var issue jiraIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return Ticket{}, fmt.Errorf("jira %s: decode: %w", key, err)
	}
	if issue.Key == "" {
		issue.Key = key
	}
	return ticketFromIssue(base, issue), nil
}

func ticketFromIssue(base string, issue jiraIssue) Ticket {
	t := Ticket{
		Key:     issue.Key,
		Summary: strings.TrimSpace(issue.Fields.Summary),
		Status:  issue.Fields.Status.Name,
		Type:    issue.Fields.IssueType.Name,
		URL:     base + "/browse/" + issue.Key,
	}
	description := htmlToText(issue.RenderedFields.Description)
	if description == "" {
		description = adfToText(issue.Fields.Description)
	}
	t.Description = truncateRunes(description, maxDescriptionRunes)

	rendered := issue.RenderedFields.Comment.Comments
	raw := issue.Fields.Comment.Comments
	useRendered := len(rendered) > 0 && len(rendered) >= len(raw)
	source := raw
	if useRendered {
		source = rendered
	}
	if len(source) > maxComments {
		source = source[len(source)-maxComments:]
	}
	for _, c := range source {
		var text string
		if useRendered {
			var htmlBody string
			if json.Unmarshal(c.Body, &htmlBody) == nil {
				text = htmlToText(htmlBody)
			}
		} else {
			text = adfToText(c.Body)
		}
		t.Comments = append(t.Comments, Comment{
			Author:  c.Author.DisplayName,
			Created: c.Created,
			Body:    truncateRunes(text, maxCommentRunes),
		})
	}
	return t
}

// FetchAll fetches keys sequentially and returns the tickets that loaded;
// failures are logged through logf (when non-nil) and skipped.
func FetchAll(ctx context.Context, f Fetcher, keys []string, logf func(string, ...any)) []Ticket {
	if f == nil || len(keys) == 0 {
		return nil
	}
	var out []Ticket
	for _, key := range keys {
		t, err := f.Fetch(ctx, key)
		if err != nil {
			if logf != nil {
				logf("[TICKETS] skip %s: %v", key, err)
			}
			continue
		}
		out = append(out, t)
	}
	return out
}
