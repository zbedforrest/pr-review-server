package publisher

import (
	"context"
	"fmt"
	"strings"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
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
			d.StillOpen++
			if written[id] {
				continue
			}
			next := *row
			next.LastSeenSHA = r.HeadSHA
			next.Severity = f.Severity
			backfill(&next, f, r.PriorComments[id])
			writes = append(writes, pending{row: &next})
		case row.State == db.PublishedStateOpen:
			cs, known := r.changesSince(row.LastSeenSHA)
			file, _ := payload.FingerprintParts(id)
			if known && cs.Files[file] {
				d.Fixed++
				next := *row
				next.State = db.PublishedStateFixed
				writes = append(writes, pending{row: &next})
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
			writes = append(writes, pending{row: &next, reopen: true})
			if row.CommentID != 0 {
				notes = append(notes, threadNote{commentID: row.CommentID, body: fmt.Sprintf("Back at %s.", shortSHA(r.HeadSHA))})
			}
		}
	}
	rep.StillOpen, rep.Fixed = d.StillOpen, d.Fixed
	rep.Confidence = Confidence(r.Findings, r.RequiredCheckViolated)
	now := p.now()

	postedThisRound, err := p.postInline(ctx, r, sel, rows, now, &rep)
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
			CommentText: f.Comment, Subjects: subjectsColumn(f),
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
			if _, err := replier.PostReply(ctx, r.Owner, r.Repo, r.Number, n.commentID, n.body); err == nil {
				rep.ThreadReplies++
			}
		}
	}
	return rep, nil
}

// aliasToLedger gives every current finding that restates a ledger row that
// row's fingerprint. It returns the ids the findings carried before, keyed
// by the id they carry now, for the one case that posts a fresh root.
func (p *Publisher) aliasToLedger(r *Round, rows map[string]*db.PublishedFinding) map[string]string {
	own := make([]reconcile.OwnComment, 0, len(rows))
	for id, row := range rows {
		file, bucket := payload.FingerprintParts(id)
		o := reconcile.OwnComment{CommentID: row.CommentID, FindingID: id, File: file, Line: bucket*10 + 5, Text: row.CommentText, Subjects: splitSubjects(row.Subjects)}
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
// changed since the row was last seen may post once more, as a fresh root
// that names the change; a second terminal row on the same point ends that.
func (p *Publisher) withoutTerminal(r Round, rows map[string]*db.PublishedFinding, originalIDs map[string]string) []payload.Finding {
	kept := make([]payload.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		row, ok := rows[f.ID]
		if !ok || !terminalState(row.State) {
			kept = append(kept, f)
			continue
		}
		if !securityException(r, rows, row, f) {
			continue
		}
		fresh := originalIDs[f.ID]
		if fresh == "" || fresh == f.ID {
			continue
		}
		f.ID = fresh
		f.Comment = fmt.Sprintf("Raised again: `%s` changed at %s after this point was settled. %s", f.File, shortSHA(r.HeadSHA), f.Comment)
		kept = append(kept, f)
	}
	return kept
}

func securityException(r Round, rows map[string]*db.PublishedFinding, row *db.PublishedFinding, f payload.Finding) bool {
	c := f.FindingContract
	if f.Severity != "critical" || c == nil || c.FindingKind != "security_risk" || !r.anchorChanged(row.LastSeenSHA, f.File, f.Line) {
		return false
	}
	file, _ := payload.FingerprintParts(row.Fingerprint)
	terminal := 0
	for _, other := range rows {
		otherFile, _ := payload.FingerprintParts(other.Fingerprint)
		if terminalState(other.State) && otherFile == file && (other == row || (other.Subjects != "" && other.Subjects == row.Subjects)) {
			terminal++
		}
	}
	return terminal == 1
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
