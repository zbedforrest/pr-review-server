package publisher

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

type ReviewCommentInput struct {
	Path      string
	Line      int
	StartLine int
	Body      string
}

type IssueComment struct {
	ID   int64
	Body string
}

type GitHub interface {
	CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []ReviewCommentInput) (int64, []int64, error)
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error
	ListIssueComments(ctx context.Context, owner, repo string, number int) ([]IssueComment, error)
}

// ThreadReplier is the optional slice of GitHub the ledger policy uses to
// speak inside a thread it already owns (a reopen or a severity change). A
// GitHub without it changes the ledger silently.
type ThreadReplier interface {
	PostReply(ctx context.Context, owner, repo string, number int, rootCommentID int64, body string) (int64, error)
}

type Ledger interface {
	UpsertPublishedFinding(*db.PublishedFinding) error
	GetPublishedFindingsForPR(owner, repo string, number int) ([]db.PublishedFinding, error)
}

type Publisher struct {
	GH     GitHub
	Ledger Ledger
	Policy Policy
	Now    func() time.Time
}

type Report struct {
	SummaryCommentID int64
	ReviewID         int64
	InlinePosted     int
	Annotations      int
	StillOpen        int
	Fixed            int
	Confidence       int
	Hygiene          Hygiene
	// Reopened counts fixed rows that came back; ThreadReplies the in-thread
	// notes posted for fixes, reopens and severity changes, ThreadReplyFailures
	// the ones GitHub rejected. Ledger policy only.
	Reopened            int
	ThreadReplies       int
	ThreadReplyFailures int
	// ThreadsResolved and ThreadsUnresolved count the GitHub threads the round
	// closed on a fix and reopened on a return; ThreadResolveFailures the
	// calls GitHub rejected or the threads it did not list.
	ThreadsResolved       int
	ThreadsUnresolved     int
	ThreadResolveFailures int
}

const summaryFingerprint = "summary"

// ErrHeadAlreadyPublished is returned by Publish when the summary row already
// records a completed round for the head; a re-run of the same commit (API or
// legacy trigger) would otherwise repost and resolve rows with no code change.
var ErrHeadAlreadyPublished = errors.New("publisher: head already published")

// ErrSummaryMoved is returned by RefreshSummary when a newer round rewrote
// the summary while the refresh was being built; the refresh is dropped so it
// cannot overwrite the newer round's summary with a stale one.
var ErrSummaryMoved = errors.New("publisher: summary moved to a newer round during refresh")

// HeadPublished reports whether a publication round for head completed. The
// summary row is written after the inline comments, so its LastSeenSHA names
// the last head that was published in full.
func HeadPublished(previous []db.PublishedFinding, head string) bool {
	if head == "" {
		return false
	}
	for _, row := range previous {
		if row.Kind == db.PublishedKindSummary && strings.EqualFold(row.LastSeenSHA, head) {
			return true
		}
	}
	return false
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Publish posts one review round. The ledger decision table (see doc.go) is
// the default; Policy.LegacyLedger selects the pre-memory publisher.
func (p *Publisher) Publish(ctx context.Context, r Round) (Report, error) {
	// The guard reads the ledger afresh even when the caller supplied rows,
	// so a round that completed since the caller loaded them is seen.
	if r.Previous == nil || !p.Policy.RepublishSameCommit {
		prev, err := p.Ledger.GetPublishedFindingsForPR(r.Owner, r.Repo, r.Number)
		if err != nil {
			return Report{}, fmt.Errorf("load published findings: %w", err)
		}
		if !p.Policy.RepublishSameCommit && HeadPublished(prev, r.HeadSHA) {
			return Report{}, ErrHeadAlreadyPublished
		}
		if r.Previous == nil {
			r.Previous = prev
		}
	}
	if p.Policy.LegacyLedger {
		return p.publishLegacy(ctx, r)
	}
	return p.publishLedger(ctx, r)
}

func (p *Publisher) publishLegacy(ctx context.Context, r Round) (Report, error) {
	var summaryRow *db.PublishedFinding
	published := map[string]*db.PublishedFinding{}
	for i := range r.Previous {
		row := &r.Previous[i]
		switch row.Kind {
		case db.PublishedKindSummary:
			summaryRow = row
		case db.PublishedKindFinding, db.PublishedKindAnnotation:
			if row.State == db.PublishedStateOpen {
				published[row.Fingerprint] = row
			}
		}
	}
	prior := priorRows(r.Previous)
	r.Findings = WithoutDismissed(r.Findings, r.Previous)
	r.LegacyTitles = p.Policy.LegacyTitles
	if r.RoundNumber == 0 {
		r.RoundNumber = 1
		if summaryRow != nil {
			r.RoundNumber = summaryRow.Rounds + 1
		}
	}
	alreadyPublished := make(map[string]bool, len(published))
	for id, row := range published {
		if row.Kind == db.PublishedKindFinding {
			alreadyPublished[id] = true
		}
	}

	sel := Select(r.Findings, alreadyPublished, r.Commentable, p.Policy)
	r.ShowUnverified = p.Policy.ShowUnverified
	d := r.diff()
	rep := Report{InlinePosted: len(sel.Inline), Annotations: len(sel.Annotations), StillOpen: d.StillOpen, Fixed: d.Fixed,
		Confidence: Confidence(r.Findings, r.RequiredCheckViolated)}
	now := p.now()

	postedThisRound, err := p.postInline(ctx, r, sel, prior, now, &rep, nil)
	if err != nil {
		return rep, err
	}

	if r.InlineComments == nil {
		r.InlineComments = map[string]int64{}
	}
	for id, row := range published {
		if row.Kind == db.PublishedKindFinding && row.CommentID != 0 {
			r.InlineComments[id] = row.CommentID
		}
	}
	for id, cid := range postedThisRound {
		r.InlineComments[id] = cid
	}

	summaryLedger, err := p.writeSummary(ctx, r, sel, summaryRow, now, &rep)
	if err != nil {
		return rep, err
	}

	written := map[string]bool{}
	for _, f := range sel.Inline {
		written[f.ID] = true
	}
	for _, f := range sel.Annotations {
		if written[f.ID] {
			continue
		}
		row := &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateOpen, PublishedAt: now,
		}
		if prev, ok := published[f.ID]; ok {
			// A finding already posted inline keeps its comment; an annotation
			// row just advances its last-seen sha.
			row.Kind, row.ReviewedSHA, row.PublishedAt = prev.Kind, prev.ReviewedSHA, prev.PublishedAt
			row.CommentID, row.ReviewID, row.ThreadNodeID = prev.CommentID, prev.ReviewID, prev.ThreadNodeID
		}
		if err := p.Ledger.UpsertPublishedFinding(row); err != nil {
			return rep, fmt.Errorf("record annotation %s: %w", f.ID, err)
		}
		rep.Hygiene.noteWritten(f, prior[f.ID])
		written[f.ID] = true
	}

	present := map[string]bool{}
	for _, f := range r.activeClaims() {
		present[f.ID] = true
		row, ok := published[f.ID]
		if !ok || written[f.ID] || row.LastSeenSHA == r.HeadSHA {
			continue
		}
		refreshed := *row
		refreshed.LastSeenSHA = r.HeadSHA
		// W1-1 will clamp severity per the ledger; until then the row follows
		// the latest assertion, so an escalation is counted once.
		if severityRank(f.Severity) > severityRank(row.Severity) {
			refreshed.Severity = f.Severity
		}
		if err := p.Ledger.UpsertPublishedFinding(&refreshed); err != nil {
			return rep, fmt.Errorf("refresh finding %s: %w", f.ID, err)
		}
		rep.Hygiene.noteWritten(f, row)
	}
	for id, row := range published {
		if written[id] || present[id] {
			continue
		}
		resolved := *row
		resolved.State = db.PublishedStateResolved
		if err := p.Ledger.UpsertPublishedFinding(&resolved); err != nil {
			return rep, fmt.Errorf("resolve finding %s: %w", id, err)
		}
		rep.Hygiene.noteResolved(row, r.HeadSHA, r.changedFilesSince(row.LastSeenSHA))
	}
	// Written last: HeadPublished reads this row as proof the round completed.
	if err := p.Ledger.UpsertPublishedFinding(summaryLedger); err != nil {
		return rep, fmt.Errorf("record summary comment: %w", err)
	}
	return rep, nil
}

// postInline creates the review that carries this round's inline comments
// and records one open finding row per comment, noting the hygiene of each
// post against the row the ledger held before. It returns the comment id
// each posted finding received. With a thread index the new rows also get
// the node id of the thread each comment opened.
func (p *Publisher) postInline(ctx context.Context, r Round, sel Selection, prior map[string]*db.PublishedFinding, now time.Time, rep *Report, threads *threadIndex) (map[string]int64, error) {
	postedThisRound := map[string]int64{}
	if len(sel.Inline) == 0 {
		return postedThisRound, nil
	}
	inputs := make([]ReviewCommentInput, 0, len(sel.Inline))
	for _, f := range sel.Inline {
		inputs = append(inputs, ReviewCommentInput{Path: f.File, Line: f.Line, Body: renderInline(f, r.sourceTag(f.ID), r.AgentLinkBase, r.BadgeBaseURL, r.OptOutURL, r.LegacyTitles)})
	}
	reviewID, commentIDs, err := p.GH.CreateReview(ctx, r.Owner, r.Repo, r.Number, r.HeadSHA, "", inputs)
	if err != nil {
		return postedThisRound, fmt.Errorf("create review: %w", err)
	}
	rep.ReviewID = reviewID
	if threads != nil {
		threads.reload(ctx)
	}
	for i, f := range sel.Inline {
		var commentID int64
		if i < len(commentIDs) {
			commentID = commentIDs[i]
		}
		postedThisRound[f.ID] = commentID
		if err := p.Ledger.UpsertPublishedFinding(&db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindFinding, Fingerprint: f.ID,
			SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			CommentID: commentID, ReviewID: reviewID, ThreadNodeID: threads.nodeIDOf(ctx, commentID),
			State: db.PublishedStateOpen, PublishedAt: now,
			CommentText: f.Comment, FindingKind: findingKindOf(f), Subjects: subjectsColumn(f),
		}); err != nil {
			return postedThisRound, fmt.Errorf("record finding %s: %w", f.ID, err)
		}
		// Notes follow the ledger write: a post whose row failed is a publish
		// error, and the retry counts it when it reposts.
		rep.Hygiene.notePosted(f, prior[f.ID])
		rep.Hygiene.noteWritten(f, prior[f.ID])
	}
	return postedThisRound, nil
}

// writeSummary renders the sticky summary, edits the existing comment (found
// through the ledger or its marker) or creates it, and returns the summary
// row for this round. The caller writes that row after every other ledger
// write: HeadPublished reads it as proof the round completed.
func (p *Publisher) writeSummary(ctx context.Context, r Round, sel Selection, summaryRow *db.PublishedFinding, now time.Time, rep *Report) (*db.PublishedFinding, error) {
	summary := RenderSummary(r, sel)
	summaryLedger := &db.PublishedFinding{
		RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
		Kind: db.PublishedKindSummary, Fingerprint: summaryFingerprint,
		ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA, Rounds: r.RoundNumber,
		State: db.PublishedStateOpen, PublishedAt: now,
	}
	summaryCommentID := int64(0)
	if summaryRow != nil {
		summaryCommentID = summaryRow.CommentID
		summaryLedger.ReviewedSHA = summaryRow.ReviewedSHA
	} else if existing, err := p.GH.ListIssueComments(ctx, r.Owner, r.Repo, r.Number); err == nil {
		for _, c := range existing {
			if strings.Contains(c.Body, SummaryMarker) {
				summaryCommentID = c.ID
				break
			}
		}
	}
	if summaryCommentID != 0 {
		if err := p.GH.EditIssueComment(ctx, r.Owner, r.Repo, summaryCommentID, summary); err != nil {
			if !isNotFound(err) {
				return nil, fmt.Errorf("edit summary comment: %w", err)
			}
			summaryCommentID = 0
		}
	}
	if summaryCommentID == 0 {
		id, err := p.GH.CreateIssueComment(ctx, r.Owner, r.Repo, r.Number, summary)
		if err != nil {
			return nil, fmt.Errorf("create summary comment: %w", err)
		}
		summaryCommentID = id
	}
	rep.SummaryCommentID = summaryCommentID
	summaryLedger.CommentID = summaryCommentID
	return summaryLedger, nil
}

// RefreshSummary re-renders the sticky summary from the round the ledger last
// published, posting and recording nothing else. The reply path calls it after
// a concession so the summary stops listing the conceded finding at once
// instead of at the next push. The round is the one BuildPublishRound makes
// from the stored payload of the published head; the ledger supplies the
// round number, the inline links and the new / still open split, and the
// fixed count is carried over from the summary as it stands.
func (p *Publisher) RefreshSummary(ctx context.Context, r Round) error {
	if r.Previous == nil {
		prev, err := p.Ledger.GetPublishedFindingsForPR(r.Owner, r.Repo, r.Number)
		if err != nil {
			return fmt.Errorf("load published findings: %w", err)
		}
		r.Previous = prev
	}
	var summaryRow *db.PublishedFinding
	for i := range r.Previous {
		if r.Previous[i].Kind == db.PublishedKindSummary {
			summaryRow = &r.Previous[i]
		}
	}
	if summaryRow == nil || summaryRow.CommentID == 0 {
		return nil
	}
	if r.HeadSHA == "" {
		r.HeadSHA = summaryRow.LastSeenSHA
	}
	r.RoundNumber = summaryRow.Rounds
	r.Findings = WithoutDismissed(r.Findings, r.Previous)
	r.LegacyTitles = p.Policy.LegacyTitles
	r.ShowUnverified = p.Policy.ShowUnverified
	if r.InlineComments == nil {
		r.InlineComments = map[string]int64{}
	}
	for _, row := range r.Previous {
		if row.Kind == db.PublishedKindFinding && row.CommentID != 0 {
			r.InlineComments[row.Fingerprint] = row.CommentID
		}
	}
	existing, err := p.GH.ListIssueComments(ctx, r.Owner, r.Repo, r.Number)
	if err != nil {
		return fmt.Errorf("read summary comment: %w", err)
	}
	fixed, found := 0, false
	for _, c := range existing {
		if c.ID == summaryRow.CommentID {
			found = true
			if d, ok := parseSinceLastReview(c.Body); ok {
				fixed = d.Fixed
			}
		}
	}
	if !found {
		return nil
	}
	d := r.ledgerTransitions()
	d.Fixed = fixed
	r.transitions = &d
	body := RenderSummary(r, Selection{})
	if moved, err := p.summaryMoved(r, summaryRow); err != nil {
		return err
	} else if moved {
		return ErrSummaryMoved
	}
	return p.GH.EditIssueComment(ctx, r.Owner, r.Repo, summaryRow.CommentID, body)
}

func (p *Publisher) summaryMoved(r Round, built *db.PublishedFinding) (bool, error) {
	rows, err := p.Ledger.GetPublishedFindingsForPR(r.Owner, r.Repo, r.Number)
	if err != nil {
		return false, fmt.Errorf("reload published findings: %w", err)
	}
	for _, row := range rows {
		if row.Kind == db.PublishedKindSummary {
			return row.Rounds != built.Rounds || !strings.EqualFold(row.LastSeenSHA, built.LastSeenSHA), nil
		}
	}
	return true, nil
}

// ledgerTransitions splits the shown findings the way the round diff did:
// findings first published at this head are new, older open rows are still
// open, and a folded note first posted at this head is neither.
// Fixed rows are not in the ledger as a per-round fact, so it stays 0 here.
func (r Round) ledgerTransitions() roundDiff {
	current := map[string]bool{}
	for _, f := range r.currentFindings() {
		current[f.ID] = true
	}
	shown := map[string]bool{}
	for _, f := range append(append(r.currentFindings(), r.lowerSeverityNotes()...), r.unverifiedNotes()...) {
		shown[f.ID] = true
	}
	var d roundDiff
	for _, row := range r.Previous {
		if (row.Kind != db.PublishedKindFinding && row.Kind != db.PublishedKindAnnotation) || row.State != db.PublishedStateOpen || !shown[row.Fingerprint] {
			continue
		}
		switch {
		case !strings.EqualFold(row.ReviewedSHA, r.HeadSHA):
			d.StillOpen++
		case current[row.Fingerprint]:
			d.New++
		}
	}
	return d
}

var sinceLastReviewRe = regexp.MustCompile(`\*\*Since last review:\*\* (\d+) new · (\d+) still open · (\d+) fixed`)

func parseSinceLastReview(body string) (roundDiff, bool) {
	m := sinceLastReviewRe.FindStringSubmatch(body)
	if m == nil {
		return roundDiff{}, false
	}
	n, _ := strconv.Atoi(m[1])
	s, _ := strconv.Atoi(m[2])
	f, _ := strconv.Atoi(m[3])
	return roundDiff{New: n, StillOpen: s, Fixed: f}, true
}

// WithoutDismissed drops findings the ledger records as settled by exact
// fingerprint: dismissed, contested or external rows. The ledger policy
// applies the same rule after aliasing (see doc.go); this exact form serves
// the legacy publisher and the dashboard's confidence score.
func WithoutDismissed(findings []payload.Finding, previous []db.PublishedFinding) []payload.Finding {
	dismissed := map[string]bool{}
	for _, row := range previous {
		if terminalState(row.State) && (row.Kind == db.PublishedKindFinding || row.Kind == db.PublishedKindAnnotation) {
			dismissed[row.Fingerprint] = true
		}
	}
	if len(dismissed) == 0 {
		return findings
	}
	kept := make([]payload.Finding, 0, len(findings))
	for _, f := range findings {
		if !dismissed[f.ID] {
			kept = append(kept, f)
		}
	}
	return kept
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}
