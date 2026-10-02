package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// ReviewThreadInfo is one pull request review thread: its GraphQL node id,
// the review comment that opened it and the two flags GitHub keeps on it.
type ReviewThreadInfo struct {
	NodeID        string
	RootCommentID int64
	Resolved      bool
	Outdated      bool
}

const (
	listReviewThreadsQuery  = `query($owner:String!,$repo:String!,$number:Int!,$after:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100,after:$after){nodes{id isResolved isOutdated comments(first:1){nodes{fullDatabaseId}}} pageInfo{hasNextPage endCursor}}}}}`
	resolveThreadMutation   = `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}`
	unresolveThreadMutation = `mutation($id:ID!){unresolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}`
)

type threadDatabaseID int64

func (id *threadDatabaseID) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid review comment id %q", value)
	}
	*id = threadDatabaseID(parsed)
	return nil
}

// ListReviewThreads lists every review thread on a pull request with the
// database id of its root comment, through the installation that serves the
// repository.
func (c *Client) ListReviewThreads(ctx context.Context, owner, repo string, number int) ([]ReviewThreadInfo, error) {
	var out []ReviewThreadInfo
	var cursor any
	for {
		var data struct {
			Repository struct {
				PullRequest struct {
					Threads struct {
						Nodes []struct {
							ID       string `json:"id"`
							Resolved bool   `json:"isResolved"`
							Outdated bool   `json:"isOutdated"`
							Comments struct {
								Nodes []struct {
									ID threadDatabaseID `json:"fullDatabaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
						Page struct {
							HasNext bool   `json:"hasNextPage"`
							End     string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": cursor}
		if err := c.repoGraphQL(ctx, owner, repo, listReviewThreadsQuery, vars, &data); err != nil {
			return nil, fmt.Errorf("list review threads: %w", err)
		}
		conn := data.Repository.PullRequest.Threads
		for _, node := range conn.Nodes {
			info := ReviewThreadInfo{NodeID: node.ID, Resolved: node.Resolved, Outdated: node.Outdated}
			if len(node.Comments.Nodes) > 0 {
				info.RootCommentID = int64(node.Comments.Nodes[0].ID)
			}
			out = append(out, info)
		}
		if !conn.Page.HasNext || conn.Page.End == "" || conn.Page.End == cursor {
			return out, nil
		}
		cursor = conn.Page.End
	}
}

// ResolveThread marks a review thread resolved.
func (c *Client) ResolveThread(ctx context.Context, owner, repo, threadNodeID string) error {
	return c.setThreadResolved(ctx, owner, repo, threadNodeID, true)
}

// UnresolveThread reopens a resolved review thread.
func (c *Client) UnresolveThread(ctx context.Context, owner, repo, threadNodeID string) error {
	return c.setThreadResolved(ctx, owner, repo, threadNodeID, false)
}

func (c *Client) setThreadResolved(ctx context.Context, owner, repo, threadNodeID string, resolved bool) error {
	mutation, field := resolveThreadMutation, "resolveReviewThread"
	if !resolved {
		mutation, field = unresolveThreadMutation, "unresolveReviewThread"
	}
	var data map[string]struct {
		Thread *struct {
			ID       string `json:"id"`
			Resolved bool   `json:"isResolved"`
		} `json:"thread"`
	}
	if err := c.repoGraphQL(ctx, owner, repo, mutation, map[string]any{"id": threadNodeID}, &data); err != nil {
		return fmt.Errorf("%s %s: %w", field, threadNodeID, err)
	}
	payload, ok := data[field]
	if !ok || payload.Thread == nil || payload.Thread.Resolved != resolved {
		return fmt.Errorf("%s %s: thread state not confirmed", field, threadNodeID)
	}
	return nil
}

// repoGraphQL runs one GraphQL operation with variables through the client
// that serves the repository, so an App installed on several owners signs the
// call with the right installation token. A response that carries errors or
// no data is an error.
func (c *Client) repoGraphQL(ctx context.Context, owner, repo, query string, variables map[string]any, out any) error {
	gh, err := c.clientFor(ctx, owner, repo)
	if err != nil {
		return err
	}
	req, err := gh.NewRequest("POST", "graphql", map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := gh.Do(ctx, req, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("graphql: %s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("graphql: empty response")
	}
	return json.Unmarshal(envelope.Data, out)
}
