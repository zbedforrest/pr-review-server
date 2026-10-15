package publisher

import (
	"context"
	"log"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// SettledWindow is how long a point the author settled on one pull request
// stays settled for their later pull requests in the repo.
const SettledWindow = 30 * 24 * time.Hour

// SettledPointStore is the optional slice of the ledger that remembers the
// points an author settled across pull requests. Without it the cross-PR
// pass is skipped and nothing is recorded.
type SettledPointStore interface {
	RecordSettledPoint(*db.SettledPoint) error
	ListSettledPoints(owner, repo, author string, since, until time.Time) ([]db.SettledPoint, error)
}

// settledPoints are the author's settled points within the window: those
// of other pull requests of the repo (elsewhere), and the threads other
// reviewers withdrew on this one (here), which no ledger row stands for. A
// point of this PR that PRism itself posted is the ledger's business.
func (p *Publisher) settledPoints(r Round, now time.Time) (elsewhere, here []db.SettledPoint) {
	store, ok := p.Ledger.(SettledPointStore)
	if !ok || !p.Policy.CrossPRSettled || strings.TrimSpace(r.Author) == "" {
		return nil, nil
	}
	points, err := store.ListSettledPoints(r.Owner, r.Repo, r.Author, now.Add(-SettledWindow), now)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: list settled points: %v; cross-PR memory skipped this round", r.Owner, r.Repo, r.Number, err)
		return nil, nil
	}
	for _, pt := range points {
		switch {
		case pt.PRNumber != r.Number:
			elsewhere = append(elsewhere, pt)
		case pt.Fingerprint == "":
			here = append(here, pt)
		}
	}
	return elsewhere, here
}

// MatchSettledElsewhere pairs a finding with the point the author settled
// on another pull request that it raises again, under the cross-PR rule
// (reconcile.SameEarlierPoint). A CRITICAL security_risk is exempt, as
// decision 4 exempts it on its own PR.
func MatchSettledElsewhere(f payload.Finding, points []db.SettledPoint) (db.SettledPoint, bool) {
	if len(points) == 0 || isPseudoFile(f.File) || criticalSecurity(f) {
		return db.SettledPoint{}, false
	}
	earlier := make([]reconcile.EarlierPoint, 0, len(points))
	for _, pt := range points {
		earlier = append(earlier, reconcile.EarlierPoint{Subjects: splitSubjects(pt.Subjects), Text: pt.Text})
	}
	i, _, ok := reconcile.MatchEarlier(f, earlier)
	if !ok {
		return db.SettledPoint{}, false
	}
	return points[i], true
}

func isPseudoFile(file string) bool { return file == summaryFile || file == checkFile }

func criticalSecurity(f payload.Finding) bool {
	return f.Severity == "critical" && f.FindingContract != nil && f.FindingContract.FindingKind == "security_risk"
}

// withoutSettledElsewhere drops every publishable finding the ledger does
// not know that raises again a point the author settled on an earlier pull
// request, or, for a finding above the inline bar, in a thread another
// reviewer withdrew on this one (a folded note against such a thread is
// the note reconcile's business): a settled row is written in its place
// and the summary names the settled thread. Nothing is posted, nothing is
// counted.
func (p *Publisher) withoutSettledElsewhere(r *Round, rows map[string]*db.PublishedFinding, now time.Time) ([]*db.PublishedFinding, int) {
	elsewhere, here := p.settledPoints(*r, now)
	if len(elsewhere) == 0 && len(here) == 0 {
		return nil, 0
	}
	var writes []*db.PublishedFinding
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if _, known := rows[f.ID]; known || !Publishable(f) {
			kept = append(kept, f)
			continue
		}
		points := elsewhere
		if Shown(f) {
			points = append(append([]db.SettledPoint(nil), elsewhere...), here...)
		}
		pt, ok := MatchSettledElsewhere(f, points)
		if !ok {
			kept = append(kept, f)
			continue
		}
		log.Printf("[PUBLISH] %s/%s#%d: finding %s raises again a point %s settled on #%d (%s), dropped", r.Owner, r.Repo, r.Number, f.ID, r.Author, pt.PRNumber, pt.Source)
		writes = append(writes, &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateSettled, PublishedAt: now, Line: f.Line,
			CommentText: f.Comment, FindingKind: findingKindOf(f), Subjects: subjectsColumn(f),
			SettledPRNumber: pt.PRNumber, SettledCommentID: pt.CommentID,
		})
		r.SettledMatches = append(r.SettledMatches, SettledMatch{Finding: f, PRNumber: pt.PRNumber, CommentID: pt.CommentID})
	}
	r.Findings = kept
	return writes, len(writes)
}

// rememberSettled lists, for the summary, the findings of this round whose
// row is settled: the point is still raised and still settled elsewhere.
// It runs before withoutTerminal drops them.
func rememberSettled(r *Round, rows map[string]*db.PublishedFinding) {
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || row.State != db.PublishedStateSettled || row.SettledPRNumber == 0 {
			continue
		}
		r.SettledMatches = append(r.SettledMatches, SettledMatch{Finding: f, PRNumber: row.SettledPRNumber, CommentID: row.SettledCommentID})
	}
}

// recordExternalSettled remembers every other reviewer's thread the author
// answered that the reviewer then withdrew or that is resolved now, so the
// author's later pull requests do not meet the point again. Recording is
// keyed by the thread, so a round that sees it again writes nothing.
func (p *Publisher) recordExternalSettled(ctx context.Context, r Round, threads *threadIndex, now time.Time) {
	store, ok := p.Ledger.(SettledPointStore)
	if !ok || !p.Policy.CrossPRSettled || strings.TrimSpace(r.Author) == "" {
		return
	}
	for _, t := range r.ExternalThreads {
		resolved := false
		if listed, found := threads.lookup(ctx, t.Root.CommentID); found {
			resolved = listed.Resolved
		}
		if !t.Settled(resolved) {
			continue
		}
		source := db.SettledSourceResolved
		if t.Withdrawn {
			source = db.SettledSourceWithdrawn
		}
		at := t.SettledAt
		if at.IsZero() {
			at = now
		}
		rootText := strings.TrimSpace(t.Root.Title + " " + t.Root.Body)
		reason := strings.Join(t.AuthorReplies, " ")
		if err := store.RecordSettledPoint(&db.SettledPoint{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number, CommentID: t.Root.CommentID, Author: r.Author, Source: source,
			File: t.Root.File, Subjects: strings.Join(reconcile.CodeIdentifiers(rootText), ","), Headline: t.Root.Title,
			Text: rootText + " " + reason, Reason: reason, SettledAt: at,
		}); err != nil {
			log.Printf("[PUBLISH] %s/%s#%d: record settled thread %d: %v", r.Owner, r.Repo, r.Number, t.Root.CommentID, err)
		}
	}
}

// SettledPointFromRow is the settled point a PRism thread becomes when the
// author's reply settles it: the row's prose and subjects, the reply as
// the reason, dated by the reply.
func SettledPointFromRow(row db.PublishedFinding, author, source, reason string, at time.Time) db.SettledPoint {
	text := StripRendered(row.CommentText)
	return db.SettledPoint{
		RepoOwner: row.RepoOwner, RepoName: row.RepoName, PRNumber: row.PRNumber, CommentID: row.CommentID, Author: author, Source: source,
		Fingerprint: row.Fingerprint, File: payload.FingerprintFile(row.Fingerprint), FindingKind: row.FindingKind, Subjects: row.Subjects,
		Headline: strings.TrimSpace(FirstSentence(text)), Text: strings.TrimSpace(text + " " + reason), Reason: reason, SettledAt: at,
	}
}
