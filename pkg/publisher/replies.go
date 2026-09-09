package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
)

// ReplyClass is what an author's reply to one of our inline comments amounts
// to. Only questions and pushback ever earn a text response; everything else
// is acknowledged with a reaction.
type ReplyClass string

const (
	ReplyResolution ReplyClass = "resolution"
	ReplyQuestion   ReplyClass = "question"
	ReplyPushback   ReplyClass = "pushback"
	ReplyOther      ReplyClass = "other"
)

// ThreadComment is one review comment on a PR, as much of it as reply
// handling needs.
type ThreadComment struct {
	ID          int64
	InReplyToID int64
	ReviewID    int64
	AuthorID    int64
	Author      string
	Body        string
	CreatedAt   time.Time
}

// AuthorReply is a PR author's reply under one of our published findings.
type AuthorReply struct {
	CommentID     int64
	RootCommentID int64
	Fingerprint   string
	Body          string
	CreatedAt     time.Time
	Class         ReplyClass
}

var (
	resolutionOpeners = regexp.MustCompile(`^(fixed|done|addressed|resolved|updated|removed|handled|good catch|accepted for now)\b`)
	acknowledgements  = regexp.MustCompile(`^(ok(ay)?|thanks?( you)?|thx|ty|ack(nowledged)?|will fix|will do|noted|sure|yep|yes|sounds good|makes sense|got it)\b`)
	minPushbackRunes  = 20
)

// ClassifyReply uses anchored whole-message rules for the unambiguous cases
// and leaves everything else as pushback, which the model handles later.
// "Not fixed" and "Fixed?" deliberately fall through the resolution rule.
func ClassifyReply(body string) ReplyClass {
	text := strings.ToLower(strings.TrimSpace(body))
	if strings.Contains(text, "?") {
		return ReplyQuestion
	}
	if resolutionOpeners.MatchString(text) {
		return ReplyResolution
	}
	if acknowledgements.MatchString(text) || len([]rune(text)) < minPushbackRunes {
		return ReplyOther
	}
	return ReplyPushback
}

// FindAuthorReplies returns the PR author's replies under our root comments,
// oldest first, ignoring anything created before since. roots maps our
// comment ids (from the ledger) to finding fingerprints; markers alone are
// never trusted as ownership.
func FindAuthorReplies(comments []ThreadComment, roots map[int64]string, authorID int64, since time.Time) []AuthorReply {
	var out []AuthorReply
	for _, c := range comments {
		fp, ok := roots[c.InReplyToID]
		if !ok || c.AuthorID != authorID || c.CreatedAt.Before(since) {
			continue
		}
		out = append(out, AuthorReply{
			CommentID:     c.ID,
			RootCommentID: c.InReplyToID,
			Fingerprint:   fp,
			Body:          c.Body,
			CreatedAt:     c.CreatedAt,
			Class:         ClassifyReply(c.Body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Reply modes, in increasing order of what PRism does with an author reply:
// off ignores them, observe records them, react adds a 👍, shadow also runs
// the reply model and records what it would have said, respond posts it.
const (
	ReplyModeOff     = "off"
	ReplyModeObserve = "observe"
	ReplyModeReact   = "react"
	ReplyModeShadow  = "shadow"
	ReplyModeRespond = "respond"

	replyActionObserved = "observed"
	replyActionReacted  = "reacted"
)

// Decisions the reply model can reach about an author's pushback or question.
const (
	DecisionConcede = "concede"
	DecisionHold    = "hold"
	DecisionAnswer  = "answer"
	DecisionAbstain = "abstain"
)

// ReplyMarker tags a text reply PRism posted with the author comment it
// answers, so a crash between posting and recording cannot produce a second
// reply for the same comment.
func ReplyMarker(authorCommentID int64) string {
	return fmt.Sprintf("<!-- prism:reply:v1:%d -->", authorCommentID)
}

var replyMarkerRe = regexp.MustCompile(`<!-- prism:reply:v1:(\d+) -->`)

func replyMarkerID(body string) (int64, bool) {
	m := replyMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

// ReplyGitHub is the slice of GitHub the reactor needs.
type ReplyGitHub interface {
	ListThread(ctx context.Context, owner, repo string, number int) ([]ThreadComment, error)
	React(ctx context.Context, owner, repo string, commentID int64) error
	PostReply(ctx context.Context, owner, repo string, number int, rootCommentID int64, body string) (int64, error)
}

// ReplyLedger persists what we did about each author reply.
type ReplyLedger interface {
	ListPublishedReplyTargets() ([]db.PublishedReplyTarget, error)
	GetPublishedReplyIDsForPR(owner, repo string, number int) (map[int64]bool, error)
	RecordPublishedReply(*db.PublishedReply) (bool, error)
	ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error)
	LinkPublishedFindingComment(id uint, commentID int64) error
	SetPublishedReplyDecision(owner, repo string, number int, authorCommentID int64, d db.ReplyDecisionRecord) error
	MarkPublishedReplyPosted(owner, repo string, number int, authorCommentID, replyCommentID int64, at time.Time) error
	ListPublishedRepliesForRoot(owner, repo string, number int, rootCommentID int64) ([]db.PublishedReply, error)
	CountPublishedTextRepliesSince(owner, repo string, number int, since time.Time) (int, error)
	SetPublishedFindingState(owner, repo string, number int, fingerprint, state string) error
}

// PRState is the live state of a PR that gates any reaction.
type PRState struct {
	Open        bool
	Draft       bool
	AuthorID    int64
	AuthorLogin string
	UpdatedAt   time.Time
	HeadSHA     string
	BaseRef     string
}

// EvidenceRef is a file:line the reply model cites for a hold.
type EvidenceRef struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// ReplyRequest is everything the reply model needs to answer one author reply.
type ReplyRequest struct {
	Owner       string
	Repo        string
	Number      int
	HeadSHA     string
	BaseRef     string
	Fingerprint string
	Root        ThreadComment   // our inline comment
	Thread      []ThreadComment // root and every reply under it, oldest first
	Reply       AuthorReply     // the author reply being answered
}

// ReplyDecision is the reply model's conclusion. Reply is empty for abstain.
type ReplyDecision struct {
	Decision   string
	Reply      string
	Cited      []EvidenceRef
	Model      string
	DurationMS int64
}

// Responder runs the reply model.
type Responder func(ctx context.Context, req ReplyRequest) (ReplyDecision, error)

// TextPolicy bounds text replies: questions and substantive pushback only,
// while the thread is fresh, at most MaxPerThread per thread, MaxPerPRPerDay
// per PR, and MaxChars per reply (a longer reply is dropped, not truncated).
type TextPolicy struct {
	MaxPerThread   int
	MaxPerPRPerDay int
	MaxAge         time.Duration
	MaxChars       int
}

func DefaultTextPolicy() TextPolicy {
	return TextPolicy{MaxPerThread: 2, MaxPerPRPerDay: 10, MaxAge: 24 * time.Hour, MaxChars: 600}
}

// TextEligibility returns "" when a text reply may be attempted, otherwise the
// reason it may not: class, stale, thread_cap, conceded, pr_cap.
func TextEligibility(reply AuthorReply, prior []db.PublishedReply, prTextToday int, now time.Time, p TextPolicy) string {
	if reply.Class != ReplyQuestion && reply.Class != ReplyPushback {
		return "class"
	}
	if now.Sub(reply.CreatedAt) > p.MaxAge {
		return "stale"
	}
	posted := 0
	for _, r := range prior {
		if r.ReplyCommentID != 0 {
			posted++
			if r.Decision == DecisionConcede {
				return "conceded"
			}
		}
	}
	if posted >= p.MaxPerThread {
		return "thread_cap"
	}
	if prTextToday >= p.MaxPerPRPerDay {
		return "pr_cap"
	}
	return ""
}

// ReplyReactor acknowledges PR authors' replies under our inline comments.
// Mode observe records them; react also adds a 👍. Since is the activation
// cutoff so enabling the feature never answers historical threads.
//
// LastScanned, when set, holds the live updated_at of each PR at its last
// successful scan; a PR whose updated_at has not moved is skipped without
// listing its thread (review comment replies bump updated_at). Full ignores
// the watermarks. The live value comes from PR, never from a cached PR row,
// because the poller's copy trails GitHub's search index by minutes.
type ReplyReactor struct {
	GH          ReplyGitHub
	Ledger      ReplyLedger
	PR          func(ctx context.Context, owner, repo string, number int) (PRState, error)
	Allowed     func(authorLogin string) bool
	Mode        string
	Since       time.Time
	Targets     []db.PublishedReplyTarget // optional pre-filtered subset; nil means all ledger targets
	LastScanned map[string]time.Time
	Full        bool

	// Responder answers questions and pushback in shadow and respond modes;
	// nil disables text even in those modes. Text is zero-valued to
	// DefaultTextPolicy. Now is for tests.
	Responder Responder
	Text      TextPolicy
	Now       func() time.Time
}

// ReplyReport is one scan's accounting. PRsSkipped is keyed by reason
// (closed, draft, not_allowlisted); RepliesSeen counts the author's replies
// under our roots whether or not they were new; Handled lists what this scan
// recorded, for telemetry.
type ReplyReport struct {
	PRsScanned     int
	PRsSkipped     map[string]int
	RepliesSeen    int
	AlreadyHandled int
	Reacted        int
	Recorded       int
	Handled        []db.PublishedReply
	Errors         []string

	// Text accounting: Responded posted a reply, Shadowed recorded one without
	// posting, Abstained ran the model and it declined, TextSkipped is keyed by
	// the eligibility or safety reason a reply was not attempted or posted.
	Responded   int
	Shadowed    int
	Abstained   int
	TextSkipped map[string]int
	Decisions   []ReplyOutcome
}

// ReplyOutcome is one reply-model run, for telemetry.
type ReplyOutcome struct {
	RepoOwner       string
	RepoName        string
	PRNumber        int
	AuthorCommentID int64
	Decision        string
	Posted          bool
	Model           string
	DurationMS      int64
}

func (r *ReplyReport) skipText(reason string) {
	if r.TextSkipped == nil {
		r.TextSkipped = map[string]int{}
	}
	r.TextSkipped[reason]++
}

func (r *ReplyReport) skip(reason string) {
	if r.PRsSkipped == nil {
		r.PRsSkipped = map[string]int{}
	}
	r.PRsSkipped[reason]++
}

// LinkReport accounts for one pass of LinkRoots.
type LinkReport struct {
	PRsListed int
	Linked    int
	Unmatched int
	Errors    []string
}

// MatchUnlinkedRoots pairs ledger rows that lack a comment id with the review
// comment GitHub created for them. A match needs both the review id we were
// handed when we posted the review and our finding marker in a root comment,
// so neither a quoted marker nor another review's comment can claim a row.
func MatchUnlinkedRoots(rows []db.UnlinkedPublishedFinding, comments []ThreadComment) map[uint]int64 {
	byReview := map[int64]map[string]int64{}
	for _, c := range comments {
		if c.InReplyToID != 0 || c.ReviewID == 0 {
			continue
		}
		fp, ok := FindingIDFromBody(c.Body)
		if !ok {
			continue
		}
		if byReview[c.ReviewID] == nil {
			byReview[c.ReviewID] = map[string]int64{}
		}
		byReview[c.ReviewID][fp] = c.ID
	}
	out := map[uint]int64{}
	for _, row := range rows {
		if id, ok := byReview[row.ReviewID][row.Fingerprint]; ok {
			out[row.ID] = id
		}
	}
	return out
}

// LinkRoots back-fills comment ids for review-posted findings so their threads
// become reply targets. One thread listing per PR.
func (r ReplyReactor) LinkRoots(ctx context.Context, rows []db.UnlinkedPublishedFinding) LinkReport {
	var rep LinkReport
	byPR := map[string][]db.UnlinkedPublishedFinding{}
	var order []string
	for _, row := range rows {
		key := fmt.Sprintf("%s/%s#%d", row.RepoOwner, row.RepoName, row.PRNumber)
		if _, seen := byPR[key]; !seen {
			order = append(order, key)
		}
		byPR[key] = append(byPR[key], row)
	}
	for _, key := range order {
		group := byPR[key]
		first := group[0]
		comments, err := r.GH.ListThread(ctx, first.RepoOwner, first.RepoName, first.PRNumber)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: list thread: %v", key, err))
			continue
		}
		rep.PRsListed++
		matches := MatchUnlinkedRoots(group, comments)
		for _, row := range group {
			commentID, ok := matches[row.ID]
			if !ok {
				rep.Unmatched++
				continue
			}
			if err := r.Ledger.LinkPublishedFindingComment(row.ID, commentID); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: link %s: %v", key, row.Fingerprint, err))
				continue
			}
			rep.Linked++
		}
	}
	return rep
}

func (r ReplyReactor) Run(ctx context.Context) (ReplyReport, error) {
	var rep ReplyReport
	targets := r.Targets
	if targets == nil {
		var err error
		if targets, err = r.Ledger.ListPublishedReplyTargets(); err != nil {
			return rep, err
		}
	}
	for _, t := range targets {
		if err := r.scan(ctx, t, &rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s#%d: %v", t.RepoOwner, t.RepoName, t.PRNumber, err))
		}
	}
	return rep, nil
}

func (r ReplyReactor) scan(ctx context.Context, t db.PublishedReplyTarget, rep *ReplyReport) error {
	state, err := r.PR(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	switch {
	case !state.Open:
		rep.skip("closed")
		return nil
	case state.Draft:
		rep.skip("draft")
		return nil
	case !r.Allowed(state.AuthorLogin):
		rep.skip("not_allowlisted")
		return nil
	}
	key := fmt.Sprintf("%s/%s#%d", t.RepoOwner, t.RepoName, t.PRNumber)
	if r.LastScanned != nil && !r.Full && !state.UpdatedAt.IsZero() && !state.UpdatedAt.After(r.LastScanned[key]) {
		rep.skip("unchanged")
		return nil
	}
	rep.PRsScanned++
	comments, err := r.GH.ListThread(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	seen, err := r.Ledger.GetPublishedReplyIDsForPR(t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	for _, reply := range FindAuthorReplies(comments, t.Roots, state.AuthorID, r.Since) {
		rep.RepliesSeen++
		if seen[reply.CommentID] {
			rep.AlreadyHandled++
			continue
		}
		action := replyActionObserved
		if r.reacts() {
			if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
				return err
			}
			action = replyActionReacted
			rep.Reacted++
		}
		row := db.PublishedReply{
			RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber,
			RootCommentID: reply.RootCommentID, AuthorCommentID: reply.CommentID,
			Fingerprint: reply.Fingerprint, AuthorID: state.AuthorID,
			Class: string(reply.Class), Action: action, Body: reply.Body, CreatedAt: reply.CreatedAt,
		}
		created, err := r.Ledger.RecordPublishedReply(&row)
		if err != nil {
			return err
		}
		if created {
			rep.Recorded++
			rep.Handled = append(rep.Handled, row)
			if r.textMode() {
				if err := r.respond(ctx, t, state, comments, reply, rep); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: reply to %d: %v", key, reply.CommentID, err))
				}
			}
		}
	}
	if r.LastScanned != nil {
		r.LastScanned[key] = state.UpdatedAt
	}
	return nil
}

func (r ReplyReactor) reacts() bool {
	return r.Mode == ReplyModeReact || r.Mode == ReplyModeShadow || r.Mode == ReplyModeRespond
}

func (r ReplyReactor) textMode() bool {
	return r.Responder != nil && (r.Mode == ReplyModeShadow || r.Mode == ReplyModeRespond)
}

func (r ReplyReactor) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r ReplyReactor) textPolicy() TextPolicy {
	if r.Text == (TextPolicy{}) {
		return DefaultTextPolicy()
	}
	return r.Text
}

// threadUnder returns our root comment and everything replying to it, oldest
// first.
func threadUnder(comments []ThreadComment, rootID int64) []ThreadComment {
	var out []ThreadComment
	for _, c := range comments {
		if c.ID == rootID || c.InReplyToID == rootID {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID == rootID {
			return true
		}
		if out[j].ID == rootID {
			return false
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// respond decides and, in respond mode, posts a text reply to one author
// reply. The decision is persisted before posting; the thread and PR head are
// re-read just before posting and the reply is dropped if either moved. A
// reply carrying our marker for this author comment is adopted instead of
// duplicated.
func (r ReplyReactor) respond(ctx context.Context, t db.PublishedReplyTarget, state PRState, comments []ThreadComment, reply AuthorReply, rep *ReplyReport) error {
	now := r.now()
	prior, err := r.Ledger.ListPublishedRepliesForRoot(t.RepoOwner, t.RepoName, t.PRNumber, reply.RootCommentID)
	if err != nil {
		return err
	}
	today, err := r.Ledger.CountPublishedTextRepliesSince(t.RepoOwner, t.RepoName, t.PRNumber, now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	if reason := TextEligibility(reply, prior, today, now, r.textPolicy()); reason != "" {
		rep.skipText(reason)
		return nil
	}
	thread := threadUnder(comments, reply.RootCommentID)
	for _, c := range thread {
		if id, ok := replyMarkerID(c.Body); ok && id == reply.CommentID {
			rep.skipText("already_posted")
			return r.Ledger.MarkPublishedReplyPosted(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, c.ID, c.CreatedAt)
		}
	}
	var root ThreadComment
	for _, c := range thread {
		if c.ID == reply.RootCommentID {
			root = c
		}
	}
	decision, err := r.Responder(ctx, ReplyRequest{
		Owner: t.RepoOwner, Repo: t.RepoName, Number: t.PRNumber, HeadSHA: state.HeadSHA, BaseRef: state.BaseRef,
		Fingerprint: reply.Fingerprint, Root: root, Thread: thread, Reply: reply,
	})
	if err != nil {
		return err
	}
	outcome := ReplyOutcome{RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber, AuthorCommentID: reply.CommentID,
		Decision: decision.Decision, Model: decision.Model, DurationMS: decision.DurationMS}
	defer func() { rep.Decisions = append(rep.Decisions, outcome) }()
	cited, _ := json.Marshal(decision.Cited)
	if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, db.ReplyDecisionRecord{
		Decision: decision.Decision, ReplyBody: decision.Reply, Cited: string(cited), Model: decision.Model, DurationMS: decision.DurationMS,
	}); err != nil {
		return err
	}
	text := strings.TrimSpace(decision.Reply)
	switch {
	case decision.Decision == DecisionAbstain || text == "":
		rep.Abstained++
		return nil
	case len([]rune(text)) > r.textPolicy().MaxChars:
		rep.skipText("too_long")
		return nil
	case r.Mode != ReplyModeRespond:
		rep.Shadowed++
		return nil
	}

	fresh, err := r.PR(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	if fresh.HeadSHA != state.HeadSHA || !fresh.Open || fresh.Draft {
		rep.skipText("head_moved")
		return nil
	}
	latest, err := r.GH.ListThread(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	if len(threadUnder(latest, reply.RootCommentID)) != len(thread) {
		rep.skipText("thread_moved")
		return nil
	}
	body := text + "\n\n" + ReplyMarker(reply.CommentID)
	id, err := r.GH.PostReply(ctx, t.RepoOwner, t.RepoName, t.PRNumber, reply.RootCommentID, body)
	if err != nil {
		return err
	}
	if err := r.Ledger.MarkPublishedReplyPosted(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, id, now); err != nil {
		return err
	}
	rep.Responded++
	outcome.Posted = true
	if decision.Decision == DecisionConcede {
		if err := r.Ledger.SetPublishedFindingState(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint, db.PublishedStateDismissed); err != nil {
			return err
		}
	}
	return nil
}
