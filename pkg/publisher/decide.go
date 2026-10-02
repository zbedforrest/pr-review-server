package publisher

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/pkg/reviewer/types"
)

// anchorTolerance is how far from the cited line a changed line still counts
// as a change to the finding's anchor hunk.
const anchorTolerance = 3

// terminalState reports whether a row's state ends every later wording of the
// finding on this PR.
func terminalState(state string) bool {
	switch state {
	case db.PublishedStateDismissed, db.PublishedStateContested, db.PublishedStateExternal:
		return true
	}
	return false
}

// fixedState reads the legacy resolved value as fixed.
func fixedState(state string) bool {
	return state == db.PublishedStateFixed || state == db.PublishedStateResolved
}

func isFindingRow(row *db.PublishedFinding) bool {
	return row.Kind == db.PublishedKindFinding || row.Kind == db.PublishedKindAnnotation
}

func subjectsColumn(f payload.Finding) string {
	return strings.Join(reconcile.SubjectNames(f.FindingContract), ",")
}

func findingKindOf(f payload.Finding) string {
	if f.FindingContract == nil {
		return ""
	}
	return f.FindingContract.FindingKind
}

func splitSubjects(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// threadNote is an in-thread reply the round owes once its ledger writes are
// done.
type threadNote struct {
	commentID int64
	body      string
}

// publishLedger is the ledger decision table: identity first, then one
// transition per prior row, then the posts the table allows. doc.go is the
// specification.
func (p *Publisher) publishLedger(ctx context.Context, r Round) (Report, error) {
	var summaryRow *db.PublishedFinding
	rows := map[string]*db.PublishedFinding{}
	for i := range r.Previous {
		row := &r.Previous[i]
		switch {
		case row.Kind == db.PublishedKindSummary:
			summaryRow = row
		case isFindingRow(row):
			rows[row.Fingerprint] = row
		}
	}
	if r.RoundNumber == 0 {
		r.RoundNumber = 1
		if summaryRow != nil {
			r.RoundNumber = summaryRow.Rounds + 1
		}
	}
	r.ShowUnverified = p.Policy.ShowUnverified
	threads := newThreadIndex(p.GH, p.Policy, r.Owner, r.Repo, r.Number)

	originalIDs := p.aliasToLedger(&r, rows)
	r.Findings = p.withoutTerminal(r, rows, originalIDs)
	notes := p.clampSeverity(&r, rows)

	alreadyPublished := map[string]bool{}
	if r.InlineComments == nil {
		r.InlineComments = map[string]int64{}
	}
	for id, row := range rows {
		if row.CommentID != 0 {
			alreadyPublished[id] = true
			r.InlineComments[id] = row.CommentID
		}
	}
	sel := Select(r.Findings, alreadyPublished, r.Commentable, p.Policy)

	shown := map[string]bool{}
	for _, f := range r.currentFindings() {
		shown[f.ID] = true
	}
	present := map[string]payload.Finding{}
	for _, f := range r.activeClaims() {
		present[f.ID] = f
	}
	var d roundDiff
	for id := range shown {
		if _, known := rows[id]; !known {
			d.New++
		}
	}
	r.transitions = &d

	// Transitions are decided before any write so the summary this round
	// renders describes the ledger the round leaves behind.
	type pending struct {
		row    *db.PublishedFinding
		reopen bool
		// thread is the GitHub thread change the transition owes: resolve on
		// a fix, unresolve on a reopen, none otherwise.
		thread threadChange
	}
	var writes []pending
	written := map[string]bool{}
	for _, f := range sel.Inline {
		written[f.ID] = true
	}
	for _, f := range sel.Annotations {
		written[f.ID] = true
	}
	rep := Report{InlinePosted: len(sel.Inline), Annotations: len(sel.Annotations)}
	for id, row := range rows {
		if terminalState(row.State) {
			continue
		}
		f, isPresent := present[id]
		switch {
		case row.State == db.PublishedStateOpen && isPresent:
			if shown[id] {
				d.StillOpen++
			}
			if written[id] {
				continue
			}
			next := *row
			next.LastSeenSHA = r.HeadSHA
			next.Severity = f.Severity
			backfill(&next, f, r.PriorComments[id])
			writes = append(writes, pending{row: &next})
			rep.Hygiene.noteWritten(f, row)
		case row.State == db.PublishedStateOpen:
			cs, known := r.changesSince(row.LastSeenSHA)
			file, _ := payload.FingerprintParts(id)
			if (known && cs.Files[file]) || (!known && outdatedCorroborates(ctx, threads, r, row)) {
				d.Fixed++
				next := *row
				next.State = db.PublishedStateFixed
				writes = append(writes, pending{row: &next, thread: resolveThread})
				if threads != nil && row.CommentID != 0 {
					notes = append(notes, threadNote{commentID: row.CommentID, body: notSeenNote(r.HeadSHA)})
				}
				rep.Hygiene.noteResolved(row, r.HeadSHA, cs.Files)
				continue
			}
			d.StillOpen++
			if next, ok := backfilled(row, r.PriorComments[id]); ok {
				writes = append(writes, pending{row: next})
			}
		case fixedState(row.State) && isPresent:
			d.StillOpen++
			rep.Reopened++
			if written[id] {
				continue
			}
			next := *row
			next.State = db.PublishedStateOpen
			next.LastSeenSHA = r.HeadSHA
			next.Severity = f.Severity
			backfill(&next, f, r.PriorComments[id])
			writes = append(writes, pending{row: &next, reopen: true, thread: unresolveThread})
			if row.CommentID != 0 {
				notes = append(notes, threadNote{commentID: row.CommentID, body: fmt.Sprintf("Back at %s.", shortSHA(r.HeadSHA))})
			}
		}
	}
	rep.StillOpen, rep.Fixed = d.StillOpen, d.Fixed
	rep.Confidence = Confidence(r.Findings, r.RequiredCheckViolated)
	now := p.now()

	// Threads are looked up before the rows are written so a row that
	// predates the ThreadNodeID column keeps the id the listing found.
	var actions []threadAction
	if threads != nil {
		for _, w := range writes {
			if w.thread == noThreadChange || w.row.CommentID == 0 {
				continue
			}
			w.row.ThreadNodeID = threads.nodeID(ctx, w.row.ThreadNodeID, w.row.CommentID)
			actions = append(actions, threadAction{nodeID: w.row.ThreadNodeID, resolve: w.thread == resolveThread})
		}
	}

	postedThisRound, err := p.postInline(ctx, r, sel, rows, now, &rep, threads)
	if err != nil {
		return rep, err
	}
	for id, cid := range postedThisRound {
		r.InlineComments[id] = cid
	}
	if err := p.writeSummary(ctx, r, sel, summaryRow, now, &rep); err != nil {
		return rep, err
	}

	for _, f := range sel.Annotations {
		if _, posted := postedThisRound[f.ID]; posted {
			continue
		}
		row := &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateOpen, PublishedAt: now,
			CommentText: f.Comment, FindingKind: findingKindOf(f), Subjects: subjectsColumn(f),
		}
		if prev, ok := rows[f.ID]; ok {
			// A row that already exists keeps its first publication and its
			// comment; only its state, severity and last-seen sha move.
			row.Kind, row.ReviewedSHA, row.PublishedAt = prev.Kind, prev.ReviewedSHA, prev.PublishedAt
			row.CommentID, row.ReviewID, row.ThreadNodeID = prev.CommentID, prev.ReviewID, prev.ThreadNodeID
		}
		if err := p.Ledger.UpsertPublishedFinding(row); err != nil {
			return rep, fmt.Errorf("record annotation %s: %w", f.ID, err)
		}
		rep.Hygiene.noteWritten(f, rows[f.ID])
	}
	for _, w := range writes {
		if err := p.Ledger.UpsertPublishedFinding(w.row); err != nil {
			return rep, fmt.Errorf("update finding %s: %w", w.row.Fingerprint, err)
		}
	}

	if replier, ok := p.GH.(ThreadReplier); ok {
		for _, n := range notes {
			// A missed note is not worth failing a round whose ledger is already
			// consistent; the next round sees the same state and says nothing new.
			if _, err := replier.PostReply(ctx, r.Owner, r.Repo, r.Number, n.commentID, n.body); err != nil {
				rep.ThreadReplyFailures++
				log.Printf("[PUBLISH] %s/%s#%d: thread note on comment %d lost: %v", r.Owner, r.Repo, r.Number, n.commentID, err)
			} else {
				rep.ThreadReplies++
			}
		}
	}
	threads.apply(ctx, actions, &rep)
	return rep, nil
}

type threadChange int

const (
	noThreadChange threadChange = iota
	resolveThread
	unresolveThread
)

// outdatedCorroborates reads GitHub's outdated flag as evidence that the
// lines a finding cited changed, for a round whose compare is unknown. It
// counts only when the head moved and the row was last seen at the head it
// was posted for, so the outdating cannot predate the last sighting.
func outdatedCorroborates(ctx context.Context, threads *threadIndex, r Round, row *db.PublishedFinding) bool {
	if threads == nil || row.CommentID == 0 || row.LastSeenSHA == "" || strings.EqualFold(row.LastSeenSHA, r.HeadSHA) || !strings.EqualFold(row.ReviewedSHA, row.LastSeenSHA) {
		return false
	}
	return threads.outdated(ctx, row.CommentID)
}

// aliasToLedger gives every current finding that restates a ledger row that
// row's fingerprint. It returns the ids the findings carried before, keyed
// by the id they carry now, for the one case that posts a fresh root.
func (p *Publisher) aliasToLedger(r *Round, rows map[string]*db.PublishedFinding) map[string]string {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	own := make([]reconcile.OwnComment, 0, len(rows))
	for _, id := range ids {
		row := rows[id]
		file, bucket := payload.FingerprintParts(id)
		o := reconcile.OwnComment{CommentID: row.CommentID, FindingID: id, File: file, Line: bucket*10 + 5, Text: row.CommentText, Kind: row.FindingKind, Subjects: splitSubjects(row.Subjects)}
		if prior, ok := r.PriorComments[id]; ok {
			if prior.Line > 0 {
				o.Line = prior.Line
			}
			if o.Text == "" {
				o.Text = StripRendered(prior.Text)
			}
		}
		own = append(own, o)
	}
	aliases := reconcile.AliasPrior(r.Findings, own)
	original := map[string]string{}
	for i := range r.Findings {
		if prior, ok := aliases[r.Findings[i].ID]; ok {
			original[prior] = r.Findings[i].ID
			r.Findings[i].ID = prior
		}
	}
	return original
}

// withoutTerminal drops every finding that aliases to a dismissed, contested
// or external row. The one exception: a CRITICAL security_risk whose anchor
// changed since the row was settled may post once more, as a fresh root that
// names the change. Once that root exists, a later wording that aliases back
// to the terminal row is handed to the fresh row instead, so the point keeps
// one live thread and never a third root.
func (p *Publisher) withoutTerminal(r Round, rows map[string]*db.PublishedFinding, originalIDs map[string]string) []payload.Finding {
	carried := make(map[string]bool, len(r.Findings))
	for _, f := range r.Findings {
		carried[f.ID] = true
	}
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || !terminalState(row.State) {
			kept = append(kept, f)
			continue
		}
		others, keyed := rowsOnPoint(rows, row, f)
		if len(others) > 0 {
			if successor := liveSuccessor(others); successor != nil && !carried[successor.Fingerprint] {
				carried[successor.Fingerprint] = true
				f.ID = successor.Fingerprint
				kept = append(kept, f)
			}
			continue
		}
		if !keyed || !securityException(r, row, f) {
			continue
		}
		f.Comment = fmt.Sprintf("Raised again: `%s` changed at %s after this point was settled. %s", f.File, shortSHA(r.HeadSHA), f.Comment)
		fresh := originalIDs[f.ID]
		if fresh == "" || fresh == f.ID {
			// The wording and line did not move, so the marker is minted from
			// the text that names the change.
			fresh = payload.Fingerprint(f.File, f.Line, f.Comment)
		}
		f.ID = fresh
		if row.Subjects != "" {
			// The fresh root is keyed to the settled row so rowsOnPoint finds
			// it whatever this wording's own subjects or file are.
			c := *f.FindingContract
			c.Subjects = nil
			for _, name := range splitSubjects(row.Subjects) {
				c.Subjects = append(c.Subjects, types.FindingSubject{Kind: "symbol", Path: f.File, Name: name})
			}
			f.FindingContract = &c
		}
		kept = append(kept, f)
	}
	return kept
}

// securityException is decision 4's one exception; withoutTerminal has
// already established that no other row exists on the point.
func securityException(r Round, row *db.PublishedFinding, f payload.Finding) bool {
	c := f.FindingContract
	return f.Severity == "critical" && c != nil && c.FindingKind == "security_risk" && r.anchorChanged(row.LastSeenSHA, f.File, f.Line)
}

// rowsOnPoint lists every other row about the same point as row: the same
// subject set in any file (a fresh root may re-anchor elsewhere), the
// finding's own subjects standing in for a row written before the column
// existed. keyed is false when neither names a subject, in which case the
// point cannot be tracked across rows.
func rowsOnPoint(rows map[string]*db.PublishedFinding, row *db.PublishedFinding, f payload.Finding) (others []*db.PublishedFinding, keyed bool) {
	key := row.Subjects
	if key == "" {
		key = subjectsColumn(f)
	}
	if key == "" {
		return nil, false
	}
	for _, other := range rows {
		if other != row && other.Subjects == key {
			others = append(others, other)
		}
	}
	return others, true
}

// liveSuccessor picks the newest non-terminal row among those on a point.
func liveSuccessor(others []*db.PublishedFinding) *db.PublishedFinding {
	var best *db.PublishedFinding
	for _, o := range others {
		if terminalState(o.State) {
			continue
		}
		if best == nil || o.PublishedAt.After(best.PublishedAt) || (o.PublishedAt.Equal(best.PublishedAt) && o.ID > best.ID) {
			best = o
		}
	}
	return best
}

// clampSeverity keeps an aliased finding at its row's severity unless the
// cited lines changed since the row was last seen; an accepted change on a
// row with a thread is announced there once.
func (p *Publisher) clampSeverity(r *Round, rows map[string]*db.PublishedFinding) []threadNote {
	var notes []threadNote
	for i := range r.Findings {
		f := &r.Findings[i]
		row, ok := rows[f.ID]
		if !ok || terminalState(row.State) || row.Severity == "" || severityRank(row.Severity) == severityRank(f.Severity) {
			continue
		}
		if !r.anchorChanged(row.LastSeenSHA, f.File, f.Line) {
			f.Severity = row.Severity
			continue
		}
		if row.CommentID != 0 {
			notes = append(notes, threadNote{commentID: row.CommentID,
				body: fmt.Sprintf("Severity %s to %s at %s: the cited lines changed.", row.Severity, f.Severity, shortSHA(r.HeadSHA))})
		}
	}
	return notes
}

func (r Round) changesSince(base string) (ChangeSet, bool) {
	if r.Changes == nil || base == "" || strings.EqualFold(base, r.HeadSHA) {
		return ChangeSet{}, false
	}
	return r.Changes(base)
}

// changedFilesSince is the file set the hygiene notes judge a resolve by;
// nil when the change set is unknown.
func (r Round) changedFilesSince(base string) map[string]bool {
	cs, ok := r.changesSince(base)
	if !ok {
		return nil
	}
	return cs.Files
}

// anchorChanged reports whether the lines a finding cites changed since
// base: by line when the change set carries lines for the file, else by the
// file having changed at all. Unknown changes never count as a change.
func (r Round) anchorChanged(base, file string, line int) bool {
	cs, ok := r.changesSince(base)
	if !ok || !cs.Files[file] {
		return false
	}
	lines, known := cs.Lines[file]
	if !known || line <= 0 {
		return true
	}
	for l := line - anchorTolerance; l <= line+anchorTolerance; l++ {
		if lines[l] {
			return true
		}
	}
	return false
}

// backfill fills the memory columns of a row written before they existed
// from the finding now matched to it, or from the comment GitHub shows.
func backfill(row *db.PublishedFinding, f payload.Finding, prior PriorComment) {
	if row.CommentText == "" {
		if f.Comment != "" {
			row.CommentText = f.Comment
		} else if prior.Text != "" {
			row.CommentText = StripRendered(prior.Text)
		}
	}
	if row.FindingKind == "" {
		row.FindingKind = findingKindOf(f)
	}
	if row.Subjects == "" {
		row.Subjects = subjectsColumn(f)
	}
}

// backfilled returns a copy of an untouched row with its comment text filled
// from GitHub, and whether there was anything to fill.
func backfilled(row *db.PublishedFinding, prior PriorComment) (*db.PublishedFinding, bool) {
	if row.CommentText != "" || prior.Text == "" {
		return nil, false
	}
	next := *row
	next.CommentText = StripRendered(prior.Text)
	return &next, true
}
