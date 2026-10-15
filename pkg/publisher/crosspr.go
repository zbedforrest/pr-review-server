package publisher

import (
	"context"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"pr-review-server/db"
	"pr-review-server/pkg/publisher/replytext"
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
// (reconcile.SameEarlierPoint), in the finding's own file. A CRITICAL
// finding of any kind is exempt: a wrong match would hide it for thirty
// days, and one more answer from the author is the cheaper error.
func MatchSettledElsewhere(f payload.Finding, points []db.SettledPoint) (db.SettledPoint, bool) {
	pt, _, ok := ScoreSettledElsewhere(f, points)
	return pt, ok
}

// ScoreSettledElsewhere is MatchSettledElsewhere with the containment and
// the shared subject behind the match, for the replay's pair listing.
func ScoreSettledElsewhere(f payload.Finding, points []db.SettledPoint) (db.SettledPoint, reconcile.EarlierScore, bool) {
	if len(points) == 0 || isPseudoFile(f.File) || f.Severity == "critical" {
		return db.SettledPoint{}, reconcile.EarlierScore{}, false
	}
	// Only a point settled in the finding's own file can be the same point:
	// across files the replay corpus matched on shared generic symbols
	// (start, lookup, failures) and prose that happened to overlap, and the
	// hand-checked pairs were distinct defects. A pattern repeated in
	// another file costs the author one more sentence; a wrong match hides
	// the finding for thirty days.
	var same []db.SettledPoint
	for _, pt := range points {
		if pt.Source != db.SettledSourceDeferred && pt.File != "" && pt.File == f.File {
			same = append(same, pt)
		}
	}
	best, bestScore := -1, reconcile.EarlierScore{}
	for i, pt := range same {
		score, ok := reconcile.SameEarlierPoint(f, reconcile.EarlierPoint{Subjects: splitSubjects(pt.Subjects), Text: pt.Text, Kind: pt.FindingKind})
		if !ok || genericSubject(score.Subject, f.File) {
			continue
		}
		if best < 0 || score.Text > bestScore.Text {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return db.SettledPoint{}, bestScore, false
	}
	return same[best], bestScore, true
}

// genericSubject reports whether the shared subject is too plain to carry a
// match: a bare short word (start, main, lookup, output) that distinct
// defects name all the time, or the file's own stem (creators for
// creators.py). A qualified name, one with a separator or a digit, or a
// long one is specific enough.
func genericSubject(subject, file string) bool {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return true
	}
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)))
	if subject == stem {
		return true
	}
	letters := 0
	for _, r := range subject {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < genericSubjectMinLetters {
		return true
	}
	if strings.ContainsAny(subject, "_.:#-0123456789") {
		return false
	}
	return len(subject) < genericSubjectMaxLen
}

// genericSubjectMinLetters is the letter count under which a token (v2,
// s3, md5) is generic whatever else it carries.
const genericSubjectMinLetters = 5

// genericSubjectMaxLen is the length under which a bare word is generic.
const genericSubjectMaxLen = 10

func isPseudoFile(file string) bool { return file == summaryFile || file == checkFile }

// Deferred reports whether an author's reply defers the point to later
// work instead of settling it (replytext.Deferred).
func Deferred(reason string) bool { return replytext.Deferred(reason) }

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
// It runs before withoutTerminal drops them, with the policy on or off: a
// settled row is terminal (decision 4), so the line that explains why the
// point is absent stays with it.
func rememberSettled(r *Round, rows map[string]*db.PublishedFinding) {
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || row.State != db.PublishedStateSettled || row.SettledPRNumber == 0 {
			continue
		}
		r.SettledMatches = append(r.SettledMatches, SettledMatch{Finding: f, PRNumber: row.SettledPRNumber, CommentID: row.SettledCommentID})
	}
}

// ThreadSettled reports whether another reviewer's thread counts as a
// point the author settled: withdrawn by the reviewer after the author's
// last reply stood by the code (a verdict or pushback), or resolved after
// an explicit verdict. A resolved thread is settled by whoever clicked
// resolve, so only the author's own verdict counts there: in the replay
// corpus most resolved threads the author answered in prose were fix
// reports. A thread the author answered with a fix claim, a bare
// acknowledgement or a deferral to later work is not a settlement and
// must not silence the point on their later pull requests.
func ThreadSettled(t reconcile.ExternalThread, resolved bool) bool {
	if len(t.AuthorReplies) == 0 || (!t.Withdrawn && !resolved) {
		return false
	}
	last := t.AuthorReplies[len(t.AuthorReplies)-1]
	if Deferred(last) {
		return false
	}
	switch ClassifyReply(last) {
	case ReplyVerdict:
		return true
	case ReplyPushback:
		// A leading "fixed" or "done" is a resolution already; here only
		// a cited commit or an opening work verb reads as a fix.
		return t.Withdrawn && !hasCommitSha(last) && !workClaimRe.MatchString(last)
	}
	return false
}

// workClaimRe opens a reply that reports work done rather than a stand.
var workClaimRe = regexp.MustCompile(`(?i)^\s*(?:i\s+(?:have\s+|just\s+)?|i've\s+)?(added|implemented|changed|moved|renamed|switched|reverted|dropped|replaced|refactored|extracted|made|fixed|updated|removed|rewrote|reworked)\b`)

// SettledPointOfThread is the point another reviewer's thread becomes
// when ThreadSettled holds: the root's prose and identifiers, the author's
// replies as the reason, dated by the reply or withdrawal that settled it.
func SettledPointOfThread(owner, repo string, number int, author string, t reconcile.ExternalThread, now time.Time) db.SettledPoint {
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
	return db.SettledPoint{
		RepoOwner: owner, RepoName: repo, PRNumber: number, CommentID: t.Root.CommentID, Author: author, Source: source,
		File: t.Root.File, Subjects: strings.Join(reconcile.CodeIdentifiers(rootText), ","), Headline: t.Root.Title,
		Text: rootText + " " + reason, Reason: reason, SettledAt: at,
	}
}

// recordSettledThreads remembers every thread in threads that ThreadSettled
// holds for, so the author's later pull requests do not meet the point
// again, and every thread the author's last reply defers, as a deferred
// row that supersedes an earlier stand. Recording is keyed by the thread,
// so a scan that sees it again rewrites the same row. resolved answers for
// a root the listing shows resolved.
func recordSettledThreads(store SettledPointStore, owner, repo string, number int, author string, threads []reconcile.ExternalThread, resolved func(int64) bool, now time.Time, tag string) {
	for _, t := range threads {
		var point db.SettledPoint
		switch {
		case len(t.AuthorReplies) > 0 && Deferred(t.AuthorReplies[len(t.AuthorReplies)-1]):
			// A deferral replaces the stand the thread may have recorded.
			point = SettledPointOfThread(owner, repo, number, author, t, now)
			point.Source = db.SettledSourceDeferred
		case ThreadSettled(t, resolved(t.Root.CommentID)):
			point = SettledPointOfThread(owner, repo, number, author, t, now)
		default:
			continue
		}
		if err := store.RecordSettledPoint(&point); err != nil {
			log.Printf("[%s] %s/%s#%d: record settled thread %d: %v", tag, owner, repo, number, t.Root.CommentID, err)
		}
	}
}

// recordExternalSettled remembers, from a publish round, every other
// reviewer's thread the author settled (ThreadSettled).
func (p *Publisher) recordExternalSettled(ctx context.Context, r Round, threads *threadIndex, now time.Time) {
	store, ok := p.Ledger.(SettledPointStore)
	if !ok || !p.Policy.CrossPRSettled || strings.TrimSpace(r.Author) == "" {
		return
	}
	resolved := func(root int64) bool {
		listed, found := threads.lookup(ctx, root)
		return found && listed.Resolved
	}
	recordSettledThreads(store, r.Owner, r.Repo, r.Number, r.Author, r.ExternalThreads, resolved, now, "PUBLISH")
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
