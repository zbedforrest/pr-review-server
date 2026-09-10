package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
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

// mentionCommandRe builds the matcher for one handle: the command word must
// follow the mention, so talking about a review ("why did the review flag
// X?") or forbidding one ("don't review this yet") does not trigger.
func mentionCommandRe(handle string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w@/.-])@` + regexp.QuoteMeta(strings.ToLower(handle)) + `[\s,:]+(please\s+|can you\s+|could you\s+)?(re-?review|review)\b`)
}

// mentionCommand reports whether a comment addresses the handle and asks it
// for a review. Quoted lines are ignored so quoting someone's request is not
// one.
func mentionCommand(body, handle string) bool {
	return mentionCommandRe(handle).MatchString(strings.ToLower(mentionQuoteRe.ReplaceAllString(body, "")))
}

func mentionCommandWith(re *regexp.Regexp, body string) bool {
	return re.MatchString(strings.ToLower(mentionQuoteRe.ReplaceAllString(body, "")))
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
		key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		if full || (pr.GitHubUpdatedAt == nil && lastScanned[key].IsZero()) || (pr.GitHubUpdatedAt != nil && pr.GitHubUpdatedAt.After(lastScanned[key])) {
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
	FinalizeMention(commentID int64, holder string) (bool, error)
	ReleaseMention(commentID int64, holder string) error
}

// mentionAdmit queues a review for one request comment. It returns
// ErrReviewAlreadyTracked when a review is already running for the PR,
// db.ErrReviewRunConflict when this very request was admitted before (the
// run carries the comment id as its idempotency key), and a
// *runconfig.ValidationError for a request that can never be admitted.
type mentionAdmit func(ctx context.Context, pr github.PullRequest, commentID int64, publish bool) error

// mentionRecentlyReviewed reports whether a review of this head that
// satisfies the request finished within the coalescing window: any completed
// run when the result is dashboard-only, a run whose findings were posted to
// the PR when publication is wanted. Repeated requests on an unchanged PR then
// do not queue repeated reviews.
type mentionRecentlyReviewed func(owner, repo string, number int, headSHA string, publish bool, since time.Time) (bool, error)

// mentionCoalesceWindow is how long a completed review of a head answers
// further requests for the same head.
const mentionCoalesceWindow = 30 * time.Minute

// mentionScanner is the testable core of the mention trigger.
type mentionScanner struct {
	gh       mentionGitHub
	ledger   mentionLedger
	admit    mentionAdmit
	reviewed mentionRecentlyReviewed
	handle   string
	holder   string
	since    time.Time
	allowed  func(author string) bool // publish allowlist
	now      func() time.Time
	log      func(format string, args ...any)
	onEvent  func(pr github.PullRequest, by string, publish bool)
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
	command := mentionCommandRe(m.handle)
	var live *mentionPR
	for _, c := range comments {
		if c.IsBot || c.CreatedAt.Before(m.since) || !mentionCommandWith(command, c.Body) {
			continue
		}
		// Authorisation against the cached author first: an unauthorised
		// request is never recorded, so it must not cost a GitHub read per cycle.
		if !mentionAllowed(c, pr.Author) {
			m.log("[MENTIONS] %s: ignoring request in comment %d from %s (%s)", key, c.ID, c.Author, c.Association)
			continue
		}
		handled, err := m.ledger.MentionHandled(c.ID, m.now())
		if err != nil {
			return res, err
		}
		if handled {
			continue
		}
		if live == nil {
			// The cached row may trail a push or a close by a poll cycle; admit
			// against what the requester is looking at.
			fresh, err := m.gh.LivePR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber)
			if err != nil {
				return res, err
			}
			live = &fresh
		}
		if !live.Open {
			m.log("[MENTIONS] %s: ignoring request in comment %d, the PR is no longer open", key, c.ID)
			continue
		}
		ghPR := github.PullRequest{Owner: pr.RepoOwner, Repo: pr.RepoName, Number: pr.PRNumber, CommitSHA: live.HeadSHA,
			Title: live.Title, Author: live.Author, CreatedAt: pr.CreatedAt, Draft: live.Draft}
		publish := m.allowed(ghPR.Author) && !ghPR.Draft
		now := m.now()
		reserved, err := m.ledger.ReserveMention(&db.MentionTrigger{
			CommentID: c.ID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName, PRNumber: pr.PRNumber, Holder: m.holder,
			Author: c.Author, CommitSHA: ghPR.CommitSHA, Publish: publish, CreatedAt: c.CreatedAt, TriggeredAt: now,
		})
		if err != nil {
			return res, err
		}
		if !reserved {
			continue
		}
		short := ghPR.CommitSHA[:min(7, len(ghPR.CommitSHA))]
		if m.reviewed != nil {
			done, err := m.reviewed(pr.RepoOwner, pr.RepoName, pr.PRNumber, ghPR.CommitSHA, publish, now.Add(-mentionCoalesceWindow))
			if err != nil {
				_ = m.ledger.ReleaseMention(c.ID, m.holder)
				return res, err
			}
			if done {
				owned, err := m.ledger.FinalizeMention(c.ID, m.holder)
				if err != nil || !owned {
					continue
				}
				m.acknowledge(ctx, pr, c.ID, fmt.Sprintf("A review of %s finished in the last %d minutes; its result is current. Push a new commit for another.", short, int(mentionCoalesceWindow.Minutes())))
				m.log("[MENTIONS] %s: comment %d asked for a review of %s, which completed recently", key, c.ID, short)
				continue
			}
		}
		err = m.admit(ctx, ghPR, c.ID, publish)
		var invalid *runconfig.ValidationError
		switch {
		case err == nil:
		case errors.Is(err, db.ErrReviewRunConflict):
			// This request already produced a run (a crash after admission);
			// nothing more to queue.
			m.log("[MENTIONS] %s: comment %d was already admitted", key, c.ID)
		case errors.As(err, &invalid):
			// A mention carries no overrides, so this is the deployment's own
			// defaults failing validation: an operator problem, fixed by
			// configuration, after which the request should still be served.
			m.log("[MENTIONS] %s: comment %d cannot be admitted until the review configuration is fixed: %v", key, c.ID, err)
			res.deferred++
			if rerr := m.ledger.ReleaseMention(c.ID, m.holder); rerr != nil {
				return res, rerr
			}
			continue
		default:
			// An active review, a database blip, a cancelled context: try
			// again next cycle.
			res.deferred++
			if rerr := m.ledger.ReleaseMention(c.ID, m.holder); rerr != nil {
				return res, rerr
			}
			if !errors.Is(err, ErrReviewAlreadyTracked) {
				return res, err
			}
			continue
		}
		owned, err := m.ledger.FinalizeMention(c.ID, m.holder)
		if err != nil {
			return res, err
		}
		if !owned {
			// The reservation expired and another holder took it; the run is
			// idempotent on the comment id, so they will acknowledge it.
			continue
		}
		res.triggered++
		m.acknowledge(ctx, pr, c.ID, mentionPublishNote(publish, ghPR.Draft, ghPR.CommitSHA))
		m.log("[MENTIONS] %s: review of %s requested by %s in comment %d (publish=%t)", key, short, c.Author, c.ID, publish)
		if m.onEvent != nil {
			m.onEvent(ghPR, c.Author, publish)
		}
	}
	return res, nil
}

// acknowledge reacts to the request and, when there is something the
// requester should know, says it in one line.
func (m mentionScanner) acknowledge(ctx context.Context, pr *db.PR, commentID int64, note string) {
	key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
	if err := m.gh.ReactToIssueComment(ctx, pr.RepoOwner, pr.RepoName, commentID); err != nil {
		m.log("[MENTIONS] %s: could not acknowledge comment %d: %v", key, commentID, err)
	}
	if note != "" {
		if err := m.gh.CommentOnPR(ctx, pr.RepoOwner, pr.RepoName, pr.PRNumber, note); err != nil {
			m.log("[MENTIONS] %s: could not leave a note: %v", key, err)
		}
	}
}

const (
	settingMentionEnabledAt = "mention_enabled_at"
	settingMentionHandle    = "mention_handle"
)

// mentionActivation is the durable cutoff: commands older than it are never
// acted on, so enabling the feature does not answer every old mention, and a
// restart does not lose a command posted while the service was down. The
// cutoff is re-stamped when the handle changes, so mentions of a previous
// handle or of a period the feature was off are not replayed.
func (p *Poller) mentionActivation() (time.Time, error) {
	raw, err := p.db.GetSetting(settingMentionEnabledAt)
	if err != nil {
		return time.Time{}, err
	}
	seen, err := p.db.GetSetting(settingMentionHandle)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) != "" && strings.TrimSpace(seen) == p.cfg.MentionHandle {
		return time.Parse(time.RFC3339, strings.TrimSpace(raw))
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := p.db.SetSetting(settingMentionEnabledAt, now.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	if err := p.db.SetSetting(settingMentionHandle, p.cfg.MentionHandle); err != nil {
		return time.Time{}, err
	}
	log.Printf("[MENTIONS] activated for @%s at %s; earlier mentions are ignored", p.cfg.MentionHandle, now.Format(time.RFC3339))
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
		gh: mentionGitHubAdapter{p.ghClientConcrete}, ledger: ledger, handle: p.cfg.MentionHandle, holder: p.holderID, since: since,
		allowed: func(author string) bool { return publishEnabledFor(author, enabled) },
		now:     func() time.Time { return time.Now().UTC() },
		log:     log.Printf,
		reviewed: func(owner, repo string, number int, headSHA string, publish bool, since time.Time) (bool, error) {
			runs, err := p.db.ListReviewRuns(db.ReviewRunFilter{RepoOwner: owner, RepoName: repo, PRNumber: number, CommitSHA: headSHA, Status: db.ReviewRunStatusCompleted, Limit: 1})
			if err != nil {
				return false, err
			}
			if len(runs) == 0 || runs[0].CompletedAt == nil || !runs[0].CompletedAt.After(since) {
				return false, nil
			}
			if !publish {
				return true, nil
			}
			// A dashboard-only run does not satisfy a request whose result
			// should be on the PR; the summary ledger row records the head
			// that was actually posted.
			ledger, ok := p.db.(publisher.Ledger)
			if !ok {
				return false, nil
			}
			rows, err := ledger.GetPublishedFindingsForPR(owner, repo, number)
			if err != nil {
				return false, err
			}
			for _, row := range rows {
				if row.Kind == db.PublishedKindSummary && strings.EqualFold(row.LastSeenSHA, headSHA) {
					return true, nil
				}
			}
			return false, nil
		},
		admit: func(ctx context.Context, pr github.PullRequest, commentID int64, publish bool) error {
			job, err := p.defaultReviewJob(pr, true, "mention")
			if err != nil {
				return err
			}
			job.SkipPublish = !publish
			// The comment id is the idempotency key: a crash after admission
			// cannot produce a second run for the same request.
			job.IdempotencyScope = "mention"
			job.IdempotencyKeyHash = sha256Hex(fmt.Sprintf("comment:%d", commentID))
			job.RequestHash = sha256Hex(fmt.Sprintf("comment:%d:%s", commentID, pr.CommitSHA))
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
	triggered, deferred, failed := 0, 0, 0
	for _, pr := range candidates {
		key := mentionKey(pr.RepoOwner, pr.RepoName, pr.PRNumber)
		res, err := scanner.handlePR(ctx, pr)
		if err != nil {
			failed++
			log.Printf("[MENTIONS] %s: %v", key, err)
			continue
		}
		triggered += res.triggered
		deferred += res.deferred
		// A deferred request is retried next cycle, so the PR stays a candidate.
		// A row without a cached time is stamped with now so it is not listed
		// every cycle until the poll backfills the column.
		if res.deferred == 0 {
			if pr.GitHubUpdatedAt != nil {
				p.mentionLastScanned[key] = *pr.GitHubUpdatedAt
			} else {
				p.mentionLastScanned[key] = time.Now().UTC()
			}
		}
	}
	if len(candidates) > 0 {
		log.Printf("[MENTIONS] cycle=%d full=%t handle=%s checked=%d triggered=%d deferred=%d errors=%d", cycle, full, p.cfg.MentionHandle, len(candidates), triggered, deferred, failed)
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

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
