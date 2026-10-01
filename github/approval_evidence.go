package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v57/github"
)

type ApprovalEndpoint struct {
	Name     string `json:"name"`
	Pages    int    `json:"pages"`
	Complete bool   `json:"complete"`
	Error    string `json:"error,omitempty"`
}

type ApprovalThread struct {
	ID       string  `json:"id"`
	Resolved bool    `json:"isResolved"`
	Outdated bool    `json:"isOutdated"`
	Comments []int64 `json:"comments"`
}

type ApprovalEvidence struct {
	PR              *gh.PullRequest
	Reviews         []*gh.PullRequestReview
	Comments        []*gh.IssueComment
	InlineComments  []*gh.PullRequestComment
	Threads         []ApprovalThread
	Checks          []*gh.CheckRun
	Suites          []*gh.CheckSuite
	Statuses        []*gh.RepoStatus
	RequestedUsers  []*gh.User
	RequestedTeams  []*gh.Team
	Endpoints       []ApprovalEndpoint
	Bytes           int
	AccessPartition string
	MergeBase       string
}

type ApprovalReadLimits struct{ Pages, Items, Bytes int }

var approvalRepoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type approvalReader struct {
	client *gh.Client
	limits ApprovalReadLimits
	result *ApprovalEvidence
	// limited holds the first rate-limit error; later reads fail fast with it.
	limited error
}

// approvalReadError keeps the fixed text stored in manifests while letting
// callers see the cause, so a rate limit can pause and retry collection.
type approvalReadError struct{ cause error }

func (e *approvalReadError) Error() string { return "evidence request failed" }
func (e *approvalReadError) Unwrap() error { return e.cause }

type approvalBuffer struct {
	data  []byte
	limit int
}

func (b *approvalBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, fmt.Errorf("evidence byte limit exceeded")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (r *approvalReader) read(ctx context.Context, method, path string, body any, out any) (*gh.Response, error) {
	if r.limited != nil {
		return nil, r.limited
	}
	req, err := r.client.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	buf := &approvalBuffer{limit: r.limits.Bytes - r.result.Bytes}
	resp, err := r.client.Do(ctx, req, buf)
	r.result.Bytes += len(buf.data)
	if err != nil {
		failed := &approvalReadError{cause: err}
		if _, limited := RateLimited(err, time.Now()); limited {
			r.limited = failed
		}
		return resp, failed
	}
	if err = json.Unmarshal(buf.data, out); err != nil {
		return resp, fmt.Errorf("invalid evidence response")
	}
	return resp, nil
}

func approvalPages[T any](ctx context.Context, r *approvalReader, name, path, field string) []T {
	ep := ApprovalEndpoint{Name: name}
	defer func() { r.result.Endpoints = append(r.result.Endpoints, ep) }()
	var all []T
	responseTotal := -1
	for page := 1; page <= r.limits.Pages; page++ {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var raw json.RawMessage
		resp, err := r.read(ctx, "GET", fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), nil, &raw)
		ep.Pages++
		if err != nil {
			ep.Error = err.Error()
			return all
		}
		if field != "" {
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil || obj[field] == nil {
				ep.Error = "missing required response field"
				return all
			}
			if count, ok := obj["total_count"]; ok {
				if json.Unmarshal(count, &responseTotal) != nil || responseTotal < 0 {
					ep.Error = "invalid evidence total"
					return all
				}
			}
			raw = obj[field]
		}
		var batch []T
		if string(raw) == "null" || json.Unmarshal(raw, &batch) != nil {
			ep.Error = "invalid evidence page"
			return all
		}
		if len(all)+len(batch) > r.limits.Items {
			ep.Error = "evidence item limit exceeded"
			return all
		}
		all = append(all, batch...)
		if resp.NextPage == 0 {
			if responseTotal > len(all) {
				ep.Error = "evidence total exceeds collected items"
				return all
			}
			ep.Complete = true
			return all
		}
		if resp.NextPage != page+1 {
			ep.Error = "invalid pagination"
			return all
		}
	}
	ep.Error = "evidence page limit exceeded"
	return all
}

func (c *Client) CollectApprovalEvidence(ctx context.Context, owner, repo string, number int, limits ApprovalReadLimits) (*ApprovalEvidence, error) {
	if !approvalRepoPart.MatchString(owner) || !approvalRepoPart.MatchString(repo) || owner == "." || repo == "." || owner == ".." || repo == ".." || number < 1 {
		return nil, fmt.Errorf("invalid target")
	}
	if limits.Pages <= 0 {
		limits.Pages = 20
	}
	if limits.Items <= 0 {
		limits.Items = 2000
	}
	if limits.Bytes <= 0 {
		limits.Bytes = 8 << 20
	}
	client, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out := &ApprovalEvidence{AccessPartition: "pat"}
	if c.appClient != nil {
		partition, e := c.appClient.installationFor(ctx, owner, repo)
		if e != nil {
			return nil, e
		}
		out.AccessPartition = "installation:" + partition
	}
	r := &approvalReader{client: client, limits: limits, result: out}
	base := fmt.Sprintf("repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	pr := fmt.Sprintf("%s/pulls/%d", base, number)
	var pull gh.PullRequest
	_, err = r.read(ctx, "GET", pr, nil, &pull)
	if err != nil {
		return nil, err
	}
	out.PR = &pull
	out.Endpoints = append(out.Endpoints, ApprovalEndpoint{Name: "pull", Pages: 1, Complete: true})
	out.Reviews = approvalPages[*gh.PullRequestReview](ctx, r, "reviews", pr+"/reviews", "")
	out.Comments = approvalPages[*gh.IssueComment](ctx, r, "comments", fmt.Sprintf("%s/issues/%d/comments", base, number), "")
	out.InlineComments = approvalPages[*gh.PullRequestComment](ctx, r, "inline_comments", pr+"/comments", "")
	head := pull.GetHead().GetSHA()
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(head) {
		return nil, fmt.Errorf("invalid head revision")
	}
	out.Checks = approvalPages[*gh.CheckRun](ctx, r, "checks", base+"/commits/"+head+"/check-runs?filter=all", "check_runs")
	out.Suites = approvalPages[*gh.CheckSuite](ctx, r, "check_suites", base+"/commits/"+head+"/check-suites", "check_suites")
	out.Statuses = approvalPages[*gh.RepoStatus](ctx, r, "statuses", base+"/commits/"+head+"/statuses", "")
	var comparison gh.CommitsComparison
	_, compareErr := r.read(ctx, "GET", base+"/compare/"+pull.GetBase().GetSHA()+"..."+head, nil, &comparison)
	compareEndpoint := ApprovalEndpoint{Name: "merge_base", Pages: 1, Complete: compareErr == nil}
	if compareErr != nil {
		compareEndpoint.Error = compareErr.Error()
	} else {
		out.MergeBase = comparison.GetMergeBaseCommit().GetSHA()
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(out.MergeBase) {
			compareEndpoint.Complete = false
			compareEndpoint.Error = "missing merge base"
		}
	}
	out.Endpoints = append(out.Endpoints, compareEndpoint)
	out.RequestedUsers, out.RequestedTeams = r.requestedReviewers(ctx, pr+"/requested_reviewers")
	r.threads(ctx, owner, repo, number)
	if r.limited != nil {
		return nil, r.limited
	}
	return out, nil
}

// requestedReviewers reads users and teams from one paged endpoint and
// reports them as two manifest endpoints.
func (r *approvalReader) requestedReviewers(ctx context.Context, path string) ([]*gh.User, []*gh.Team) {
	users, teams := ApprovalEndpoint{Name: "requested_users"}, ApprovalEndpoint{Name: "requested_teams"}
	var allUsers []*gh.User
	var allTeams []*gh.Team
	fail := func(reason string) { users.Error, teams.Error = reason, reason }
	defer func() { r.result.Endpoints = append(r.result.Endpoints, users, teams) }()
	for page := 1; page <= r.limits.Pages; page++ {
		var batch struct {
			Users *[]*gh.User `json:"users"`
			Teams *[]*gh.Team `json:"teams"`
		}
		resp, err := r.read(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &batch)
		users.Pages++
		teams.Pages++
		if err != nil {
			fail(err.Error())
			return allUsers, allTeams
		}
		if batch.Users == nil || batch.Teams == nil {
			fail("missing required response field")
			return allUsers, allTeams
		}
		if len(allUsers)+len(*batch.Users) > r.limits.Items || len(allTeams)+len(*batch.Teams) > r.limits.Items {
			fail("evidence item limit exceeded")
			return allUsers, allTeams
		}
		allUsers = append(allUsers, *batch.Users...)
		allTeams = append(allTeams, *batch.Teams...)
		if resp.NextPage == 0 {
			users.Complete, teams.Complete = true, true
			return allUsers, allTeams
		}
		if resp.NextPage != page+1 {
			fail("invalid pagination")
			return allUsers, allTeams
		}
	}
	fail("evidence page limit exceeded")
	return allUsers, allTeams
}

func (c *Client) ApprovalRepositoryToken(ctx context.Context, owner, repo string) (string, error) {
	if c.appClient == nil {
		return c.token, nil
	}
	token, _, err := c.appClient.TokenForRepo(ctx, owner, repo)
	return token, err
}

type approvalPageInfo struct {
	HasNext bool   `json:"hasNextPage"`
	End     string `json:"endCursor"`
}
type approvalCommentIDs struct {
	Nodes []struct {
		ID approvalDatabaseID `json:"fullDatabaseId"`
	} `json:"nodes"`
	Page *approvalPageInfo `json:"pageInfo"`
}

type approvalDatabaseID int64

func (id *approvalDatabaseID) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("invalid comment identity")
	}
	*id = approvalDatabaseID(parsed)
	return nil
}

type approvalThreadNode struct {
	ID       string              `json:"id"`
	Resolved bool                `json:"isResolved"`
	Outdated bool                `json:"isOutdated"`
	Comments *approvalCommentIDs `json:"comments"`
}

func (r *approvalReader) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	var response struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	path := "graphql"
	if r.client.BaseURL.Host == "api.github.com" {
		path = "https://api.github.com/graphql"
	}
	_, err := r.read(ctx, "POST", path, map[string]any{"query": query, "variables": variables}, &response)
	if err != nil {
		return err
	}
	if len(response.Errors) > 0 || len(response.Data) == 0 || string(response.Data) == "null" {
		return fmt.Errorf("partial GraphQL evidence")
	}
	return json.Unmarshal(response.Data, out)
}

func (r *approvalReader) threads(ctx context.Context, owner, repo string, number int) {
	ep := ApprovalEndpoint{Name: "threads"}
	defer func() { r.result.Endpoints = append(r.result.Endpoints, ep) }()
	var cursor any
	for page := 0; page < r.limits.Pages; page++ {
		var data struct {
			Repository *struct {
				PullRequest *struct {
					Threads *struct {
						Nodes []approvalThreadNode `json:"nodes"`
						Page  *approvalPageInfo    `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		query := `query($owner:String!,$repo:String!,$number:Int!,$after:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100,after:$after){nodes{id isResolved isOutdated comments(first:100){nodes{fullDatabaseId} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`
		err := r.graphql(ctx, query, map[string]any{"owner": owner, "repo": repo, "number": number, "after": cursor}, &data)
		ep.Pages++
		if err != nil {
			ep.Error = err.Error()
			return
		}
		if data.Repository == nil || data.Repository.PullRequest == nil || data.Repository.PullRequest.Threads == nil {
			ep.Error = "missing review threads"
			return
		}
		conn := data.Repository.PullRequest.Threads
		if conn.Page == nil || conn.Nodes == nil {
			ep.Error = "missing thread page metadata"
			return
		}
		for _, node := range conn.Nodes {
			if node.ID == "" || node.Comments == nil {
				ep.Error = "missing thread data"
				return
			}
			if len(r.result.Threads) >= r.limits.Items {
				ep.Error = "thread limit exceeded"
				return
			}
			thread := ApprovalThread{ID: node.ID, Resolved: node.Resolved, Outdated: node.Outdated}
			comments := *node.Comments
			for nested := 0; ; nested++ {
				if comments.Page == nil || comments.Nodes == nil {
					ep.Error = "missing reply page metadata"
					return
				}
				for _, comment := range comments.Nodes {
					if comment.ID <= 0 {
						ep.Error = "missing comment identity"
						return
					}
					thread.Comments = append(thread.Comments, int64(comment.ID))
				}
				if len(thread.Comments) > r.limits.Items {
					ep.Error = "reply limit exceeded"
					return
				}
				if !comments.Page.HasNext {
					break
				}
				if nested+1 >= r.limits.Pages || comments.Page.End == "" {
					ep.Error = "reply pagination incomplete"
					return
				}
				var reply struct {
					Node *struct {
						Comments *approvalCommentIDs `json:"comments"`
					} `json:"node"`
				}
				q := `query($id:ID!,$after:String!){node(id:$id){... on PullRequestReviewThread{comments(first:100,after:$after){nodes{fullDatabaseId} pageInfo{hasNextPage endCursor}}}}}`
				err = r.graphql(ctx, q, map[string]any{"id": node.ID, "after": comments.Page.End}, &reply)
				ep.Pages++
				if err != nil {
					ep.Error = err.Error()
					return
				}
				if reply.Node == nil || reply.Node.Comments == nil {
					ep.Error = "missing thread replies"
					return
				}
				comments = *reply.Node.Comments
			}
			r.result.Threads = append(r.result.Threads, thread)
		}
		if !conn.Page.HasNext {
			ep.Complete = true
			return
		}
		if conn.Page.End == "" || conn.Page.End == cursor {
			ep.Error = "invalid thread pagination"
			return
		}
		cursor = conn.Page.End
	}
	ep.Error = "thread pagination incomplete"
}
