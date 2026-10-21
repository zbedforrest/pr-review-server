package publisher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/pkg/reviewer/types"
)

var errTestReply = errors.New("reply rejected")

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

func TestVerify_ShadowOrNonReviseDecisionDoesNotAlias(t *testing.T) {
	for name, v := range map[string]*types.Verification{
		"shadow revise": {Status: types.VerifyRevise, Applied: false, PriorComment: "Medium thing."},
		"applied keep":  {Status: types.VerifyKeep, Applied: true, PriorComment: "Medium thing."},
	} {
		t.Run(name, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			publishRound(t, gh, ledger, ledgerRoundOne())
			other := f(fid("b.go", 20, "Another point on this line."), "medium", "b.go", 20, "Another point on this line.")
			other.Verification = v
			r2 := withM1(ledgerRoundOne(), other)
			r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
			publishRound(t, gh, ledger, r2)
			if row := ledger.get(db.PublishedKindFinding, other.ID); row == nil {
				t.Fatal("without an applied revise the finding keeps its own identity and posts")
			}
		})
	}
}

func TestVerify_RevisedFindingAliasesByPriorTextWhenTheLineDrifted(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	revised := f(fid("b.go", 47, "Medium thing, revised."), "medium", "b.go", 47, "Medium thing, revised.")
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r2 := withM1(ledgerRoundOne(), revised)
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	r2.Commentable["b.go"][47] = true
	reviews := len(gh.reviews)
	rep := publishRound(t, gh, ledger, r2)
	if row := ledger.get(db.PublishedKindFinding, m1ID); row == nil || row.State != db.PublishedStateOpen || row.Line != 47 || rep.Fixed != 0 {
		t.Fatalf("the drifted revision is still the posted row: row=%+v rep=%+v", row, rep)
	}
	if ledger.get(db.PublishedKindFinding, revised.ID) != nil || len(gh.reviews) != reviews {
		t.Fatalf("no second root: reviews=%d", len(gh.reviews))
	}
}

func TestVerify_LegacyLedgerKeepsARefutedRootOpenAndListed(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	legacyRound(t, gh, ledger, roundOne())
	r2 := roundOne()
	for i := range r2.Findings {
		if r2.Findings[i].ID == "m1" {
			r2.Findings[i] = verified(r2.Findings[i], types.VerifyDrop, true)
		}
	}
	r2.HeadSHA, r2.RoundNumber = "sha-2", 0
	rep := legacyRound(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, "m1")
	if row == nil || row.State != db.PublishedStateOpen || rep.Refuted != 0 || len(gh.replies) != 0 || len(rep.Held) != 0 {
		t.Fatalf("the legacy ledger withdraws nothing; the root stays open: row=%+v rep=%+v", row, rep)
	}
	if !strings.Contains(gh.issueEdits[501], "Medium thing") {
		t.Fatalf("its thread is still open, so it is still listed:\n%s", gh.issueEdits[501])
	}
}

func TestVerify_SuppressedFoldedNoteNeverRenders(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := ledgerRoundOne()
	for i := range r.Findings {
		if r.Findings[i].ID == "l1" {
			r.Findings[i] = verified(r.Findings[i], types.VerifyHold, true)
		}
	}
	rep := publishRound(t, gh, ledger, r)
	if len(rep.Held) != 1 || rep.Held[0].ID != "l1" || rep.Held[0].Reason != HoldVerifierHold {
		t.Fatalf("held = %+v", rep.Held)
	}
	if strings.Contains(gh.issueCreates[0], "Low thing") {
		t.Fatalf("a held low note is not folded into the summary:\n%s", gh.issueCreates[0])
	}
}

func TestVerify_ACriticalDropOnAPostedRootStaysOpen(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	r2 := ledgerRoundOne()
	for i := range r2.Findings {
		if r2.Findings[i].ID == c1ID {
			r2.Findings[i] = verified(r2.Findings[i], types.VerifyDrop, true)
		}
	}
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("a.go")
	rep := publishThreads(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, c1ID)
	if row.State != db.PublishedStateOpen || rep.Refuted != 0 || rep.Fixed != 0 || len(gh.replies) != 0 || len(gh.resolves) != 0 {
		t.Fatalf("a critical is never withdrawn: row=%+v rep=%+v replies=%v resolves=%v", row, rep, gh.replies, gh.resolves)
	}
	if !strings.Contains(gh.issueEdits[501], "Critical thing") {
		t.Fatalf("the open critical is still listed:\n%s", gh.issueEdits[501])
	}
}

func TestVerify_LostWithdrawalLeavesTheRootOpenForTheNextRound(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	gh.replyErr = errTestReply
	rep := publishThreads(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, m1ID)
	if row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-2" || rep.Refuted != 0 || rep.Fixed != 0 || rep.ThreadReplyFailures != 1 || len(gh.resolves) != 0 {
		t.Fatalf("a lost withdrawal resolves nothing: row=%+v rep=%+v resolves=%v", row, rep, gh.resolves)
	}
	if rep.StillOpen != 3 || !strings.Contains(gh.issueEdits[501], "Medium thing") || !strings.Contains(gh.issueEdits[501], "3 still open") {
		t.Fatalf("the root is still open on GitHub, so it is listed and counted: still_open=%d\n%s", rep.StillOpen, gh.issueEdits[501])
	}
	gh.replyErr = nil
	r3 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r3.HeadSHA, r3.RoundNumber, r3.Changes = "sha-3", 0, changed("b.go")
	rep = publishThreads(t, gh, ledger, r3)
	if ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateDismissed || rep.Refuted != 1 || len(gh.repliesTo(row.CommentID)) != 1 {
		t.Fatalf("the next round withdraws it: rep=%+v", rep)
	}
}

func TestVerify_SecondRevisionFollowsTheFirst(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	first := f(fid("b.go", 20, "Medium thing, first revision."), "medium", "b.go", 20, "Medium thing, first revision.")
	first.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r2 := withM1(ledgerRoundOne(), first)
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	publishRound(t, gh, ledger, r2)
	if row := ledger.get(db.PublishedKindFinding, m1ID); row.CommentText != first.Comment {
		t.Fatalf("the row follows the revision: %+v", row)
	}
	second := f(fid("b.go", 20, "Medium thing, second revision."), "medium", "b.go", 20, "Medium thing, second revision.")
	second.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: first.Comment}
	r3 := withM1(ledgerRoundOne(), second)
	r3.HeadSHA, r3.RoundNumber, r3.Changes = "sha-3", 0, changed("b.go")
	reviews := len(gh.reviews)
	publishRound(t, gh, ledger, r3)
	if ledger.get(db.PublishedKindFinding, second.ID) != nil || len(gh.reviews) != reviews || ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateOpen {
		t.Fatal("the second revision stays on the same root")
	}
}

func TestVerify_AmbiguousPriorTextKeepsTheRevisionItsOwnIdentity(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r1 := ledgerRoundOne()
	twin := f(fid("b.go", 60, "Medium thing."), "medium", "b.go", 60, "Medium thing.")
	r1.Findings = append(r1.Findings, twin)
	r1.Commentable["b.go"][60] = true
	publishRound(t, gh, ledger, r1)
	revised := f(fid("b.go", 47, "Medium thing, revised."), "medium", "b.go", 47, "Medium thing, revised.")
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r2 := withM1(ledgerRoundOne(), revised)
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	r2.Commentable["b.go"][47] = true
	publishRound(t, gh, ledger, r2)
	if ledger.get(db.PublishedKindFinding, revised.ID) == nil {
		t.Fatal("two rows with the prior text and neither carried this round: the revision posts under its own id rather than guessing")
	}
}

func TestVerify_WithdrawalReplyIsHygienicAndPostedOnce(t *testing.T) {
	note := withdrawalNote(&types.Verification{Reason: "(A-1) The base <b>already</b> guards this ```", Evidence: "b.go:20\n```\nif conn == nil { return }"})
	if strings.Contains(note, "A-1") || !strings.Contains(note, "`<b>`") || strings.Count(note, "```") != 2 || !strings.HasPrefix(note, withdrawalPrefix) {
		t.Fatalf("note = %q", note)
	}

	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	m1 := ledger.get(db.PublishedKindFinding, m1ID)
	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	r2.PriorComments = map[string]PriorComment{m1ID: {Line: 20, Text: "Medium thing.", OwnReplies: []string{"Withdrawn: the code does not support this.\n\n" + ReplyMarker(m1.CommentID)}}}
	rep := publishThreads(t, gh, ledger, r2)
	if len(gh.repliesTo(m1.CommentID)) != 0 || rep.Refuted != 1 || ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateDismissed {
		t.Fatalf("a withdrawal GitHub already shows is not posted again: replies=%v rep=%+v", gh.replies, rep)
	}
}

func TestVerify_WithdrawalCarriesTheOwnReplyMarker(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	m1 := ledger.get(db.PublishedKindFinding, m1ID)
	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	publishThreads(t, gh, ledger, r2)
	replies := gh.repliesTo(m1.CommentID)
	if len(replies) != 1 || !reconcile.IsOwnReply(replies[0]) || !strings.HasPrefix(replies[0], withdrawalPrefix) {
		t.Fatalf("the withdrawal is marked as PRism's own reply: %q", replies)
	}
}

func TestVerify_SummaryRefreshKeepsARefutedRootListed(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}
	refresh := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	refresh.RoundNumber, refresh.HeadSHA, refresh.Previous = 0, "", nil
	refresh.PushesCapped = true
	p := &Publisher{GH: gh, Ledger: ledger, Policy: testPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	body, edited := gh.issueEdits[501]
	if !edited || !strings.Contains(body, "Medium thing") {
		t.Fatalf("a refresh withdraws nothing, so the root stays listed (edited=%v):\n%s", edited, body)
	}
	if len(gh.replies) != 0 || ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateOpen {
		t.Fatalf("a refresh posts and dismisses nothing: replies=%v", gh.replies)
	}
}

func TestVerify_RevisionOfAWithdrawnPointStaysWithdrawn(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	publishThreads(t, gh, ledger, ledgerRoundOne())
	r2 := withM1(ledgerRoundOne(), verified(f(m1ID, "medium", "b.go", 20, "Medium thing."), types.VerifyDrop, true))
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	publishThreads(t, gh, ledger, r2)

	revised := f(fid("b.go", 20, "Medium thing, revised."), "medium", "b.go", 20, "Medium thing, revised.")
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r3 := withM1(ledgerRoundOne(), revised)
	r3.HeadSHA, r3.RoundNumber, r3.Changes = "sha-3", 0, changed("b.go")
	reviews := len(gh.reviews)
	publishThreads(t, gh, ledger, r3)
	if ledger.get(db.PublishedKindFinding, revised.ID) != nil || len(gh.reviews) != reviews || ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateDismissed {
		t.Fatal("a revision of a point PRism withdrew is a rewording of a settled point and does not post")
	}
}

func TestVerify_RevisionWithCorrectedSubjectsStillFindsAFixedRow(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r1 := ledgerRoundOne()
	for i := range r1.Findings {
		if r1.Findings[i].ID == m1ID {
			r1.Findings[i] = withSubjects(r1.Findings[i], "production_behavior", "oldName")
		}
	}
	publishRound(t, gh, ledger, r1)
	fixing := summaryOnly("sha-2")
	fixing.Changes = changed("b.go")
	publishRound(t, gh, ledger, fixing)
	if ledger.get(db.PublishedKindFinding, m1ID).State != db.PublishedStateFixed {
		t.Fatal("round two fixed m1")
	}

	revised := withSubjects(f(fid("b.go", 20, "Medium thing, revised."), "medium", "b.go", 20, "Medium thing, revised."), "production_behavior", "newName")
	priorContract := *withSubjects(f(m1ID, "medium", "b.go", 20, "Medium thing."), "production_behavior", "oldName").FindingContract
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing.", PriorContract: &priorContract}
	r3 := withM1(ledgerRoundOne(), revised)
	r3.HeadSHA, r3.RoundNumber, r3.Changes = "sha-3", 0, changed("b.go")
	reviews := len(gh.reviews)
	rep := publishRound(t, gh, ledger, r3)
	if ledger.get(db.PublishedKindFinding, revised.ID) != nil || len(gh.reviews) != reviews || rep.Reopened != 1 {
		t.Fatalf("the revision is the fixed row returning, not a second root: rep=%+v reviews=%d", rep, len(gh.reviews))
	}
}

func TestVerify_RevisionDoesNotShareARowWithARestatement(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	revised := f(fid("b.go", 20, "Medium thing, revised."), "medium", "b.go", 20, "Medium thing, revised.")
	revised.Verification = &types.Verification{Status: types.VerifyRevise, Applied: true, PriorComment: "Medium thing."}
	r2 := ledgerRoundOne()
	r2.Findings = append(r2.Findings, revised)
	r2.HeadSHA, r2.RoundNumber, r2.Changes = "sha-2", 0, changed("b.go")
	publishRound(t, gh, ledger, r2)
	if ledger.get(db.PublishedKindFinding, revised.ID) == nil {
		t.Fatal("with the restatement already on the row, the revision keeps its own identity")
	}
}
