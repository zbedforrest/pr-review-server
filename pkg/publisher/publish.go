package publisher

import (
	"context"
	"fmt"
	"time"

	"pr-review-server/db"
)

type ReviewCommentInput struct {
	Path      string
	Line      int
	StartLine int
	Body      string
}

type GitHub interface {
	CreateReview(ctx context.Context, owner, repo string, number int, commitSHA, body string, comments []ReviewCommentInput) (int64, []int64, error)
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) error
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
		case db.PublishedKindFinding:
			published[row.Fingerprint] = row
		}
	}
	if r.RoundNumber == 0 {
		r.RoundNumber = 1
		if summaryRow != nil {
			r.RoundNumber = summaryRow.Rounds + 1
		}
	}
	alreadyPublished := make(map[string]bool, len(published))
	for id := range published {
		alreadyPublished[id] = true
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
	if summaryRow != nil {
		if err := p.GH.EditIssueComment(ctx, r.Owner, r.Repo, summaryRow.CommentID, summary); err != nil {
			return rep, fmt.Errorf("edit summary comment: %w", err)
		}
		rep.SummaryCommentID = summaryRow.CommentID
		summaryLedger.CommentID = summaryRow.CommentID
		summaryLedger.ReviewedSHA = summaryRow.ReviewedSHA
	} else {
		id, err := p.GH.CreateIssueComment(ctx, r.Owner, r.Repo, r.Number, summary)
		if err != nil {
			return rep, fmt.Errorf("create summary comment: %w", err)
		}
		rep.SummaryCommentID = id
		summaryLedger.CommentID = id
	}
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

	for _, f := range r.currentFindings() {
		row, ok := published[f.ID]
		if !ok || row.LastSeenSHA == r.HeadSHA {
			continue
		}
		updated := *row
		updated.LastSeenSHA = r.HeadSHA
		if err := p.Ledger.UpsertPublishedFinding(&updated); err != nil {
			return rep, fmt.Errorf("update last seen for %s: %w", f.ID, err)
		}
	}
	return rep, nil
}
