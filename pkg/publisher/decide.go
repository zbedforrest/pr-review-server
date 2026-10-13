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
// done; external marks the delta reply under another reviewer's root. A
// status note (not seen, return, severity change) is rendered per thread so
// that a thread gets one note per round at most.
type threadNote struct {
	commentID int64
	body      string
	external  bool
	kind      noteKind
	from, to  string
}

type noteKind int

const (
	noteDelta noteKind = iota
	noteNotSeen
	noteReturn
	noteSeverity
)

// renderNotes turns the round's status notes into at most one reply per
// thread: a return and a severity change on one thread are one sentence.
// The delta replies under other reviewers' roots pass through untouched.
func renderNotes(notes []threadNote, head string, legacy bool) []threadNote {
	sha := shortSHA(head)
	render := func(n threadNote) string {
		switch n.kind {
		case noteNotSeen:
			return notSeenNote(head)
		case noteReturn:
			return fmt.Sprintf("Back at %s.", sha)
		case noteSeverity:
			return fmt.Sprintf("Severity %s to %s at %s: the cited lines changed.", n.from, n.to, sha)
		}
		return n.body
	}
	if legacy {
		out := make([]threadNote, 0, len(notes))
		for _, n := range notes {
			out = append(out, threadNote{commentID: n.commentID, body: render(n), external: n.external})
		}
		return out
	}
	var out []threadNote
	perThread := map[int64]int{}
	for _, n := range notes {
		if n.external {
			out = append(out, n)
			continue
		}
		if i, seen := perThread[n.commentID]; seen {
			first := &out[i]
			if first.kind == noteReturn && n.kind == noteSeverity {
				first.body = fmt.Sprintf("Back at %s; severity %s to %s: the cited lines changed.", sha, n.from, n.to)
			} else if first.kind == noteSeverity && n.kind == noteReturn {
				first.body = fmt.Sprintf("Back at %s; severity %s to %s: the cited lines changed.", sha, first.from, first.to)
				first.kind = noteReturn
			}
			continue
		}
		perThread[n.commentID] = len(out)
		out = append(out, threadNote{commentID: n.commentID, body: render(n), kind: n.kind, from: n.from, to: n.to})
	}
	return out
}

// transitionGuard names the files no fixed, return or severity transition
// may come from this round: paths the review scope marks inherited from the
// base branch and every path when the review's merge-base moved since the
// last published round (base drift, which changes what the agent read
// without any change to the PR). A path outside the PR diff GitHub lists
// blocks a return and a severity change but not a fix: a change at the cited
// lines is evidence whether or not the file is still in the diff. A scope
// without a merge-base (the base fetch failed) is an unknown base and guards
// like drift. Without a review scope, or with Policy.NoScopeGuard, nothing
// is guarded.
type transitionGuard struct {
	inherited map[string]bool
	diff      map[string]bool
	drift     bool
	unknown   bool
}

func newTransitionGuard(r Round, summaryRow *db.PublishedFinding, off bool) transitionGuard {
	var g transitionGuard
	if off || r.Scope == nil {
		return g
	}
	g.diff = r.DiffFiles
	if len(r.Scope.InheritedPaths) > 0 {
		g.inherited = make(map[string]bool, len(r.Scope.InheritedPaths))
		for _, path := range r.Scope.InheritedPaths {
			g.inherited[path] = true
		}
	}
	if r.Scope.MergeBase == "" {
		g.unknown = true
		return g
	}
	if summaryRow != nil && summaryRow.MergeBase != "" && !strings.EqualFold(summaryRow.MergeBase, r.Scope.MergeBase) {
		g.drift = true
	}
	return g
}

func (g transitionGuard) blocks(file string) bool {
	return g.blocksFix(file) || (g.diff != nil && !g.diff[file])
}

func (g transitionGuard) blocksFix(file string) bool {
	return g.drift || g.unknown || g.inherited[file]
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
	r.LegacyClamp = p.Policy.LegacyClamp
	threads := newThreadIndex(p.GH, p.Policy, r.Owner, r.Repo, r.Number)
	guard := newTransitionGuard(r, summaryRow, p.Policy.NoScopeGuard)
	if guard.drift {
		log.Printf("[PUBLISH] %s/%s#%d: merge-base moved from %s to %s; no fixed, return or severity transition this round", r.Owner, r.Repo, r.Number, shortSHA(summaryRow.MergeBase), shortSHA(r.Scope.MergeBase))
	}
	if guard.unknown {
		log.Printf("[PUBLISH] %s/%s#%d: review scope has no merge-base; no fixed, return or severity transition this round", r.Owner, r.Repo, r.Number)
	}

	originalIDs := p.aliasToLedger(&r, rows)
	rememberExternal(ctx, &r, rows, threads, p.Policy.NoteReconcile)
	settledDropped, bodyCovered := 0, 0
	if p.Policy.SettledAlias {
		// Before withoutTerminal: the root the security exception mints
		// there restates its settled row by construction.
		r.Findings, settledDropped = withoutSettled(r, rows)
	}
	r.Findings = p.withoutTerminal(r, rows, originalIDs)
	r.Findings = withoutGuardedReturns(r, rows, guard)
	forgetShown(&r)
	var rep Report
	held := p.holdFindings(&r, &rep, func(id string) bool {
		row, ok := rows[id]
		return ok && row.CommentID != 0 && row.State == db.PublishedStateOpen
	})
	now := p.now()
	var external externalPass
	if p.Policy.ExternalReconcile {
		p.refreshExternal(ctx, &r, rows)
		external = p.matchExternal(ctx, &r, rows, now, threads)
		r.GreptileOnly = withoutClaimedRoots(r.GreptileOnly, rows, external.rows)
	}
	// After the threads: a point another reviewer raised is remembered as
	// external; the body check leaves no row.
	if p.Policy.BodyCheck {
		r.Findings, bodyCovered = withoutBodyCovered(r, rows)
	}
	notes := append(p.clampSeverity(&r, rows, guard), external.notes...)

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
	sel := Select(r.Findings, alreadyPublished, r.Commentable, p.Policy.forRound(r.Previous))

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
		row *db.PublishedFinding
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
	rep.InlinePosted, rep.Annotations, rep.ExternalMatched = len(sel.Inline), len(sel.Annotations), external.matched
	rep.ExternalSettled, rep.SettledDropped, rep.BodyCovered = external.settled, settledDropped, bodyCovered
	for _, row := range external.rows {
		writes = append(writes, pending{row: row})
	}
	lastPublished := ""
	if summaryRow != nil {
		lastPublished = summaryRow.LastSeenSHA
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := rows[id]
		if terminalState(row.State) {
			continue
		}
		f, isPresent := present[id]
		heldFinding, isHeld := held[id]
		switch {
		case row.State == db.PublishedStateOpen && isHeld:
			// The gate kept the finding off GitHub; the row stays open and
			// silent, since absence by the gate is not a fix.
			next := *row
			next.LastSeenSHA = r.HeadSHA
			backfill(&next, heldFinding, r.PriorComments[id])
			writes = append(writes, pending{row: &next})
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
			next.Line = f.Line
			backfill(&next, f, r.PriorComments[id])
			writes = append(writes, pending{row: &next})
			rep.Hygiene.noteWritten(f, row)
		case row.State == db.PublishedStateOpen:
			file, _ := payload.FingerprintParts(id)
			if guard.blocksFix(file) {
				// The round cannot tell whether the point is fixed, so it is
				// neither fixed nor counted as still open.
				continue
			}
			cs, fixed, hunksUnknown := r.fixedSince(row, p.Policy.FixedByFile)
			if hunksUnknown {
				rep.HunksUnknown++
				if rep.HunksUnknown == 1 {
					log.Printf("[PUBLISH] %s/%s#%d: compare %s...%s carries no hunks; absent findings on its files stay open", r.Owner, r.Repo, r.Number, shortSHA(row.LastSeenSHA), shortSHA(r.HeadSHA))
				}
			}
			if fixed {
				d.Fixed++
				next := *row
				next.State = db.PublishedStateFixed
				writes = append(writes, pending{row: &next, thread: resolveThread})
				if t, found := threads.thread(ctx, row.ThreadNodeID, row.CommentID); found {
					next.ThreadNodeID = t.NodeID
				}
				// The stored id stands in for the resolve, but the note needs the
				// listing to show the thread open.
				if t, listed := threads.lookup(ctx, row.CommentID); listed && !t.Resolved && threads.resolving() && threads.noting() {
					notes = append(notes, threadNote{commentID: row.CommentID, kind: noteNotSeen})
				} else if !listed && threads.listFailed() && threads.resolving() && threads.noting() && row.CommentID != 0 {
					rep.ThreadReplyFailures++
				}
				rep.Hygiene.noteResolved(row, r.HeadSHA, cs.Files)
				if covered, known := r.anchorInHunks(cs, row); known && !covered {
					rep.Hygiene.noteResolvedOutsideHunk(row, r.HeadSHA)
				}
				continue
			}
			d.StillOpen++
			if next, ok := backfilled(row, r.PriorComments[id]); ok {
				writes = append(writes, pending{row: next})
			}
		case concededSinceLastRound(row, isPresent, lastPublished, r.HeadSHA):
			d.Fixed++
			next := *row
			next.Rounds = r.RoundNumber
			writes = append(writes, pending{row: &next})
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
			next.Line = f.Line
			backfill(&next, f, r.PriorComments[id])
			writes = append(writes, pending{row: &next, thread: unresolveThread})
			if t, found := threads.thread(ctx, row.ThreadNodeID, row.CommentID); found {
				next.ThreadNodeID = t.NodeID
			}
			if row.CommentID != 0 {
				notes = append(notes, threadNote{commentID: row.CommentID, kind: noteReturn})
			}
		}
	}
	rep.StillOpen, rep.Fixed = d.StillOpen, d.Fixed
	rep.Confidence = r.confidence()

	postedThisRound, err := p.postInline(ctx, r, sel, rows, now, &rep, threads)
	if err != nil {
		return rep, err
	}
	for id, cid := range postedThisRound {
		r.InlineComments[id] = cid
	}
	summaryLedger, err := p.writeSummary(ctx, r, sel, summaryRow, now, &rep)
	if err != nil {
		return rep, err
	}
	if r.Scope != nil {
		summaryLedger.MergeBase = r.Scope.MergeBase
		if summaryLedger.MergeBase == "" && summaryRow != nil {
			summaryLedger.MergeBase = summaryRow.MergeBase
		}
	}
	notes = renderNotes(notes, r.HeadSHA, p.Policy.LegacyStatusNotes)

	for _, f := range sel.Annotations {
		if _, posted := postedThisRound[f.ID]; posted {
			continue
		}
		row := &db.PublishedFinding{
			RepoOwner: r.Owner, RepoName: r.Repo, PRNumber: r.Number,
			Kind: db.PublishedKindAnnotation, Fingerprint: f.ID,
			SourceTag: r.sourceTag(f.ID), Severity: f.Severity,
			ReviewedSHA: r.HeadSHA, LastSeenSHA: r.HeadSHA,
			State: db.PublishedStateOpen, PublishedAt: now, Line: f.Line,
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
	var actions []threadAction
	for _, w := range writes {
		if err := p.Ledger.UpsertPublishedFinding(w.row); err != nil {
			// Rows written before the failure are never revisited, so their
			// thread changes go out now.
			threads.apply(ctx, actions, &rep)
			return rep, fmt.Errorf("update finding %s: %w", w.row.Fingerprint, err)
		}
		if threads.resolving() && w.thread != noThreadChange && w.row.CommentID != 0 {
			actions = append(actions, threadAction{nodeID: w.row.ThreadNodeID, resolve: w.thread == resolveThread})
		}
	}
	// Written last: HeadPublished reads this row as proof the round completed.
	if err := p.Ledger.UpsertPublishedFinding(summaryLedger); err != nil {
		threads.apply(ctx, actions, &rep)
		return rep, fmt.Errorf("record summary comment: %w", err)
	}

	if replier, ok := p.GH.(ThreadReplier); ok {
		for _, n := range notes {
			// A missed note is not worth failing a round whose ledger is already
			// consistent; the next round sees the same state and says nothing new.
			_, err := replier.PostReply(ctx, r.Owner, r.Repo, r.Number, n.commentID, n.body)
			switch {
			case err != nil && n.external:
				rep.ExternalReplyFailures++
				log.Printf("[PUBLISH] %s/%s#%d: delta reply under comment %d lost: %v", r.Owner, r.Repo, r.Number, n.commentID, err)
			case err != nil:
				rep.ThreadReplyFailures++
				log.Printf("[PUBLISH] %s/%s#%d: thread note on comment %d lost: %v", r.Owner, r.Repo, r.Number, n.commentID, err)
			case n.external:
				rep.ExternalReplies++
			default:
				rep.ThreadReplies++
			}
		}
	}
	threads.apply(ctx, actions, &rep)
	return rep, nil
}

// concededSinceLastRound recognises a row a conceded fix claim marked fixed
// between rounds: stamped with the last published head by the reply path,
// absent now, and not yet counted. Rounds on such a row records the round
// whose summary counted it, so the next round does not count it again.
func concededSinceLastRound(row *db.PublishedFinding, present bool, lastPublished, head string) bool {
	return row.State == db.PublishedStateFixed && !present && row.Rounds == 0 && lastPublished != "" &&
		strings.EqualFold(row.LastSeenSHA, lastPublished) && !strings.EqualFold(row.LastSeenSHA, head)
}

type threadChange int

const (
	noThreadChange threadChange = iota
	resolveThread
	unresolveThread
)

// aliasToLedger gives every current finding that restates a ledger row that
// row's fingerprint. It returns the ids the findings carried before, keyed
// by the id they carry now, for the one case that posts a fresh root.
func (p *Publisher) aliasToLedger(r *Round, rows map[string]*db.PublishedFinding) map[string]string {
	original := map[string]string{}
	placed := map[string]bool{}
	if p.Policy.PriorIDAlias {
		placed = aliasByPriorID(r, rows, original)
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		if !placed[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	own := make([]reconcile.OwnComment, 0, len(rows))
	for _, id := range ids {
		row := rows[id]
		file, bucket := payload.FingerprintParts(id)
		o := reconcile.OwnComment{CommentID: row.CommentID, FindingID: id, File: file, Line: bucket*payload.LineBucket + payload.LineBucket/2, Text: row.CommentText, Kind: row.FindingKind, Subjects: splitSubjects(row.Subjects)}
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
	// A finding its prior_id already placed is settled; only the rest are
	// matched by text and subjects.
	unplaced := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if !placed[f.ID] {
			unplaced = append(unplaced, f)
		}
	}
	aliases := reconcile.AliasPrior(unplaced, own)
	for i := range r.Findings {
		prior, ok := aliases[r.Findings[i].ID]
		if !ok || placed[r.Findings[i].ID] {
			continue
		}
		if !p.Policy.LegacyStatusNotes && !sameSubject(rows[prior], r.Findings[i]) {
			// A text match near a fixed row about other symbols is another
			// point, not the fixed one coming back: it keeps its own identity.
			log.Printf("[PUBLISH] %s/%s#%d: finding %s matches fixed row %s by text but names other subjects, not a return", r.Owner, r.Repo, r.Number, r.Findings[i].ID, prior)
			continue
		}
		original[prior] = r.Findings[i].ID
		r.Findings[i].ID = prior
	}
	return original
}

// sameSubject reports whether a finding may count as the return of a fixed
// row: the row is not fixed, either side names no subject, or they share
// one. Open and terminal rows are matched as before; only a return needs
// the finding re-detected on the root's subject.
func sameSubject(row *db.PublishedFinding, f payload.Finding) bool {
	if row == nil || !fixedState(row.State) || row.Subjects == "" {
		return true
	}
	names := reconcile.SubjectNames(f.FindingContract)
	if len(names) == 0 {
		return true
	}
	for _, mine := range names {
		for _, theirs := range splitSubjects(row.Subjects) {
			if mine == theirs {
				return true
			}
		}
	}
	return false
}

// withoutGuardedReturns drops a finding that would reopen a fixed row on a
// file the guard blocks: nothing is posted, the row stays fixed and its
// thread quiet, and the next round on a settled base sees it again.
func withoutGuardedReturns(r Round, rows map[string]*db.PublishedFinding, guard transitionGuard) []payload.Finding {
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if row, ok := rows[f.ID]; ok && fixedState(row.State) && guard.blocks(f.File) {
			log.Printf("[PUBLISH] %s/%s#%d: finding %s would reopen a fixed row on a guarded path %s, dropped", r.Owner, r.Repo, r.Number, f.ID, f.File)
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// aliasByPriorID hands a finding the identity of the ledger row its
// contract's prior_id names: the agent saw that row and said this is the
// same point, which outranks any text or subject match. A prior_id the
// ledger does not hold, one another finding of the round already carries or
// claimed, or one named by a finding that is itself already a ledger row,
// changes nothing: fingerprint equality outranks the claim. Returns the
// row ids placed this way (now carried by the findings that named them) and
// records each such finding's original id.
func aliasByPriorID(r *Round, rows map[string]*db.PublishedFinding, original map[string]string) map[string]bool {
	claimed := map[string]bool{}
	carried := make(map[string]bool, len(r.Findings))
	for _, f := range r.Findings {
		carried[f.ID] = true
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		if f.FindingContract == nil || f.FindingContract.PriorID == "" {
			continue
		}
		prior := f.FindingContract.PriorID
		if prior == f.ID || carried[prior] || claimed[prior] {
			continue
		}
		if _, self := rows[f.ID]; self {
			continue
		}
		if _, known := rows[prior]; !known {
			continue
		}
		claimed[prior] = true
		original[prior] = f.ID
		f.ID = prior
	}
	return claimed
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
		if from := originalIDs[f.ID]; from != "" && from != f.ID {
			log.Printf("[PUBLISH] %s/%s#%d: finding %s aliased to %s row %s, dropped", r.Owner, r.Repo, r.Number, from, row.State, f.ID)
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

// settledState reports whether a row's point is settled on this PR: the
// terminal states, and fixed (by a change or a conceded fix claim), where a
// restatement from elsewhere is a repeat rather than a return.
func settledState(state string) bool {
	return terminalState(state) || fixedState(state)
}

// withoutSettled drops every finding the alias tiers left unmatched that
// restates a settled row from another file or another part of its file:
// same kind, a specific subject PRism named in its own replies on the row's
// thread (the concession, withdrawal or verification), and the finding's
// text inside that thread's text. The row is left as it is: nothing
// returns, nothing is posted, nothing is counted.
func withoutSettled(r Round, rows map[string]*db.PublishedFinding) ([]payload.Finding, int) {
	var points []reconcile.SettledPoint
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := rows[id]
		if !settledState(row.State) {
			continue
		}
		prior := r.PriorComments[id]
		if len(prior.OwnReplies) == 0 {
			// Only PRism's own reply says what the thread settled.
			continue
		}
		text := row.CommentText
		if text == "" {
			text = StripRendered(prior.Text)
		}
		var replies []string
		for _, reply := range prior.OwnReplies {
			replies = append(replies, StripRendered(reply))
		}
		file, _ := payload.FingerprintParts(id)
		points = append(points, reconcile.SettledPoint{FindingID: id, File: file, Kind: row.FindingKind, Text: text, Replies: strings.Join(replies, " ")})
	}
	if len(points) == 0 {
		return r.Findings, 0
	}
	unmatched := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if _, known := rows[f.ID]; !known {
			unmatched = append(unmatched, f)
		}
	}
	matches := reconcile.MatchSettled(unmatched, points)
	if len(matches) == 0 {
		return r.Findings, 0
	}
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if settled, ok := matches[f.ID]; ok {
			log.Printf("[PUBLISH] %s/%s#%d: finding %s restates settled row %s (%s), dropped", r.Owner, r.Repo, r.Number, f.ID, settled, rows[settled].State)
			continue
		}
		kept = append(kept, f)
	}
	return kept, len(matches)
}

// withoutBodyCovered drops the folded notes whose point the pull request
// body already states; findings above the inline bar are never held this
// way, since a stated behaviour can still be a defect worth a thread, and
// neither is a finding the ledger holds a row for, whose absence the table
// below would read as a fix.
func withoutBodyCovered(r Round, rows map[string]*db.PublishedFinding) ([]payload.Finding, int) {
	if strings.TrimSpace(r.PRBody) == "" {
		return r.Findings, 0
	}
	kept := make([]payload.Finding, 0, len(r.Findings))
	covered := 0
	for _, f := range r.Findings {
		if _, known := rows[f.ID]; known {
			kept = append(kept, f)
			continue
		}
		if summaryNote(f) && reconcile.BodyCovers(f, r.PRBody) {
			log.Printf("[PUBLISH] %s/%s#%d: note %s is stated in the pull request body, held", r.Owner, r.Repo, r.Number, f.ID)
			covered++
			continue
		}
		kept = append(kept, f)
	}
	return kept, covered
}

// summaryNote reports whether a finding reaches GitHub only as a folded
// summary note: confirmed and publishable, but below the inline bar.
func summaryNote(f payload.Finding) bool {
	return Publishable(f) && !Shown(f)
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
func (p *Publisher) clampSeverity(r *Round, rows map[string]*db.PublishedFinding, guard transitionGuard) []threadNote {
	var notes []threadNote
	for i := range r.Findings {
		f := &r.Findings[i]
		row, ok := rows[f.ID]
		if !ok || terminalState(row.State) || row.Severity == "" || severityRank(row.Severity) == severityRank(f.Severity) {
			continue
		}
		if guard.blocks(f.File) || !r.anchorChanged(row.LastSeenSHA, f.File, f.Line) {
			f.Severity = row.Severity
			continue
		}
		if row.CommentID != 0 {
			notes = append(notes, threadNote{commentID: row.CommentID, kind: noteSeverity, from: row.Severity, to: f.Severity})
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
// base: by line when the change set carries lines for the file, by hunk
// (head side) when it carries hunks, else by the file having changed at
// all. Unknown changes never count as a change.
func (r Round) anchorChanged(base, file string, line int) bool {
	cs, ok := r.changesSince(base)
	if !ok || !cs.Files[file] {
		return false
	}
	if line <= 0 {
		return true
	}
	if lines, known := cs.Lines[file]; known {
		for l := line - anchorTolerance; l <= line+anchorTolerance; l++ {
			if lines[l] {
				return true
			}
		}
		return false
	}
	if cs.Hunks != nil {
		return hunksCover(cs.Hunks[file], line, line, true)
	}
	return true
}

// fixedSince judges an absent open row fixed: its file changed since the
// row's last-seen head and, unless byFile restores the file rule, the lines
// it cited fall inside a changed hunk of that file. A file the compare lists
// without a patch (too large, binary) proves nothing, and neither does a
// compare that carries no hunks at all; hunksUnknown reports the latter so
// the round can say once that it held the row.
func (r Round) fixedSince(row *db.PublishedFinding, byFile bool) (cs ChangeSet, fixed, hunksUnknown bool) {
	cs, known := r.changesSince(row.LastSeenSHA)
	file := payload.FingerprintFile(row.Fingerprint)
	if !known || !cs.Files[file] {
		return cs, false, false
	}
	if byFile {
		return cs, true, false
	}
	covered, hunksKnown := r.anchorInHunks(cs, row)
	if !hunksKnown {
		return cs, false, cs.Hunks == nil
	}
	return cs, covered, false
}

// anchorInHunks reports whether the row's cited lines fall inside a hunk of
// its file in the change set, and whether the set could tell (it carries
// hunks and has the file's). The anchor is the row's stored line, in the
// base's coordinates; for a row written before the column, the line GitHub
// shows for its comment, in the head's; else the fingerprint's line bucket.
func (r Round) anchorInHunks(cs ChangeSet, row *db.PublishedFinding) (covered, known bool) {
	if cs.Hunks == nil {
		return false, false
	}
	file, bucket := payload.FingerprintParts(row.Fingerprint)
	hunks, ok := cs.Hunks[file]
	if !ok || len(hunks) == 0 {
		return false, false
	}
	start, end, head := row.Line, row.Line, false
	if start <= 0 {
		if prior := r.PriorComments[row.Fingerprint]; prior.Line > 0 {
			start, end, head = prior.Line, prior.Line, true
		} else {
			start, end = bucket*payload.LineBucket, bucket*payload.LineBucket+payload.LineBucket-1
		}
	}
	return hunksCover(hunks, start, end, head), true
}

// RowAnchorInHunks is anchorInHunks for a caller outside a round, such as
// the replay harness judging a resolve it observed: the row's stored line
// or bucket against the change set's hunks.
func RowAnchorInHunks(cs ChangeSet, row db.PublishedFinding) (covered, known bool) {
	return Round{}.anchorInHunks(cs, &row)
}

// hunksCover reports whether any hunk, on its base side (or head side when
// head is set), comes within anchorTolerance lines of the range start..end.
func hunksCover(hunks []Hunk, start, end int, head bool) bool {
	for _, h := range hunks {
		from, count := h.OldStart, h.OldLines
		if head {
			from, count = h.NewStart, h.NewLines
		}
		if count < 1 {
			count = 1
		}
		to := from + count - 1
		if start <= to+anchorTolerance && end >= from-anchorTolerance {
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
