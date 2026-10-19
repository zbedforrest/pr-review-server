package publisher

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func verified(x payload.Finding, status string, applied bool) payload.Finding {
	x.Verification = &types.Verification{Status: status, Applied: applied, Reason: "The base branch already guards this call", Evidence: "base b.go:20: if conn == nil { return }"}
	return x
}

func withM1(r Round, m1 payload.Finding) Round {
	for i := range r.Findings {
		if r.Findings[i].ID == m1ID {
			r.Findings[i] = m1
		}
	}
	return r
}

func TestVerify_SuppressedNewFindingNeverPostsOrGetsARow(t *testing.T) {
	for status, reason := range map[string]string{types.VerifyHold: HoldVerifierHold, types.VerifyDrop: HoldVerifierDrop} {
		t.Run(status, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			r := ledgerRoundOne()
			r = withM1(r, verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), status, true))
			rep := publishRound(t, gh, ledger, r)
			if row := ledger.get(db.PublishedKindFinding, m1ID); row != nil {
				t.Fatalf("a suppressed finding gets no row: %+v", row)
			}
			if len(rep.Held) != 1 || rep.Held[0].ID != m1ID || rep.Held[0].Reason != reason {
				t.Fatalf("held = %+v", rep.Held)
			}
			if len(gh.issueCreates) != 1 || strings.Contains(gh.issueCreates[0], "Medium thing") {
				t.Fatalf("the summary must not list it:\n%v", gh.issueCreates)
			}
			for _, rv := range gh.reviews {
				for _, c := range rv.comments {
					if strings.Contains(c.Body, "Medium thing") {
						t.Fatal("a suppressed finding was posted inline")
					}
				}
			}
		})
	}
}

func TestVerify_ShadowDecisionIsIgnored(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, false))
	rep := publishRound(t, gh, ledger, r)
	if row := ledger.get(db.PublishedKindFinding, m1ID); row == nil || row.State != db.PublishedStateOpen {
		t.Fatalf("a shadow decision changes nothing: row=%+v", row)
	}
	if len(rep.Held) != 0 || rep.Refuted != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerify_ACriticalDropIsReadAsAHold(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := ledgerRoundOne()
	for i := range r.Findings {
		if r.Findings[i].ID == c1ID {
			r.Findings[i] = verified(r.Findings[i], types.VerifyDrop, true)
		}
	}
	rep := publishRound(t, gh, ledger, r)
	if len(rep.Held) != 1 || rep.Held[0].ID != c1ID || rep.Held[0].Reason != HoldVerifierHold {
		t.Fatalf("held = %+v", rep.Held)
	}
	if row := ledger.get(db.PublishedKindFinding, c1ID); row != nil {
		t.Fatalf("a held critical is not posted: %+v", row)
	}
}

func TestVerify_HeldPostedFindingStaysOpenAndSilent(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyHold, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	rep := publishRound(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, m1ID)
	if row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-2" || rep.Fixed != 0 || rep.Refuted != 0 || len(gh.replies) != 0 {
		t.Fatalf("a held root stays open and silent: row=%+v rep=%+v replies=%v", row, rep, gh.replies)
	}
	if !strings.Contains(gh.issueEdits[501], "Medium thing") {
		t.Fatalf("an open thread is still listed, held or not:\n%s", gh.issueEdits[501])
	}
}

func TestVerify_RefutedPostedFindingIsWithdrawnResolvedAndNeverFixed(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	m1 := ledger.get(db.PublishedKindFinding, m1ID)
	if m1 == nil || m1.CommentID == 0 {
		t.Fatalf("round one posted m1: %+v", m1)
	}

	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	rep := publishThreads(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, m1ID)
	if row.State != db.PublishedStateDismissed || rep.Refuted != 1 || rep.Fixed != 0 {
		t.Fatalf("a refuted root is dismissed, not fixed: row=%+v rep=%+v", row, rep)
	}
	replies := gh.repliesTo(m1.CommentID)
	if len(replies) != 1 || !strings.HasPrefix(replies[0], "Withdrawn:") || !strings.Contains(replies[0], "already guards this call") || !strings.Contains(replies[0], "if conn == nil") {
		t.Fatalf("withdrawal reply = %q", replies)
	}
	if fmt.Sprint(gh.resolves) != fmt.Sprint([]string{threadNode(m1.CommentID)}) || rep.ThreadsResolved != 1 {
		t.Fatalf("resolved = %v, want the m1 thread only (rep %+v)", gh.resolves, rep)
	}
	summary := gh.issueEdits[501]
	if strings.Contains(summary, "Medium thing") || !strings.Contains(summary, "0 fixed") {
		t.Fatalf("a refuted finding is neither open nor fixed in the summary:\n%s", summary)
	}

	r3 := ledgerRoundOne()
	r3.HeadSHA, r3.RoundNumber, r3.Changes = "sha-3", 0, changed("b.go")
	reviews := len(gh.reviews)
	publishThreads(t, gh, ledger, r3)
	if ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateDismissed || len(gh.reviews) != reviews {
		t.Fatalf("a withdrawn finding is never reposted: row=%+v reviews=%d", ledger.get(db.PublishedKindFinding, m1ID), len(gh.reviews))
	}
}

func TestVerify_RevisedFindingKeepsItsRowAndRendersTheRevision(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	revised := f(fid("b.go", 20, "Medium thing, now with the right mechanism."), "medium", "b.go", 20, "Medium thing, now with the right mechanism.")
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r2 := withM1(ledgerRoundOne(), revised)
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	reviews := len(gh.reviews)
	rep := publishRound(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, m1ID)
	if row == nil || row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-2" || rep.Fixed != 0 {
		t.Fatalf("the revised finding is the posted row, still open: row=%+v rep=%+v", row, rep)
	}
	if other := ledger.get(db.PublishedKindFinding, revised.ID); other != nil || len(gh.reviews) != reviews {
		t.Fatalf("a revision opens no second root: other=%+v reviews=%d", other, len(gh.reviews))
	}
	if !strings.Contains(gh.issueEdits[501], "right mechanism") {
		t.Fatalf("the summary renders the revision:\n%s", gh.issueEdits[501])
	}
}
