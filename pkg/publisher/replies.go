package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	ReplyActionObserved = "observed"
	ReplyActionReacted  = "reacted"
	ReplyActionPending  = "pending" // reaction deferred to the reply model's decision
)

// Decisions the reply model can reach about an author's pushback or question.
const (
	DecisionConcede = "concede"
	DecisionHold    = "hold"
	DecisionAnswer  = "answer"
	DecisionAbstain = "abstain"
)

// errClaimedElsewhere reports a text step another instance currently holds;
// the scan neither settles the PR nor reports an outcome for it.
var errClaimedElsewhere = errors.New("text step claimed by another instance")

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
	RecordPublishedReply(*db.PublishedReply) (bool, error)
	ListPublishedRepliesForPR(owner, repo string, number int) ([]db.PublishedReply, error)
	ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error)
	LinkPublishedFindingComment(id uint, commentID int64) error
	SetPublishedReplyOutcome(owner, repo string, number int, authorCommentID int64, outcome string) error
	SetPublishedReplyAction(owner, repo string, number int, authorCommentID int64, action string) error
	ClaimPublishedReply(owner, repo string, number int, authorCommentID int64, holder string, now time.Time, lease time.Duration) (bool, error)
	ReleasePublishedReplyClaim(owner, repo string, number int, authorCommentID int64, holder string) error
	IncrementPublishedReplyAttempts(owner, repo string, number int, authorCommentID int64) (int, error)
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
// React says whether the author's comment gets a 👍 alongside (or instead of)
// the text; the model chooses so a rebutted pushback is not thumbed up.
type ReplyDecision struct {
	Decision   string
	Reply      string
	Cited      []EvidenceRef
	React      bool
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
	MaxAttempts    int // model runs per reply before the step is marked failed
}

func DefaultTextPolicy() TextPolicy {
	return TextPolicy{MaxPerThread: 2, MaxPerPRPerDay: 10, MaxAge: 24 * time.Hour, MaxChars: 600, MaxAttempts: 3}
}

// TextEligibility returns "" when a text reply may be attempted, otherwise the
// reason it may not: class, stale, thread_cap, conceded, pr_cap. The caps are
// exact within one process (text steps on a PR are serialized) and best-effort
// across two instances that both believe they lead, where sibling replies
// claimed separately can overshoot a cap by one.
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

	// Background, when set, runs each text step off the scan so a burst of
	// replies cannot hold up reactions; InFlight then prevents a later scan
	// from starting the same reply twice. OnOutcome receives every finished
	// or failed text step in either mode.
	Background func(task func())
	InFlight   *ReplyInFlight
	OnOutcome  func(ReplyOutcome, error)

	// Live, when set, re-reads the mode and allowlist right before a post so a
	// switch flipped during a long model run is honoured. A read error leaves
	// the step unfinished rather than recording a policy change.
	Live func() (mode string, allowed func(authorLogin string) bool, err error)

	// Holder names this instance in the ledger claim that makes the text
	// step exclusive across processes; ClaimLease bounds how long a dead
	// holder's claim blocks others (zero: 10 minutes).
	Holder     string
	ClaimLease time.Duration
}

func (r ReplyReactor) claimLease() time.Duration {
	if r.ClaimLease > 0 {
		return r.ClaimLease
	}
	return 10 * time.Minute
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
	Dispatched  int
	TextSkipped map[string]int
}

// ReplyOutcome is the result of one text step, for telemetry. Outcome is the
// terminal state recorded on the row (empty when the step errored).
type ReplyOutcome struct {
	RepoOwner       string
	RepoName        string
	PRNumber        int
	AuthorCommentID int64
	Decision        string
	Outcome         string
	Posted          bool
	Action          string // how the author's comment was acknowledged: reacted or observed
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
	rows, err := r.Ledger.ListPublishedRepliesForPR(t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return err
	}
	seen := make(map[int64]db.PublishedReply, len(rows))
	for _, row := range rows {
		seen[row.AuthorCommentID] = row
	}
	// The watermark only advances when nothing on this PR failed or is still
	// mid-flight, so incomplete text steps are resumed next cycle instead of
	// waiting for updated_at to move.
	settled := true
	for _, reply := range FindAuthorReplies(comments, t.Roots, state.AuthorID, r.Since) {
		rep.RepliesSeen++
		row, handled := seen[reply.CommentID]
		if !handled {
			action := ReplyActionObserved
			switch {
			case r.reacts() && r.textMode() && textClass(reply.Class):
				// The model decides whether this one gets a 👍; see text().
				action = ReplyActionPending
			case r.reacts():
				if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
					return err
				}
				action = ReplyActionReacted
				rep.Reacted++
			}
			row = db.PublishedReply{
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
			}
		} else {
			rep.AlreadyHandled++
		}
		if handled && row.Action == ReplyActionPending && row.Outcome != "" {
			// A finished step that never settled its reaction (a rolling deploy
			// mixing builds): nothing else will write this row. A posted reply
			// follows the recorded decision (a rebuttal is not thumbed up);
			// anything else is acknowledged.
			if row.Outcome == "posted" && row.Decision != "" && !row.DecisionReact {
				if err := r.Ledger.SetPublishedReplyAction(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, ReplyActionObserved); err != nil {
					return err
				}
				continue
			}
			if r.reacts() {
				if err := r.reactAndRecord(ctx, t, reply, &row, rep); err != nil {
					return err
				}
			}
			continue
		}
		if !r.textMode() {
			// The mode was turned down to react while this reply waited for the
			// model: acknowledge it now. Under observe or off the row stays
			// pending on purpose, so a later return to text mode still runs
			// the model on it; the PR stays unsettled so that happens on the
			// first cycle after the switch rather than the next full scan.
			if handled && row.Action == ReplyActionPending {
				if !r.reacts() {
					settled = false
					continue
				}
				claimed, err := r.settlePendingReaction(ctx, t, reply, &row, rep)
				if err != nil {
					return err
				}
				if !claimed {
					settled = false
				}
			}
			continue
		}
		if row.Outcome != "" {
			continue
		}
		if r.Background != nil {
			settled = false
			if r.InFlight == nil || r.InFlight.add(reply.CommentID) {
				rep.Dispatched++
				r.Background(func() {
					// Text steps on one PR run one at a time so the per-thread
					// and per-PR caps are checked and acted on by a single
					// goroutine; different PRs still run in parallel.
					if r.InFlight != nil {
						unlock := r.InFlight.lockPR(key)
						defer unlock()
						defer r.InFlight.remove(reply.CommentID)
					}
					outcome, err := r.text(ctx, t, state, comments, reply, row, nil)
					if r.OnOutcome != nil && !errors.Is(err, errClaimedElsewhere) {
						r.OnOutcome(outcome, err)
					}
				})
			}
			continue
		}
		outcome, err := r.text(ctx, t, state, comments, reply, row, rep)
		if errors.Is(err, errClaimedElsewhere) {
			settled = false
			continue
		}
		if r.OnOutcome != nil {
			r.OnOutcome(outcome, err)
		}
		if err != nil {
			settled = false
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: reply to %d: %v", key, reply.CommentID, err))
			continue
		}
		if outcome.Posted {
			// The next reply in this scan must see what was just posted.
			if comments, err = r.GH.ListThread(ctx, t.RepoOwner, t.RepoName, t.PRNumber); err != nil {
				return err
			}
		}
	}
	if r.LastScanned != nil && settled {
		r.LastScanned[key] = state.UpdatedAt
	}
	return nil
}

// settlePendingReaction acknowledges a reply whose text step will not run any
// more (the mode was lowered). It takes the row's claim first so a worker
// still finishing that step on another instance cannot write over it; a
// refused claim reports false so the caller keeps the PR unsettled.
func (r ReplyReactor) settlePendingReaction(ctx context.Context, t db.PublishedReplyTarget, reply AuthorReply, row *db.PublishedReply, rep *ReplyReport) (bool, error) {
	claimed, err := r.Ledger.ClaimPublishedReply(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, r.Holder, r.now(), r.claimLease())
	if err != nil || !claimed {
		return false, err
	}
	defer func() {
		_ = r.Ledger.ReleasePublishedReplyClaim(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, r.Holder)
	}()
	// The text step will not run for this reply any more; leave the same
	// terminal marker the step itself leaves when the mode changes under it,
	// so a later return to text mode does not rebut an acknowledged comment.
	// The outcome goes first: if the reaction then fails, the row is a
	// terminal pending one and the scan's recovery branch settles it.
	if err := r.Ledger.SetPublishedReplyOutcome(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, "skipped:mode_changed"); err != nil {
		return true, err
	}
	row.Outcome = "skipped:mode_changed"
	return true, r.reactAndRecord(ctx, t, reply, row, rep)
}

func (r ReplyReactor) reactAndRecord(ctx context.Context, t db.PublishedReplyTarget, reply AuthorReply, row *db.PublishedReply, rep *ReplyReport) error {
	if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
		return err
	}
	if err := r.Ledger.SetPublishedReplyAction(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, ReplyActionReacted); err != nil {
		return err
	}
	rep.Reacted++
	row.Action = ReplyActionReacted
	rep.Handled = append(rep.Handled, *row)
	return nil
}

// ReplyInFlight tracks author comments whose text step is running in the
// background so a later scan does not start a second model run for them, and
// serializes text steps per PR.
type ReplyInFlight struct {
	mu    sync.Mutex
	ids   map[int64]bool
	locks map[string]*prLock
}

type prLock struct {
	sync.Mutex
	waiters int
}

// lockPR serializes text steps on one PR; the entry is dropped when the last
// waiter releases it so the map does not grow with every PR ever replied to.
func (f *ReplyInFlight) lockPR(key string) func() {
	f.mu.Lock()
	if f.locks == nil {
		f.locks = map[string]*prLock{}
	}
	l, ok := f.locks[key]
	if !ok {
		l = &prLock{}
		f.locks[key] = l
	}
	l.waiters++
	f.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		f.mu.Lock()
		l.waiters--
		if l.waiters == 0 {
			delete(f.locks, key)
		}
		f.mu.Unlock()
	}
}

func (f *ReplyInFlight) add(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[int64]bool{}
	}
	if f.ids[id] {
		return false
	}
	f.ids[id] = true
	return true
}

func (f *ReplyInFlight) remove(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, id)
}

// textClass reports whether a reply class is one the reply model handles.
func textClass(c ReplyClass) bool {
	return c == ReplyQuestion || c == ReplyPushback
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

// text runs the resumable text step for one author reply. Every terminal
// exit records an Outcome; a returned error leaves Outcome empty so the next
// scan resumes from the persisted state (decision, posted reply) rather than
// redoing it. rep may be nil when running in the background.
func (r ReplyReactor) text(ctx context.Context, t db.PublishedReplyTarget, state PRState, comments []ThreadComment, reply AuthorReply, row db.PublishedReply, rep *ReplyReport) (outcome ReplyOutcome, err error) {
	outcome = ReplyOutcome{RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber, AuthorCommentID: reply.CommentID,
		Decision: row.Decision, Model: row.Model, DurationMS: row.DurationMS}
	// A deferred reaction is settled with the step. The model's choice applies
	// only when its reply is posted (a thumbs-up on a comment about to be
	// rebutted reads as agreement); every other ending, including shadow and
	// abstain, acknowledges the author so no reply goes unanswered.
	// The reaction is decided against the live mode and allowlist at the
	// moment it would be posted, so a switch flipped during a long model run
	// is honoured on every terminal path.
	liveReacts := func() (bool, error) {
		if r.Live == nil {
			return r.reacts(), nil
		}
		mode, allowed, err := r.Live()
		if err != nil {
			return false, err
		}
		return (mode == ReplyModeReact || mode == ReplyModeShadow || mode == ReplyModeRespond) && (allowed == nil || allowed(state.AuthorLogin)), nil
	}
	// Only a reaction settled by this step is reported on the outcome;
	// rows already acknowledged at scan time produced their event then.
	settledHere := false
	react := func(want bool) error {
		if row.Action != ReplyActionPending {
			return nil
		}
		settledHere = true
		if want {
			live, err := liveReacts()
			if err != nil {
				return err
			}
			want = live
		}
		action := ReplyActionObserved
		if want {
			// GitHub returns the existing reaction on a repeat, so a ledger
			// failure after this call retries safely next cycle.
			if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
				return err
			}
			action = ReplyActionReacted
			if rep != nil {
				rep.Reacted++
			}
		}
		if err := r.Ledger.SetPublishedReplyAction(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, action); err != nil {
			return err
		}
		row.Action = action
		return nil
	}
	finish := func(result string) (ReplyOutcome, error) {
		want := true
		if result == "posted" && row.Decision != "" {
			want = row.DecisionReact
		}
		if err := react(want); err != nil {
			return outcome, err
		}
		if settledHere {
			outcome.Action = row.Action
		}
		if err := r.Ledger.SetPublishedReplyOutcome(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, result); err != nil {
			return outcome, err
		}
		outcome.Outcome = result
		return outcome, nil
	}
	skip := func(reason string) (ReplyOutcome, error) {
		if rep != nil {
			rep.skipText(reason)
		}
		return finish("skipped:" + reason)
	}
	now := r.now()
	eligibility := func() (string, error) {
		prior, err := r.Ledger.ListPublishedRepliesForRoot(t.RepoOwner, t.RepoName, t.PRNumber, reply.RootCommentID)
		if err != nil {
			return "", err
		}
		var others []db.PublishedReply
		for _, p := range prior {
			if p.AuthorCommentID != reply.CommentID {
				others = append(others, p)
			}
		}
		today, err := r.Ledger.CountPublishedTextRepliesSince(t.RepoOwner, t.RepoName, t.PRNumber, now.Add(-24*time.Hour))
		if err != nil {
			return "", err
		}
		return TextEligibility(reply, others, today, now, r.textPolicy()), nil
	}
	thread := threadUnder(comments, reply.RootCommentID)
	var root ThreadComment
	for _, c := range thread {
		if c.ID == reply.RootCommentID {
			root = c
		}
	}
	// Every write below, the reaction and action included, happens under a
	// claim on the row so two instances cannot settle the same reply twice:
	// claim first, release on any error so the next scan resumes, and let
	// finish() leave the terminal outcome in place.
	claimed, err := r.Ledger.ClaimPublishedReply(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, r.Holder, now, r.claimLease())
	if err != nil {
		return outcome, err
	}
	if !claimed {
		if rep != nil {
			rep.skipText("claimed_elsewhere")
		}
		return outcome, errClaimedElsewhere
	}
	defer func() {
		if outcome.Outcome == "" {
			if rerr := r.Ledger.ReleasePublishedReplyClaim(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, r.Holder); rerr != nil && err == nil {
				err = rerr
			}
		}
	}()
	// Another holder may have decided or posted since the scan read its rows;
	// under the claim the ledger is the truth.
	current, err := r.Ledger.ListPublishedRepliesForRoot(t.RepoOwner, t.RepoName, t.PRNumber, reply.RootCommentID)
	if err != nil {
		return outcome, err
	}
	for _, f := range current {
		if f.AuthorCommentID == reply.CommentID {
			row = f
			outcome.Decision, outcome.Model, outcome.DurationMS = row.Decision, row.Model, row.DurationMS
		}
	}
	// A reply we posted but never recorded (crash between the two, or another
	// instance) is adopted before anything else: a posted reply exists whether
	// or not the author comment would still be eligible today. Only the
	// root's author, that is our own bot, can own such a marker.
	adoptPosted := func(in []ThreadComment) (bool, error) {
		for _, c := range in {
			if id, ok := replyMarkerID(c.Body); ok && id == reply.CommentID && c.InReplyToID == reply.RootCommentID && c.AuthorID == root.AuthorID && c.ID != reply.CommentID {
				return true, r.adopt(t, reply, c, &outcome)
			}
		}
		return false, nil
	}
	if adopted, err := adoptPosted(thread); err != nil {
		return outcome, err
	} else if adopted {
		if rep != nil {
			rep.skipText("already_posted")
		}
		return finish("posted")
	}
	reason, err := eligibility()
	if err != nil {
		return outcome, err
	}
	if reason != "" {
		if rep != nil {
			rep.skipText(reason)
		}
		return finish("ineligible:" + reason)
	}
	fingerprint := threadFingerprint(thread, root.AuthorID)
	if row.Decision != "" && (row.DecisionHead != state.HeadSHA || row.DecisionThread != fingerprint) {
		// The decision was made against a head or thread that has since moved;
		// a resumed step must not post it.
		if row.DecisionHead != state.HeadSHA {
			return skip("head_moved")
		}
		return skip("thread_moved")
	}
	if row.Decision == "" {
		if row.Attempts >= r.textPolicy().MaxAttempts {
			return finish("failed")
		}
		decision, err := r.Responder(ctx, ReplyRequest{
			Owner: t.RepoOwner, Repo: t.RepoName, Number: t.PRNumber, HeadSHA: state.HeadSHA, BaseRef: state.BaseRef,
			Fingerprint: reply.Fingerprint, Root: root, Thread: thread, Reply: reply,
		})
		// A run that was cut off by shutdown is not the model failing; the
		// claim lease keeps a crash loop to one run per lease anyway.
		if ctx.Err() == nil {
			if _, ierr := r.Ledger.IncrementPublishedReplyAttempts(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID); ierr != nil {
				return outcome, ierr
			}
		}
		if err != nil {
			return outcome, err
		}
		cited, _ := json.Marshal(decision.Cited)
		if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, db.ReplyDecisionRecord{
			Decision: decision.Decision, ReplyBody: decision.Reply, Cited: string(cited), Model: decision.Model, DurationMS: decision.DurationMS,
			Head: state.HeadSHA, Thread: fingerprint, React: decision.React,
		}); err != nil {
			return outcome, err
		}
		row.Decision, row.ReplyBody, row.DecisionReact = decision.Decision, decision.Reply, decision.React
		outcome.Decision, outcome.Model, outcome.DurationMS = decision.Decision, decision.Model, decision.DurationMS
	}
	text := strings.TrimSpace(row.ReplyBody)
	switch {
	case row.Decision == DecisionAbstain || text == "":
		if rep != nil {
			rep.Abstained++
		}
		return finish("abstained")
	case len([]rune(text)) > r.textPolicy().MaxChars:
		return skip("too_long")
	case r.Mode != ReplyModeRespond:
		if rep != nil {
			rep.Shadowed++
		}
		return finish("shadowed")
	}

	// Everything below re-reads live state: the model may have run for
	// minutes, another instance may have posted, or an operator may have
	// turned the feature down.
	if r.Live != nil {
		mode, allowed, err := r.Live()
		if err != nil {
			return outcome, err
		}
		switch {
		case mode == ReplyModeShadow:
			if rep != nil {
				rep.Shadowed++
			}
			return finish("shadowed")
		case mode != ReplyModeRespond || (allowed != nil && !allowed(state.AuthorLogin)):
			return skip("mode_changed")
		}
	}
	if reason, err := eligibility(); err != nil {
		return outcome, err
	} else if reason != "" {
		if rep != nil {
			rep.skipText(reason)
		}
		return finish("ineligible:" + reason)
	}
	fresh, err := r.PR(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return outcome, err
	}
	switch {
	case !fresh.Open:
		return skip("closed")
	case fresh.Draft:
		return skip("draft")
	case fresh.HeadSHA != state.HeadSHA:
		return skip("head_moved")
	}
	latest, err := r.GH.ListThread(ctx, t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return outcome, err
	}
	latestThread := threadUnder(latest, reply.RootCommentID)
	if adopted, err := adoptPosted(latestThread); err != nil {
		return outcome, err
	} else if adopted {
		return finish("posted")
	}
	if threadFingerprint(latestThread, root.AuthorID) != fingerprint {
		return skip("thread_moved")
	}
	id, err := r.GH.PostReply(ctx, t.RepoOwner, t.RepoName, t.PRNumber, reply.RootCommentID, text+"\n\n"+ReplyMarker(reply.CommentID))
	if err != nil {
		return outcome, err
	}
	if err := r.adopt(t, reply, ThreadComment{ID: id, CreatedAt: r.now()}, &outcome); err != nil {
		return outcome, err
	}
	if rep != nil {
		rep.Responded++
	}
	return finish("posted")
}

// adopt records a posted reply and applies a concession's side effect.
func (r ReplyReactor) adopt(t db.PublishedReplyTarget, reply AuthorReply, posted ThreadComment, outcome *ReplyOutcome) error {
	if err := r.Ledger.MarkPublishedReplyPosted(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, posted.ID, posted.CreatedAt); err != nil {
		return err
	}
	outcome.Posted = true
	if outcome.Decision == DecisionConcede {
		return r.Ledger.SetPublishedFindingState(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint, db.PublishedStateDismissed)
	}
	return nil
}

// threadFingerprint identifies the finding and everything written under it by
// anyone but us: an edit to the root or an author edit or deletion changes
// it, our own posted replies do not.
func threadFingerprint(thread []ThreadComment, ourID int64) string {
	h := sha256.New()
	for _, c := range thread {
		if c.AuthorID == ourID && c.InReplyToID != 0 {
			continue
		}
		fmt.Fprintf(h, "%d:%d:%s\x00", c.ID, len(c.Body), c.Body)
	}
	return hex.EncodeToString(h.Sum(nil))
}
