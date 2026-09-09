package poller

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/service"
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

// replyLinkDue picks the unlinked ledger rows to try matching this cycle: every
// PR not yet attempted, or all of them on a full scan. Attempts are stamped so
// a comment that was deleted on GitHub does not cost a thread listing per
// cycle; unstampFailedLinks clears the stamp for PRs whose attempt errored.
func replyLinkDue(rows []db.UnlinkedPublishedFinding, tried map[string]time.Time, full bool, now time.Time) []db.UnlinkedPublishedFinding {
	var due []db.UnlinkedPublishedFinding
	stamp := map[string]bool{}
	for _, row := range rows {
		key := replyKey(row.RepoOwner, row.RepoName, row.PRNumber)
		if _, done := tried[key]; done && !full && !stamp[key] {
			continue
		}
		stamp[key] = true
		due = append(due, row)
	}
	for key := range stamp {
		tried[key] = now
	}
	return due
}

// unstampFailedLinks lets PRs whose link attempt hit a GitHub or ledger error
// retry next cycle instead of waiting for a full scan. Errors are prefixed
// "owner/repo#n:", the replyKey format.
func unstampFailedLinks(tried map[string]time.Time, errors []string) {
	for _, e := range errors {
		if key, _, ok := strings.Cut(e, ":"); ok {
			delete(tried, key)
		}
	}
}

// replyTelemetryEvents turns one scan into telemetry rows: one per reply
// handled (reply_reacted / reply_observed), one per scan error, one per link
// pass that changed anything, one per link error. PR coordinates are parsed
// from the "owner/repo#n:" prefix the reactor puts on every error.
func replyTelemetryEvents(rep publisher.ReplyReport, link publisher.LinkReport, userID int) []db.TelemetryEvent {
	var events []db.TelemetryEvent
	for _, h := range rep.Handled {
		events = append(events, db.TelemetryEvent{
			UserID: userID, Action: "reply_" + h.Action,
			Label:   truncateLabel(fmt.Sprintf("class=%s fp=%s comment=%d", h.Class, h.Fingerprint, h.AuthorCommentID), 255),
			PROwner: h.RepoOwner, PRRepo: h.RepoName, PRNumber: h.PRNumber,
		})
	}
	for _, e := range rep.Errors {
		events = append(events, replyErrorEvent("reply_scan_error", e, userID))
	}
	if link.Linked > 0 {
		events = append(events, db.TelemetryEvent{
			UserID: userID, Action: "reply_roots_linked",
			Label: fmt.Sprintf("linked=%d unmatched=%d", link.Linked, link.Unmatched),
		})
	}
	for _, e := range link.Errors {
		events = append(events, replyErrorEvent("reply_link_error", e, userID))
	}
	return events
}

func replyErrorEvent(action, msg string, userID int) db.TelemetryEvent {
	ev := db.TelemetryEvent{UserID: userID, Action: action, Label: truncateLabel(msg, 255)}
	key, _, _ := strings.Cut(msg, ":")
	ownerRepo, num, ok := strings.Cut(key, "#")
	owner, repo, ok2 := strings.Cut(ownerRepo, "/")
	if number, err := strconv.Atoi(num); ok && ok2 && err == nil {
		ev.PROwner, ev.PRRepo, ev.PRNumber = owner, repo, number
	}
	return ev
}

// replyOutcomeEvent records one finished or failed text step.
func replyOutcomeEvent(o publisher.ReplyOutcome, err error, userID int) db.TelemetryEvent {
	label := fmt.Sprintf("outcome=%s decision=%s posted=%t model=%s ms=%d comment=%d", o.Outcome, o.Decision, o.Posted, o.Model, o.DurationMS, o.AuthorCommentID)
	action := "reply_decision"
	if err != nil {
		action = "reply_text_error"
		label = fmt.Sprintf("comment=%d: %v", o.AuthorCommentID, err)
	}
	return db.TelemetryEvent{UserID: userID, Action: action, Label: truncateLabel(label, 255), PROwner: o.RepoOwner, PRRepo: o.RepoName, PRNumber: o.PRNumber}
}

// replyInputFromRequest shapes the reactor's request for the reply model.
// ourID is the App's bot user id; comments it authored are marked Ours.
func replyInputFromRequest(req publisher.ReplyRequest, ourID int64) service.ReplyInput {
	thread := make([]service.ReplyMessage, 0, len(req.Thread))
	for _, c := range req.Thread {
		thread = append(thread, service.ReplyMessage{Author: c.Author, Ours: c.AuthorID == ourID, Body: c.Body, At: c.CreatedAt})
	}
	return service.ReplyInput{
		Owner: req.Owner, Repo: req.Repo, DefaultBranch: req.BaseRef, PRNumber: req.Number, HeadSHA: req.HeadSHA,
		Fingerprint: req.Fingerprint, FindingBody: req.Root.Body, Thread: thread,
		AuthorReply: req.Reply.Body, Class: string(req.Reply.Class),
	}
}

// replyResponder runs the reply model under its own concurrency cap so a busy
// review pool cannot delay acknowledgements, and a burst of replies cannot
// starve reviews.
func (p *Poller) replyResponder() publisher.Responder {
	return func(ctx context.Context, req publisher.ReplyRequest) (publisher.ReplyDecision, error) {
		select {
		case p.replySlots <- struct{}{}:
			defer func() { <-p.replySlots }()
		case <-ctx.Done():
			return publisher.ReplyDecision{}, ctx.Err()
		}
		token, err := p.ghClientConcrete.CurrentToken(ctx)
		if err != nil {
			return publisher.ReplyDecision{}, fmt.Errorf("get GitHub token: %w", err)
		}
		model := p.cfg.ReplyModel
		if model == "" {
			model = p.cfg.AgentModel
		}
		cfg := service.AgentConfig{
			CloneRootDir: p.cfg.AgentCloneRootDir, LogsDir: p.cfg.AgentLogsDir,
			WallClock: time.Duration(p.cfg.ReplyWallClockSec) * time.Second, MaxTurns: p.cfg.ReplyMaxTurns,
			GitHubToken: token, Backend: p.cfg.AgentBackend, Model: model, Effort: p.cfg.AgentEffort,
			AnthropicAPIKey: p.cfg.AnthropicAPIKey, OpenRouterAPIKey: p.cfg.OpenRouterAPIKey, OpenRouterBaseURL: p.cfg.OpenRouterBaseURL,
			FailureLogSink: p.persistAgentFailureLog,
		}
		ourID := req.Root.AuthorID
		out, err := service.RunAgentReply(ctx, cfg, p.agentSpawner, replyInputFromRequest(req, ourID))
		if err != nil {
			return publisher.ReplyDecision{}, err
		}
		cited := make([]publisher.EvidenceRef, 0, len(out.Cited))
		for _, e := range out.Cited {
			cited = append(cited, publisher.EvidenceRef{File: e.File, Line: e.Line})
		}
		log.Printf("[REPLY %s/%s#%d] decision=%s cited=%d unresolved=%d turns=%d ms=%d model=%s",
			req.Owner, req.Repo, req.Number, out.Decision, len(out.Cited), len(out.Unresolved), out.AssistantTurns, out.DurationMS, out.ServedModel)
		return publisher.ReplyDecision{Decision: out.Decision, Reply: out.Reply, Cited: cited, Model: out.ServedModel, DurationMS: out.DurationMS}, nil
	}
}

func truncateLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
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
	if p.replyLinkTried == nil {
		p.replyLinkTried = map[string]time.Time{}
	}
	cycle := p.replyScanCycle.Add(1)
	full := cycle%replyFullScanEvery == 1
	enabled, _ := p.db.GetSetting(settingPublishEnabledAuthors)
	reactor := publisher.ReplyReactor{
		GH:          ghReplyAdapter{p.ghClientConcrete},
		Ledger:      ledger,
		Mode:        mode,
		Since:       since,
		LastScanned: p.replyLastScanned,
		Full:        full,
		Responder:   p.replyResponder(),
		InFlight:    &p.replyInFlight,
		Background:  func(task func()) { go task() },
		Live: func() (string, func(string) bool) {
			liveMode, _ := p.db.GetSetting(settingPublishReplyMode)
			liveEnabled, _ := p.db.GetSetting(settingPublishEnabledAuthors)
			return strings.TrimSpace(strings.ToLower(liveMode)), func(login string) bool { return publishEnabledFor(login, liveEnabled) }
		},
		OnOutcome: func(o publisher.ReplyOutcome, err error) {
			if err != nil {
				log.Printf("[REPLY %s/%s#%d] text step for comment %d failed, will resume: %v", o.RepoOwner, o.RepoName, o.PRNumber, o.AuthorCommentID, err)
			} else {
				log.Printf("[REPLY %s/%s#%d] comment %d: outcome=%s decision=%s posted=%t", o.RepoOwner, o.RepoName, o.PRNumber, o.AuthorCommentID, o.Outcome, o.Decision, o.Posted)
			}
			if userID := p.systemTelemetryUserID(); userID != 0 {
				if terr := p.db.CreateTelemetryEvents([]db.TelemetryEvent{replyOutcomeEvent(o, err, userID)}); terr != nil {
					log.Printf("[REPLIES] WARN: could not record reply outcome: %v", terr)
				}
			}
		},
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
				UpdatedAt:   ghPR.GetUpdatedAt().Time,
				HeadSHA:     ghPR.GetHead().GetSHA(),
				BaseRef:     ghPR.GetBase().GetRef(),
			}, nil
		},
	}

	// Findings posted through a review before the ledger recorded comment ids
	// have no thread to match replies against; link them before targeting.
	var link publisher.LinkReport
	if unlinked, err := ledger.ListUnlinkedPublishedFindings(); err != nil {
		log.Printf("[REPLIES] list unlinked roots: %v", err)
	} else if due := replyLinkDue(unlinked, p.replyLinkTried, full, time.Now()); len(due) > 0 {
		link = reactor.LinkRoots(ctx, due)
		unstampFailedLinks(p.replyLinkTried, link.Errors)
		log.Printf("[REPLIES] link roots: candidates=%d prs=%d linked=%d unmatched=%d errors=%d", len(due), link.PRsListed, link.Linked, link.Unmatched, len(link.Errors))
		for _, e := range link.Errors {
			log.Printf("[REPLIES] link: %s", e)
		}
		if link.Linked > 0 {
			if targets, err = ledger.ListPublishedReplyTargets(); err != nil {
				log.Printf("[REPLIES] list targets after link: %v", err)
				return
			}
		}
	}

	var rep publisher.ReplyReport
	if len(targets) > 0 {
		reactor.Targets = targets
		rep, err = reactor.Run(ctx)
		if err != nil {
			log.Printf("[REPLIES] scan failed: %v", err)
			return
		}
		for _, e := range rep.Errors {
			log.Printf("[REPLIES] %s", e)
		}
	}
	log.Printf("[REPLIES] cycle=%d full=%t mode=%s targets=%d scanned=%d skipped=%v replies_seen=%d already_handled=%d recorded=%d reacted=%d text_dispatched=%d errors=%d",
		cycle, full, mode, len(targets), rep.PRsScanned, rep.PRsSkipped, rep.RepliesSeen, rep.AlreadyHandled, rep.Recorded, rep.Reacted, rep.Dispatched, len(rep.Errors))
	userID := p.systemTelemetryUserID()
	if userID == 0 {
		return
	}
	if events := replyTelemetryEvents(rep, link, userID); len(events) > 0 {
		if err := p.db.CreateTelemetryEvents(events); err != nil {
			log.Printf("[REPLIES] WARN: could not record telemetry: %v", err)
		}
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
			ID: c.ID, InReplyToID: c.InReplyToID, ReviewID: c.ReviewID, AuthorID: c.AuthorID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

func (a ghReplyAdapter) React(ctx context.Context, owner, repo string, commentID int64) error {
	return a.c.CreateCommentReaction(ctx, owner, repo, commentID, "+1")
}

func (a ghReplyAdapter) PostReply(ctx context.Context, owner, repo string, number int, rootCommentID int64, body string) (int64, error) {
	return a.c.CreateReviewCommentReply(ctx, owner, repo, number, rootCommentID, body)
}
