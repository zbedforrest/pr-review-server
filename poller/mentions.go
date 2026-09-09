package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/runconfig"
)

// Authors ask for a review by mentioning the App in a PR comment, the way
// they would with any review bot: "@<handle> review". The poller already
// tracks every open PR's updated_at, and a new comment bumps it, so each cycle
// lists issue comments only on PRs that moved and looks for the command.
// Handled comment ids are recorded so a restart or a re-scan never triggers
// twice.
const mentionFullScanEvery = 10

func mentionKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

var mentionQuoteRe = regexp.MustCompile(`(?m)^[ \t]*>.*$`)

// mentionCommand reports whether a comment addresses the handle and asks it
// for a review: the command word must follow the mention, so talking about a
// review ("why did the review flag X?") or forbidding one ("don't review
// this yet") does not trigger. Quoted lines are ignored so quoting someone's
// request is not one either.
func mentionCommand(body, handle string) bool {
	text := strings.ToLower(mentionQuoteRe.ReplaceAllString(body, ""))
	command := regexp.MustCompile(`(^|[^\w@/.-])@` + regexp.QuoteMeta(strings.ToLower(handle)) + `[\s,:]+(please\s+|can you\s+|could you\s+)?(re-?review|review)\b`)
	return command.MatchString(text)
}

// mentionCandidates picks the open PRs whose cached updated_at moved since the
// last scan (every open PR on a full scan). An empty cached state predates
// the column and is treated as open, as the rest of the poller does.
func mentionCandidates(prs []*db.PR, lastScanned map[string]time.Time, full bool) []*db.PR {
	var out []*db.PR
	for _, pr := range prs {
		if pr.PRState != "" && !strings.EqualFold(pr.PRState, "open") {
			continue
		}
		if full || pr.GitHubUpdatedAt == nil || pr.GitHubUpdatedAt.After(lastScanned[mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)]) {
			out = append(out, pr)
		}
	}
	return out
}

// mentionAllowed decides who may ask for a review: the PR's author, or someone
// GitHub reports as an owner, member or collaborator of the repository. On a
// public repository anyone else can comment, and a review costs money.
func mentionAllowed(c github.IssueCommentInfo, prAuthor string) bool {
	if strings.EqualFold(c.Author, prAuthor) {
		return true
	}
	switch strings.ToUpper(c.Association) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// mentionPublishNote is the short reply left when a requested review cannot
// be posted to the PR, so the requester knows where to look and why.
func mentionPublishNote(publishable, draft bool, headSHA string) string {
	if publishable {
		return ""
	}
	if len(headSHA) > 7 {
		headSHA = headSHA[:7]
	}
	reason := "the author is not in the comment pilot"
	if draft {
		reason = "the pull request is a draft"
	}
	return fmt.Sprintf("Reviewing %s. The result will be on the PRism dashboard only, because %s.", headSHA, reason)
}

func mentionTelemetryEvent(pr github.PullRequest, by string, publish bool, userID int) db.TelemetryEvent {
	return db.TelemetryEvent{UserID: userID, Action: "mention_trigger", Label: fmt.Sprintf("by=%s publish=%t", by, publish),
		PROwner: pr.Owner, PRRepo: pr.Repo, PRNumber: pr.Number}
}

// mentionGitHub is the slice of GitHub the mention scan needs.
type mentionGitHub interface {
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]github.IssueCommentInfo, error)
	LivePR(ctx context.Context, owner, repo string, number int) (mentionPR, error)
	ReactToIssueComment(ctx context.Context, owner, repo string, commentID int64) error
	CommentOnPR(ctx context.Context, owner, repo string, number int, body string) error
}

// mentionPR is the live state a request is admitted against.
type mentionPR struct {
	Open    bool
	Draft   bool
	HeadSHA string
	Title   string
	Author  string
}

type mentionLedger interface {
	MentionHandled(commentID int64, now time.Time) (bool, error)
	ReserveMention(*db.MentionTrigger) (bool, error)
	FinalizeMention(commentID int64) error
	ReleaseMention(commentID int64) error
}

// mentionAdmit queues a review; it returns ErrReviewAlreadyTracked when one is
// already running for the PR and a *runconfig.ValidationError for a request
// that can never be admitted.
type mentionAdmit func(ctx context.Context, pr github.PullRequest, publish bool) error

// mentionScanner is the testable core of the mention trigger.
type mentionScanner struct {
	gh      mentionGitHub
	ledger  mentionLedger
	admit   mentionAdmit
	handle  string
	since   time.Time
	allowed func(author string) bool // publish allowlist
	now     func() time.Time
	log     func(format string, args ...any)
	onEvent func(pr github.PullRequest, by string, publish bool)
}

type mentionResult struct {
	triggered int
	deferred  int // requests waiting for an active review to finish, or a retry
}

// handlePR acts on new review requests in one PR's conversation. The ledger
// row is reserved before admission and finalised after, so a crash in between
// can never admit the same request twice; a request that cannot be admitted
// right now (a review is already running, a transient error) releases its
// reservation and is retried next cycle.
func (m mentionScanner) handlePR(ctx context.Context, pr *db.PR) (mentionResult, error) {
	var res mentionResult
	comments, err := m.gh.ListIssueComments(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
	if err != nil {
		return res, err
	}
	key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
	for _, c := range comments {
		if c.IsBot || c.CreatedAt.Before(m.since) || !mentionCommand(c.Body, m.handle) {
			continue
		}
		handled, err := m.ledger.MentionHandled(c.ID, m.now())
		if err != nil {
			return res, err
		}
		if handled {
			continue
		}
		// The cached row may trail a push or a close by a poll cycle; admit
		// against what the requester is looking at.
		live, err := m.gh.LivePR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if err != nil {
			return res, err
		}
		if !mentionAllowed(c, live.Author) {
			m.log("[MENTIONS] %s: ignoring request in comment %d from %s (%s)", key, c.ID, c.Author, c.Association)
			continue
		}
		if !live.Open {
			m.log("[MENTIONS] %s: ignoring request in comment %d, the PR is no longer open", key, c.ID)
			continue
		}
		ghPR := github.PullRequest{Owner: pr.RepoOwner, Repo: pr.RepoName, Number: pr.PRNumber, CommitSHA: live.HeadSHA,
			Title: live.Title, Author: live.Author, CreatedAt: pr.CreatedAt, Draft: live.Draft}
		publish := m.allowed(ghPR.Author) && !ghPR.Draft
		reserved, err := m.ledger.ReserveMention(&db.MentionTrigger{
			CommentID: c.ID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber,
			Author: c.Author, CommitSHA: ghPR.CommitSHA, Publish: publish, CreatedAt: c.CreatedAt, TriggeredAt: m.now(),
		})
		if err != nil {
			return res, err
		}
		if !reserved {
			continue
		}
		err = m.admit(ctx, ghPR, publish)
		var invalid *runconfig.ValidationError
		switch {
		case err == nil:
		case errors.As(err, &invalid):
			// The request itself is bad and a retry cannot fix it; keep the
			// row so the same comment is not retried every cycle.
			m.log("[MENTIONS] %s: comment %d asked for an invalid review: %v", key, c.ID, err)
			if err := m.ledger.FinalizeMention(c.ID); err != nil {
				return res, err
			}
			continue
		default:
			// An active review, a database blip, a cancelled context: try
			// again next cycle.
			res.deferred++
			if rerr := m.ledger.ReleaseMention(c.ID); rerr != nil {
				return res, rerr
			}
			if !errors.Is(err, ErrReviewAlreadyTracked) {
				return res, err
			}
			continue
		}
		if err := m.ledger.FinalizeMention(c.ID); err != nil {
			return res, err
		}
		res.triggered++
		if err := m.gh.ReactToIssueComment(ctx, pr.RepoOwner, pr.RepoName, c.ID); err != nil {
			m.log("[MENTIONS] %s: could not acknowledge comment %d: %v", key, c.ID, err)
		}
		if note := mentionPublishNote(publish, ghPR.Draft, ghPR.CommitSHA); note != "" {
			if err := m.gh.CommentOnPR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber, note); err != nil {
				m.log("[MENTIONS] %s: could not leave the dashboard-only note: %v", key, err)
			}
		}
		m.log("[MENTIONS] %s: review of %s requested by %s in comment %d (publish=%t)", key, ghPR.CommitSHA[:min(7, len(ghPR.CommitSHA))], c.Author, c.ID, publish)
		if m.onEvent != nil {
			m.onEvent(ghPR, c.Author, publish)
		}
	}
	return res, nil
}

const settingMentionEnabledAt = "mention_enabled_at"

// mentionActivation is the durable cutoff: commands older than it are never
// acted on, so enabling the feature does not answer every old mention, and a
// restart does not lose a command posted while the service was down. It is
// stamped at startup so nothing posted before the first tick is missed.
func (p *Poller) mentionActivation() (time.Time, error) {
	raw, err := p.db.GetSetting(settingMentionEnabledAt)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) != "" {
		return time.Parse(time.RFC3339, strings.TrimSpace(raw))
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := p.db.SetSetting(settingMentionEnabledAt, now.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	log.Printf("[MENTIONS] activated at %s; earlier mentions are ignored", now.Format(time.RFC3339))
	return now, nil
}

// scanMentions runs once per poll cycle on the leader.
func (p *Poller) scanMentions(ctx context.Context) {
	if p.cfg.MentionHandle == "" || !p.mentionScanRunning.CompareAndSwap(false, true) {
		return
	}
	defer p.mentionScanRunning.Store(false)
	ledger, ok := p.db.(mentionLedger)
	if !ok || p.ghClientConcrete == nil {
		return
	}
	if p.mentionLastScanned == nil {
		p.mentionLastScanned = map[string]time.Time{}
	}
	since, err := p.mentionActivation()
	if err != nil {
		log.Printf("[MENTIONS] activation timestamp: %v", err)
		return
	}
	cycle := p.mentionScanCycle.Add(1)
	full := cycle%mentionFullScanEvery == 1
	all, err := p.db.GetAllPRs()
	if err != nil {
		log.Printf("[MENTIONS] list PRs: %v", err)
		return
	}
	prs := make([]*db.PR, 0, len(all))
	for i := range all {
		prs = append(prs, &all[i])
	}
	candidates := mentionCandidates(prs, p.mentionLastScanned, full)
	enabled, _ := p.db.GetSetting(settingPublishEnabledAuthors)
	scanner := mentionScanner{
		gh: mentionGitHubAdapter{p.ghClientConcrete}, ledger: ledger, handle: p.cfg.MentionHandle, since: since,
		allowed: func(author string) bool { return publishEnabledFor(author, enabled) },
		now:     func() time.Time { return time.Now().UTC() },
		log:     log.Printf,
		admit: func(ctx context.Context, pr github.PullRequest, publish bool) error {
			job, err := p.defaultReviewJob(pr, true, "mention")
			if err != nil {
				return err
			}
			job.SkipPublish = !publish
			return p.ProcessReviewJob(ctx, job)
		},
		onEvent: func(pr github.PullRequest, by string, publish bool) {
			if userID := p.systemTelemetryUserID(); userID != 0 {
				if terr := p.db.CreateTelemetryEvents([]db.TelemetryEvent{mentionTelemetryEvent(pr, by, publish, userID)}); terr != nil {
					log.Printf("[MENTIONS] WARN: could not record telemetry: %v", terr)
				}
			}
		},
	}
	triggered, deferred, errors := 0, 0, 0
	for _, pr := range candidates {
		key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		res, err := scanner.handlePR(ctx, pr)
		if err != nil {
			errors++
			log.Printf("[MENTIONS] %s: %v", key, err)
			continue
		}
		triggered += res.triggered
		deferred += res.deferred
		// A deferred request is retried next cycle, so the PR stays a candidate.
		if res.deferred == 0 && pr.GitHubUpdatedAt != nil {
			p.mentionLastScanned[key] = *pr.GitHubUpdatedAt
		}
	}
	if len(candidates) > 0 {
		log.Printf("[MENTIONS] cycle=%d full=%t handle=%s checked=%d triggered=%d deferred=%d errors=%d", cycle, full, p.cfg.MentionHandle, len(candidates), triggered, deferred, errors)
	}
}

type mentionGitHubAdapter struct{ c *github.Client }

func (a mentionGitHubAdapter) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]github.IssueCommentInfo, error) {
	return a.c.ListIssueComments(ctx, owner, repo, number)
}

func (a mentionGitHubAdapter) LivePR(ctx context.Context, owner, repo string, number int) (mentionPR, error) {
	pr, _, err := a.c.GetPR(ctx, owner, repo, number)
	if err != nil {
		return mentionPR{}, err
	}
	return mentionPR{Open: strings.EqualFold(pr.GetState(), "open"), Draft: pr.GetDraft(), HeadSHA: pr.GetHead().GetSHA(),
		Title: pr.GetTitle(), Author: pr.GetUser().GetLogin()}, nil
}

func (a mentionGitHubAdapter) ReactToIssueComment(ctx context.Context, owner, repo string, commentID int64) error {
	return a.c.CreateIssueCommentReaction(ctx, owner, repo, commentID, "eyes")
}

func (a mentionGitHubAdapter) CommentOnPR(ctx context.Context, owner, repo string, number int, body string) error {
	_, err := a.c.CreateIssueComment(ctx, owner, repo, number, body)
	return err
}
