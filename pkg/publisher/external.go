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
}

// refreshExternal re-lists the PR's review comments when GitHub allows and
// replaces Round.External with every root PRism does not own, so a reviewer
// who posted after the round was built is still reconciled against.
func (p *Publisher) refreshExternal(ctx context.Context, r *Round, rows map[string]*db.PublishedFinding) {
	lister, ok := p.GH.(ReviewCommentLister)
	if !ok {
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

// rememberExternal lists, for the summary, the findings of this round that
// alias to an external row: the point is still raised and still lives in
// the other reviewer's thread. It runs before those findings are dropped.
func rememberExternal(r *Round, rows map[string]*db.PublishedFinding) {
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || row.State != db.PublishedStateExternal || row.ExternalCommentID == 0 {
			continue
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

// matchExternal pairs the round's shown findings with the other reviewers'
// roots and takes every match out of the round: an external row remembers
// the point for the PR's life, the summary links to the root, and the
// thread gets one delta reply carrying only what PRism adds, unless a row
// already claimed that root.
func (p *Publisher) matchExternal(ctx context.Context, r *Round, rows map[string]*db.PublishedFinding, now time.Time, threads *threadIndex) externalPass {
	var pass externalPass
	if threads == nil {
		// Without a thread listing a settled root cannot be told from an
		// open one, so nothing is deferred to and nothing is replied into.
		return pass
	}
	roots := openRoots(ctx, r.External, threads)
	if len(roots) == 0 {
		return pass
	}
	claimed := map[int64]bool{}
	for _, row := range rows {
		if row.ExternalCommentID != 0 {
			claimed[row.ExternalCommentID] = true
		}
	}
	var candidates []payload.Finding
	for _, f := range r.currentFindings() {
		// A finding the ledger already knows has its own history; only a
		// point PRism has never recorded can be someone else's first.
		if _, known := rows[f.ID]; !known {
			candidates = append(candidates, f)
		}
	}
	matches := reconcile.MatchExternal(candidates, roots)
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
		pass.matched++
		pass.rows = append(pass.rows, &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: m.External.Source.SourceTag(), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateExternal, PublishedAt: now,
			CommentText: f.Comment, FindingKind: findingKindOf(f), Subjects: subjectsColumn(f),
			ExternalCommentID: m.External.CommentID,
		})
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

// openRoots drops the roots whose thread the listing shows resolved: the
// other reviewer's point is settled there, so PRism neither defers to it
// nor replies into it.
func openRoots(ctx context.Context, roots []reconcile.ExternalFinding, threads *threadIndex) []reconcile.ExternalFinding {
	open := make([]reconcile.ExternalFinding, 0, len(roots))
	for _, g := range roots {
		if t, ok := threads.lookup(ctx, g.CommentID); ok && t.Resolved {
			continue
		}
		open = append(open, g)
	}
	return open
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
			parts = append(parts, impact)
		}
	}
	if code := suggestedCode(commentText(f)); code != "" && ext.Suggestion == "" {
		parts = append(parts, fmt.Sprintf("Suggested change for line %d:\n```\n%s\n```", f.Line, code))
	}
	if hasContract && c.Falsifiability == "falsifiable" && c.FalsifiableCondition != nil && c.ExpectedObservable != nil {
		condition := strings.TrimSuffix(strings.TrimSpace(*c.FalsifiableCondition), ".")
		observable := strings.TrimSuffix(strings.TrimSpace(*c.ExpectedObservable), ".")
		if !covered(condition+" "+observable, extText) {
			parts = append(parts, fmt.Sprintf("**How to verify:** %s. Expected: %s.", condition, observable))
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
