package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
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
	aliasLineTolerance = 10
	aliasSimilarity    = 0.20
)

// post is one inline root comment the replayed publisher would have created.
type post struct {
	Round     int
	SHA       string
	CommentID int64
	FindingID string
	File      string
	Line      int
	Body      string
	RawText   string
	Kind      string
	Subjects  []string
	// SubjectKinds maps each subject name to its contract kind (symbol,
	// selector, file, ...).
	SubjectKinds map[string]string
}

// recorder is the fake GitHub: it hands out ids and remembers every write.
// Its own posts are fed back into the next round as the PR's review comments,
// exactly as the poller lists them from GitHub, and every root opens a
// thread the publisher may resolve.
type recorder struct {
	nextComment int64
	nextIssue   int64
	nextReview  int64
	round       int
	sha         string
	posts       []post
	summaries   int
	edits       int
	replies     int
	resolved    map[int64]bool
	resolves    int
	unresolves  int
}

func newRecorder() *recorder {
	return &recorder{nextComment: 1_000_000, nextIssue: 2_000_000, nextReview: 3_000_000, resolved: map[int64]bool{}}
}

const threadNodePrefix = "T"

func (r *recorder) ListReviewThreads(context.Context, string, string, int) ([]publisher.ReviewThread, error) {
	out := make([]publisher.ReviewThread, 0, len(r.posts))
	for _, p := range r.posts {
		out = append(out, publisher.ReviewThread{NodeID: threadNodePrefix + strconv.FormatInt(p.CommentID, 10), RootCommentID: p.CommentID, Resolved: r.resolved[p.CommentID]})
	}
	return out, nil
}

func (r *recorder) ResolveThread(_ context.Context, _, _, nodeID string) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(nodeID, threadNodePrefix), 10, 64)
	if err != nil {
		return fmt.Errorf("unknown thread %q", nodeID)
	}
	if !r.resolved[id] {
		r.resolves++
	}
	r.resolved[id] = true
	return nil
}

func (r *recorder) UnresolveThread(_ context.Context, _, _, nodeID string) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(nodeID, threadNodePrefix), 10, 64)
	if err != nil {
		return fmt.Errorf("unknown thread %q", nodeID)
	}
	if r.resolved[id] {
		r.unresolves++
	}
	delete(r.resolved, id)
	return nil
}

// rootsResolved counts the roots whose thread is resolved now.
func (r *recorder) rootsResolved() int {
	n := 0
	for _, p := range r.posts {
		if r.resolved[p.CommentID] {
			n++
		}
	}
	return n
}

func (r *recorder) begin(round int, sha string) { r.round, r.sha = round, sha }

func (r *recorder) CreateReview(_ context.Context, _, _ string, _ int, _, _ string, comments []publisher.ReviewCommentInput) (int64, []int64, error) {
	r.nextReview++
	ids := make([]int64, 0, len(comments))
	for _, c := range comments {
		r.nextComment++
		id, _ := publisher.FindingIDFromBody(c.Body)
		r.posts = append(r.posts, post{Round: r.round, SHA: r.sha, CommentID: r.nextComment, FindingID: id, File: c.Path, Line: c.Line, Body: c.Body})
		ids = append(ids, r.nextComment)
	}
	return r.nextReview, ids, nil
}

func (r *recorder) CreateIssueComment(context.Context, string, string, int, string) (int64, error) {
	r.nextIssue++
	r.summaries++
	return r.nextIssue, nil
}

func (r *recorder) EditIssueComment(context.Context, string, string, int64, string) error {
	r.edits++
	return nil
}

func (r *recorder) ListIssueComments(context.Context, string, string, int) ([]publisher.IssueComment, error) {
	return nil, nil
}

func (r *recorder) PostReply(context.Context, string, string, int, int64, string) (int64, error) {
	r.nextComment++
	r.replies++
	return r.nextComment, nil
}

func (r *recorder) ownComments() []github.ReviewCommentInfo {
	out := make([]github.ReviewCommentInfo, 0, len(r.posts))
	for _, p := range r.posts {
		out = append(out, github.ReviewCommentInfo{ID: p.CommentID, Author: "prism", Body: p.Body, Path: p.File, Line: p.Line})
	}
	return out
}

// PRResult is one CSV row.
type PRResult struct {
	PR                        string
	Rounds                    int
	RoundsMissingSidecar      int
	SameCommitRounds          int
	RootsPosted               int
	SameMarkerReposts         int
	SameDefectReposts         int
	SameRoundDuplicates       int
	Fixed                     int
	FixedWithoutFileChange    int
	FixedFileChangeUnknown    int
	SameCommitResolves        int
	InThreadReplies           int
	ThreadsResolved           int
	ThreadsUnresolved         int
	RootsResolved             int
	ObservedRoots             int
	ObservedSameMarkerReposts int
	ObservedSameDefectReposts int
}

// Metrics is the JSON the command prints. Observed values come straight from
// the dumps; everything else is what the publisher under test would do.
type Metrics struct {
	PRs                  int `json:"prs"`
	PRsWithRounds        int `json:"prs_with_rounds"`
	Rounds               int `json:"rounds"`
	RoundsReplayed       int `json:"rounds_replayed"`
	RoundsMissingSidecar int `json:"rounds_missing_sidecar"`
	SameCommitRounds     int `json:"same_commit_rounds"`

	RootsPosted            int     `json:"roots_posted"`
	SameMarkerReposts      int     `json:"same_marker_reposts"`
	SameDefectReposts      int     `json:"same_defect_reposts"`
	SameDefectRepostsPerPR float64 `json:"same_defect_reposts_per_pr"`
	SameRoundDuplicates    int     `json:"same_round_duplicates"`
	PRsWithReposts         int     `json:"prs_with_reposts"`

	Fixed                  int `json:"fixed"`
	FixedWithoutFileChange int `json:"fixed_without_file_change"`
	FixedFileChangeUnknown int `json:"fixed_file_change_unknown"`
	SameCommitResolves     int `json:"same_commit_resolves"`
	InThreadReplies        int `json:"in_thread_replies"`
	ThreadsResolved        int `json:"threads_resolved"`
	ThreadsUnresolved      int `json:"threads_unresolved"`

	CommentsPerPushP50 float64 `json:"comments_per_push_p50"`
	RoundsPerPRP50     float64 `json:"rounds_per_pr_p50"`
	// PrismResolvedShare is the share of replayed roots whose thread the
	// publisher itself left resolved at the end of the PR's rounds; the
	// observed resolved_share is the human counterpart over the same roots.
	PrismResolvedShare float64 `json:"prism_resolved_share"`
	PrismResolvedNote  string  `json:"prism_resolved_note"`

	Observed ObservedMetrics `json:"observed"`
}

type ObservedMetrics struct {
	Roots             int     `json:"roots"`
	SameMarkerReposts int     `json:"same_marker_reposts"`
	SameDefectReposts int     `json:"same_defect_reposts"`
	ThreadsResolved   int     `json:"threads_resolved"`
	ResolvedShare     float64 `json:"resolved_share"`
}

type Options struct {
	Dumps   []*prDump
	Store   *store
	Policy  publisher.Policy
	BotName string
	Logf    func(string, ...any)
	// TextOnlyAlias counts same-defect reposts by the text clause alone, to
	// show how much the subject clause contributes.
	TextOnlyAlias bool
}

type Result struct {
	Metrics Metrics
	PerPR   []PRResult
}

func (o Options) bots(d *prDump) map[string]bool {
	if o.BotName != "" {
		return map[string]bool{bareLogin(o.BotName): true}
	}
	return d.botLogins()
}

// Run replays every PR's rounds through the publisher against a fresh
// in-memory ledger and aggregates the metrics.
func Run(ctx context.Context, o Options) (Result, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Store == nil {
		return Result{}, errors.New("replay: Options.Store is nil")
	}
	ledger, err := db.NewGormSQLite(":memory:")
	if err != nil {
		return Result{}, fmt.Errorf("open ledger: %w", err)
	}
	var res Result
	var roundsPerPR, commentsPerPush []int
	rootsResolved := 0
	res.Metrics.PRs = len(o.Dumps)
	for _, d := range o.Dumps {
		bots := o.bots(d)
		pr, pushes, err := replayPR(ctx, d, bots, o.Store, ledger, o.Policy, o.TextOnlyAlias, logf)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", d.key(), err)
		}
		observe(d, bots, &pr, &res.Metrics.Observed)
		res.PerPR = append(res.PerPR, pr)
		if pr.Rounds > 0 {
			res.Metrics.PRsWithRounds++
			roundsPerPR = append(roundsPerPR, pr.Rounds)
		}
		commentsPerPush = append(commentsPerPush, pushes...)
		m := &res.Metrics
		m.Rounds += pr.Rounds
		m.RoundsReplayed += pr.Rounds - pr.RoundsMissingSidecar
		m.RoundsMissingSidecar += pr.RoundsMissingSidecar
		m.SameCommitRounds += pr.SameCommitRounds
		m.RootsPosted += pr.RootsPosted
		m.SameMarkerReposts += pr.SameMarkerReposts
		m.SameDefectReposts += pr.SameDefectReposts
		m.SameRoundDuplicates += pr.SameRoundDuplicates
		if pr.SameMarkerReposts+pr.SameDefectReposts > 0 {
			m.PRsWithReposts++
		}
		m.Fixed += pr.Fixed
		m.FixedWithoutFileChange += pr.FixedWithoutFileChange
		m.FixedFileChangeUnknown += pr.FixedFileChangeUnknown
		m.SameCommitResolves += pr.SameCommitResolves
		m.InThreadReplies += pr.InThreadReplies
		m.ThreadsResolved += pr.ThreadsResolved
		m.ThreadsUnresolved += pr.ThreadsUnresolved
		rootsResolved += pr.RootsResolved
		if pr.Rounds > 0 {
			logf("%s: rounds=%d roots=%d same_marker=%d same_defect=%d fixed=%d fixed_no_change=%d", d.key(), pr.Rounds, pr.RootsPosted, pr.SameMarkerReposts, pr.SameDefectReposts, pr.Fixed, pr.FixedWithoutFileChange)
		}
	}
	m := &res.Metrics
	if m.PRsWithRounds > 0 {
		m.SameDefectRepostsPerPR = round3(float64(m.SameDefectReposts) / float64(m.PRsWithRounds))
	}
	m.CommentsPerPushP50 = p50(commentsPerPush)
	m.RoundsPerPRP50 = p50(roundsPerPR)
	m.PrismResolvedNote = "roots whose thread the replayed publisher left resolved, over roots posted"
	if m.RootsPosted > 0 {
		m.PrismResolvedShare = round3(float64(rootsResolved) / float64(m.RootsPosted))
	}
	if m.Observed.Roots > 0 {
		m.Observed.ResolvedShare = round3(float64(m.Observed.ThreadsResolved) / float64(m.Observed.Roots))
	}
	return res, nil
}

func replayPR(ctx context.Context, d *prDump, bots map[string]bool, s *store, ledger *db.GormDB, policy publisher.Policy, textOnly bool, logf func(string, ...any)) (PRResult, []int, error) {
	pr := PRResult{PR: d.key()}
	rec := newRecorder()
	pub := &publisher.Publisher{GH: rec, Ledger: ledger, Policy: policy}
	rounds := d.rounds(bots)
	pr.Rounds = len(rounds)
	var pushes []int
	for i, rd := range rounds {
		if i > 0 && rounds[i-1].SHA == rd.SHA {
			pr.SameCommitRounds++
		}
		raw, ok, err := s.sidecar(d.Owner, d.Repo, d.Number, rd.SHA7)
		if err != nil {
			logf("%s: sidecar %s: %v; round counts as missing", d.key(), rd.SHA7, err)
			ok = false
		}
		if !ok {
			pr.RoundsMissingSidecar++
			continue
		}
		pl, err := payload.Decode(raw)
		if err != nil {
			logf("%s: sidecar %s: %v; round counts as missing", d.key(), rd.SHA7, err)
			pr.RoundsMissingSidecar++
			continue
		}
		previous, err := ledger.GetPublishedFindingsForPR(d.Owner, d.Repo, d.Number)
		if err != nil {
			return pr, nil, err
		}
		comments := append(externalCommentsBefore(d, bots, rd.At), rec.ownComments()...)
		ghPR := github.PullRequest{Owner: d.Owner, Repo: d.Repo, Number: d.Number, CommitSHA: rd.SHA, Author: d.Author.Login}
		round := poller.BuildPublishRoundWith(ghPR, pl, comments, patchesFromHunks(pl), previous, "", policy)
		round.Changes = changeLookup(d, s, rd.SHA, logf)
		at := rd.At
		pub.Now = func() time.Time { return at }
		rec.begin(i, rd.SHA)
		before := len(rec.posts)
		if _, err := pub.Publish(ctx, round); err != nil {
			return pr, nil, fmt.Errorf("round %d (%s): %w", i+1, rd.SHA7, err)
		}
		pushes = append(pushes, len(rec.posts)-before)
		pr.InThreadReplies = rec.replies
		pr.ThreadsResolved, pr.ThreadsUnresolved, pr.RootsResolved = rec.resolves, rec.unresolves, rec.rootsResolved()
		annotate(rec.posts[before:], round.Findings)
		for j := before; j < len(rec.posts); j++ {
			pr.RootsPosted++
			switch classifyRepost(rec.posts[j], rec.posts[:before], textOnly) {
			case repostSameMarker:
				pr.SameMarkerReposts++
			case repostSameDefect:
				pr.SameDefectReposts++
				logf("%s: round %d same-defect repost: %s", d.key(), i+1, explainRepost(rec.posts[j], rec.posts[:before], textOnly))
			}
			if classifyRepost(rec.posts[j], rec.posts[before:j], textOnly) != repostNone {
				pr.SameRoundDuplicates++
			}
		}
		after, err := ledger.GetPublishedFindingsForPR(d.Owner, d.Repo, d.Number)
		if err != nil {
			return pr, nil, err
		}
		countResolutions(d, s, rd.SHA, previous, after, &pr, logf)
	}
	return pr, pushes, nil
}

// changeLookup gives the publisher the same compare the metrics judge it by:
// the files changed between a row's last-seen head and this round's head.
func changeLookup(d *prDump, s *store, head string, logf func(string, ...any)) func(base string) (publisher.ChangeSet, bool) {
	return func(base string) (publisher.ChangeSet, bool) {
		cmp, known, err := s.compare(d.Owner, d.Repo, base, head)
		if err != nil {
			logf("%s: compare %s...%s: %v; changes unknown to the publisher", d.key(), short(base), short(head), err)
			return publisher.ChangeSet{}, false
		}
		if !known {
			return publisher.ChangeSet{}, false
		}
		files := make(map[string]bool, len(cmp.Files))
		for _, f := range cmp.Files {
			files[f] = true
		}
		return publisher.ChangeSet{Files: files}, true
	}
}

// countResolutions classifies every finding row the round flipped out of
// open: same commit, cited file untouched, or a change the compare could not
// be fetched or was too large to list in full.
func countResolutions(d *prDump, s *store, head string, previous, after []db.PublishedFinding, pr *PRResult, logf func(string, ...any)) {
	wasOpen := map[string]db.PublishedFinding{}
	for _, row := range previous {
		if isFindingRow(row) && row.State == db.PublishedStateOpen {
			wasOpen[row.Fingerprint] = row
		}
	}
	for _, row := range after {
		prev, ok := wasOpen[row.Fingerprint]
		if !ok || !isFindingRow(row) || (row.State != db.PublishedStateResolved && row.State != db.PublishedStateFixed) {
			continue
		}
		pr.Fixed++
		if strings.EqualFold(prev.LastSeenSHA, head) {
			pr.SameCommitResolves++
			continue
		}
		cmp, known, err := s.compare(d.Owner, d.Repo, prev.LastSeenSHA, head)
		if err != nil {
			logf("%s: compare %s...%s: %v; file change unknown", d.key(), short(prev.LastSeenSHA), short(head), err)
			known = false
		}
		switch {
		case !known:
			pr.FixedFileChangeUnknown++
		case !fileChanged(cmp.Files, fileOfFingerprint(row.Fingerprint)):
			pr.FixedWithoutFileChange++
		}
	}
}

func isFindingRow(row db.PublishedFinding) bool {
	return row.Kind == db.PublishedKindFinding || row.Kind == db.PublishedKindAnnotation
}

// fileOfFingerprint reads the file out of file:bucket:hash.
func fileOfFingerprint(fp string) string {
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

func fileChanged(changed []string, file string) bool {
	for _, c := range changed {
		if c == file {
			return true
		}
	}
	return false
}

func sameFile(a, b string) bool {
	return a == b
}

// externalCommentsBefore is what other reviewers had on the PR when the
// round ran, so cross-bot reconciliation sees what the poller saw.
func externalCommentsBefore(d *prDump, bots map[string]bool, at time.Time) []github.ReviewCommentInfo {
	var out []github.ReviewCommentInfo
	for _, t := range d.ReviewThreads.Nodes {
		line := t.Line
		if line == 0 {
			line = t.OriginalLine
		}
		for _, c := range t.Comments.Nodes {
			login := actorLogin(c.Author)
			if bots[bareLogin(login)] || !c.CreatedAt.Before(at) {
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

// patchesFromHunks rebuilds enough of each file's patch from the hunks the
// sidecar stored with its findings for CommentableLines to accept the lines
// the review cited. A finding without a hunk gets its own line.
func patchesFromHunks(pl payload.Payload) map[string]string {
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

// annotate attaches the raw agent prose and subjects to each new post so the
// alias check compares raw text, not rendered bodies.
func annotate(posts []post, findings []payload.Finding) {
	byID := map[string]payload.Finding{}
	for _, f := range findings {
		byID[f.ID] = f
	}
	for i := range posts {
		f, ok := byID[posts[i].FindingID]
		if !ok {
			posts[i].RawText = stripMarkup(posts[i].Body)
			continue
		}
		posts[i].RawText = f.Comment
		if f.FindingContract != nil {
			posts[i].Kind = f.FindingContract.FindingKind
			posts[i].SubjectKinds = map[string]string{}
			for _, s := range f.FindingContract.Subjects {
				if name := strings.ToLower(strings.TrimSpace(s.Name)); name != "" && posts[i].SubjectKinds[name] == "" {
					posts[i].Subjects = append(posts[i].Subjects, name)
					posts[i].SubjectKinds[name] = s.Kind
				}
			}
			sort.Strings(posts[i].Subjects)
		}
	}
}

type repostKind int

const (
	repostNone repostKind = iota
	repostSameMarker
	repostSameDefect
)

func classifyRepost(p post, earlier []post, textOnly bool) repostKind {
	kind := repostNone
	for _, e := range earlier {
		if p.FindingID != "" && e.FindingID == p.FindingID {
			return repostSameMarker
		}
		if sameDefect(p, e, textOnly) {
			kind = repostSameDefect
		}
	}
	return kind
}

// explainRepost names the earlier post a repost aliases to and why, for the
// log that accompanies the metric.
func explainRepost(p post, earlier []post, textOnly bool) string {
	for _, e := range earlier {
		if sameDefect(p, e, textOnly) {
			return fmt.Sprintf("%s:%d (round %d) vs %s:%d (round %d) jaccard=%.2f kind=%s/%s subjects=%v/%v new=%q prior=%q",
				p.File, p.Line, p.Round+1, e.File, e.Line, e.Round+1, reconcile.Similarity(p.RawText, e.RawText), p.Kind, e.Kind, p.Subjects, e.Subjects, head(p.RawText), head(e.RawText))
		}
	}
	return ""
}

func head(s string) string {
	if r := []rune(s); len(r) > 90 {
		return string(r[:90]) + "..."
	}
	return s
}

// sameDefect is the alias rule from the program spec: same file, both lines
// known and within ten, and either raw-text Jaccard at or above 0.20 or the
// publisher's subject key (sharedSubjectKey). textOnly drops the key.
func sameDefect(p, e post, textOnly bool) bool {
	if !sameFile(p.File, e.File) {
		return false
	}
	if p.Line <= 0 || e.Line <= 0 || abs(p.Line-e.Line) > aliasLineTolerance {
		return false
	}
	if reconcile.Similarity(p.RawText, e.RawText) >= aliasSimilarity {
		return true
	}
	return !textOnly && sharedSubjectKey(p, e)
}

// sharedSubjectKey mirrors the publisher's line-independent key: the same
// finding kind and the same sorted subject set, or a shared symbol or
// selector subject. A subject that is the only one on either side (a bare
// file, or the enclosing function alone) never joins two findings: distinct
// defects in one function share it by construction.
func sharedSubjectKey(p, e post) bool {
	if p.Kind == "" || p.Kind != e.Kind || len(p.Subjects) == 0 || len(e.Subjects) == 0 {
		return false
	}
	if sameSubjectSet(p.Subjects, e.Subjects) {
		return true
	}
	if len(p.Subjects) < 2 || len(e.Subjects) < 2 {
		return false
	}
	for _, s := range p.Subjects {
		if !specificSubject(p.SubjectKinds[s]) || !specificSubject(e.SubjectKinds[s]) {
			continue
		}
		for _, o := range e.Subjects {
			if s == o {
				return true
			}
		}
	}
	return false
}

func specificSubject(kind string) bool {
	return kind == "symbol" || kind == "selector"
}

func sameSubjectSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// observe counts the historical roots the dump holds, with the same alias
// rule applied to their rendered prose.
func observe(d *prDump, bots map[string]bool, pr *PRResult, m *ObservedMetrics) {
	var earlier []post
	for _, r := range d.observedRoots(bots) {
		pr.ObservedRoots++
		if r.Resolved {
			m.ThreadsResolved++
		}
		p := post{FindingID: r.FindingID, File: r.File, Line: r.Line, RawText: r.Text}
		switch classifyRepost(p, earlier, true) {
		case repostSameMarker:
			pr.ObservedSameMarkerReposts++
		case repostSameDefect:
			pr.ObservedSameDefectReposts++
		}
		earlier = append(earlier, p)
	}
	m.Roots += pr.ObservedRoots
	m.SameMarkerReposts += pr.ObservedSameMarkerReposts
	m.SameDefectReposts += pr.ObservedSameDefectReposts
}

func p50(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return float64(s[n/2-1]+s[n/2]) / 2
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}

var csvHeader = []string{"pr", "rounds", "rounds_missing_sidecar", "same_commit_rounds", "roots_posted", "same_marker_reposts", "same_defect_reposts", "same_round_duplicates", "fixed", "fixed_without_file_change", "fixed_file_change_unknown", "same_commit_resolves", "in_thread_replies", "threads_resolved", "threads_unresolved", "roots_resolved", "observed_roots", "observed_same_marker_reposts", "observed_same_defect_reposts"}

func writeCSV(w io.Writer, rows []PRResult) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{r.PR, itoa(r.Rounds), itoa(r.RoundsMissingSidecar), itoa(r.SameCommitRounds), itoa(r.RootsPosted), itoa(r.SameMarkerReposts), itoa(r.SameDefectReposts), itoa(r.SameRoundDuplicates), itoa(r.Fixed), itoa(r.FixedWithoutFileChange), itoa(r.FixedFileChangeUnknown), itoa(r.SameCommitResolves), itoa(r.InThreadReplies), itoa(r.ThreadsResolved), itoa(r.ThreadsUnresolved), itoa(r.RootsResolved), itoa(r.ObservedRoots), itoa(r.ObservedSameMarkerReposts), itoa(r.ObservedSameDefectReposts)}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func itoa(n int) string { return strconv.Itoa(n) }
