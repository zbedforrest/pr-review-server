package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	ListPublishedRepliesForPR(owner, repo string, number int) ([]db.PublishedReply, error)
	ListUnlinkedPublishedFindings() ([]db.UnlinkedPublishedFinding, error)
	LinkPublishedFindingComment(id uint, commentID int64) error
	SetPublishedReplyOutcome(owner, repo string, number int, authorCommentID int64, outcome string) error
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
	MaxAttempts    int // model runs per reply before the step is marked failed
}

func DefaultTextPolicy() TextPolicy {
	return TextPolicy{MaxPerThread: 2, MaxPerPRPerDay: 10, MaxAge: 24 * time.Hour, MaxChars: 600, MaxAttempts: 3}
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

	// Background, when set, runs each text step off the scan so a burst of
	// replies cannot hold up reactions; InFlight then prevents a later scan
	// from starting the same reply twice. OnOutcome receives every finished
	// or failed text step in either mode.
	Background func(task func())
	InFlight   *ReplyInFlight
	OnOutcome  func(ReplyOutcome, error)

	// Live, when set, re-reads the mode and allowlist right before a post so a
	// switch flipped during a long model run is honoured.
	Live func() (mode string, allowed func(authorLogin string) bool)
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
			action := replyActionObserved
			if r.reacts() {
				if err := r.GH.React(ctx, t.RepoOwner, t.RepoName, reply.CommentID); err != nil {
					return err
				}
				action = replyActionReacted
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
		if !r.textMode() || row.Outcome != "" {
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
					if r.OnOutcome != nil {
						r.OnOutcome(outcome, err)
					}
				})
			}
			continue
		}
		outcome, err := r.text(ctx, t, state, comments, reply, row, rep)
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

// ReplyInFlight tracks author comments whose text step is running in the
// background so a later scan does not start a second model run for them, and
// serializes text steps per PR.
type ReplyInFlight struct {
	mu    sync.Mutex
	ids   map[int64]bool
	locks map[string]*sync.Mutex
}

func (f *ReplyInFlight) lockPR(key string) func() {
	f.mu.Lock()
	if f.locks == nil {
		f.locks = map[string]*sync.Mutex{}
	}
	l, ok := f.locks[key]
	if !ok {
		l = &sync.Mutex{}
		f.locks[key] = l
	}
	f.mu.Unlock()
	l.Lock()
	return l.Unlock
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
func (r ReplyReactor) text(ctx context.Context, t db.PublishedReplyTarget, state PRState, comments []ThreadComment, reply AuthorReply, row db.PublishedReply, rep *ReplyReport) (ReplyOutcome, error) {
	outcome := ReplyOutcome{RepoOwner: t.RepoOwner, RepoName: t.RepoName, PRNumber: t.PRNumber, AuthorCommentID: reply.CommentID,
		Decision: row.Decision, Model: row.Model, DurationMS: row.DurationMS}
	finish := func(result string) (ReplyOutcome, error) {
		outcome.Outcome = result
		return outcome, r.Ledger.SetPublishedReplyOutcome(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, result)
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
	thread := threadUnder(comments, reply.RootCommentID)
	var root ThreadComment
	for _, c := range thread {
		if c.ID == reply.RootCommentID {
			root = c
		}
	}
	// A reply we posted but never recorded (crash between the two, or another
	// instance) is adopted. Only the root's author, that is our own bot, can
	// own such a marker.
	adoptPosted := func(in []ThreadComment) (bool, error) {
		for _, c := range in {
			if id, ok := replyMarkerID(c.Body); ok && id == reply.CommentID && c.AuthorID == root.AuthorID && c.ID != reply.CommentID {
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
		if _, err := r.Ledger.IncrementPublishedReplyAttempts(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID); err != nil {
			return outcome, err
		}
		decision, err := r.Responder(ctx, ReplyRequest{
			Owner: t.RepoOwner, Repo: t.RepoName, Number: t.PRNumber, HeadSHA: state.HeadSHA, BaseRef: state.BaseRef,
			Fingerprint: reply.Fingerprint, Root: root, Thread: thread, Reply: reply,
		})
		if err != nil {
			return outcome, err
		}
		cited, _ := json.Marshal(decision.Cited)
		if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, db.ReplyDecisionRecord{
			Decision: decision.Decision, ReplyBody: decision.Reply, Cited: string(cited), Model: decision.Model, DurationMS: decision.DurationMS,
			Head: state.HeadSHA, Thread: fingerprint,
		}); err != nil {
			return outcome, err
		}
		row.Decision, row.ReplyBody = decision.Decision, decision.Reply
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
		mode, allowed := r.Live()
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
	if fresh.HeadSHA != state.HeadSHA || !fresh.Open || fresh.Draft {
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
	if err := r.adopt(t, reply, ThreadComment{ID: id, CreatedAt: now}, &outcome); err != nil {
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
