package main

import (
	"context"
	"fmt"

	"pr-review-server/db"
	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
)

type closer interface {
	ResolveThread(ctx context.Context, owner, repo, threadNodeID string) error
	CreateReviewCommentReply(ctx context.Context, owner, repo string, number int, commentID int64, body string) (int64, error)
}

type gate func(author string) bool

type applied struct {
	Resolved        int `json:"resolved"`
	Notes           int `json:"notes"`
	NoteFailures    int `json:"note_failures"`
	ResolveFailures int `json:"resolve_failures"`
	GateDenied      int `json:"gate_denied"`
	LedgerRows      int `json:"ledger_rows"`
	LedgerFailures  int `json:"ledger_failures"`
}

func note(d decision) string {
	switch d.Verdict {
	case stalePRMerged:
		return "Resolving: the PR was merged."
	case stalePRClosed:
		return "Resolving: the PR was closed without merging."
	default:
		return fmt.Sprintf("Resolving: %s changed after this comment (now at %s); reopen it if the point still stands.", d.Thread.Path, replaykit.Short(d.PR.HeadSHA))
	}
}

// apply closes every stale thread whose author passes the gate. The resolve
// goes first: a thread that resolved is skipped by every later run, so a
// failing resolve never leaves a trail of notes. The note and the ledger
// row follow and a lost one is only counted.
func apply(ctx context.Context, gh closer, ledger publisher.Ledger, allowed gate, stale []decision, logf func(string, ...any)) applied {
	var a applied
	rows := map[*pr]openRows{}
	for _, d := range stale {
		p := d.PR
		if !allowed(p.Author) {
			a.GateDenied++
			continue
		}
		if d.Thread.NodeID == "" {
			a.ResolveFailures++
			logf("%s: comment %d: no thread node id", p.ref(), d.Thread.RootCommentID)
			continue
		}
		if err := gh.ResolveThread(ctx, p.Owner, p.Repo, d.Thread.NodeID); err != nil {
			a.ResolveFailures++
			logf("%s: resolve %s lost: %v", p.ref(), d.Thread.NodeID, err)
			continue
		}
		a.Resolved++
		if _, err := gh.CreateReviewCommentReply(ctx, p.Owner, p.Repo, p.Number, d.Thread.RootCommentID, note(d)); err != nil {
			a.NoteFailures++
			logf("%s: note on comment %d lost: %v", p.ref(), d.Thread.RootCommentID, err)
		} else {
			a.Notes++
		}
		if ledger == nil {
			continue
		}
		if _, ok := rows[p]; !ok {
			rows[p] = loadOpenRows(ledger, p, logf)
		}
		row := rows[p].find(d.Thread)
		if row == nil {
			continue
		}
		row.State, row.ThreadNodeID = db.PublishedStateFixed, d.Thread.NodeID
		if err := ledger.UpsertPublishedFinding(row); err != nil {
			a.LedgerFailures++
			logf("%s: ledger row %s: %v", p.ref(), row.Fingerprint, err)
			continue
		}
		a.LedgerRows++
	}
	return a
}

// openRows indexes by fingerprint as well because rows predating the
// comment id column have none.
type openRows struct {
	byComment     map[int64]*db.PublishedFinding
	byFingerprint map[string]*db.PublishedFinding
}

func loadOpenRows(ledger publisher.Ledger, p *pr, logf func(string, ...any)) openRows {
	out := openRows{byComment: map[int64]*db.PublishedFinding{}, byFingerprint: map[string]*db.PublishedFinding{}}
	found, err := ledger.GetPublishedFindingsForPR(p.Owner, p.Repo, p.Number)
	if err != nil {
		logf("%s: ledger read: %v", p.ref(), err)
		return out
	}
	for i := range found {
		row := &found[i]
		if row.Kind != db.PublishedKindFinding || row.State != db.PublishedStateOpen {
			continue
		}
		if row.CommentID != 0 {
			out.byComment[row.CommentID] = row
		} else {
			out.byFingerprint[row.Fingerprint] = row
		}
	}
	return out
}

func (r openRows) find(t thread) *db.PublishedFinding {
	if row, ok := r.byComment[t.RootCommentID]; ok {
		return row
	}
	return r.byFingerprint[t.FindingID]
}
