package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v57/github"
)

// ReviewCommentInput mirrors publisher.ReviewCommentInput; this package must
// not import publisher, so the wiring layer converts between the two.
type ReviewCommentInput struct {
	Path      string
	Line      int
	StartLine int
	Body      string
}

type IssueCommentInfo struct {
	ID     int64
	Author string
	Body   string
}

type ReviewCommentInfo struct {
	ID          int64
	Author      string
	Body        string
	Path        string
	Line        int
	StartLine   int
	InReplyToID int64
}

const listPageSize = 100

// CreateReview submits one COMMENT review carrying all inline comments. The
// create response omits the comment ids, so they are read back from the
// review and matched to the inputs by position, falling back to path+line.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []ReviewCommentInput) (int64, []int64, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return 0, nil, err
	}
	drafts := make([]*github.DraftReviewComment, 0, len(comments))
	for _, in := range comments {
		d := &github.DraftReviewComment{
			Path: github.String(in.Path),
			Body: github.String(in.Body),
			Line: github.Int(in.Line),
			Side: github.String("RIGHT"),
		}
		if in.StartLine > 0 && in.StartLine < in.Line {
			d.StartLine = github.Int(in.StartLine)
			d.StartSide = github.String("RIGHT")
		}
		drafts = append(drafts, d)
	}
	req := &github.PullRequestReviewRequest{
		CommitID: github.String(commitSHA),
		Event:    github.String("COMMENT"),
		Comments: drafts,
	}
	if body != "" {
		req.Body = github.String(body)
	}
	review, _, err := gh.PullRequests.CreateReview(ctx, owner, repo, number, req)
	if err != nil {
		return 0, nil, fmt.Errorf("create review: %w", err)
	}
	reviewID := review.GetID()
	if len(comments) == 0 {
		return reviewID, nil, nil
	}

	var created []*github.PullRequestComment
	opts := &github.ListOptions{PerPage: listPageSize}
	for {
		page, resp, err := gh.PullRequests.ListReviewComments(ctx, owner, repo, number, reviewID, opts)
		if err != nil {
			return reviewID, nil, fmt.Errorf("list review %d comments: %w", reviewID, err)
		}
		created = append(created, page...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	// Match by body first: the caller authored every body and the per-review
	// listing may omit line numbers. Path and line is the fallback.
	used := make([]bool, len(created))
	ids := make([]int64, len(comments))
	for i, in := range comments {
		for j, rc := range created {
			if !used[j] && rc.GetBody() == in.Body {
				ids[i] = rc.GetID()
				used[j] = true
				break
			}
		}
	}
	for i, in := range comments {
		if ids[i] != 0 {
			continue
		}
		for j, rc := range created {
			if !used[j] && rc.GetPath() == in.Path && rc.GetLine() == in.Line {
				ids[i] = rc.GetID()
				used[j] = true
				break
			}
		}
	}
	return reviewID, ids, nil
}

func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	created, _, err := gh.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{Body: github.String(body)})
	if err != nil {
		return 0, fmt.Errorf("create issue comment: %w", err)
	}
	return created.GetID(), nil
}

func (c *Client) EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return err
	}
	if _, _, err := gh.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{Body: github.String(body)}); err != nil {
		return fmt.Errorf("edit issue comment %d: %w", commentID, err)
	}
	return nil
}

func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]IssueCommentInfo, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []IssueCommentInfo
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: listPageSize}}
	for {
		page, resp, err := gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list issue comments: %w", err)
		}
		for _, ic := range page {
			out = append(out, IssueCommentInfo{ID: ic.GetID(), Author: ic.GetUser().GetLogin(), Body: ic.GetBody()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) ListReviewComments(ctx context.Context, owner, repo string, number int) ([]ReviewCommentInfo, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []ReviewCommentInfo
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: listPageSize}}
	for {
		page, resp, err := gh.PullRequests.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list review comments: %w", err)
		}
		for _, rc := range page {
			out = append(out, ReviewCommentInfo{
				ID:          rc.GetID(),
				Author:      rc.GetUser().GetLogin(),
				Body:        rc.GetBody(),
				Path:        rc.GetPath(),
				Line:        rc.GetLine(),
				StartLine:   rc.GetStartLine(),
				InReplyToID: rc.GetInReplyTo(),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// GetPRFilePatches returns filename -> unified diff patch. Files GitHub
// serves without a patch (binary, too large) are omitted.
func (c *Client) GetPRFilePatches(ctx context.Context, owner, repo string, number int) (map[string]string, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	opts := &github.ListOptions{PerPage: listPageSize}
	for {
		page, resp, err := gh.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list pull request files: %w", err)
		}
		for _, f := range page {
			if f.Patch != nil {
				out[f.GetFilename()] = f.GetPatch()
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}
