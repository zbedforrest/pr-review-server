package github

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ReactionInfo is one reaction on a comment. The REST reaction object carries
// a created_at the go-github struct drops, so the list is read raw.
type ReactionInfo struct {
	ID        int64
	User      string
	IsBot     bool
	Content   string
	CreatedAt time.Time
}

type rawReaction struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	User    *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// ListReviewCommentReactions lists the reactions on an inline review comment.
func (c *Client) ListReviewCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]ReactionInfo, error) {
	return c.listReactions(ctx, owner, repo, fmt.Sprintf("repos/%s/%s/pulls/comments/%d/reactions", owner, repo, commentID))
}

// ListIssueCommentReactions lists the reactions on a PR conversation comment.
func (c *Client) ListIssueCommentReactions(ctx context.Context, owner, repo string, commentID int64) ([]ReactionInfo, error) {
	return c.listReactions(ctx, owner, repo, fmt.Sprintf("repos/%s/%s/issues/comments/%d/reactions", owner, repo, commentID))
}

func (c *Client) listReactions(ctx context.Context, owner, repo, path string) ([]ReactionInfo, error) {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []ReactionInfo
	for page := 1; ; page++ {
		req, err := gh.NewRequest(http.MethodGet, fmt.Sprintf("%s?per_page=%d&page=%d", path, listPageSize, page), nil)
		if err != nil {
			return nil, err
		}
		var raw []rawReaction
		resp, err := gh.Do(ctx, req, &raw)
		if err != nil {
			return nil, fmt.Errorf("list reactions: %w", err)
		}
		for _, r := range raw {
			info := ReactionInfo{ID: r.ID, Content: r.Content, CreatedAt: r.CreatedAt}
			if r.User != nil {
				info.User, info.IsBot = r.User.Login, r.User.Type == "Bot"
			}
			out = append(out, info)
		}
		if resp.NextPage == 0 {
			return out, nil
		}
	}
}
