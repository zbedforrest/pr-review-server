package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher/replytext"
)

// ReplyClass is what an author's reply to one of our inline comments amounts
// to. Questions, pushback and fix claims (resolution) earn a text response;
// bare acknowledgements and everything classed other get a reaction only.
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
	// ThumbsDown is GitHub's reaction rollup for the comment; who reacted
	// takes a second call, made only when this is non-zero.
	ThumbsDown int
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

var (
	shortAckMaxWords = 6
	commitShaRe      = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	hexLetterRe      = regexp.MustCompile(`[a-f]`)
	nonWordRe        = regexp.MustCompile(`[^\pL\pN]`)
	// Emoji and shortcodes that read as agreement; a thumbs-down or a cross
	// is not an acknowledgment.
	ackEmojiRe    = regexp.MustCompile(`^(:(\+1|thumbsup|thumbs_up|white_check_mark|heavy_check_mark|ok_hand|pray|tada|heart|100):|[\x{1F44D}\x{2705}\x{2714}\x{1F44C}\x{1F64F}\x{1F389}\x{2764}\x{1F4AF}][\x{FE0F}\x{1F3FB}-\x{1F3FF}]*)$`)
	punctOnlyRe   = regexp.MustCompile(`^[[:punct:]]+$`)
	fixClaimRe    = regexp.MustCompile(`(?i)\b(fixed|done|addressed|resolved|updated|removed|handled)\b`)
	ackVocabulary = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields("done fixed fix fixing resolved addressed updated removed handled ok okay ack acked thanks thank thx ty you yes yep yup sure noted good catch got it will do sounds makes sense this that now all in the latest push and too as well") {
		ackVocabulary[w] = true
	}
}

// hasCommitSha finds an abbreviated or full sha; a run of digits alone (a
// build or ticket number) is not one.
func hasCommitSha(text string) bool {
	for _, m := range commitShaRe.FindAllString(strings.ToLower(text), -1) {
		if hexLetterRe.MatchString(m) {
			return true
		}
	}
	return false
}

// shortAcknowledgment reports a bare "done" / "fixed" / "ok" / thumbs-up: a
// few words drawn only from acknowledgement vocabulary (or emoji). Such a
// reply names nothing the model could verify, so it never earns text; a word
// outside the vocabulary ("fixed the race") is a claim and does.
func shortAcknowledgment(body string) bool {
	words := strings.Fields(strings.TrimSpace(body))
	if len(words) == 0 || len(words) >= shortAckMaxWords {
		return false
	}
	for _, w := range words {
		if ackEmojiRe.MatchString(w) || punctOnlyRe.MatchString(w) {
			continue
		}
		w = nonWordRe.ReplaceAllString(strings.ToLower(w), "")
		if !ackVocabulary[w] {
			return false
		}
	}
	return true
}

// acceptsWithoutFix reports a resolution-class reply that accepts the finding
// and defers it ("accepted for now, tracked in XO-291", "good catch, will do
// in a follow-up") rather than claiming a fix: no commit sha anywhere and
// every substantive sentence is deferral language. The code still has the
// problem by the author's own account, so running the model would only rebut
// an agreement. A sentence that neither defers nor merely acknowledges, or
// that carries a fix verb anywhere ("good catch, fixed the race, cleanup is
// tracked in ABC-1"), is a claim to verify. Code spans stay in as opaque
// words so "fixed `validateToken`" is not mistaken for a bare "fixed".
func acceptsWithoutFix(body string) bool {
	if hasCommitSha(body) {
		return false
	}
	deferred := false
	for _, s := range sentenceEndRe.Split(codeSpanRe.ReplaceAllString(body, " code "), -1) {
		s = strings.TrimSpace(s)
		if s == "" || shortAcknowledgment(s) {
			continue
		}
		if !defersFinding(s) || fixClaimRe.MatchString(s) {
			return false
		}
		deferred = true
	}
	return deferred
}

// wantsText reports whether a reply is one the reply model should answer:
// questions and pushback always, fix claims unless they are a bare
// acknowledgement or an acceptance that defers the fix, never anything
// classed other.
func wantsText(reply AuthorReply) bool {
	switch reply.Class {
	case ReplyQuestion, ReplyPushback:
		return true
	case ReplyResolution:
		return !shortAcknowledgment(reply.Body) && !acceptsWithoutFix(reply.Body)
	}
	return false
}

var (
	ticketKeyRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-\d+)\b`)
	// Uppercase-dash-number tokens that show up in review threads and are
	// not tracker keys.
	notTicketPrefixes = map[string]bool{"PR": true, "GH": true, "SHA": true, "MD": true, "UTF": true, "ISO": true, "RFC": true, "HTTP": true, "HTTPS": true, "CVE": true, "TLS": true, "IPV": true, "UUID": true, "AES": true, "GPT": true, "COVID": true, "EC": true, "EC2": true, "ES": true, "X": true}
	// A deferral is a construction, not a keyword: "later", "ticket" and
	// "defer" on their own are ordinary fix vocabulary ("released later in
	// the callback", "the ticket parser", "the defer block").
	deferralPhraseRe = regexp.MustCompile(`(?i)\b(follow[ -]?ups?|out of scope|known gaps?|for now|will (fix|do|handle|address|take care)|(separate|another|later|future|different|subsequent|next|new) (pr|mr|pull request|change|patch))\b`)
	// These verbs defer only when the same clause names the ticket they
	// defer to; "scoped to the request" and "tracked by the flag" do not.
	deferralVerbRe    = regexp.MustCompile(`(?i)\b(tracked|tracking|scoped|deferred|deferring|punted|filed|ticketed)\b`)
	negatedDeferralRe = regexp.MustCompile(`(?i)\b(not|never|no longer|isn'?t|aren'?t|wasn'?t|won'?t|don'?t|doesn'?t)\s+(\w+\s+){0,2}(deferr|track|scop|punt|filed|out of scope|follow[ -]?up|for now|known gap)`)
	codeSpanRe        = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
	outOfScopeRe      = regexp.MustCompile(`(?i)\bout of scope\b`)
	sentenceEndRe     = regexp.MustCompile(`[.;!?]+(\s+|$)|\n+`)
	clauseEndRe       = regexp.MustCompile(`,\s*`)
)

// defersFinding reports whether one sentence postpones the finding: an
// explicit deferral phrase, or a deferral verb in the same clause as a ticket
// key, and neither negated ("this is not deferred to ABC-1").
func defersFinding(sentence string) bool {
	if negatedDeferralRe.MatchString(sentence) {
		return false
	}
	if deferralPhraseRe.MatchString(sentence) {
		return true
	}
	for _, clause := range clauseEndRe.Split(sentence, -1) {
		if deferralVerbRe.MatchString(clause) && ticketKeyRe.MatchString(clause) {
			return true
		}
	}
	return false
}

// ticketKeys returns every tracker key ([A-Z][A-Z0-9]{1,9}-\d+) in body
// outside code spans, deduplicated in order of appearance. It feeds the
// deferral scan below, which supplies the context; a key the author talks
// about as a ticket in the reply text is replytext.TicketKeys' job.
func ticketKeys(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ticketKeyRe.FindAllStringSubmatch(codeSpanRe.ReplaceAllString(body, " "), -1) {
		key := m[1]
		prefix, _, _ := strings.Cut(key, "-")
		if notTicketPrefixes[prefix] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// DeferredTickets returns the tracker keys an author comment defers the
// finding to: a key in a deferring sentence (see defersFinding) whose own
// clause carries the deferral and no fix verb ("fixed the issue tracked in
// AUTH-42" is a reference, "fixed X, Y is tracked in AUTH-42" a deferral,
// "AUTH-42 introduced this, cleanup is a follow-up" names no ticket). A
// deferral verb takes only the keys after it, since English puts the ticket
// there ("tracked in X", "deferred to X"): "AUTH-42 introduced this and
// cleanup is tracked in MSG-1" defers to MSG-1 alone. A key on its own, or
// in another sentence, is not a deferral.
func DeferredTickets(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, sentence := range sentenceEndRe.Split(codeSpanRe.ReplaceAllString(body, " "), -1) {
		if !defersFinding(sentence) {
			continue
		}
		for _, clause := range clauseEndRe.Split(sentence, -1) {
			if fixClaimRe.MatchString(clause) {
				continue
			}
			if verb := deferralVerbRe.FindStringIndex(clause); verb != nil {
				clause = clause[verb[1]:]
			} else if !deferralPhraseRe.MatchString(clause) {
				continue
			}
			for _, k := range ticketKeys(clause) {
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
	}
	return out
}

// outOfScopeSentences returns the sentences of our reply that call the
// finding out of scope without negating it ("this is not out of scope"
// does not count).
func outOfScopeSentences(ourReply string) []string {
	var out []string
	for _, s := range sentenceEndRe.Split(ourReply, -1) {
		if outOfScopeRe.MatchString(s) && !negatedDeferralRe.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

// outOfScopeTickets is the deferral list for an author comment our reply
// called out of scope: what the comment itself deferred plus any key in the
// out-of-scope sentences of our reply that the author also named. A key the
// author merely mentioned ("AUTH-42 introduced this") is not taken on its
// own, and a key only the model wrote is not taken at all.
func outOfScopeTickets(recorded, authorBody string, ourSentences []string) string {
	named := map[string]bool{}
	for _, k := range ticketKeys(authorBody) {
		named[k] = true
	}
	var ours []string
	for _, s := range ourSentences {
		for _, k := range ticketKeys(s) {
			if named[k] {
				ours = append(ours, k)
			}
		}
	}
	return mergeTicketKeys(recorded, strings.Join(DeferredTickets(authorBody), ","), strings.Join(ours, ","))
}

// mergeTicketKeys joins comma-separated key lists without duplicates.
func mergeTicketKeys(lists ...string) string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, k := range strings.Split(l, ",") {
			k = strings.TrimSpace(k)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return strings.Join(out, ",")
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
// Withdraw is PRism retracting its own finding (misread code, a point already
// settled elsewhere); like a concession it dismisses the row, but the reply
// says so in as many words.
const (
	DecisionConcede  = "concede"
	DecisionHold     = "hold"
	DecisionAnswer   = "answer"
	DecisionAbstain  = "abstain"
	DecisionWithdraw = "withdraw"
)

// errRequeued reports a text step whose head moved after the model decided;
// the decision was cleared and the next scan decides again on the new head.
var errRequeued = errors.New("reply requeued against the new head")

// errClaimedElsewhere reports a text step another instance currently holds;
// the scan neither settles the PR nor reports an outcome for it.
var errClaimedElsewhere = errors.New("text step claimed by another instance")

// ErrBudgetExhausted is what a Responder wraps when the model ran out of
// turns or wall clock before deciding. The step then abstains with a reaction
// and marks the finding contested, so the claim is acknowledged, nothing
// unverified is posted, and the finding is not raised again on the PR. Under
// the legacy policy it posts a fixed notice instead.
var ErrBudgetExhausted = errors.New("reply model budget exhausted")

// NoteBudgetExhausted marks a ledger row the model never decided: an abstain
// under the current policy, the fixed notice under the legacy one.
const NoteBudgetExhausted = "budget_exhausted"

const budgetExhaustedReply = "PRism could not complete verification of this claim within its budget and is leaving the finding as written. Please have a reviewer confirm it."

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

// Reaction is one reaction on a review comment.
type Reaction struct {
	UserID  int64
	Content string
}

// ReactionLister is the optional part of ReplyGitHub that says who reacted
// to a comment; without it an author's thumbs-down is not read.
type ReactionLister interface {
	ListReactions(ctx context.Context, owner, repo string, commentID int64) ([]Reaction, error)
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
	ContestPublishedFinding(owner, repo string, number int, fingerprint string) (bool, error)
	GetPublishedFindingsForPR(owner, repo string, number int) ([]db.PublishedFinding, error)
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
	Body        string
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
	PRBody      string
	Root        ThreadComment   // our inline comment
	Thread      []ThreadComment // root and every reply under it, oldest first
	Reply       AuthorReply     // the author reply being answered
	Siblings    []SiblingThread // PRism's other threads on the PR, dismissed ones left out
	Other       []OtherFinding  // PRism's open findings on the PR that have no inline thread
}

// OtherFinding is an open ledger row the summary carries without an inline
// root, so the reply model still sees a point PRism raised elsewhere on the
// PR. File and Line come from the fingerprint, whose line is bucketed by ten.
type OtherFinding struct {
	Fingerprint string
	File        string
	Line        int
	Severity    string
	State       string
}

// SiblingThread is another PRism finding on the same PR with whatever was
// said under it, so the reply model sees the review as a whole (a gate it
// already flagged two files over, an author verdict given on a twin finding).
type SiblingThread struct {
	Fingerprint string
	State       string // ledger state when the ledger knows the row
	Root        ThreadComment
	Replies     []ThreadComment
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

// renderDecision applies the posting conventions to the model's reply before
// it is recorded, so the ledger holds what will be posted, and returns the
// rune count of the sentences it appended. A body with nothing postable left
// turns the decision into an abstain, and so does a hold that cites no
// file:line (unless legacy): a rebuttal the reader cannot open is not posted.
// Legacy has no withdraw decision, so one is posted as the concession it
// would have been.
func renderDecision(d ReplyDecision, reply AuthorReply, root ThreadComment, prBody string, legacy bool) (ReplyDecision, int) {
	abstain := func() ReplyDecision {
		return ReplyDecision{Decision: DecisionAbstain, Cited: d.Cited, React: true, Model: d.Model, DurationMS: d.DurationMS}
	}
	if d.Decision == DecisionAbstain {
		d.Reply = ""
		return d, 0
	}
	if !legacy && d.Decision == DecisionHold && len(d.Cited) == 0 {
		return abstain(), 0
	}
	if legacy && d.Decision == DecisionWithdraw {
		d.Decision = DecisionConcede
	}
	ctx := replytext.Context{AuthorComment: reply.Body, FindingBody: root.Body, PRBody: prBody, Decision: d.Decision}
	paragraph, appendix, ok := replytext.RenderParts(d.Reply, ctx)
	if !ok {
		return abstain(), 0
	}
	d.Reply = paragraph + appendix
	return d, len([]rune(appendix))
}

// Responder runs the reply model.
type Responder func(ctx context.Context, req ReplyRequest) (ReplyDecision, error)

// TextPolicy bounds text replies: questions, substantive pushback and fix
// claims only, while the thread is fresh, at most MaxPerThread per thread,
// MaxPerPRPerDay per PR, and MaxChars per reply paragraph (a longer reply is
// dropped, not truncated; the sentences the renderer itself appends are
// allowed on top).
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
// reason it may not: class, acknowledgment, deferred, stale, thread_cap,
// conceded, pr_cap. The caps are
// exact within one process (text steps on a PR are serialized) and best-effort
// across two instances that both believe they lead, where sibling replies
// claimed separately can overshoot a cap by one.
func TextEligibility(reply AuthorReply, prior []db.PublishedReply, prTextToday int, now time.Time, p TextPolicy) string {
	if !wantsText(reply) {
		switch {
		case reply.Class != ReplyResolution:
			return "class"
		case shortAcknowledgment(reply.Body):
			return "acknowledgment"
		}
		return "deferred"
	}
	if now.Sub(reply.CreatedAt) > p.MaxAge {
		return "stale"
	}
	posted := 0
	for _, r := range prior {
		if r.ReplyCommentID != 0 {
			posted++
			if r.Decision == DecisionConcede || r.Decision == DecisionWithdraw {
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

	// ResolveThreads resolves the GitHub thread of a finding a posted
	// concession dismisses (PUBLISH_THREAD_RESOLUTION); it needs a GH that
	// implements ThreadResolver.
	ResolveThreads bool

	// Legacy restores the reply policy before contested findings: budget
	// exhaustion posts the fixed notice, a hold needs no citation here, a
	// decision made against an older head is dropped instead of retaken,
	// neither a hold nor an author's thumbs-down marks the finding contested,
	// the model gets neither the PR body nor the other threads, a withdraw is
	// posted as a concession, no thread is resolved and the accepted-risk ask
	// ignores the PR body. The prompt text itself is not switched.
	Legacy bool
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
	Requeued    int // decisions retaken because the head moved, inline text steps only
	Contested   int // findings marked contested by a thumbs-down this scan
	TextSkipped map[string]int
	// ThreadsResolved counts the threads closed on a posted concession,
	// ThreadResolveFailures the ones GitHub rejected or did not list.
	ThreadsResolved       int
	ThreadResolveFailures int
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
	Note            string // NoteBudgetExhausted for the budget notice, else the renderer's note
	Thread          string // ThreadResolved or ThreadLost after a concession, else empty
	Model           string
	DurationMS      int64
	Requeued        bool // a decision made against an older head was retaken
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
	reactionsRead, err := r.contestThumbsDown(ctx, t, state, comments, rep)
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
	settled := reactionsRead
	for _, reply := range FindAuthorReplies(comments, t.Roots, state.AuthorID, r.Since) {
		rep.RepliesSeen++
		row, handled := seen[reply.CommentID]
		if !handled {
			action := ReplyActionObserved
			switch {
			case r.reacts() && r.textMode() && wantsText(reply):
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
				DeferredTo: strings.Join(DeferredTickets(reply.Body), ","),
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
					if errors.Is(err, errRequeued) {
						err = nil
					}
					if r.OnOutcome != nil && !errors.Is(err, errClaimedElsewhere) {
						r.OnOutcome(outcome, err)
					}
				})
			}
			continue
		}
		outcome, err := r.text(ctx, t, state, comments, reply, row, rep)
		if errors.Is(err, errRequeued) {
			err = nil
		}
		if errors.Is(err, errClaimedElsewhere) {
			settled = false
			continue
		}
		if r.OnOutcome != nil {
			r.OnOutcome(outcome, err)
		}
		if outcome.Outcome == "" && err == nil {
			settled = false
			continue
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

// contestThumbsDown marks a finding contested when the PR author put a
// thumbs-down on its root comment. The rollup on the listed comment says
// whether anyone did; who did takes one more call per such root. Reactions do
// not move a PR's updated_at, so one left on a quiet PR is seen on the next
// full scan. Shadow mode writes nothing to a finding's state, like its
// budget path. complete is false when a reactions read failed, so the
// caller keeps the PR's watermark where it is and retries next scan.
func (r ReplyReactor) contestThumbsDown(ctx context.Context, t db.PublishedReplyTarget, state PRState, comments []ThreadComment, rep *ReplyReport) (complete bool, err error) {
	lister, ok := r.GH.(ReactionLister)
	if !ok || r.Legacy || (r.Mode != ReplyModeReact && r.Mode != ReplyModeRespond) {
		return true, nil
	}
	complete = true
	var states map[string]string
	for _, c := range comments {
		fp, isRoot := t.Roots[c.ID]
		if !isRoot || c.ThumbsDown == 0 {
			continue
		}
		if states == nil {
			if states, err = r.findingStates(t); err != nil {
				return false, err
			}
		}
		if st := states[fp]; st != "" && st != db.PublishedStateOpen && st != db.PublishedStateResolved {
			continue
		}
		reactions, err := lister.ListReactions(ctx, t.RepoOwner, t.RepoName, c.ID)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s#%d: reactions on %d: %v", t.RepoOwner, t.RepoName, t.PRNumber, c.ID, err))
			complete = false
			continue
		}
		for _, re := range reactions {
			if re.UserID != state.AuthorID || re.Content != "-1" {
				continue
			}
			changed, err := r.Ledger.ContestPublishedFinding(t.RepoOwner, t.RepoName, t.PRNumber, fp)
			if err != nil {
				return false, err
			}
			if changed {
				rep.Contested++
			}
			break
		}
	}
	return complete, nil
}

func (r ReplyReactor) findingStates(t db.PublishedReplyTarget) (map[string]string, error) {
	rows, err := r.Ledger.GetPublishedFindingsForPR(t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(rows))
	for _, row := range rows {
		states[row.Fingerprint] = row.State
	}
	return states, nil
}

// prContext collects the rest of PRism's review on the PR for the reply
// model: the other inline threads, oldest root first, leaving out findings the
// ledger already dismissed, and the open findings that only the summary
// carries. Legacy hands the model nothing beyond the thread. A ledger read
// error fails the step so it is resumed with the full context next scan.
func (r ReplyReactor) prContext(t db.PublishedReplyTarget, comments []ThreadComment, rootID int64) ([]SiblingThread, []OtherFinding, error) {
	if r.Legacy {
		return nil, nil, nil
	}
	rows, err := r.Ledger.GetPublishedFindingsForPR(t.RepoOwner, t.RepoName, t.PRNumber)
	if err != nil {
		return nil, nil, err
	}
	states := map[string]string{}
	var other []OtherFinding
	for _, row := range rows {
		states[row.Fingerprint] = row.State
		summaryOnly := row.Kind == db.PublishedKindAnnotation || (row.Kind == db.PublishedKindFinding && row.CommentID == 0)
		if summaryOnly && row.State == db.PublishedStateOpen {
			file, line := fingerprintAnchor(row.Fingerprint)
			other = append(other, OtherFinding{Fingerprint: row.Fingerprint, File: file, Line: line, Severity: row.Severity, State: row.State})
		}
	}
	var out []SiblingThread
	for id, fp := range t.Roots {
		if id == rootID || states[fp] == db.PublishedStateDismissed {
			continue
		}
		thread := threadUnder(comments, id)
		if len(thread) == 0 || thread[0].ID != id {
			continue
		}
		out = append(out, SiblingThread{Fingerprint: fp, State: states[fp], Root: thread[0], Replies: thread[1:]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root.ID < out[j].Root.ID })
	sort.Slice(other, func(i, j int) bool { return other[i].Fingerprint < other[j].Fingerprint })
	return out, other, nil
}

// fingerprintAnchor reads the file and the first line of the ten-line bucket
// out of a finding fingerprint (<file>:<line/10>:<hash>).
func fingerprintAnchor(fp string) (string, int) {
	i := strings.LastIndexByte(fp, ':')
	if i < 0 {
		return fp, 0
	}
	j := strings.LastIndexByte(fp[:i], ':')
	if j < 0 {
		return fp[:i], 0
	}
	bucket, err := strconv.Atoi(fp[j+1 : i])
	if err != nil {
		return fp[:j], 0
	}
	return fp[:j], bucket * 10
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
		Decision: row.Decision, Note: row.Note, Model: row.Model, DurationMS: row.DurationMS}
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
			outcome.Decision, outcome.Note, outcome.Model, outcome.DurationMS = row.Decision, row.Note, row.Model, row.DurationMS
		}
	}
	// A reply we posted but never recorded (crash between the two, or another
	// instance) is adopted before anything else: a posted reply exists whether
	// or not the author comment would still be eligible today. Only the
	// root's author, that is our own bot, can own such a marker.
	adoptPosted := func(in []ThreadComment) (bool, error) {
		for _, c := range in {
			if id, ok := replyMarkerID(c.Body); ok && id == reply.CommentID && c.InReplyToID == reply.RootCommentID && c.AuthorID == root.AuthorID && c.ID != reply.CommentID {
				return true, r.adopt(ctx, t, reply, c, &outcome, rep)
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
	// A decision stored against a thread that has since changed is never
	// posted. One stored against an older head is retaken on the new head
	// (the author may have pushed the fix they described), within the
	// attempt budget; legacy drops it.
	clearDecision := func() error {
		if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, db.ReplyDecisionRecord{DeferredTo: row.DeferredTo}); err != nil {
			return err
		}
		row.Decision, row.ReplyBody, row.Cited, row.Model, row.DurationMS = "", "", "", "", 0
		row.DecisionHead, row.DecisionThread, row.DecisionReact, row.Note = "", "", false, ""
		outcome.Decision, outcome.Note, outcome.Model, outcome.DurationMS = "", "", "", 0
		return nil
	}
	if row.Decision != "" && row.DecisionThread != fingerprint {
		return skip("thread_moved")
	}
	if row.Decision != "" && row.DecisionHead != state.HeadSHA {
		if r.Legacy || row.Attempts >= r.textPolicy().MaxAttempts {
			return skip("head_moved")
		}
		if err := clearDecision(); err != nil {
			return outcome, err
		}
		outcome.Requeued = true
		if rep != nil {
			rep.Requeued++
		}
	}
	prBody := state.Body
	if r.Legacy {
		prBody = ""
	}
	decidedNow, appendixRunes := false, 0
	if row.Decision == "" {
		if row.Attempts >= r.textPolicy().MaxAttempts {
			return finish("failed")
		}
		siblings, other, err := r.prContext(t, comments, reply.RootCommentID)
		if err != nil {
			return outcome, err
		}
		decision, err := r.Responder(ctx, ReplyRequest{
			Owner: t.RepoOwner, Repo: t.RepoName, Number: t.PRNumber, HeadSHA: state.HeadSHA, BaseRef: state.BaseRef,
			Fingerprint: reply.Fingerprint, PRBody: prBody, Root: root, Thread: thread, Reply: reply,
			Siblings: siblings, Other: other,
		})
		// A run that was cut off by shutdown is not the model failing; the
		// claim lease keeps a crash loop to one run per lease anyway.
		if ctx.Err() == nil {
			n, ierr := r.Ledger.IncrementPublishedReplyAttempts(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID)
			if ierr != nil {
				return outcome, ierr
			}
			row.Attempts = n
		}
		record := db.ReplyDecisionRecord{Head: state.HeadSHA, Thread: fingerprint, DeferredTo: row.DeferredTo}
		switch {
		case errors.Is(err, ErrBudgetExhausted) && ctx.Err() == nil && (reply.Class == ReplyPushback || (r.Legacy && reply.Class == ReplyResolution)):
			// The model never decided, so nothing it might have said can be
			// retried. The author's pushback is acknowledged with the reaction
			// and the finding is left to humans as contested; legacy posts a
			// fixed hold instead, on fix claims too. A question has no claim to
			// hold against and a fix claim is worth another look, so both keep
			// the retry path.
			budget := ReplyDecision{Decision: DecisionAbstain, React: true, Model: decision.Model, DurationMS: decision.DurationMS}
			if r.Legacy {
				budget = ReplyDecision{Decision: DecisionHold, Reply: budgetExhaustedReply, Cited: []EvidenceRef{}, Model: decision.Model, DurationMS: decision.DurationMS}
			}
			decision = budget
			record.Note = NoteBudgetExhausted
		case err != nil:
			return outcome, err
		}
		decision, appendixRunes = renderDecision(decision, reply, root, prBody, r.Legacy)
		cited, _ := json.Marshal(decision.Cited)
		record.Decision, record.ReplyBody, record.Cited, record.Model, record.DurationMS, record.React = decision.Decision, decision.Reply, string(cited), decision.Model, decision.DurationMS, decision.React
		if ours := outOfScopeSentences(decision.Reply); record.Note == "" && len(ours) > 0 && (decision.Decision == DecisionHold || decision.Decision == DecisionConcede) {
			record.DeferredTo = outOfScopeTickets(row.DeferredTo, reply.Body, ours)
		}
		if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, record); err != nil {
			return outcome, err
		}
		// The in-memory row mirrors the record in full, so a later write-back
		// of this row cannot blank the head, thread or citations just stored.
		row.Decision, row.ReplyBody, row.Cited, row.Model, row.DurationMS = record.Decision, record.ReplyBody, record.Cited, record.Model, record.DurationMS
		row.DecisionHead, row.DecisionThread, row.DecisionReact, row.Note, row.DeferredTo = record.Head, record.Thread, record.React, record.Note, record.DeferredTo
		outcome.Decision, outcome.Note, outcome.Model, outcome.DurationMS = record.Decision, record.Note, record.Model, record.DurationMS
		decidedNow = true
	}
	// A row decided before this convention took effect is rendered here and
	// written back so the ledger holds what is posted; rendering is a no-op
	// on an already rendered body. The note is derived here so a resumed step
	// reports it too. A budget notice stored under the legacy policy and not
	// yet posted becomes the abstain the current policy would have recorded.
	decided := row.Decision
	if !r.Legacy && row.Note == NoteBudgetExhausted && row.Decision == DecisionHold {
		row.Decision, row.DecisionReact, row.ReplyBody = DecisionAbstain, true, ""
	}
	rctx := replytext.Context{AuthorComment: reply.Body, FindingBody: root.Body, PRBody: prBody, Decision: row.Decision}
	paragraph, appendix, renderable := replytext.RenderParts(row.ReplyBody, rctx)
	text := paragraph + appendix
	if !renderable && row.Decision != DecisionAbstain {
		row.Decision, row.DecisionReact, text = DecisionAbstain, true, ""
	}
	rctx.Decision = row.Decision
	// A row rendered by an earlier step does not say what that step
	// appended, so it is capped whole rather than crediting a sentence the
	// model may have written itself.
	if !decidedNow && text != row.ReplyBody {
		appendixRunes = len([]rune(appendix))
	}
	if text != row.ReplyBody || row.Decision != decided {
		if err := r.Ledger.SetPublishedReplyDecision(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, db.ReplyDecisionRecord{
			Decision: row.Decision, ReplyBody: text, Cited: row.Cited, Model: row.Model, DurationMS: row.DurationMS,
			Head: row.DecisionHead, Thread: row.DecisionThread, React: row.DecisionReact, Note: row.Note, DeferredTo: row.DeferredTo,
		}); err != nil {
			return outcome, err
		}
		row.ReplyBody, outcome.Decision = text, row.Decision
	}
	if outcome.Note == "" {
		outcome.Note = replytext.Note(rctx)
	}
	switch {
	case row.Decision == DecisionAbstain || !renderable:
		if rep != nil {
			rep.Abstained++
		}
		if row.Note == NoteBudgetExhausted && !r.Legacy && r.Mode == ReplyModeRespond {
			if _, err := r.Ledger.ContestPublishedFinding(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint); err != nil {
				return outcome, err
			}
		}
		return finish("abstained")
	case len([]rune(text))-appendixRunes > r.textPolicy().MaxChars:
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
	case fresh.HeadSHA != state.HeadSHA && (r.Legacy || row.Attempts >= r.textPolicy().MaxAttempts):
		return skip("head_moved")
	case fresh.HeadSHA != state.HeadSHA:
		// The claim is released by the deferred cleanup since no outcome is
		// recorded; the next scan decides again on the head the author sees.
		if err := clearDecision(); err != nil {
			return outcome, err
		}
		outcome.Requeued = true
		if rep != nil {
			rep.Requeued++
		}
		return outcome, errRequeued
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
	if err := r.adopt(ctx, t, reply, ThreadComment{ID: id, CreatedAt: r.now()}, &outcome, rep); err != nil {
		return outcome, err
	}
	if rep != nil {
		rep.Responded++
	}
	return finish("posted")
}

// adopt records a posted reply and applies the decision's side effect on the
// finding. A concession or withdrawal dismisses the row and resolves the
// thread: a conceded fix claim is dismissed like any other concession, since
// fingerprints hash the finding's wording rather than the code and a resolved
// row would come back as a fresh comment the next time the reviewer phrases
// it the same way. The trade-off, accepted for every concession, is that a
// later regression with the same wording stays suppressed on this PR. A
// posted hold marks the row contested: the thread stays open for humans and
// the finding is not raised again under another wording.
func (r ReplyReactor) adopt(ctx context.Context, t db.PublishedReplyTarget, reply AuthorReply, posted ThreadComment, outcome *ReplyOutcome, rep *ReplyReport) error {
	if err := r.Ledger.MarkPublishedReplyPosted(t.RepoOwner, t.RepoName, t.PRNumber, reply.CommentID, posted.ID, posted.CreatedAt); err != nil {
		return err
	}
	outcome.Posted = true
	switch outcome.Decision {
	case DecisionConcede, DecisionWithdraw:
		if err := r.Ledger.SetPublishedFindingState(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint, db.PublishedStateDismissed); err != nil {
			return err
		}
		if rep == nil {
			rep = &ReplyReport{}
		}
		outcome.Thread = r.resolveThread(ctx, t, reply.RootCommentID, rep)
	case DecisionHold:
		if r.Legacy {
			return nil
		}
		_, err := r.Ledger.ContestPublishedFinding(t.RepoOwner, t.RepoName, t.PRNumber, reply.Fingerprint)
		return err
	}
	return nil
}

// Thread outcomes of a concession, carried on ReplyOutcome so the background
// path, which has no per-scan report, still records them.
const (
	ThreadResolved = "resolved"
	ThreadLost     = "lost"
)

// resolveThread closes the GitHub thread under a root comment once the
// finding is dismissed and counts the outcome on the report. The ledger state
// is already written, so a failure never fails the step.
func (r ReplyReactor) resolveThread(ctx context.Context, t db.PublishedReplyTarget, rootCommentID int64, rep *ReplyReport) string {
	resolver, ok := r.GH.(ThreadResolver)
	if !ok || !r.ResolveThreads || rootCommentID == 0 {
		return ""
	}
	ti := &threadIndex{gh: resolver, owner: t.RepoOwner, repo: t.RepoName, number: t.PRNumber}
	nodeID := ti.nodeIDOf(ctx, rootCommentID)
	if nodeID == "" {
		rep.ThreadResolveFailures++
		log.Printf("[REPLY %s/%s#%d] no thread listed for comment %d; left open", t.RepoOwner, t.RepoName, t.PRNumber, rootCommentID)
		return ThreadLost
	}
	if err := resolver.ResolveThread(ctx, t.RepoOwner, t.RepoName, nodeID); err != nil {
		rep.ThreadResolveFailures++
		log.Printf("[REPLY %s/%s#%d] resolve thread %s lost: %v", t.RepoOwner, t.RepoName, t.PRNumber, nodeID, err)
		return ThreadLost
	}
	rep.ThreadsResolved++
	return ThreadResolved
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
