package poller

import (
	"context"
	"log"
	"os"
	"strings"

	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

// ReportHideWithheldEnv turns off leaving publisher-held findings out of the
// HTML report ("false", "0", "off", "no"); on by default.
const ReportHideWithheldEnv = "REPORT_HIDE_WITHHELD"

// ReportPublishDecisionsEnv re-renders the canonical report after publishing
// so it also leaves out what the round dropped (settled, deduped) and groups
// by the inline threads that exist ("true", "1", "on", "yes"); off by default.
const ReportPublishDecisionsEnv = "REPORT_PUBLISH_DECISIONS"

func reportPublishDecisions() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ReportPublishDecisionsEnv))) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

func reportHideWithheld() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ReportHideWithheldEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// reportPublishView works out what the publisher will make of the review so
// the report shows what GitHub shows: the findings it holds off the PR
// (verifier hold or drop, low value, gate BAD, hygiene and claim holds) and
// the merge confidence over the rest. Ledger dedupe and caps are not known
// before publishing and stay visible.
func (p *Poller) reportPublishView(result *service.ReviewResult) html.PublishView {
	if !reportHideWithheld() || result == nil {
		return html.PublishView{}
	}
	pol := p.publishPolicy()
	pl := payload.Build("", "", 0, "", result.Comments, result.Diff, result.FileContents)
	held, inline, titles := map[string]bool{}, map[string]bool{}, map[string]string{}
	var posted []payload.Finding
	for _, f := range pl.Findings {
		titles[f.ID] = publisher.TitleOf(f)
		if f.Active && publisher.HoldReason(f, pol) != "" {
			held[f.ID] = true
			continue
		}
		inline[f.ID] = publisher.Shown(f)
		posted = append(posted, f)
	}
	key := func(c types.LineComment) string { return payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody) }
	return html.PublishView{
		Withhold:   func(c types.LineComment) bool { return held[key(c)] },
		Inline:     func(c types.LineComment) bool { return inline[key(c)] },
		Title:      func(c types.LineComment) string { return titles[key(c)] },
		Confidence: publisher.Confidence(posted, result.Checks.ChecksViolated > 0),
	}
}

const (
	notPublishedNotAsked = "this run was not asked to post"
	notPublishedDraft    = "the pull request is a draft"
	notPublishedAuthor   = "PRism does not post on this author's pull requests"
)

// reportPublishViewFor is reportPublishView with what is already known about
// whether the run will post at all.
func (p *Poller) reportPublishViewFor(job ReviewJob, result *service.ReviewResult) html.PublishView {
	view := p.reportPublishView(result)
	if view.Withhold == nil {
		return view
	}
	switch {
	case job.SkipPublish:
		view.NotPublished = notPublishedNotAsked
	case job.PR.Draft && !p.publishesDraft(job):
		view.NotPublished = notPublishedDraft
	default:
		if gate, err := p.publishGate(); err != nil || !gate.Allowed(job.PR.Author) {
			view.NotPublished = notPublishedAuthor
		}
	}
	return view
}

// notPublishedReason words a publication outcome that posted nothing for
// the report, "" when the round posted.
func notPublishedReason(outcome string) string {
	switch {
	case outcome == "" || outcome == publicationPosted:
		return ""
	case outcome == publicationAlreadyPosted:
		return "an earlier run already posted a review of this commit"
	case outcome == publicationNotAllowed:
		return notPublishedAuthor
	case outcome == publicationMuted:
		return "the author muted PRism on this pull request"
	case outcome == publicationUnavailable:
		return "posting is not set up on this server"
	case strings.HasPrefix(outcome, publicationSkippedPrefix):
		return "the " + strings.TrimPrefix(outcome, publicationSkippedPrefix)
	}
	return "posting to GitHub failed"
}

// withPublishDecisions narrows a pre-publish view to what the round actually
// did: dropped findings join the withheld ones, inline grouping follows the
// threads that exist, and the score is the one the summary posted.
func withPublishDecisions(view html.PublishView, rep *publisher.Report) html.PublishView {
	if rep == nil || rep.Decisions == nil {
		return view
	}
	d := rep.Decisions
	key := func(c types.LineComment) string { return payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody) }
	before := view.Withhold
	view.Withhold = func(c types.LineComment) bool {
		return d.Dropped[key(c)] || before != nil && before(c)
	}
	view.Inline = func(c types.LineComment) bool { return d.Inline[key(c)] }
	if rep.Confidence > 0 {
		view.Confidence = rep.Confidence
	}
	return view
}

// refreshReportAfterPublish replaces the canonical report with one rendered
// from the round's decisions, or from why the round posted nothing; the
// immutable run artifact keeps the first render.
func (p *Poller) refreshReportAfterPublish(ctx context.Context, pr github.PullRequest, rr *ReviewResult, rep *publisher.Report, outcome string, sidecar []byte) {
	if !reportPublishDecisions() || rr == nil || rr.RenderAfterPublish == nil {
		return
	}
	notPublished := ""
	if rep == nil || rep.Decisions == nil {
		if notPublished = notPublishedReason(outcome); notPublished == "" {
			return
		}
	}
	body := rr.RenderAfterPublish(rep, notPublished)
	if body == nil {
		return
	}
	if _, err := p.saveCanonicalReviewAliasesWithRetry(ctx, pr.Owner, pr.Repo, pr.Number, pr.CommitSHA, body, sidecar); err != nil {
		log.Printf("[REVIEWER] WARN: %s/%s#%d: could not refresh the report after publishing: %v", pr.Owner, pr.Repo, pr.Number, err)
	}
}
