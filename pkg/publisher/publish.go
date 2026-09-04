package publisher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"pr-review-server/db"
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
}

const summaryFingerprint = "summary"

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Publisher) Publish(ctx context.Context, r Round) (Report, error) {
	if r.Previous == nil {
		prev, err := p.Ledger.GetPublishedFindingsForPR(r.Owner, r.Repo, r.Number)
		if err != nil {
			return Report{}, fmt.Errorf("load published findings: %w", err)
		}
		r.Previous = prev
	}

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
	d := r.diff()
	rep := Report{InlinePosted: len(sel.Inline), Annotations: len(sel.Annotations), StillOpen: d.StillOpen, Fixed: d.Fixed}
	now := p.now()

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
				return rep, fmt.Errorf("edit summary comment: %w", err)
			}
			summaryCommentID = 0
		}
	}
	if summaryCommentID == 0 {
		id, err := p.GH.CreateIssueComment(ctx, r.Owner, r.Repo, r.Number, summary)
		if err != nil {
			return rep, fmt.Errorf("create summary comment: %w", err)
		}
		summaryCommentID = id
	}
	rep.SummaryCommentID = summaryCommentID
	summaryLedger.CommentID = summaryCommentID
	if err := p.Ledger.UpsertPublishedFinding(summaryLedger); err != nil {
		return rep, fmt.Errorf("record summary comment: %w", err)
	}

	if len(sel.Inline) > 0 {
		inputs := make([]ReviewCommentInput, 0, len(sel.Inline))
		for _, f := range sel.Inline {
			inputs = append(inputs, ReviewCommentInput{Path: f.File, Line: f.Line, Body: RenderInline(f, r.sourceTag(f.ID), r.AgentLinkBase)})
		}
		reviewID, commentIDs, err := p.GH.CreateReview(ctx, r.Owner, r.Repo, r.Number, r.HeadSHA, "", inputs)
		if err != nil {
			return rep, fmt.Errorf("create review: %w", err)
		}
		rep.ReviewID = reviewID
		for i, f := range sel.Inline {
			var commentID int64
			if i < len(commentIDs) {
				commentID = commentIDs[i]
			}
			if err := p.Ledger.UpsertPublishedFinding(&db.PublishedFinding{
				RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
				Kind: db.PublishedKindFinding, Fingerprint: f.ID,
				SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
				ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
				CommentID: commentID, ReviewID: reviewID,
				State: db.PublishedStateOpen, PublishedAt: now,
			}); err != nil {
				return rep, fmt.Errorf("record finding %s: %w", f.ID, err)
			}
		}
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
		written[f.ID] = true
	}

	present := map[string]bool{}
	for _, f := range r.currentFindings() {
		present[f.ID] = true
		row, ok := published[f.ID]
		if !ok || written[f.ID] || row.LastSeenSHA == r.HeadSHA {
			continue
		}
		refreshed := *row
		refreshed.LastSeenSHA = r.HeadSHA
		if err := p.Ledger.UpsertPublishedFinding(&refreshed); err != nil {
			return rep, fmt.Errorf("refresh finding %s: %w", f.ID, err)
		}
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
	}
	return rep, nil
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}
