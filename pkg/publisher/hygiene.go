package publisher

import (
	"fmt"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// HygieneNote is one publication defect the round committed, keyed by the
// ledger fingerprint it concerns.
type HygieneNote struct {
	Fingerprint string
	Detail      string
}

// Hygiene counts what a round did that a comment-must-add-context policy
// forbids: posting a root the PR already carries, resolving a finding no
// code change could have fixed, or raising a severity the ledger already
// settled. Publish fills it from the ledger it was given; the poller turns
// each note into a telemetry event.
type Hygiene struct {
	RepeatedPosts          []HygieneNote
	RepeatAfterDismiss     []HygieneNote
	FixedWithoutFileChange []HygieneNote
	SameCommitResolves     []HygieneNote
	SeverityEscalations    []HygieneNote
}

// priorRows indexes every finding and annotation row the ledger holds for the
// PR, whatever its state, by fingerprint.
func priorRows(previous []db.PublishedFinding) map[string]*db.PublishedFinding {
	rows := make(map[string]*db.PublishedFinding, len(previous))
	for i := range previous {
		row := &previous[i]
		if row.Kind == db.PublishedKindFinding || row.Kind == db.PublishedKindAnnotation {
			rows[row.Fingerprint] = row
		}
	}
	return rows
}

// notePosted records the defects of posting f as a new root: a repeat when
// the fingerprint already had a root comment (an open row is never reposted,
// so this is a resolved or dismissed one coming back), a repeat after
// dismiss when that row was dismissed. Aliasing widens what "same
// fingerprint" means upstream; this reads the result.
func (h *Hygiene) notePosted(f payload.Finding, prior *db.PublishedFinding) {
	if prior == nil || prior.CommentID == 0 {
		return
	}
	h.RepeatedPosts = append(h.RepeatedPosts, HygieneNote{Fingerprint: f.ID, Detail: fmt.Sprintf("prior_state=%s prior_comment=%d", prior.State, prior.CommentID)})
	if prior.State == db.PublishedStateDismissed {
		h.RepeatAfterDismiss = append(h.RepeatAfterDismiss, HygieneNote{Fingerprint: f.ID, Detail: fmt.Sprintf("prior_comment=%d", prior.CommentID)})
	}
}

// noteWritten records a severity escalation when f is written over a row the
// ledger holds at a lower severity, inline or annotation alike.
func (h *Hygiene) noteWritten(f payload.Finding, prior *db.PublishedFinding) {
	if prior == nil || severityRank(f.Severity) <= severityRank(prior.Severity) {
		return
	}
	h.SeverityEscalations = append(h.SeverityEscalations, HygieneNote{Fingerprint: f.ID, Detail: fmt.Sprintf("from=%s to=%s prior_state=%s", prior.Severity, f.Severity, prior.State)})
}

// noteResolved records a row flipped to resolved without evidence of a fix:
// on the same head it was last seen at, or on a head whose changed-file set
// (when the round knows it) does not include the cited file.
func (h *Hygiene) noteResolved(row *db.PublishedFinding, headSHA string, changedFiles map[string]bool) {
	if row.LastSeenSHA == headSHA {
		h.SameCommitResolves = append(h.SameCommitResolves, HygieneNote{Fingerprint: row.Fingerprint, Detail: "head=" + headSHA})
		return
	}
	if changedFiles == nil {
		return
	}
	if file := payload.FingerprintFile(row.Fingerprint); !changedFiles[file] {
		h.FixedWithoutFileChange = append(h.FixedWithoutFileChange, HygieneNote{Fingerprint: row.Fingerprint, Detail: fmt.Sprintf("file=%s from=%s to=%s", file, row.LastSeenSHA, headSHA)})
	}
}
