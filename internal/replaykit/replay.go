// Package replaykit replays historical PRism publication rounds through the
// real publisher in-process: GitHub dumps and review sidecars in, a recording
// fake GitHub and an in-memory ledger out. cmd/publishreplay and
// cmd/commentbench share it.
package replaykit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/poller"
)

const (
	AliasLineTolerance = 10
	AliasSimilarity    = 0.20
)

// Post is one inline root comment the replayed publisher would have created.
type Post struct {
	Round     int
	SHA       string
	CommentID int64
	FindingID string
	File      string
	Line      int
	Body      string
	RawText   string
	Subjects  []string
}

// Reply is a text reply the reply reactor would have posted under a root.
type Reply struct {
	CommentID     int64
	RootCommentID int64
	Body          string
	At            time.Time
}

// Recorder is the fake GitHub: it hands out ids and remembers every write.
// Its own posts are fed back into the next round as the PR's review comments,
// exactly as the poller lists them from GitHub.
type Recorder struct {
	nextComment int64
	nextIssue   int64
	nextReview  int64
	round       int
	sha         string
	now         func() time.Time

	Posts      []Post
	Summaries  int
	Edits      int
	SummaryLog []string // every summary body created or edited, in order
	Replies    []Reply
	Reactions  []int64
	Resolved   []int64 // root comment ids of threads resolved
	Unresolved []int64
}

func NewRecorder() *Recorder {
	return &Recorder{nextComment: 1_000_000, nextIssue: 2_000_000, nextReview: 3_000_000}
}

// Begin tags the writes that follow with a round index and head.
func (r *Recorder) Begin(round int, sha string) { r.round, r.sha = round, sha }

// SetClock stamps replies with the replay's clock instead of the wall clock.
func (r *Recorder) SetClock(now func() time.Time) { r.now = now }

func (r *Recorder) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}

func (r *Recorder) CreateReview(_ context.Context, _, _ string, _ int, _, _ string, comments []publisher.ReviewCommentInput) (int64, []int64, error) {
	r.nextReview++
	ids := make([]int64, 0, len(comments))
	for _, c := range comments {
		r.nextComment++
		id, _ := publisher.FindingIDFromBody(c.Body)
		r.Posts = append(r.Posts, Post{Round: r.round, SHA: r.sha, CommentID: r.nextComment, FindingID: id, File: c.Path, Line: c.Line, Body: c.Body})
		ids = append(ids, r.nextComment)
	}
	return r.nextReview, ids, nil
}

func (r *Recorder) CreateIssueComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	r.nextIssue++
	r.Summaries++
	r.SummaryLog = append(r.SummaryLog, body)
	return r.nextIssue, nil
}

func (r *Recorder) EditIssueComment(_ context.Context, _, _ string, _ int64, body string) error {
	r.Edits++
	r.SummaryLog = append(r.SummaryLog, body)
	return nil
}

func (r *Recorder) ListIssueComments(context.Context, string, string, int) ([]publisher.IssueComment, error) {
	return nil, nil
}

func (r *Recorder) React(_ context.Context, _, _ string, commentID int64) error {
	r.Reactions = append(r.Reactions, commentID)
	return nil
}

func (r *Recorder) PostReply(_ context.Context, _, _ string, _ int, rootCommentID int64, body string) (int64, error) {
	r.nextComment++
	r.Replies = append(r.Replies, Reply{CommentID: r.nextComment, RootCommentID: rootCommentID, Body: body, At: r.clock()})
	return r.nextComment, nil
}

const threadNodePrefix = "T"

// ListReviewThreads opens one thread per posted root, as GitHub does.
func (r *Recorder) ListReviewThreads(context.Context, string, string, int) ([]publisher.ReviewThread, error) {
	out := make([]publisher.ReviewThread, 0, len(r.Posts))
	for _, p := range r.Posts {
		out = append(out, publisher.ReviewThread{NodeID: threadNodePrefix + strconv.FormatInt(p.CommentID, 10), RootCommentID: p.CommentID, Resolved: r.isResolved(p.CommentID)})
	}
	return out, nil
}

func (r *Recorder) isResolved(commentID int64) bool {
	resolved := false
	for _, id := range r.Resolved {
		if id == commentID {
			resolved = true
		}
	}
	for _, id := range r.Unresolved {
		if id == commentID {
			resolved = false
		}
	}
	return resolved
}

func rootOfThread(nodeID string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(nodeID, threadNodePrefix), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unknown thread %q", nodeID)
	}
	return id, nil
}

// ResolveThread and UnresolveThread record thread resolution by the root
// comment id behind the thread node.
func (r *Recorder) ResolveThread(_ context.Context, _, _ string, nodeID string) error {
	id, err := rootOfThread(nodeID)
	if err != nil {
		return err
	}
	if !r.isResolved(id) {
		r.Resolved = append(r.Resolved, id)
	}
	return nil
}

func (r *Recorder) UnresolveThread(_ context.Context, _, _ string, nodeID string) error {
	id, err := rootOfThread(nodeID)
	if err != nil {
		return err
	}
	r.Unresolved = append(r.Unresolved, id)
	return nil
}

// LastSummary is the most recent summary body, or "".
func (r *Recorder) LastSummary() string {
	if len(r.SummaryLog) == 0 {
		return ""
	}
	return r.SummaryLog[len(r.SummaryLog)-1]
}

// OwnComments lists every root and reply the replay posted, as the poller
// would list them from GitHub.
func (r *Recorder) OwnComments(bot string) []github.ReviewCommentInfo {
	out := make([]github.ReviewCommentInfo, 0, len(r.Posts)+len(r.Replies))
	for _, p := range r.Posts {
		out = append(out, github.ReviewCommentInfo{ID: p.CommentID, Author: bot, Body: p.Body, Path: p.File, Line: p.Line})
	}
	for _, rp := range r.Replies {
		out = append(out, github.ReviewCommentInfo{ID: rp.CommentID, Author: bot, Body: rp.Body, InReplyToID: rp.RootCommentID, CreatedAt: rp.At})
	}
	return out
}

// PublisherCanResolve reports whether the replay's recorder satisfies the
// publisher's optional thread resolution interface, so resolution metrics
// are measured.
func PublisherCanResolve() bool {
	_, ok := any(NewRecorder()).(publisher.ThreadResolver)
	return ok
}

// PublisherTakesChangedFiles reports whether publisher.Round accepts the
// inter-push change set SetChangedFiles fills.
func PublisherTakesChangedFiles() bool { return true }

// SetChangedFiles hands the files changed since the previous round to the
// publisher as the change set for every base the ledger asks about; an
// unknown compare leaves the round without one, so nothing is judged fixed.
func SetChangedFiles(r *publisher.Round, files []string, known bool) {
	if !known {
		return
	}
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[f] = true
	}
	r.Changes = func(string) (publisher.ChangeSet, bool) { return publisher.ChangeSet{Files: set}, true }
}

// RoundInput is everything one replayed round needs beyond the ledger.
type RoundInput struct {
	Index        int
	SHA          string
	At           time.Time
	Payload      payload.Payload
	Comments     []github.ReviewCommentInfo // other reviewers' comments present at At
	ChangedFiles []string
	ChangedKnown bool
}

// RoundResult is what one round did.
type RoundResult struct {
	Round    publisher.Round
	Report   publisher.Report
	Previous []db.PublishedFinding
	After    []db.PublishedFinding
	Posts    []Post // posted this round, annotated with raw text
	Summary  string // summary body after the round
}

// PRReplay replays one PR's rounds against a ledger and a recorder.
type PRReplay struct {
	Owner, Repo string
	Number      int
	Author      string
	Bot         string
	Ledger      *db.GormDB
	Rec         *Recorder
	Pub         *publisher.Publisher
}

func NewPRReplay(owner, repo string, number int, author string, ledger *db.GormDB, policy publisher.Policy) *PRReplay {
	rec := NewRecorder()
	return &PRReplay{Owner: owner, Repo: repo, Number: number, Author: author, Bot: "prism", Ledger: ledger, Rec: rec,
		Pub: &publisher.Publisher{GH: rec, Ledger: ledger, Policy: policy}}
}

// PublishRound builds the round the way the poller does and publishes it.
func (p *PRReplay) PublishRound(ctx context.Context, in RoundInput) (RoundResult, error) {
	var res RoundResult
	previous, err := p.Ledger.GetPublishedFindingsForPR(p.Owner, p.Repo, p.Number)
	if err != nil {
		return res, err
	}
	res.Previous = previous
	comments := append(append([]github.ReviewCommentInfo{}, in.Comments...), p.Rec.OwnComments(p.Bot)...)
	ghPR := github.PullRequest{Owner: p.Owner, Repo: p.Repo, Number: p.Number, CommitSHA: in.SHA, Author: p.Author}
	round := poller.BuildPublishRound(ghPR, in.Payload, comments, PatchesFromHunks(in.Payload), previous, "")
	SetChangedFiles(&round, in.ChangedFiles, in.ChangedKnown)
	at := in.At
	p.Pub.Now = func() time.Time { return at }
	p.Rec.Begin(in.Index, in.SHA)
	before := len(p.Rec.Posts)
	rep, err := p.Pub.Publish(ctx, round)
	if errors.Is(err, publisher.ErrHeadAlreadyPublished) {
		rep, err = publisher.Report{}, nil
	}
	if err != nil {
		return res, fmt.Errorf("round %d (%s): %w", in.Index+1, Short(in.SHA), err)
	}
	Annotate(p.Rec.Posts[before:], round.Findings)
	res.Round, res.Report = round, rep
	res.Posts = p.Rec.Posts[before:]
	res.Summary = p.Rec.LastSummary()
	if res.After, err = p.Ledger.GetPublishedFindingsForPR(p.Owner, p.Repo, p.Number); err != nil {
		return res, err
	}
	return res, nil
}

// ExternalCommentsBefore is what other reviewers had on the PR when the
// round ran, so cross-bot reconciliation sees what the poller saw.
func (d *PRDump) ExternalCommentsBefore(bots map[string]bool, at time.Time) []github.ReviewCommentInfo {
	var out []github.ReviewCommentInfo
	for _, t := range d.ReviewThreads.Nodes {
		line := t.Line
		if line == 0 {
			line = t.OriginalLine
		}
		for _, c := range t.Comments.Nodes {
			login := ActorLogin(c.Author)
			if bots[BareLogin(login)] || !c.CreatedAt.Before(at) {
				continue
			}
			info := github.ReviewCommentInfo{ID: int64(c.DatabaseID), Author: login, Body: c.Body, Path: t.Path, Line: line, CreatedAt: c.CreatedAt}
			if c.ReplyTo != nil {
				info.InReplyToID = int64(c.ReplyTo.DatabaseID)
			}
			out = append(out, info)
		}
	}
	return out
}

// PatchesFromHunks rebuilds enough of each file's patch from the hunks the
// sidecar stored with its findings for CommentableLines to accept the lines
// the review cited. A finding without a hunk gets its own line.
func PatchesFromHunks(pl payload.Payload) map[string]string {
	patches := map[string]string{}
	for _, f := range pl.Findings {
		if f.File == "" || f.Line <= 0 {
			continue
		}
		hunk := strings.TrimSpace(f.DiffHunk)
		if !strings.HasPrefix(hunk, "@@") {
			hunk = fmt.Sprintf("@@ -%d +%d @@\n ", f.Line, f.Line)
		}
		if patches[f.File] != "" {
			patches[f.File] += "\n"
		}
		patches[f.File] += hunk
	}
	return patches
}

// Annotate attaches the raw agent prose and subjects to each new post so the
// alias check compares raw text, not rendered bodies.
func Annotate(posts []Post, findings []payload.Finding) {
	byID := map[string]payload.Finding{}
	for _, f := range findings {
		byID[f.ID] = f
	}
	for i := range posts {
		f, ok := byID[posts[i].FindingID]
		if !ok {
			posts[i].RawText = StripMarkup(posts[i].Body)
			continue
		}
		posts[i].RawText = f.Comment
		posts[i].Subjects = Subjects(f)
	}
}

// Subjects are the lower-cased subject names of a finding's contract, sorted.
func Subjects(f payload.Finding) []string {
	if f.FindingContract == nil {
		return nil
	}
	var out []string
	for _, s := range f.FindingContract.Subjects {
		if name := strings.ToLower(strings.TrimSpace(s.Name)); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

type RepostKind int

const (
	RepostNone RepostKind = iota
	RepostSameMarker
	RepostSameDefect
)

func ClassifyRepost(p Post, earlier []Post) RepostKind {
	kind := RepostNone
	for _, e := range earlier {
		if p.FindingID != "" && e.FindingID == p.FindingID {
			return RepostSameMarker
		}
		if SameDefect(p.File, p.Line, p.RawText, p.Subjects, e.File, e.Line, e.RawText, e.Subjects) {
			kind = RepostSameDefect
		}
	}
	return kind
}

// SameDefect is the alias rule from the program spec: same file, both lines
// known and within ten, and either raw-text Jaccard at or above 0.20 or a
// shared subject.
func SameDefect(file string, line int, text string, subjects []string, oFile string, oLine int, oText string, oSubjects []string) bool {
	if !SameFile(file, oFile) {
		return false
	}
	if line <= 0 || oLine <= 0 || abs(line-oLine) > AliasLineTolerance {
		return false
	}
	if reconcile.Similarity(text, oText) >= AliasSimilarity {
		return true
	}
	for _, s := range subjects {
		for _, o := range oSubjects {
			if s == o {
				return true
			}
		}
	}
	return false
}

func SameFile(a, b string) bool {
	return a == b
}

func IsFindingRow(row db.PublishedFinding) bool {
	return row.Kind == db.PublishedKindFinding || row.Kind == db.PublishedKindAnnotation
}

// FileOfFingerprint reads the file out of file:bucket:hash.
func FileOfFingerprint(fp string) string {
	i := strings.LastIndex(fp, ":")
	if i < 0 {
		return fp
	}
	j := strings.LastIndex(fp[:i], ":")
	if j < 0 {
		return fp[:i]
	}
	return fp[:j]
}

func FileChanged(changed []string, file string) bool {
	for _, c := range changed {
		if c == file {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
