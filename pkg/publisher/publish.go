package publisher

import (
	"context"
	"fmt"
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
	// notes posted for reopens and severity changes, ThreadReplyFailures the
	// ones GitHub rejected. Ledger policy only.
	Reopened            int
	ThreadReplies       int
	ThreadReplyFailures int
}

const summaryFingerprint = "summary"

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Publish posts one review round. The ledger decision table (see doc.go) is
// the default; Policy.LegacyLedger selects the pre-memory publisher.
func (p *Publisher) Publish(ctx context.Context, r Round) (Report, error) {
	if r.Previous == nil {
		prev, err := p.Ledger.GetPublishedFindingsForPR(r.Owner, r.Repo, r.Number)
		if err != nil {
			return Report{}, fmt.Errorf("load published findings: %w", err)
		}
		r.Previous = prev
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

	postedThisRound, err := p.postInline(ctx, r, sel, prior, now, &rep)
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

	if err := p.writeSummary(ctx, r, sel, summaryRow, now, &rep); err != nil {
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
	return rep, nil
}

// postInline creates the review that carries this round's inline comments
// and records one open finding row per comment, noting the hygiene of each
// post against the row the ledger held before. It returns the comment id
// each posted finding received.
func (p *Publisher) postInline(ctx context.Context, r Round, sel Selection, prior map[string]*db.PublishedFinding, now time.Time, rep *Report) (map[string]int64, error) {
	postedThisRound := map[string]int64{}
	if len(sel.Inline) == 0 {
		return postedThisRound, nil
	}
	inputs := make([]ReviewCommentInput, 0, len(sel.Inline))
	for _, f := range sel.Inline {
		inputs = append(inputs, ReviewCommentInput{Path: f.File, Line: f.Line, Body: RenderInline(f, r.sourceTag(f.ID), r.AgentLinkBase, r.BadgeBaseURL)})
	}
	reviewID, commentIDs, err := p.GH.CreateReview(ctx, r.Owner, r.Repo, r.Number, r.HeadSHA, "", inputs)
	if err != nil {
		return postedThisRound, fmt.Errorf("create review: %w", err)
	}
	rep.ReviewID = reviewID
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
			CommentID: commentID, ReviewID: reviewID,
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
// through the ledger or its marker) or creates it, and records the summary
// row for this round.
func (p *Publisher) writeSummary(ctx context.Context, r Round, sel Selection, summaryRow *db.PublishedFinding, now time.Time, rep *Report) error {
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
				return fmt.Errorf("edit summary comment: %w", err)
			}
			summaryCommentID = 0
		}
	}
	if summaryCommentID == 0 {
		id, err := p.GH.CreateIssueComment(ctx, r.Owner, r.Repo, r.Number, summary)
		if err != nil {
			return fmt.Errorf("create summary comment: %w", err)
		}
		summaryCommentID = id
	}
	rep.SummaryCommentID = summaryCommentID
	summaryLedger.CommentID = summaryCommentID
	if err := p.Ledger.UpsertPublishedFinding(summaryLedger); err != nil {
		return fmt.Errorf("record summary comment: %w", err)
	}
	return nil
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
