package poller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
)

// Author replies under PRism's inline comments are acknowledged per poll
// cycle. Mode off (default) does nothing, observe records them, react adds a
// 👍. The activation timestamp is written the first time the mode leaves off
// so enabling the feature never answers threads from before the switch.
const (
	settingPublishReplyMode      = "publish_reply_mode"
	settingPublishReplyEnabledAt = "publish_reply_enabled_at"
	replyFullScanEvery           = 10
)

func replyKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// replyTargetsToScan drops closed and draft PRs and, on incremental cycles,
// PRs whose GitHub updated_at has not moved since we last scanned them. Review
// comment replies bump updated_at, so this catches every reply within a cycle
// and the periodic full scan is the safety net. The candidate watermarks are
// returned rather than committed so a failed scan retries next cycle.
func replyTargetsToScan(targets []db.PublishedReplyTarget, lookup func(owner, repo string, number int) *db.PR, lastScanned map[string]time.Time, full bool) ([]db.PublishedReplyTarget, map[string]time.Time) {
	var out []db.PublishedReplyTarget
	marks := map[string]time.Time{}
	for _, t := range targets {
		pr := lookup(t.RepoOwner, t.RepoName, t.PRNumber)
		if pr == nil || !strings.EqualFold(pr.PRState, "open") || pr.Draft {
			continue
		}
		key := replyKey(t.RepoOwner, t.RepoName, t.PRNumber)
		if !full && pr.GitHubUpdatedAt != nil && !pr.GitHubUpdatedAt.After(lastScanned[key]) {
			continue
		}
		if pr.GitHubUpdatedAt != nil {
			marks[key] = *pr.GitHubUpdatedAt
		}
		out = append(out, t)
	}
	return out, marks
}

// commitReplyWatermarks advances the scan position for every target except
// those the reactor reported an error for (errors are prefixed "owner/repo#n:").
func commitReplyWatermarks(lastScanned, marks map[string]time.Time, errors []string) {
	for key, ts := range marks {
		failed := false
		for _, e := range errors {
			if strings.HasPrefix(e, key+":") {
				failed = true
				break
			}
		}
		if !failed {
			lastScanned[key] = ts
		}
	}
}

func (p *Poller) scanAuthorReplies(ctx context.Context) {
	if !p.replyScanRunning.CompareAndSwap(false, true) {
		return
	}
	defer p.replyScanRunning.Store(false)

	mode, _ := p.db.GetSetting(settingPublishReplyMode)
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" || mode == publisher.ReplyModeOff {
		return
	}
	ledger, ok := p.db.(publisher.ReplyLedger)
	if !ok || p.ghClientConcrete == nil {
		log.Printf("[REPLIES] mode %s but no ledger or GitHub client available", mode)
		return
	}
	since, err := p.replyActivation()
	if err != nil {
		log.Printf("[REPLIES] activation timestamp: %v", err)
		return
	}
	targets, err := ledger.ListPublishedReplyTargets()
	if err != nil {
		log.Printf("[REPLIES] list targets: %v", err)
		return
	}
	if p.replyLastScanned == nil {
		p.replyLastScanned = map[string]time.Time{}
	}
	cycle := p.replyScanCycle.Add(1)
	lookup := func(owner, repo string, number int) *db.PR {
		pr, err := p.db.GetPR(owner, repo, number)
		if err != nil {
			return nil
		}
		return pr
	}
	subset, marks := replyTargetsToScan(targets, lookup, p.replyLastScanned, cycle%replyFullScanEvery == 1)
	if len(subset) == 0 {
		return
	}
	enabled, _ := p.db.GetSetting(settingPublishEnabledAuthors)
	reactor := publisher.ReplyReactor{
		GH:      ghReplyAdapter{p.ghClientConcrete},
		Ledger:  ledger,
		Mode:    mode,
		Since:   since,
		Targets: subset,
		Allowed: func(login string) bool { return publishEnabledFor(login, enabled) },
		PR: func(ctx context.Context, owner, repo string, number int) (publisher.PRState, error) {
			ghPR, _, err := p.ghClientConcrete.GetPR(ctx, owner, repo, number)
			if err != nil {
				return publisher.PRState{}, err
			}
			return publisher.PRState{
				Open:        strings.EqualFold(ghPR.GetState(), "open"),
				Draft:       ghPR.GetDraft(),
				AuthorID:    ghPR.GetUser().GetID(),
				AuthorLogin: ghPR.GetUser().GetLogin(),
			}, nil
		},
	}
	rep, err := reactor.Run(ctx)
	if err != nil {
		log.Printf("[REPLIES] scan failed: %v", err)
		return
	}
	commitReplyWatermarks(p.replyLastScanned, marks, rep.Errors)
	for _, e := range rep.Errors {
		log.Printf("[REPLIES] %s", e)
	}
	if rep.Recorded > 0 || rep.Reacted > 0 {
		log.Printf("[REPLIES] mode=%s scanned=%d recorded=%d reacted=%d", mode, rep.PRsScanned, rep.Recorded, rep.Reacted)
	}
}

// replyActivation reads the stamp the settings API wrote when the mode left
// off. A missing stamp (mode set outside the API) is stamped now, once; a
// read error is an error, never a reason to overwrite it.
func (p *Poller) replyActivation() (time.Time, error) {
	raw, err := p.db.GetSetting(settingPublishReplyEnabledAt)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(raw) != "" {
		return time.Parse(time.RFC3339, strings.TrimSpace(raw))
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := p.db.SetSetting(settingPublishReplyEnabledAt, now.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	log.Printf("[REPLIES] activated at %s; earlier replies are ignored", now.Format(time.RFC3339))
	return now, nil
}

type ghReplyAdapter struct{ c *github.Client }

func (a ghReplyAdapter) ListThread(ctx context.Context, owner, repo string, number int) ([]publisher.ThreadComment, error) {
	comments, err := a.c.ListReviewComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]publisher.ThreadComment, 0, len(comments))
	for _, c := range comments {
		out = append(out, publisher.ThreadComment{
			ID: c.ID, InReplyToID: c.InReplyToID, AuthorID: c.AuthorID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

func (a ghReplyAdapter) React(ctx context.Context, owner, repo string, commentID int64) error {
	return a.c.CreateCommentReaction(ctx, owner, repo, commentID, "+1")
}
