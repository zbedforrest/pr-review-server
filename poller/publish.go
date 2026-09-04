package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// GitHub publication is gated per PR author so the pilot can widen team by
// team from the settings API without a deploy. "*" enables everyone.
const (
	settingPublishEnabledAuthors    = "publish_enabled_authors"
	settingPublishInlineCap         = "publish_inline_cap"
	settingPublishInlineMinSeverity = "publish_inline_min_severity"
)

func publishEnabledFor(author, enabledCSV string) bool {
	author = strings.TrimSpace(author)
	if author == "" {
		return false
	}
	for _, entry := range strings.Split(enabledCSV, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "*" || strings.EqualFold(entry, author) {
			return true
		}
	}
	return false
}

// buildPublishRound assembles everything the publisher needs from the review
// sidecar plus what is already on the PR: Greptile's inline comments are
// reconciled against PRism's findings so nothing is posted twice, and the
// file patches bound which lines may take an inline comment.
func buildPublishRound(pr github.PullRequest, pl payload.Payload, comments []github.ReviewCommentInfo, patches map[string]string, previous []db.PublishedFinding, baseURL string) publisher.Round {
	external := make([]reconcile.ExternalComment, 0, len(comments))
	for _, c := range comments {
		external = append(external, reconcile.ExternalComment{
			ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path,
			Line: c.Line, StartLine: c.StartLine, InReplyToID: c.InReplyToID,
		})
	}
	res := reconcile.Reconcile(pl.Findings, reconcile.ParseGreptileComments(external))

	tags := make(map[string]string, len(res.Findings))
	for _, t := range res.Findings {
		if t.SourceTag != reconcile.SourceTagPrismOnly {
			tags[t.Finding.ID] = t.SourceTag
		}
	}
	greptileOnly := make([]publisher.GreptileOnlyRef, 0, len(res.GreptileOnly))
	for _, g := range res.GreptileOnly {
		greptileOnly = append(greptileOnly, publisher.GreptileOnlyRef{
			Title: g.Title, File: g.File, Line: g.StartLine, Severity: g.Severity, CommentID: g.CommentID,
		})
	}
	commentable := make(map[string]map[int]bool, len(patches))
	for file, patch := range patches {
		commentable[file] = publisher.CommentableLines(patch)
	}

	r := publisher.Round{
		Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.CommitSHA,
		Findings:     pl.Findings,
		SourceTags:   tags,
		GreptileOnly: greptileOnly,
		Previous:     previous,
		Commentable:  commentable,
	}
	if base := strings.TrimRight(baseURL, "/"); base != "" {
		r.AgentLinkBase = fmt.Sprintf("%s/go/agent?o=%s&r=%s&n=%d", base, pr.Owner, pr.Repo, pr.Number)
		r.DashboardURL = fmt.Sprintf("%s/api/review/%s/%s/%d?format=html", base, pr.Owner, pr.Repo, pr.Number)
	}
	return r
}

// ghPublishAdapter bridges the concrete GitHub client to the publisher's
// interface; the two packages keep separate input structs to avoid a cycle.
type ghPublishAdapter struct{ c *github.Client }

func (a ghPublishAdapter) CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []publisher.ReviewCommentInput) (int64, []int64, error) {
	inputs := make([]github.ReviewCommentInput, len(comments))
	for i, c := range comments {
		inputs[i] = github.ReviewCommentInput{Path: c.Path, Line: c.Line, StartLine: c.StartLine, Body: c.Body}
	}
	return a.c.CreateReview(ctx, owner, repo, number, commitSHA, body, inputs)
}

func (a ghPublishAdapter) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	return a.c.CreateIssueComment(ctx, owner, repo, number, body)
}

func (a ghPublishAdapter) EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	return a.c.EditIssueComment(ctx, owner, repo, commentID, body)
}

func (p *Poller) publishPolicy() publisher.Policy {
	pol := publisher.Policy{InlineCap: 5, InlineMinSeverity: "medium"}
	if v, err := p.db.GetSetting(settingPublishInlineCap); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil && n >= 0 {
			pol.InlineCap = n
		}
	}
	if v, err := p.db.GetSetting(settingPublishInlineMinSeverity); err == nil && strings.TrimSpace(v) != "" {
		pol.InlineMinSeverity = strings.ToLower(strings.TrimSpace(v))
	}
	return pol
}

// publishGitHubReview posts a completed review to the PR. Best-effort by
// design: the review is already saved and visible on the dashboard, so any
// failure here is logged and never fails the run.
func (p *Poller) publishGitHubReview(ctx context.Context, pr github.PullRequest, sidecar []byte) {
	enabled, err := p.db.GetSetting(settingPublishEnabledAuthors)
	if err != nil || !publishEnabledFor(pr.Author, enabled) {
		return
	}
	ledger, ok := p.db.(publisher.Ledger)
	if !ok || p.ghClientConcrete == nil {
		log.Printf("[PUBLISH] %s/%s#%d: publication enabled but no ledger or GitHub client available", pr.Owner, pr.Repo, pr.Number)
		return
	}
	var pl payload.Payload
	if err := json.Unmarshal(sidecar, &pl); err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: sidecar unreadable: %v", pr.Owner, pr.Repo, pr.Number, err)
		return
	}
	comments, err := p.ghClientConcrete.ListReviewComments(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: list review comments: %v", pr.Owner, pr.Repo, pr.Number, err)
		return
	}
	patches, err := p.ghClientConcrete.GetPRFilePatches(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: list file patches: %v", pr.Owner, pr.Repo, pr.Number, err)
		return
	}
	previous, err := ledger.GetPublishedFindingsForPR(pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: load ledger: %v", pr.Owner, pr.Repo, pr.Number, err)
		return
	}

	round := buildPublishRound(pr, pl, comments, patches, previous, p.cfg.BaseURL)
	pub := &publisher.Publisher{GH: ghPublishAdapter{p.ghClientConcrete}, Ledger: ledger, Policy: p.publishPolicy()}
	report, err := pub.Publish(ctx, round)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
		return
	}
	log.Printf("[PUBLISH] %s/%s#%d: summary=%d review=%d inline=%d annotations=%d still_open=%d fixed=%d",
		pr.Owner, pr.Repo, pr.Number, report.SummaryCommentID, report.ReviewID, report.InlinePosted, report.Annotations, report.StillOpen, report.Fixed)
}
