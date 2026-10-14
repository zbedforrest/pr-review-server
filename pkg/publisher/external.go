package publisher

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// externalReplyMarkerPrefix tags the one delta reply PRism leaves under
// another reviewer's root, distinct from the finding marker so the reply
// scanner never takes that thread for one of PRism's own.
const externalReplyMarkerPrefix = "<!-- prism:external:"

// ExternalReplyMarker is the hidden marker on a delta reply for finding id.
func ExternalReplyMarker(id string) string {
	return externalReplyMarkerPrefix + id + " -->"
}

// IsExternalReply reports whether body is one of PRism's delta replies.
func IsExternalReply(body string) bool {
	return strings.HasPrefix(body, externalReplyMarkerPrefix)
}

// deltaCovered is the containment above which a sentence of PRism's finding
// counts as already said by the other reviewer.
const deltaCovered = 0.6

// externalPass is what the cross-reviewer pass leaves for the round: the
// external rows to write, the delta replies to post once those rows exist,
// and the count of findings it took out of the round.
type externalPass struct {
	rows    []*db.PublishedFinding
	notes   []threadNote
	matched int
	// settled counts the folded notes matched to a resolved thread:
	// recorded, never linked or replied to.
	settled int
}

// refreshExternal re-lists the PR's review comments when GitHub allows and
// replaces Round.External with every root PRism does not own, so a reviewer
// who posted after the round was built is still reconciled against.
func (p *Publisher) refreshExternal(ctx context.Context, r *Round, rows map[string]*db.PublishedFinding) {
	lister, ok := p.GH.(ReviewCommentLister)
	if !ok || !hasUnknownFinding(r, rows, p.Policy.NoteReconcile) {
		return
	}
	listed, err := lister.ListReviewComments(ctx, r.Owner, r.Repo, r.Number)
	if err != nil {
		log.Printf("[PUBLISH] %s/%s#%d: re-list review comments: %v; reconciling against the round's listing", r.Owner, r.Repo, r.Number, err)
		return
	}
	own := make(map[int64]bool, len(rows))
	for _, row := range rows {
		if row.CommentID != 0 {
			own[row.CommentID] = true
		}
	}
	comments := make([]reconcile.ExternalComment, 0, len(listed))
	for _, c := range listed {
		comments = append(comments, reconcile.ExternalComment{ID: c.ID, Author: c.Author, Body: c.Body, Path: c.Path, Line: c.Line, StartLine: c.StartLine, OriginalLine: c.OriginalLine, OriginalStartLine: c.OriginalStartLine, InReplyToID: c.InReplyToID})
	}
	r.External = reconcile.ParseExternalComments(comments, own)
}

// hasUnknownFinding reports whether the round shows a finding the ledger has
// never recorded, the only kind that can match another reviewer's root.
func hasUnknownFinding(r *Round, rows map[string]*db.PublishedFinding, notes bool) bool {
	for _, f := range r.externalCandidates(notes) {
		if _, known := rows[f.ID]; !known {
			return true
		}
	}
	return false
}

// externalCandidates are the findings another reviewer's root may stand in
// for: the shown findings, and with notes the folded lower-severity ones.
func (r Round) externalCandidates(notes bool) []payload.Finding {
	if !notes {
		return r.currentFindings()
	}
	var out []payload.Finding
	for _, f := range r.Findings {
		if Publishable(f) {
			out = append(out, f)
		}
	}
	return out
}

// forgetShown drops the external links of findings the round still carries
// (a live successor row kept them), so a point is never both a bullet and a
// linked external entry.
func forgetShown(r *Round) {
	if len(r.ExternalMatches) == 0 {
		return
	}
	shown := make(map[string]bool, len(r.Findings))
	for _, f := range r.Findings {
		shown[f.File+"\x00"+f.Comment] = true
	}
	kept := r.ExternalMatches[:0]
	for _, m := range r.ExternalMatches {
		if !shown[m.Finding.File+"\x00"+m.Finding.Comment] {
			kept = append(kept, m)
		}
	}
	r.ExternalMatches = kept
}

// rememberExternal lists, for the summary, the findings of this round that
// alias to an external row: the point is still raised and still lives in
// the other reviewer's thread. It runs before those findings are dropped.
// With skipSettled, a root whose thread the listing shows resolved is a
// settled point and is not listed.
func rememberExternal(ctx context.Context, r *Round, rows map[string]*db.PublishedFinding, threads *threadIndex, skipSettled bool) {
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || row.State != db.PublishedStateExternal || row.ExternalCommentID == 0 {
			continue
		}
		if skipSettled {
			if t, listed := threads.lookup(ctx, row.ExternalCommentID); listed && t.Resolved {
				continue
			}
		}
		source, _ := reconcile.SourceFromTag(row.SourceTag)
		r.ExternalMatches = append(r.ExternalMatches, ExternalMatch{Finding: f, Source: source, CommentID: row.ExternalCommentID})
	}
}

// confidence is the merge-confidence score the round reports. A finding
// another reviewer raised first still counts: the score is a verdict on the
// PR, not on who found the defect.
func (r Round) confidence() int {
	findings := r.Findings
	if len(r.ExternalMatches) > 0 {
		findings = append(append([]payload.Finding(nil), r.Findings...), r.externalFindings()...)
	}
	return Confidence(findings, r.RequiredCheckViolated)
}

func (r Round) externalFindings() []payload.Finding {
	out := make([]payload.Finding, 0, len(r.ExternalMatches))
	for _, m := range r.ExternalMatches {
		out = append(out, m.Finding)
	}
	return out
}

// matchExternal pairs the round's findings with the other reviewers' roots
// and takes every match out of the round: an external row remembers the
// point for the PR's life. A shown finding matches an open thread: linked
// from the summary and answered with one delta reply carrying only what
// PRism adds, unless a row already claimed that root. A resolved thread is
// no candidate for a shown finding. With Policy.NoteReconcile a folded note
// matches a resolved thread only: the point is settled there, so the row
// is recorded and the note dropped, never linked or replied for. A note
// never claims an open root, so the shown finding that restates it later
// still earns the thread's delta reply.
func (p *Publisher) matchExternal(ctx context.Context, r *Round, rows map[string]*db.PublishedFinding, now time.Time, threads *threadIndex) externalPass {
	var pass externalPass
	if threads == nil {
		// Without a thread listing a settled root cannot be told from an
		// open one, so nothing is deferred to and nothing is replied into.
		return pass
	}
	notes := p.Policy.NoteReconcile
	open, resolved := splitRoots(ctx, r.External, threads)
	if threads.listFailed() {
		log.Printf("[PUBLISH] %s/%s#%d: thread listing failed; external matching skipped this round", r.Owner, r.Repo, r.Number)
		return pass
	}
	if !notes {
		resolved = nil
	}
	if len(open) == 0 && len(resolved) == 0 {
		return pass
	}
	claimed := map[int64]bool{}
	for _, row := range rows {
		if row.ExternalCommentID != 0 {
			claimed[row.ExternalCommentID] = true
		}
	}
	// A finding the ledger already knows has its own history; only a point
	// PRism has never recorded can be someone else's first.
	var shownCandidates, noteCandidates []payload.Finding
	for _, f := range r.externalCandidates(notes) {
		if _, known := rows[f.ID]; known {
			continue
		}
		if Shown(f) {
			shownCandidates = append(shownCandidates, f)
		} else {
			noteCandidates = append(noteCandidates, f)
		}
	}
	matches := reconcile.MatchExternalWith(shownCandidates, open, p.Policy.matchOptions())
	if len(noteCandidates) > 0 && len(resolved) > 0 {
		matches = append(matches, reconcile.MatchExternalWith(noteCandidates, resolved, p.Policy.matchOptions())...)
	}
	if len(matches) == 0 {
		return pass
	}
	taken := make(map[string]reconcile.Match, len(matches))
	for _, m := range matches {
		taken[m.FindingID] = m
	}
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		m, ok := taken[f.ID]
		if !ok {
			kept = append(kept, f)
			continue
		}
		pass.rows = append(pass.rows, &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: m.External.Source.SourceTag(), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateExternal, PublishedAt: now,
			CommentText: f.Comment, FindingKind: findingKindOf(f), Subjects: subjectsColumn(f),
			ExternalCommentID: m.External.CommentID,
		})
		if !Shown(f) {
			pass.settled++
			log.Printf("[PUBLISH] %s/%s#%d: note %s restates %s's resolved thread %d, dropped", r.Owner, r.Repo, r.Number, f.ID, m.External.Source.Label(), m.External.CommentID)
			continue
		}
		pass.matched++
		r.ExternalMatches = append(r.ExternalMatches, ExternalMatch{Finding: f, Source: m.External.Source, CommentID: m.External.CommentID})
		if claimed[m.External.CommentID] {
			continue
		}
		claimed[m.External.CommentID] = true
		if body := deltaReply(f, m.External); body != "" {
			pass.notes = append(pass.notes, threadNote{commentID: m.External.CommentID, body: body, external: true})
		}
	}
	r.Findings = kept
	sort.SliceStable(pass.notes, func(i, j int) bool { return pass.notes[i].commentID < pass.notes[j].commentID })
	return pass
}

// matchOptions is the external scoring the policy selects.
func (p Policy) matchOptions() reconcile.MatchOptions {
	return reconcile.MatchOptions{NoteSentence: p.NoteSentenceMatch}
}

// withoutSettledNotes drops, for a re-rendered summary, the folded notes
// that restate a root whose thread the listing now shows resolved: the
// round's note pass records such a note as external and drops it; a
// refresh records nothing, so it only leaves the note out and the next
// round writes the row. Without a listing nothing is dropped.
func withoutSettledNotes(ctx context.Context, r Round, rows map[string]*db.PublishedFinding, threads *threadIndex, pol Policy) []payload.Finding {
	if threads == nil || len(r.External) == 0 {
		return r.Findings
	}
	_, resolved := splitRoots(ctx, r.External, threads)
	if threads.listFailed() || len(resolved) == 0 {
		return r.Findings
	}
	var notes []payload.Finding
	for _, f := range r.Findings {
		if _, known := rows[f.ID]; !known && summaryNote(f) {
			notes = append(notes, f)
		}
	}
	matches := reconcile.MatchExternalWith(notes, resolved, pol.matchOptions())
	if len(matches) == 0 {
		return r.Findings
	}
	settled := make(map[string]bool, len(matches))
	for _, m := range matches {
		settled[m.FindingID] = true
	}
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if settled[f.ID] {
			log.Printf("[PUBLISH] %s/%s#%d: note %s restates a resolved thread, left out of the refreshed summary", r.Owner, r.Repo, r.Number, f.ID)
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// withoutClaimedRoots drops the Greptile-only points whose root an external
// row claims, so a point is never listed twice.
func withoutClaimedRoots(refs []GreptileOnlyRef, rows map[string]*db.PublishedFinding, added []*db.PublishedFinding) []GreptileOnlyRef {
	if len(refs) == 0 {
		return refs
	}
	claimed := map[int64]bool{}
	for _, row := range rows {
		if row.ExternalCommentID != 0 {
			claimed[row.ExternalCommentID] = true
		}
	}
	for _, row := range added {
		claimed[row.ExternalCommentID] = true
	}
	kept := make([]GreptileOnlyRef, 0, len(refs))
	for _, ref := range refs {
		if !claimed[ref.CommentID] {
			kept = append(kept, ref)
		}
	}
	return kept
}

// splitRoots sorts the roots by thread state: open threads (including ones
// the listing does not show) and threads the listing shows resolved.
func splitRoots(ctx context.Context, roots []reconcile.ExternalFinding, threads *threadIndex) (open, resolved []reconcile.ExternalFinding) {
	for _, g := range roots {
		if t, ok := threads.lookup(ctx, g.CommentID); ok && t.Resolved {
			resolved = append(resolved, g)
			continue
		}
		open = append(open, g)
	}
	return open, resolved
}

// deltaReply is the one reply PRism may leave under another reviewer's root:
// the impact sentence when the root does not state it, PRism's suggested
// code when the root has none, and how to verify. Empty when the root
// already says everything PRism would. The suggested code goes out as a
// plain fence: a suggestion block in a reply applies to the lines that
// thread anchors, which are the other reviewer's, not PRism's.
func deltaReply(f payload.Finding, ext reconcile.ExternalFinding) string {
	extText := strings.Join([]string{ext.Title, ext.Body, ext.Suggestion}, " ")
	var parts []string
	c := f.FindingContract
	hasContract := c != nil && f.FindingContractStatus == "valid"
	if hasContract {
		if impact := strings.TrimSpace(c.CurrentImpact); impact != "" && !covered(impact, extText) {
			parts = append(parts, EscapeProseHTML(impact))
		}
	}
	if code := suggestedCode(commentText(f)); code != "" && ext.Suggestion == "" {
		parts = append(parts, fmt.Sprintf("Suggested change for line %d:\n```\n%s\n```", f.Line, code))
	}
	if hasContract && c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		condition := strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), ".")
		observable := strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), ".")
		if !covered(condition+" "+observable, extText) {
			parts = append(parts, fmt.Sprintf("**How to verify:** %s. Expected: %s.", EscapeProseHTML(condition), EscapeProseHTML(observable)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ExternalReplyMarker(f.ID) + "\nPRism found the same point. What this thread does not say yet:\n\n" + strings.Join(parts, "\n\n") + "\n"
}

// suggestedCode is the body of the finding's suggestion block, if any.
func suggestedCode(comment string) string {
	m := suggestionFenceRe.FindStringSubmatch(comment)
	if m == nil {
		return ""
	}
	return strings.TrimRight(m[1], "\n")
}

func covered(text, ext string) bool {
	score, _ := reconcile.Containment(text, ext)
	return score >= deltaCovered
}
