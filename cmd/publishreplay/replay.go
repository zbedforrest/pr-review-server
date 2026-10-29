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
	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/health"
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
	// roundSummary is the summary body the current round wrote, or "".
	roundSummary string
	// external are the other reviewers' threads as GitHub would list them
	// at the current round, set by the replay before each round.
	external []publisher.ReviewThread
	// Renderer defects seen in the bodies written this PR.
	nilImpactBullets int
	bareLabelTitles  int
	// notesPerThread counts the status notes each thread received in each
	// round; doubleNotes the (round, thread) pairs that got more than one.
	notesPerThread map[[2]int64]int
	doubleNotes    int
	// Output hygiene defects of the 2026-10-13 audit, read off the bodies.
	confidenceLabels  int
	leakedFindingIDs  int
	sourceFooters     int
	praiseShown       int
	leadingMarkers    int
	zeroStatusLines   int
	noopSummaryEdits  int
	lastSummaryBodies map[int64]string
}

func newRecorder() *recorder {
	return &recorder{nextComment: 1_000_000, nextIssue: 2_000_000, nextReview: 3_000_000, resolved: map[int64]bool{}, notesPerThread: map[[2]int64]int{}, lastSummaryBodies: map[int64]string{}}
}

const threadNodePrefix = "T"

func (r *recorder) ListReviewThreads(context.Context, string, string, int) ([]publisher.ReviewThread, error) {
	out := append(make([]publisher.ReviewThread, 0, len(r.posts)+len(r.external)), r.external...)
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

func (r *recorder) begin(round int, sha string) { r.round, r.sha, r.roundSummary = round, sha, "" }

func (r *recorder) CreateReview(_ context.Context, _, _ string, _ int, _, _ string, comments []publisher.ReviewCommentInput) (int64, []int64, error) {
	r.nextReview++
	ids := make([]int64, 0, len(comments))
	for _, c := range comments {
		r.nextComment++
		id, _ := publisher.FindingIDFromBody(c.Body)
		r.posts = append(r.posts, post{Round: r.round, SHA: r.sha, CommentID: r.nextComment, FindingID: id, File: c.Path, Line: c.Line, Body: c.Body})
		ids = append(ids, r.nextComment)
		if bareLabelTitle(c.Body) {
			r.bareLabelTitles++
		}
		r.confidenceLabels += confidenceLabelLines(c.Body)
		if leaksFindingID(c.Body) {
			r.leakedFindingIDs++
		}
		if sourceFooterRe.MatchString(c.Body) {
			r.sourceFooters++
		}
		if praiseTitle(c.Body) {
			r.praiseShown++
		}
		if leadingMarkerTitle(c.Body) {
			r.leadingMarkers++
		}
	}
	return r.nextReview, ids, nil
}

func (r *recorder) CreateIssueComment(_ context.Context, _, _ string, _ int, body string) (int64, error) {
	r.nextIssue++
	r.summaries++
	r.roundSummary = body
	r.noteSummary(r.nextIssue, body)
	return r.nextIssue, nil
}

func (r *recorder) EditIssueComment(_ context.Context, _, _ string, id int64, body string) error {
	r.edits++
	if r.lastSummaryBodies[id] == body {
		r.noopSummaryEdits++
	}
	r.roundSummary = body
	r.noteSummary(id, body)
	return nil
}

// lastSummary is the body of the newest sticky summary the replay posted;
// the highest id is the newest, since the recorder hands ids out in order.
func (r *recorder) lastSummary() string {
	return newestSummary(r.lastSummaryBodies)
}

func newestSummary(bodies map[int64]string) string {
	var newest int64 = -1
	for id := range bodies {
		if id > newest {
			newest = id
		}
	}
	return bodies[newest]
}

func (r *recorder) noteSummary(id int64, body string) {
	r.lastSummaryBodies[id] = body
	r.nilImpactBullets += nilImpactBullets(body)
	r.praiseShown += praiseBullets(body)
	r.leadingMarkers += leadingMarkerBullets(body)
	if leaksFindingID(body) {
		r.leakedFindingIDs++
	}
	if zeroStatusOverNotes(body) {
		r.zeroStatusLines++
	}
}

func (r *recorder) ListIssueComments(context.Context, string, string, int) ([]publisher.IssueComment, error) {
	return nil, nil
}

// PostReply counts the notes in PRism's own threads; the delta reply under
// another reviewer's root is the publisher's external_replies, not one.
func (r *recorder) PostReply(_ context.Context, _, _ string, _ int, root int64, body string) (int64, error) {
	r.nextComment++
	if !publisher.IsExternalReply(body) {
		r.replies++
		key := [2]int64{int64(r.round), root}
		r.notesPerThread[key]++
		if r.notesPerThread[key] == 2 {
			r.doubleNotes++
		}
	}
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
	PR                         string
	Rounds                     int
	RoundsAfterMergeCommit     int
	RoundsSkippedUnchangedDiff int
	RoundsSkippedDailyCap      int
	RoundsMissingSidecar       int
	SameCommitRounds           int
	SameCommitSkipped          int
	RootsPosted                int
	SameMarkerReposts          int
	SameDefectReposts          int
	SameRoundDuplicates        int
	Fixed                      int
	FixedWithoutFileChange     int
	FixedWithoutHunkChange     int
	FixedHunkUnknown           int
	FixedFileChangeUnknown     int
	SameCommitResolves         int
	InThreadReplies            int
	DoubleNotes                int
	ThreadsResolved            int
	ThreadsUnresolved          int
	RootsResolved              int
	ObservedRoots              int
	ObservedSameMarkerReposts  int
	ObservedSameDefectReposts  int
	NilImpactBullets           int
	BareLabelTitles            int
	// Output hygiene (2026-10-13 audit): what the gate held, by reason, and
	// what still reached a rendered body.
	HeldLibraryClaims     int
	HeldRegressionClaims  int
	HeldPraise            int
	HeldReasonless        int
	LibraryClaimsShown    int
	RegressionClaimsShown int
	// Repo-evidence verifier: new findings it held or dropped off GitHub,
	// and posted roots it withdrew.
	VerifierHeld         int
	VerifierDropped      int
	QualityGateHeld      int
	Refuted              int
	ReasonlessNotesShown int
	PraiseShown          int
	ConfidenceLabels     int
	LeakedFindingIDs     int
	SourceFooters        int
	ZeroStatusLines      int
	NoopSummaryEdits     int
	LeadingMarkers       int
	// External duplicates: roots posted on a point another reviewer's
	// earlier root already carried, split by that thread's state; Matched
	// and Replies are what the publisher did instead of posting.
	ExternalDuplicatesNear         int
	ExternalDuplicatesOpen         int
	ExternalDuplicatesUnknown      int
	ExternalMatched                int
	ExternalReplies                int
	ObservedExternalDuplicatesNear int
	ObservedExternalDuplicatesOpen int
	// Settled points: what the publisher dropped as a restatement of a
	// resolved thread, of one of its own settled rows, or of the PR body;
	// the summary notes it still wrote on a point another reviewer's root
	// carried (and the subset whose thread is resolved). The cross-file
	// restatement is observed only: the replay posts no replies, so no row
	// is ever settled in a thread.
	ExternalSettled                      int
	SettledDropped                       int
	BodyCovered                          int
	SummaryNotesNearExternalRoot         int
	SummaryNotesNearSettledRoot          int
	ObservedSummaryNotesNearExternalRoot int
	ObservedSummaryNotesNearSettledRoot  int
	ObservedCrossFileSamePoint           int
	// Cross-PR settled memory: roots the publisher still posted on a point
	// the author settled on an earlier PR of the repo within thirty days,
	// the findings it dropped as settled instead, and the historical
	// counterpart read off the dumps.
	CrossPRResettled         int
	CrossPRSettled           int
	ObservedCrossPRResettled int

	// dailyRounds is one count per UTC day the PR had rounds on: the
	// distinct-head rounds the poller would admit (observed minus the
	// unchanged-diff and daily-cap skips); observedDailyRounds the same
	// before either skip.
	dailyRounds, observedDailyRounds []int
}

// CrossPRPair is one new root (or a finding the publisher dropped) on a
// point the same author settled on an earlier pull request, for the owner
// to spot-check.
type CrossPRPair struct {
	PR       string `json:"pr"`
	Round    int    `json:"round"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Headline string `json:"headline"`
	Severity string `json:"severity"`
	// Score and Subject are the containment and the shared subject behind
	// the match, recomputed against the settled point alone.
	Score         float64 `json:"score"`
	Subject       string  `json:"subject"`
	SettledPR     int     `json:"settled_pr"`
	SettledSource string  `json:"settled_source"`
	SettledAt     string  `json:"settled_at"`
	SettledFile   string  `json:"settled_file"`
	// Outcome is "posted" (the replayed publisher still posted the root),
	// "settled" (it wrote a settled row instead) or "observed" (the
	// historical root in the dump).
	Outcome string `json:"outcome"`
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
	// SameCommitSkipped counts the rounds the publisher refused because the
	// head was already published; they post and resolve nothing.
	SameCommitSkipped int `json:"same_commit_rounds_skipped"`
	// RoundsAfterMergeCommit counts the rounds whose push since the previous
	// round carried at least one merge commit; RoundsSkippedUnchangedDiff
	// those where it carried nothing else (or the same tree), which the
	// poller would not run and the replay does not replay.
	RoundsAfterMergeCommit     int `json:"rounds_after_merge_commit"`
	RoundsSkippedUnchangedDiff int `json:"rounds_skipped_unchanged_diff"`
	// RoundsSkippedDailyCap counts the rounds the daily round cap would
	// have turned away; RoundsPerPRPerDay is the admitted rounds per PR per
	// UTC day after the cap, judged against the cap in force.
	RoundsSkippedDailyCap int                    `json:"rounds_skipped_daily_cap"`
	RoundsPerPRPerDay     health.DailyRoundStats `json:"rounds_per_pr_per_day"`

	RootsPosted            int     `json:"roots_posted"`
	SameMarkerReposts      int     `json:"same_marker_reposts"`
	SameDefectReposts      int     `json:"same_defect_reposts"`
	SameDefectRepostsPerPR float64 `json:"same_defect_reposts_per_pr"`
	SameRoundDuplicates    int     `json:"same_round_duplicates"`
	PRsWithReposts         int     `json:"prs_with_reposts"`

	Fixed                  int `json:"fixed"`
	FixedWithoutFileChange int `json:"fixed_without_file_change"`
	// FixedWithoutHunkChange counts rows fixed on a push that changed the
	// cited file but no hunk near the cited lines; FixedHunkUnknown the
	// rows fixed where the compare listed the file without a patch.
	FixedWithoutHunkChange int `json:"fixed_without_hunk_change"`
	FixedHunkUnknown       int `json:"fixed_hunk_unknown"`
	FixedFileChangeUnknown int `json:"fixed_file_change_unknown"`
	SameCommitResolves     int `json:"same_commit_resolves"`
	InThreadReplies        int `json:"in_thread_replies"`
	// DoubleNotes counts the threads that received more than one status
	// note in one round.
	DoubleNotes       int `json:"double_notes"`
	ThreadsResolved   int `json:"threads_resolved"`
	ThreadsUnresolved int `json:"threads_unresolved"`

	// Renderer defects over every summary and inline body written.
	NilImpactBullets int `json:"nil_impact_bullets"`
	BareLabelTitles  int `json:"bare_label_titles"`

	// Output hygiene (2026-10-13 audit, classes 2, 6, 9 and 10). The held
	// counts are what the gate kept off GitHub; the shown counts are the
	// same defects that still reached a rendered body (an inline title or a
	// summary bullet), and the rest are read off the bodies alone.
	HeldLibraryClaims     int `json:"held_library_claims"`
	HeldRegressionClaims  int `json:"held_regression_claims"`
	HeldPraise            int `json:"held_praise"`
	HeldReasonless        int `json:"held_reasonless_notes"`
	LibraryClaimsShown    int `json:"library_claims_shown"`
	RegressionClaimsShown int `json:"uncited_regression_claims_shown"`
	// Repo-evidence verifier (chunk 8): new findings held or dropped off
	// GitHub by an applied decision, and posted roots withdrawn.
	VerifierHeld         int `json:"verifier_held"`
	VerifierDropped      int `json:"verifier_dropped"`
	QualityGateHeld      int `json:"quality_gate_held"`
	Refuted              int `json:"refuted"`
	ReasonlessNotesShown int `json:"reasonless_notes_shown"`
	PraiseShown          int `json:"praise_shown"`
	ConfidenceLabels     int `json:"confidence_labels_rendered"`
	LeakedFindingIDs     int `json:"leaked_finding_ids"`
	SourceFooters        int `json:"source_footers"`
	ZeroStatusLines      int `json:"zero_status_lines_over_notes"`
	NoopSummaryEdits     int `json:"noop_summary_edits"`
	// LeadingMarkers counts the inline titles and summary bullets that
	// open with the clamp marker or a lowercase connective (2026-10-14
	// annoyance audit).
	LeadingMarkers int `json:"leading_markers"`

	// ExternalDuplicatesNear counts replayed roots posted while an earlier
	// root by another reviewer sat on the same point (same file, line within
	// ten, text or subject overlap); Open is the subset whose thread the dump
	// shows unresolved, counting a thread of unknown state as open, and
	// ResolutionUnknown says how many of those are unknown. ExternalMatched
	// and ExternalReplies are the findings the publisher matched instead of
	// posting and the delta replies it left.
	ExternalDuplicatesNear              int `json:"external_duplicates_near"`
	ExternalDuplicatesOpen              int `json:"external_duplicates_open"`
	ExternalDuplicatesResolutionUnknown int `json:"external_duplicates_resolution_unknown"`
	PRsWithExternalDuplicates           int `json:"prs_with_external_duplicates"`
	ExternalMatched                     int `json:"external_matched"`
	ExternalReplies                     int `json:"external_replies"`
	// ExternalSettled counts the findings dropped because another
	// reviewer's resolved thread already carried the point, SettledDropped
	// those dropped because one of PRism's own settled rows did (from
	// another file or elsewhere in the file), BodyCovered the folded notes
	// held because the pull request body states them.
	// SummaryNotesNearExternalRoot counts the summary bullets written
	// without a thread on a point an earlier root by another reviewer
	// carried (same file, line within ten, text overlap), and
	// SummaryNotesNearSettledRoot the subset whose thread the dump shows
	// resolved.
	ExternalSettled              int `json:"external_settled"`
	SettledDropped               int `json:"settled_dropped"`
	BodyCovered                  int `json:"body_covered"`
	SummaryNotesNearExternalRoot int `json:"summary_notes_near_external_root"`
	SummaryNotesNearSettledRoot  int `json:"summary_notes_near_settled_root"`

	// CrossPRResettled counts replayed roots posted on a point the same
	// author settled on an earlier PR of the repo within thirty days (an
	// author verdict under a PRism root, or another reviewer's thread the
	// author answered that was withdrawn or resolved, read off the dumps
	// and seeded into the settled store before the replay); CrossPRSettled
	// the findings the publisher dropped as settled rows instead;
	// SettledPointsSeeded the points the dumps yielded.
	CrossPRResettled    int `json:"cross_pr_resettled"`
	CrossPRSettled      int `json:"cross_pr_settled"`
	SettledPointsSeeded int `json:"settled_points_seeded"`

	// CommentsPerPush is over the rounds the publisher ran; RoundsPerPR over
	// the rounds the poller would admit (observed minus unchanged-diff
	// skips), the observed figure being under observed.
	CommentsPerPushP50  float64 `json:"comments_per_push_p50"`
	CommentsPerPushMean float64 `json:"comments_per_push_mean"`
	RoundsPerPRP50      float64 `json:"rounds_per_pr_p50"`
	// PRsAtInlinePRCap counts the PRs whose replayed roots reached the
	// policy's lifetime inline cap.
	PRsAtInlinePRCap int `json:"prs_at_inline_pr_cap"`
	// PrismResolvedShare is the share of replayed roots whose thread the
	// publisher itself left resolved at the end of the PR's rounds; the
	// observed resolved_share is the human counterpart over the same roots.
	PrismResolvedShare float64 `json:"prism_resolved_share"`
	PrismResolvedNote  string  `json:"prism_resolved_note"`

	Observed ObservedMetrics `json:"observed"`
}

type ObservedMetrics struct {
	RoundsPerPRP50 float64 `json:"rounds_per_pr_p50"`
	// RoundsPerPRPerDay is the distinct-head rounds per PR per UTC day as
	// they happened, judged against the default daily cap.
	RoundsPerPRPerDay         health.DailyRoundStats `json:"rounds_per_pr_per_day"`
	Roots                     int                    `json:"roots"`
	SameMarkerReposts         int                    `json:"same_marker_reposts"`
	SameDefectReposts         int                    `json:"same_defect_reposts"`
	ThreadsResolved           int                    `json:"threads_resolved"`
	ResolvedShare             float64                `json:"resolved_share"`
	ExternalDuplicatesNear    int                    `json:"external_duplicates_near"`
	ExternalDuplicatesOpen    int                    `json:"external_duplicates_open"`
	PRsWithExternalDuplicates int                    `json:"prs_with_external_duplicates"`
	// The sticky summary's bullets as GitHub holds them now, and the
	// historical roots judged by the settled-point rule against the earlier
	// roots of other files, with PRism's own replies in those threads as
	// what it conceded.
	SummaryNotesNearExternalRoot int `json:"summary_notes_near_external_root"`
	SummaryNotesNearSettledRoot  int `json:"summary_notes_near_settled_root"`
	CrossFileSamePoint           int `json:"cross_file_same_point"`
	// CrossPRResettled counts the historical roots posted on a point the
	// same author had settled on an earlier PR of the repo within thirty
	// days, judged on the contract behind each root when its sidecar is
	// cached.
	CrossPRResettled int `json:"cross_pr_resettled"`
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
	// ReplayUnchangedDiff replays a round whose push left the PR diff
	// unchanged, as the poller did before the unchanged-diff skip.
	ReplayUnchangedDiff bool
	// DailyRoundCap skips a PR's rounds past this many on one UTC day, as
	// auto_review_daily_round_cap does; 0 replays them all.
	DailyRoundCap int
}

type Result struct {
	Metrics Metrics
	PerPR   []PRResult
	// CrossPRPairs lists every cross-PR settled match, replayed and
	// observed, for the PR body.
	CrossPRPairs []CrossPRPair
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
	var roundsPerPR, observedRoundsPerPR, commentsPerPush, dailyRounds, observedDailyRounds []int
	rootsResolved := 0
	res.Metrics.PRs = len(o.Dumps)
	res.Metrics.SettledPointsSeeded = seedSettledPoints(ledger, o.Dumps, o.bots, o.Store, logf)
	for _, d := range o.Dumps {
		bots := o.bots(d)
		pr, pushes, pairs, err := replayPR(ctx, d, bots, o.Store, ledger, o.Policy, o.TextOnlyAlias, !o.ReplayUnchangedDiff, o.DailyRoundCap, logf)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", d.key(), err)
		}
		pairs = append(pairs, observe(d, bots, o.Store, ledger, &pr, &res.Metrics.Observed, logf)...)
		res.CrossPRPairs = append(res.CrossPRPairs, pairs...)
		res.PerPR = append(res.PerPR, pr)
		if pr.Rounds > 0 {
			res.Metrics.PRsWithRounds++
			observedRoundsPerPR = append(observedRoundsPerPR, pr.Rounds)
			roundsPerPR = append(roundsPerPR, pr.Rounds-pr.RoundsSkippedUnchangedDiff-pr.RoundsSkippedDailyCap)
		}
		dailyRounds = append(dailyRounds, pr.dailyRounds...)
		observedDailyRounds = append(observedDailyRounds, pr.observedDailyRounds...)
		if cap := o.Policy.InlinePRCap; cap > 0 && pr.RootsPosted >= cap {
			res.Metrics.PRsAtInlinePRCap++
		}
		commentsPerPush = append(commentsPerPush, pushes...)
		m := &res.Metrics
		m.Rounds += pr.Rounds
		m.RoundsReplayed += pr.Rounds - pr.RoundsMissingSidecar - pr.RoundsSkippedUnchangedDiff - pr.RoundsSkippedDailyCap
		m.RoundsMissingSidecar += pr.RoundsMissingSidecar
		m.RoundsSkippedUnchangedDiff += pr.RoundsSkippedUnchangedDiff
		m.RoundsSkippedDailyCap += pr.RoundsSkippedDailyCap
		m.RoundsAfterMergeCommit += pr.RoundsAfterMergeCommit
		m.SameCommitRounds += pr.SameCommitRounds
		m.SameCommitSkipped += pr.SameCommitSkipped
		m.NilImpactBullets += pr.NilImpactBullets
		m.BareLabelTitles += pr.BareLabelTitles
		m.HeldLibraryClaims += pr.HeldLibraryClaims
		m.HeldRegressionClaims += pr.HeldRegressionClaims
		m.HeldPraise += pr.HeldPraise
		m.HeldReasonless += pr.HeldReasonless
		m.VerifierHeld += pr.VerifierHeld
		m.VerifierDropped += pr.VerifierDropped
		m.QualityGateHeld += pr.QualityGateHeld
		m.Refuted += pr.Refuted
		m.LibraryClaimsShown += pr.LibraryClaimsShown
		m.RegressionClaimsShown += pr.RegressionClaimsShown
		m.ReasonlessNotesShown += pr.ReasonlessNotesShown
		m.PraiseShown += pr.PraiseShown
		m.ConfidenceLabels += pr.ConfidenceLabels
		m.LeakedFindingIDs += pr.LeakedFindingIDs
		m.SourceFooters += pr.SourceFooters
		m.ZeroStatusLines += pr.ZeroStatusLines
		m.NoopSummaryEdits += pr.NoopSummaryEdits
		m.LeadingMarkers += pr.LeadingMarkers
		m.RootsPosted += pr.RootsPosted
		m.SameMarkerReposts += pr.SameMarkerReposts
		m.SameDefectReposts += pr.SameDefectReposts
		m.SameRoundDuplicates += pr.SameRoundDuplicates
		if pr.SameMarkerReposts+pr.SameDefectReposts > 0 {
			m.PRsWithReposts++
		}
		m.Fixed += pr.Fixed
		m.FixedWithoutFileChange += pr.FixedWithoutFileChange
		m.FixedWithoutHunkChange += pr.FixedWithoutHunkChange
		m.FixedHunkUnknown += pr.FixedHunkUnknown
		m.DoubleNotes += pr.DoubleNotes
		m.FixedFileChangeUnknown += pr.FixedFileChangeUnknown
		m.SameCommitResolves += pr.SameCommitResolves
		m.InThreadReplies += pr.InThreadReplies
		m.ThreadsResolved += pr.ThreadsResolved
		m.ThreadsUnresolved += pr.ThreadsUnresolved
		m.ExternalDuplicatesNear += pr.ExternalDuplicatesNear
		m.ExternalDuplicatesOpen += pr.ExternalDuplicatesOpen
		m.ExternalDuplicatesResolutionUnknown += pr.ExternalDuplicatesUnknown
		if pr.ExternalDuplicatesNear > 0 {
			m.PRsWithExternalDuplicates++
		}
		m.ExternalMatched += pr.ExternalMatched
		m.ExternalReplies += pr.ExternalReplies
		m.ExternalSettled += pr.ExternalSettled
		m.SettledDropped += pr.SettledDropped
		m.BodyCovered += pr.BodyCovered
		m.SummaryNotesNearExternalRoot += pr.SummaryNotesNearExternalRoot
		m.SummaryNotesNearSettledRoot += pr.SummaryNotesNearSettledRoot
		m.CrossPRResettled += pr.CrossPRResettled
		m.CrossPRSettled += pr.CrossPRSettled
		m.Observed.CrossPRResettled += pr.ObservedCrossPRResettled
		rootsResolved += pr.RootsResolved
		if pr.Rounds > 0 {
			logf("%s: rounds=%d roots=%d same_marker=%d same_defect=%d fixed=%d fixed_no_change=%d fixed_no_hunk=%d double_notes=%d", d.key(), pr.Rounds, pr.RootsPosted, pr.SameMarkerReposts, pr.SameDefectReposts, pr.Fixed, pr.FixedWithoutFileChange, pr.FixedWithoutHunkChange, pr.DoubleNotes)
		}
	}
	m := &res.Metrics
	if m.PRsWithRounds > 0 {
		m.SameDefectRepostsPerPR = round3(float64(m.SameDefectReposts) / float64(m.PRsWithRounds))
	}
	m.CommentsPerPushP50 = p50(commentsPerPush)
	m.CommentsPerPushMean = mean(commentsPerPush)
	m.RoundsPerPRP50 = p50(roundsPerPR)
	m.Observed.RoundsPerPRP50 = p50(observedRoundsPerPR)
	m.RoundsPerPRPerDay = health.SummariseDailyRounds(dailyRounds, o.DailyRoundCap)
	m.Observed.RoundsPerPRPerDay = health.SummariseDailyRounds(observedDailyRounds, poller.DefaultAutoReviewDailyRoundCap)
	m.PrismResolvedNote = "roots whose thread the replayed publisher left resolved, over roots posted"
	if m.RootsPosted > 0 {
		m.PrismResolvedShare = round3(float64(rootsResolved) / float64(m.RootsPosted))
	}
	if m.Observed.Roots > 0 {
		m.Observed.ResolvedShare = round3(float64(m.Observed.ThreadsResolved) / float64(m.Observed.Roots))
	}
	return res, nil
}

// seedSettledPoints records, before any round runs, every point the dumps
// show an author settled (an author verdict under a PRism root, another
// reviewer's thread the author answered that was withdrawn or resolved),
// dated by the reply that settled it, so a later PR's rounds meet what the
// live store would hold. Contracts come from the cached sidecar of the
// commit each root was posted on.
func seedSettledPoints(ledger *db.GormDB, dumps []*prDump, bots func(*prDump) map[string]bool, s *store, logf func(string, ...any)) int {
	n := 0
	for _, d := range dumps {
		kit, err := replaykit.LoadDump(d.Path)
		if err != nil {
			logf("%s: settled points not seeded: %v", d.key(), err)
			continue
		}
		cache := map[string]map[string]payload.Finding{}
		lookup := func(sha7, id string) (payload.Finding, bool) {
			if s == nil {
				return payload.Finding{}, false
			}
			if _, ok := cache[sha7]; !ok {
				cache[sha7] = map[string]payload.Finding{}
				if raw, ok, err := s.sidecar(d.Owner, d.Repo, d.Number, sha7); err == nil && ok {
					if pl, err := payload.Decode(raw); err == nil {
						for _, f := range pl.Findings {
							cache[sha7][f.ID] = f
						}
					}
				}
			}
			f, ok := cache[sha7][id]
			return f, ok
		}
		for _, p := range kit.SettledPointsOf(bots(d), lookup) {
			point := p
			if err := ledger.RecordSettledPoint(&point); err != nil {
				logf("%s: settled point %d not seeded: %v", d.key(), p.CommentID, err)
				continue
			}
			n++
			logf("%s: settled point seeded: %s %s at %s (%s)", d.key(), p.Source, p.File, p.SettledAt.Format(time.RFC3339), head(p.Reason))
		}
	}
	return n
}

// settledPointOf finds the point a settled row names in the store, for the
// pair listing; a point the store no longer lists is named by the row alone.
func settledPointOf(ledger *db.GormDB, d *prDump, at time.Time, row db.PublishedFinding) db.SettledPoint {
	points, err := ledger.ListSettledPoints(d.Owner, d.Repo, d.Author.Login, at.Add(-publisher.SettledWindow), at)
	if err == nil {
		for _, pt := range points {
			if pt.PRNumber == row.SettledPRNumber && pt.CommentID == row.SettledCommentID {
				return pt
			}
		}
	}
	// The store no longer lists the point; the row's file is the point's
	// file under the same-file rule, the rest is unknown.
	return db.SettledPoint{PRNumber: row.SettledPRNumber, CommentID: row.SettledCommentID, File: payload.FingerprintFile(row.Fingerprint)}
}

// crossPRPair describes a finding against the point it raises again.
func crossPRPair(d *prDump, round int, f payload.Finding, pt db.SettledPoint, outcome string) CrossPRPair {
	_, score, _ := publisher.ScoreSettledElsewhere(f, []db.SettledPoint{pt})
	return CrossPRPair{PR: d.key(), Round: round + 1, File: f.File, Line: f.Line, Headline: head(publisher.TitleOf(f)), Severity: f.Severity, Score: score.Text, Subject: score.Subject,
		SettledPR: pt.PRNumber, SettledSource: pt.Source, SettledAt: pt.SettledAt.Format(time.RFC3339), SettledFile: pt.File, Outcome: outcome}
}

func replayPR(ctx context.Context, d *prDump, bots map[string]bool, s *store, ledger *db.GormDB, policy publisher.Policy, textOnly, skipUnchanged bool, dailyCap int, logf func(string, ...any)) (PRResult, []int, []CrossPRPair, error) {
	pr := PRResult{PR: d.key()}
	var pairs []CrossPRPair
	rec := newRecorder()
	pub := &publisher.Publisher{GH: rec, Ledger: ledger, Policy: policy}
	rounds := d.rounds(bots)
	pr.Rounds = len(rounds)
	roots := d.externalRoots(bots)
	var pushes []int
	days := newDailyRounds()
	for i, rd := range rounds {
		sameCommit := i > 0 && rounds[i-1].SHA == rd.SHA
		if sameCommit {
			pr.SameCommitRounds++
		} else if i > 0 {
			push := pushSince(d, s, rounds[i-1].SHA, rd.SHA, logf)
			if push.MergeCommits > 0 {
				pr.RoundsAfterMergeCommit++
			}
			if skipUnchanged && push.unchangedDiff() {
				days.observe(rd.At)
				pr.RoundsSkippedUnchangedDiff++
				logf("%s: round %d skipped, the push %s..%s left the diff unchanged", d.key(), i+1, short(rounds[i-1].SHA), rd.SHA7)
				continue
			}
		}
		// A same-commit round is a manual re-run of a reviewed head, which
		// the cap exempts; every other round is an automatic admission.
		if !sameCommit {
			days.observe(rd.At)
			if dailyCap > 0 && days.admittedOn(rd.At) >= dailyCap {
				pr.RoundsSkippedDailyCap++
				logf("%s: round %d skipped, the PR had its %d rounds for %s", d.key(), i+1, dailyCap, rd.At.UTC().Format("2006-01-02"))
				continue
			}
			days.admit(rd.At)
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
			return pr, nil, nil, err
		}
		comments := append(externalCommentsBefore(d, bots, rd.At), rec.ownComments()...)
		rec.external = externalThreadsBefore(d, bots, rd.At)
		ghPR := github.PullRequest{Owner: d.Owner, Repo: d.Repo, Number: d.Number, CommitSHA: rd.SHA, Author: d.Author.Login}
		round := poller.BuildPublishRoundWith(ghPR, pl, comments, patchesFromHunks(pl), previous, "", policy)
		round.PRBody = d.Body
		round.Changes = changeLookup(d, s, rd.SHA, logf)
		at := rd.At
		pub.Now = func() time.Time { return at }
		rec.begin(i, rd.SHA)
		before := len(rec.posts)
		rep, err := pub.Publish(ctx, round)
		if errors.Is(err, publisher.ErrHeadAlreadyPublished) {
			pr.SameCommitSkipped++
			continue
		} else if err != nil {
			return pr, nil, nil, fmt.Errorf("round %d (%s): %w", i+1, rd.SHA7, err)
		}
		pr.ExternalMatched += rep.ExternalMatched
		pr.ExternalReplies += rep.ExternalReplies
		countHeld(rep, &pr)
		for _, h := range rep.Held {
			logf("%s: round %d held %s:%d (%s)", d.key(), i+1, h.File, h.Line, h.Reason)
		}
		countShownDefects(round.Findings, rep, rec.posts[before:], rec.lastSummaryBodies, &pr)
		for j := before; j < len(rec.posts); j++ {
			if leaksFindingID(rec.posts[j].Body) {
				logf("%s: round %d inline %s:%d leaks a finding label", d.key(), i+1, rec.posts[j].File, rec.posts[j].Line)
			}
		}
		if leaksFindingID(rec.lastSummary()) {
			logf("%s: round %d summary leaks a finding label", d.key(), i+1)
		}
		pr.ExternalSettled += rep.ExternalSettled
		pr.SettledDropped += rep.SettledDropped
		pr.BodyCovered += rep.BodyCovered
		for _, n := range summaryNotes(rec.roundSummary) {
			root, ok := nearExternalRoot(pl, n, roots, rd.At)
			if !ok {
				continue
			}
			pr.SummaryNotesNearExternalRoot++
			if root.Resolved {
				pr.SummaryNotesNearSettledRoot++
			}
			logf("%s: round %d summary note near %s's root %d: %s:%d (thread resolved=%v)", d.key(), i+1, root.Author, root.CommentID, n.File, n.Line, root.Resolved)
		}
		pushes = append(pushes, len(rec.posts)-before)
		pr.InThreadReplies, pr.DoubleNotes = rec.replies, rec.doubleNotes
		pr.ThreadsResolved, pr.ThreadsUnresolved, pr.RootsResolved = rec.resolves, rec.unresolves, rec.rootsResolved()
		annotate(rec.posts[before:], round.Findings)
		findingsByID := map[string]payload.Finding{}
		for _, f := range round.Findings {
			findingsByID[f.ID] = f
		}
		pr.CrossPRSettled += rep.CrossPRSettled
		for j := before; j < len(rec.posts); j++ {
			pr.RootsPosted++
			if f, ok := findingsByID[rec.posts[j].FindingID]; ok {
				if pt, ok := replaykit.SettledElsewhere(ledger, d.Owner, d.Repo, d.Author.Login, d.Number, rd.At, f); ok {
					pr.CrossPRResettled++
					pairs = append(pairs, crossPRPair(d, i, f, pt, "posted"))
					logf("%s: round %d root %s:%d raises again a point %s settled on #%d (%s)", d.key(), i+1, f.File, f.Line, d.Author.Login, pt.PRNumber, pt.Source)
				}
			}
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
			if root, ok := replaykit.ExternalDuplicate(rec.posts[j].File, rec.posts[j].Line, rec.posts[j].RawText, roots, rd.At); ok {
				pr.ExternalDuplicatesNear++
				if !root.ResolvedKnown {
					pr.ExternalDuplicatesUnknown++
				}
				if !root.Resolved {
					pr.ExternalDuplicatesOpen++
				}
				logf("%s: round %d external duplicate: %s:%d near %s's root %d at %s:%d", d.key(), i+1, rec.posts[j].File, rec.posts[j].Line, root.Author, root.CommentID, root.File, root.Line)
			}
		}
		after, err := ledger.GetPublishedFindingsForPR(d.Owner, d.Repo, d.Number)
		if err != nil {
			return pr, nil, nil, err
		}
		for _, row := range after {
			if row.State != db.PublishedStateSettled || !row.PublishedAt.Equal(rd.At) {
				continue
			}
			f, ok := findingsByID[row.Fingerprint]
			if !ok {
				f = payload.Finding{ID: row.Fingerprint, File: fileOfFingerprint(row.Fingerprint), Line: row.Line, Comment: row.CommentText, Severity: row.Severity}
			}
			pairs = append(pairs, crossPRPair(d, i, f, settledPointOf(ledger, d, rd.At, row), "settled"))
			logf("%s: round %d finding %s:%d settled earlier on #%d, dropped", d.key(), i+1, f.File, f.Line, row.SettledPRNumber)
		}
		countResolutions(d, s, rd.SHA, previous, after, &pr, logf)
	}
	pr.dailyRounds, pr.observedDailyRounds = days.counts()
	pr.NilImpactBullets, pr.BareLabelTitles = rec.nilImpactBullets, rec.bareLabelTitles
	pr.PraiseShown, pr.ConfidenceLabels, pr.LeakedFindingIDs = rec.praiseShown, rec.confidenceLabels, rec.leakedFindingIDs
	pr.SourceFooters, pr.ZeroStatusLines, pr.NoopSummaryEdits = rec.sourceFooters, rec.zeroStatusLines, rec.noopSummaryEdits
	pr.LeadingMarkers = rec.leadingMarkers
	return pr, pushes, pairs, nil
}

// dailyRounds tallies a PR's rounds per UTC day, as they happened and as the
// poller would admit them.
type dailyRounds struct {
	observed, admitted map[string]int
	order              []string
}

func newDailyRounds() *dailyRounds {
	return &dailyRounds{observed: map[string]int{}, admitted: map[string]int{}}
}

func (d *dailyRounds) day(at time.Time) string {
	key := at.UTC().Format("2006-01-02")
	if _, seen := d.observed[key]; !seen {
		d.order = append(d.order, key)
		d.observed[key] = 0
	}
	return key
}

func (d *dailyRounds) observe(at time.Time) { d.observed[d.day(at)]++ }
func (d *dailyRounds) admit(at time.Time)   { d.admitted[d.day(at)]++ }

func (d *dailyRounds) admittedOn(at time.Time) int { return d.admitted[d.day(at)] }

// counts returns the admitted and observed rounds per day, in first-seen
// order; a day whose every round was skipped still appears in observed and
// is absent from admitted.
func (d *dailyRounds) counts() (admitted, observed []int) {
	for _, key := range d.order {
		observed = append(observed, d.observed[key])
		if n := d.admitted[key]; n > 0 {
			admitted = append(admitted, n)
		}
	}
	return admitted, observed
}

func countHeld(rep publisher.Report, pr *PRResult) {
	pr.Refuted += rep.Refuted
	for _, h := range rep.Held {
		switch h.Reason {
		case publisher.HoldLibraryClaim:
			pr.HeldLibraryClaims++
		case publisher.HoldRegressionClaim:
			pr.HeldRegressionClaims++
		case publisher.HoldPraise:
			pr.HeldPraise++
		case publisher.HoldReasonless:
			pr.HeldReasonless++
		case publisher.HoldVerifierHold:
			pr.VerifierHeld++
		case publisher.HoldVerifierDrop:
			pr.VerifierDropped++
		case publisher.HoldQualityGate:
			pr.QualityGateHeld++
		}
	}
}

// countShownDefects counts the round's findings that the claim rules flag
// and that still reached a body: an inline post of this round, or a bullet
// of the summary the round left behind (matched by title prefix, which is
// what the bullet renders).
func countShownDefects(findings []payload.Finding, rep publisher.Report, posts []post, summaries map[int64]string, pr *PRResult) {
	held := map[string]bool{}
	for _, h := range rep.Held {
		// Aliasing may have renamed the id; the cited location survives it.
		held[h.ID] = true
		held[fmt.Sprintf("%s:%d", h.File, h.Line)] = true
	}
	posted := map[string]bool{}
	for _, p := range posts {
		posted[p.FindingID] = true
	}
	summary := newestSummary(summaries)
	for _, f := range findings {
		if held[f.ID] || held[fmt.Sprintf("%s:%d", f.File, f.Line)] || !publisher.Publishable(f) {
			continue
		}
		prefix := titlePrefix(publisher.TitleOf(f))
		shown := posted[f.ID] || (prefix != "" && strings.Contains(summary, prefix))
		if !shown {
			continue
		}
		switch {
		case publisher.UnverifiedLibraryClaim(f):
			pr.LibraryClaimsShown++
		case publisher.UncitedRegressionClaim(f):
			pr.RegressionClaimsShown++
		case publisher.ReasonlessNote(f):
			pr.ReasonlessNotesShown++
		}
	}
}

func titlePrefix(title string) string {
	r := []rune(strings.TrimSuffix(strings.TrimSpace(title), "."))
	if len(r) > 60 {
		r = r[:60]
	}
	return string(r)
}

// pushSince is the compare from the previous round's head to this one, the
// one the harness already fetches between round heads; a failed compare
// reads as a real push with no merge commits.
func pushSince(d *prDump, s *store, prev, head string, logf func(string, ...any)) compareResult {
	cmp, _, err := s.push(d.Owner, d.Repo, prev, head)
	if err != nil {
		logf("%s: compare %s...%s: %v; push treated as a real change", d.key(), short(prev), short(head), err)
		return compareResult{Error: err.Error()}
	}
	return cmp
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
		return cmp.changeSet(), true
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
			continue
		case !fileChanged(cmp.Files, fileOfFingerprint(row.Fingerprint)):
			pr.FixedWithoutFileChange++
			continue
		}
		cs := cmp.changeSet()
		if cs.Hunks == nil {
			continue
		}
		switch covered, hunksKnown := publisher.RowAnchorInHunks(cs, row); {
		case !hunksKnown:
			pr.FixedHunkUnknown++
		case !covered:
			pr.FixedWithoutHunkChange++
			logf("%s: row %s fixed at %s without a hunk near line %d (compare %s...%s)", d.key(), row.Fingerprint, short(head), row.Line, short(prev.LastSeenSHA), short(head))
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
// round ran, so cross-bot reconciliation sees what the poller saw. The
// thread's original line stands in for the line the poller would have
// listed then; the dump's current line has drifted with every later push.
func externalCommentsBefore(d *prDump, bots map[string]bool, at time.Time) []github.ReviewCommentInfo {
	var out []github.ReviewCommentInfo
	for _, t := range d.ReviewThreads.Nodes {
		line := t.OriginalLine
		if line == 0 {
			line = t.Line
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

// externalThreadsBefore lists the other reviewers' threads as GitHub would
// have at the round, for the publisher's thread index. The dump records only
// the thread's state today, so a thread reads as resolved at the round when
// it is resolved now and someone had replied in it by then. A thread
// resolved silently reads as open; one replied to by the round but resolved
// only later still reads as resolved, so the settled counts are an upper
// bound.
func externalThreadsBefore(d *prDump, bots map[string]bool, at time.Time) []publisher.ReviewThread {
	var out []publisher.ReviewThread
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		if root.ReplyTo != nil || bots[bareLogin(actorLogin(root.Author))] || !root.CreatedAt.Before(at) {
			continue
		}
		replied := false
		for _, c := range t.Comments.Nodes[1:] {
			if c.CreatedAt.Before(at) {
				replied = true
			}
		}
		out = append(out, publisher.ReviewThread{NodeID: "E" + strconv.FormatInt(int64(root.DatabaseID), 10), RootCommentID: int64(root.DatabaseID), Resolved: t.resolved() && replied, Outdated: t.IsOutdated})
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

// nearExternalRoot finds the other reviewer's root a replayed summary
// bullet sits on, by the bullet's own text (what a reader compares, and
// what the observed counterpart reads from the dumps) or by the raw agent
// prose behind it.
func nearExternalRoot(pl payload.Payload, n summaryNote, roots []replaykit.ExternalRoot, at time.Time) (replaykit.ExternalRoot, bool) {
	if root, ok := replaykit.ExternalDuplicate(n.File, n.Line, n.Text, roots, at); ok {
		return root, true
	}
	return replaykit.ExternalDuplicate(n.File, n.Line, noteText(pl, n), roots, at)
}

// noteText is the raw agent prose behind a summary bullet when the sidecar
// has the finding at that file and line, else the bullet's own text.
func noteText(pl payload.Payload, n summaryNote) string {
	for _, f := range pl.Findings {
		if f.File == n.File && f.Line == n.Line && f.Comment != "" {
			return f.Comment
		}
	}
	return n.Text
}

// observe counts the historical roots the dump holds, with the same alias
// rule applied to their rendered prose, and the historical summary bullets
// against the other reviewers' roots. The settled-point rule needs the
// contract behind each root, read from the cached sidecar of the commit it
// was posted on; a root whose sidecar is not cached is judged on its prose
// alone and can only fail the subject test.
func observe(d *prDump, bots map[string]bool, s *store, ledger *db.GormDB, pr *PRResult, m *ObservedMetrics, logf func(string, ...any)) []CrossPRPair {
	var pairs []CrossPRPair
	var earlier []post
	var earlierPoints []reconcile.SettledPoint
	roots := d.externalRoots(bots)
	findings := map[string]payload.Finding{}
	loaded := map[string]bool{}
	for _, r := range d.observedRoots(bots) {
		pr.ObservedRoots++
		if r.Resolved {
			m.ThreadsResolved++
		}
		if root, ok := replaykit.ExternalDuplicate(r.File, r.Line, r.Text, roots, r.At); ok {
			pr.ObservedExternalDuplicatesNear++
			if !root.Resolved {
				pr.ObservedExternalDuplicatesOpen++
			}
		}
		p := post{FindingID: r.FindingID, File: r.File, Line: r.Line, RawText: r.Text}
		switch classifyRepost(p, earlier, true) {
		case repostSameMarker:
			pr.ObservedSameMarkerReposts++
		case repostSameDefect:
			pr.ObservedSameDefectReposts++
		}
		earlier = append(earlier, p)

		f := payload.Finding{ID: r.FindingID, File: r.File, Line: r.Line, Comment: r.Text}
		if s != nil && len(r.Commit) >= 7 && !loaded[short(r.Commit)] {
			loaded[short(r.Commit)] = true
			if raw, ok, err := s.sidecar(d.Owner, d.Repo, d.Number, short(r.Commit)); err == nil && ok {
				if pl, err := payload.Decode(raw); err == nil {
					for _, sf := range pl.Findings {
						findings[sf.ID] = sf
					}
				}
			}
		}
		if sf, ok := findings[r.FindingID]; ok {
			f.Comment, f.FindingContract, f.Severity = sf.Comment, sf.FindingContract, sf.Severity
		}
		if pt, ok := replaykit.SettledElsewhere(ledger, d.Owner, d.Repo, d.Author.Login, d.Number, r.At, f); ok {
			pr.ObservedCrossPRResettled++
			pairs = append(pairs, crossPRPair(d, 0, f, pt, "observed"))
			logf("%s: observed root %s:%d raises again a point %s settled on #%d (%s)", d.key(), r.File, r.Line, d.Author.Login, pt.PRNumber, pt.Source)
		}
		for _, e := range earlierPoints {
			if e.File == r.File {
				continue
			}
			point := e
			point.Replies = strings.Join(r.earlierReplies(e.FindingID, d, bots), " ")
			if _, ok := reconcile.SamePoint(f, point); ok {
				pr.ObservedCrossFileSamePoint++
				logf("%s: observed cross-file same point: %s:%d restates %s", d.key(), r.File, r.Line, e.FindingID)
				break
			}
		}
		earlierPoints = append(earlierPoints, reconcile.SettledPoint{FindingID: r.FindingID, File: r.File, Kind: findingKind(f), Text: f.Comment})
	}
	if notes, at := d.observedNotes(bots); len(notes) > 0 {
		for _, n := range notes {
			root, ok := replaykit.ExternalDuplicate(n.File, n.Line, n.Text, roots, at)
			if !ok {
				continue
			}
			pr.ObservedSummaryNotesNearExternalRoot++
			if root.Resolved {
				pr.ObservedSummaryNotesNearSettledRoot++
			}
		}
	}
	m.Roots += pr.ObservedRoots
	m.SameMarkerReposts += pr.ObservedSameMarkerReposts
	m.SameDefectReposts += pr.ObservedSameDefectReposts
	m.ExternalDuplicatesNear += pr.ObservedExternalDuplicatesNear
	m.ExternalDuplicatesOpen += pr.ObservedExternalDuplicatesOpen
	m.SummaryNotesNearExternalRoot += pr.ObservedSummaryNotesNearExternalRoot
	m.SummaryNotesNearSettledRoot += pr.ObservedSummaryNotesNearSettledRoot
	m.CrossFileSamePoint += pr.ObservedCrossFileSamePoint
	if pr.ObservedExternalDuplicatesNear > 0 {
		m.PRsWithExternalDuplicates++
	}
	return pairs
}

func findingKind(f payload.Finding) string {
	if f.FindingContract == nil {
		return ""
	}
	return f.FindingContract.FindingKind
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

func mean(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return round3(float64(sum) / float64(len(xs)))
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}

var csvHeader = []string{"pr", "rounds", "rounds_after_merge_commit", "rounds_skipped_unchanged_diff", "rounds_skipped_daily_cap", "rounds_missing_sidecar", "same_commit_rounds", "roots_posted", "same_marker_reposts", "same_defect_reposts", "same_round_duplicates", "fixed", "fixed_without_file_change", "fixed_without_hunk_change", "fixed_hunk_unknown", "fixed_file_change_unknown", "same_commit_resolves", "in_thread_replies", "double_notes", "threads_resolved", "threads_unresolved", "roots_resolved", "observed_roots", "observed_same_marker_reposts", "observed_same_defect_reposts", "same_commit_rounds_skipped", "nil_impact_bullets", "bare_label_titles", "external_duplicates_near", "external_duplicates_open", "external_duplicates_unknown", "external_matched", "external_replies", "observed_external_duplicates_near", "observed_external_duplicates_open",
	"held_library_claims", "held_regression_claims", "held_praise", "held_reasonless_notes", "library_claims_shown", "uncited_regression_claims_shown", "reasonless_notes_shown", "praise_shown", "confidence_labels_rendered", "leaked_finding_ids", "source_footers", "zero_status_lines_over_notes", "noop_summary_edits", "leading_markers",
	"external_settled", "settled_dropped", "body_covered", "summary_notes_near_external_root", "summary_notes_near_settled_root", "observed_summary_notes_near_external_root", "observed_summary_notes_near_settled_root", "observed_cross_file_same_point",
	"cross_pr_resettled", "cross_pr_settled", "observed_cross_pr_resettled", "verifier_held", "verifier_dropped", "refuted"}

func writeCSV(w io.Writer, rows []PRResult) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{r.PR, itoa(r.Rounds), itoa(r.RoundsAfterMergeCommit), itoa(r.RoundsSkippedUnchangedDiff), itoa(r.RoundsSkippedDailyCap), itoa(r.RoundsMissingSidecar), itoa(r.SameCommitRounds), itoa(r.RootsPosted), itoa(r.SameMarkerReposts), itoa(r.SameDefectReposts), itoa(r.SameRoundDuplicates), itoa(r.Fixed), itoa(r.FixedWithoutFileChange), itoa(r.FixedWithoutHunkChange), itoa(r.FixedHunkUnknown), itoa(r.FixedFileChangeUnknown), itoa(r.SameCommitResolves), itoa(r.InThreadReplies), itoa(r.DoubleNotes), itoa(r.ThreadsResolved), itoa(r.ThreadsUnresolved), itoa(r.RootsResolved), itoa(r.ObservedRoots), itoa(r.ObservedSameMarkerReposts), itoa(r.ObservedSameDefectReposts), itoa(r.SameCommitSkipped), itoa(r.NilImpactBullets), itoa(r.BareLabelTitles), itoa(r.ExternalDuplicatesNear), itoa(r.ExternalDuplicatesOpen), itoa(r.ExternalDuplicatesUnknown), itoa(r.ExternalMatched), itoa(r.ExternalReplies), itoa(r.ObservedExternalDuplicatesNear), itoa(r.ObservedExternalDuplicatesOpen),
			itoa(r.HeldLibraryClaims), itoa(r.HeldRegressionClaims), itoa(r.HeldPraise), itoa(r.HeldReasonless), itoa(r.LibraryClaimsShown), itoa(r.RegressionClaimsShown), itoa(r.ReasonlessNotesShown), itoa(r.PraiseShown), itoa(r.ConfidenceLabels), itoa(r.LeakedFindingIDs), itoa(r.SourceFooters), itoa(r.ZeroStatusLines), itoa(r.NoopSummaryEdits), itoa(r.LeadingMarkers),
			itoa(r.ExternalSettled), itoa(r.SettledDropped), itoa(r.BodyCovered), itoa(r.SummaryNotesNearExternalRoot), itoa(r.SummaryNotesNearSettledRoot), itoa(r.ObservedSummaryNotesNearExternalRoot), itoa(r.ObservedSummaryNotesNearSettledRoot), itoa(r.ObservedCrossFileSamePoint),
			itoa(r.CrossPRResettled), itoa(r.CrossPRSettled), itoa(r.ObservedCrossPRResettled), itoa(r.VerifierHeld), itoa(r.VerifierDropped), itoa(r.Refuted)}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func itoa(n int) string { return strconv.Itoa(n) }
