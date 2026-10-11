package main

import (
	"context"
	"fmt"
	"strings"

	gogithub "github.com/google/go-github/v57/github"

	"pr-review-server/github"
	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
)

// fromDump reads a PR and its PRism root threads out of a GitHub dump. The
// bots set names the logins whose roots count; empty means detect them from
// the finding markers the way the replay harness does.
func fromDump(d *replaykit.PRDump, bots map[string]bool) *pr {
	if len(bots) == 0 {
		bots = d.BotLogins()
	}
	p := &pr{Owner: d.Owner, Repo: d.Repo, Number: d.Number, Author: d.Author.Login, HeadSHA: d.HeadRefOid, State: stateOf(d.State, d.Merged)}
	known := d.ResolutionKnown()
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		if root.ReplyTo != nil || !bots[replaykit.BareLogin(replaykit.ActorLogin(root.Author))] {
			continue
		}
		id, ok := publisher.FindingIDFromBody(root.Body)
		if !ok {
			continue
		}
		path := t.Path
		if path == "" {
			path = root.Path
		}
		p.Threads = append(p.Threads, thread{
			RootCommentID: int64(root.DatabaseID), NodeID: nodeIDOf(t.ID), FindingID: id, Path: path,
			PostedSHA: root.OriginalCommit.Oid, PostedAt: root.CreatedAt,
			Resolved: t.IsResolved, ResolvedKnown: known, Outdated: t.IsOutdated,
		})
	}
	return p
}

// nodeIDOf keeps a GraphQL thread id and drops the placeholder a REST dump
// stores, which is the root comment's database id.
func nodeIDOf(id string) string {
	if strings.HasPrefix(id, "PRRT_") {
		return id
	}
	return ""
}

func stateOf(state string, merged bool) prState {
	switch {
	case merged:
		return prMerged
	case strings.EqualFold(state, "closed"):
		return prClosed
	}
	return prOpen
}

type githubSource interface {
	GetPR(ctx context.Context, owner, repo string, number int) (*gogithub.PullRequest, *gogithub.Response, error)
	ListReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.ReviewThreadInfo, error)
	ListReviewComments(ctx context.Context, owner, repo string, number int) ([]github.ReviewCommentInfo, error)
}

// fromGitHub reads a PR and its PRism root threads live: the thread listing
// supplies node ids and resolution, the comment listing the markers, paths
// and the commit each root was posted against.
func fromGitHub(ctx context.Context, src githubSource, owner, repo string, number int, bots map[string]bool) (*pr, error) {
	info, _, err := src.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR: %w", err)
	}
	p := &pr{Owner: owner, Repo: repo, Number: number, Author: info.GetUser().GetLogin(), HeadSHA: info.GetHead().GetSHA(), State: stateOf(info.GetState(), info.GetMerged())}
	threads, err := src.ListReviewThreads(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	byRoot := map[int64]github.ReviewThreadInfo{}
	for _, t := range threads {
		byRoot[t.RootCommentID] = t
	}
	comments, err := src.ListReviewComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	if len(bots) == 0 {
		bots = map[string]bool{}
		for _, c := range comments {
			if _, ok := publisher.FindingIDFromBody(c.Body); ok && c.InReplyToID == 0 {
				bots[replaykit.BareLogin(c.Author)] = true
			}
		}
	}
	for _, c := range comments {
		if c.InReplyToID != 0 || !bots[replaykit.BareLogin(c.Author)] {
			continue
		}
		id, ok := publisher.FindingIDFromBody(c.Body)
		if !ok {
			continue
		}
		t, listed := byRoot[c.ID]
		p.Threads = append(p.Threads, thread{
			RootCommentID: c.ID, NodeID: t.NodeID, FindingID: id, Path: c.Path,
			PostedSHA: c.OriginalCommitID, PostedAt: c.CreatedAt,
			Resolved: t.Resolved, ResolvedKnown: listed, Outdated: t.Outdated,
		})
	}
	return p, nil
}

func parsePRRef(s string) (owner, repo string, number int, err error) {
	hash := strings.LastIndex(s, "#")
	slash := strings.Index(s, "/")
	if hash < 0 || slash < 0 || slash > hash {
		return "", "", 0, fmt.Errorf("--pr %q: want owner/repo#number", s)
	}
	if _, err := fmt.Sscanf(s[hash+1:], "%d", &number); err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("--pr %q: want owner/repo#number", s)
	}
	return s[:slash], s[slash+1 : hash], number, nil
}
