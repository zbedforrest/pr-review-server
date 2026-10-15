package replaykit

import (
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// SettledPointsOf reads the points the PR author settled on this PR off
// the dump, as the live system records them: an author verdict under a
// PRism root (the finding's contract from the sidecar of the commit the
// root was posted on, when the loader has it), and another reviewer's
// thread the author answered that the reviewer withdrew or that is
// resolved now with the author standing by the code (publisher.ThreadSettled).
// Each point is dated by the reply that settled it.
func (d *PRDump) SettledPointsOf(bots map[string]bool, finding func(sha7, id string) (payload.Finding, bool)) []db.SettledPoint {
	author := strings.ToLower(d.Author.Login)
	if author == "" {
		return nil
	}
	var out []db.SettledPoint
	var comments []reconcile.ExternalComment
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) == 0 {
			continue
		}
		root := t.Comments.Nodes[0]
		line := t.OriginalLine
		if line == 0 {
			line = t.Line
		}
		for _, c := range t.Comments.Nodes {
			ec := reconcile.ExternalComment{ID: int64(c.DatabaseID), Author: ActorLogin(c.Author), Body: c.Body, Path: t.Path, Line: line, CreatedAt: c.CreatedAt}
			if c.ReplyTo != nil {
				ec.InReplyToID = int64(c.ReplyTo.DatabaseID)
			}
			comments = append(comments, ec)
		}
		if root.ReplyTo != nil || !bots[BareLogin(ActorLogin(root.Author))] {
			continue
		}
		id, ok := publisher.FindingIDFromBody(root.Body)
		if !ok {
			continue
		}
		for _, c := range t.Comments.Nodes[1:] {
			if strings.ToLower(ActorLogin(c.Author)) != author || !publisher.IsVerdict(c.Body) {
				continue
			}
			row := db.PublishedFinding{RepoOwner: d.Owner, RepoName: d.Repo, PRNumber: d.Number, Kind: db.PublishedKindFinding, Fingerprint: id, CommentID: int64(root.DatabaseID),
				CommentText: StripMarkup(root.Body)}
			if finding != nil && len(root.OriginalCommit.Oid) >= 7 {
				if f, ok := finding(root.OriginalCommit.Oid[:7], id); ok {
					row.CommentText = f.Comment
					if c := f.FindingContract; c != nil {
						row.CommentText = strings.TrimSpace(strings.Join([]string{f.Comment, c.Headline, c.CurrentImpact}, " "))
						row.FindingKind = c.FindingKind
						row.Subjects = strings.Join(lowered(reconcile.SpecificSubjects(c)), ",")
					}
				}
			}
			if row.Subjects == "" {
				row.Subjects = strings.Join(reconcile.CodeIdentifiers(root.Body), ",")
			}
			out = append(out, publisher.SettledPointFromRow(row, author, db.SettledSourceVerdict, c.Body, c.CreatedAt))
			break
		}
	}
	roots := reconcile.ParseExternalComments(comments, nil)
	resolved := map[int64]bool{}
	for _, t := range d.ReviewThreads.Nodes {
		if len(t.Comments.Nodes) > 0 {
			resolved[int64(t.Comments.Nodes[0].DatabaseID)] = t.Resolved()
		}
	}
	for _, th := range reconcile.BuildExternalThreads(comments, roots, author) {
		if bots[BareLogin(strings.ToLower(th.Root.Source.Label()))] {
			continue
		}
		if !publisher.ThreadSettled(th, resolved[th.Root.CommentID]) {
			continue
		}
		out = append(out, publisher.SettledPointOfThread(d.Owner, d.Repo, d.Number, author, th, th.SettledAt))
	}
	return out
}

func lowered(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.ToLower(n))
	}
	return out
}

// SettledElsewhere finds the point the PR author settled on another pull
// request of the repo within the window before at that finding f raises
// again, under the publisher's rule. The store is the same one the
// publisher under test reads.
func SettledElsewhere(store publisher.SettledPointStore, owner, repo, author string, number int, at time.Time, f payload.Finding) (db.SettledPoint, bool) {
	points, err := store.ListSettledPoints(owner, repo, author, at.Add(-publisher.SettledWindow), at)
	if err != nil || len(points) == 0 {
		return db.SettledPoint{}, false
	}
	kept := points[:0]
	for _, p := range points {
		if p.PRNumber != number {
			kept = append(kept, p)
		}
	}
	return publisher.MatchSettledElsewhere(f, kept)
}
