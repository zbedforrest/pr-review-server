package publisher

import (
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
)

// unrelatedC1 is a wording of the a.go finding that shares no words and no
// line with the posted one, so only a prior_id can tie it to its row.
const unrelatedC1 = "Request path drops the trailing segment before routing."

func priorIDRound(head, priorID string) Round {
	r := ledgerRoundOne()
	r.HeadSHA, r.RoundNumber = head, 0
	moved := f(fid("a.go", 60, unrelatedC1), "critical", "a.go", 60, unrelatedC1)
	moved.FindingContract.PriorID = priorID
	r.Findings = []payload.Finding{
		f("sum", "unknown", "SUMMARY", 0, "Narrative."),
		moved,
		f(m1ID, "medium", "b.go", 20, "Medium thing."),
		f(m2ID, "medium", "a.go", 99, "Medium outside hunk."),
	}
	r.Commentable["a.go"][60] = true
	return r
}

func TestPriorID_AliasesAFindingNoTextMatchWouldReach(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	rep := publishRound(t, gh, ledger, priorIDRound("sha-2", c1ID))
	if rep.InlinePosted != 0 || len(gh.reviews) != 1 {
		t.Fatalf("a finding naming its prior must not become a second root: %+v reviews=%d", rep, len(gh.reviews))
	}
	if rep.StillOpen != 3 || rep.Fixed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if row := ledger.rows[c1ID]; row.LastSeenSHA != "sha-2" || row.State != db.PublishedStateOpen {
		t.Fatalf("the named row is refreshed in place: %+v", row)
	}
	if _, fresh := ledger.rows[fid("a.go", 60, unrelatedC1)]; fresh {
		t.Fatal("the new wording must not get its own row")
	}
	if !strings.Contains(gh.issueEdits[501], "**Since last review:** 0 new · 3 still open · 0 fixed") {
		t.Fatalf("summary:\n%s", gh.issueEdits[501])
	}
}

func TestPriorID_WithoutItTheSameWordingPostsAgain(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	rep := publishRound(t, gh, ledger, priorIDRound("sha-2", ""))
	if rep.InlinePosted != 1 {
		t.Fatalf("the control: with no prior_id this wording is a new finding: %+v", rep)
	}
}

func TestPriorID_KillSwitchFallsBackToTextMatching(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	pol := testPolicy()
	pol.PriorIDAlias = false
	rep := publishWith(t, gh, ledger, priorIDRound("sha-2", c1ID), pol)
	if rep.InlinePosted != 1 {
		t.Fatalf("PUBLISH_PRIOR_ID_ALIAS=false must ignore prior_id: %+v", rep)
	}
}

func TestPriorID_NamingASettledRowDropsTheFinding(t *testing.T) {
	for _, state := range []string{db.PublishedStateDismissed, db.PublishedStateContested, db.PublishedStateExternal} {
		t.Run(state, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			publishRound(t, gh, ledger, ledgerRoundOne())
			ledger.rows[c1ID].State = state
			rep := publishRound(t, gh, ledger, priorIDRound("sha-2", c1ID))
			if rep.InlinePosted != 0 || rep.StillOpen != 2 {
				t.Fatalf("a wording the agent ties to a settled row is terminal: %+v", rep)
			}
			if ledger.rows[c1ID].State != state {
				t.Fatalf("the settled row is untouched: %+v", ledger.rows[c1ID])
			}
			if _, fresh := ledger.rows[fid("a.go", 60, unrelatedC1)]; fresh {
				t.Fatal("no fresh row for a settled point")
			}
		})
	}
}

func TestPriorID_NamingAFixedRowReopensIt(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	ledger.rows[c1ID].State = db.PublishedStateFixed
	rep := publishRound(t, gh, ledger, priorIDRound("sha-2", c1ID))
	if rep.InlinePosted != 0 || rep.Reopened != 1 {
		t.Fatalf("the fixed row comes back in its own thread, never as a new root: %+v", rep)
	}
	if ledger.rows[c1ID].State != db.PublishedStateOpen {
		t.Fatalf("row = %+v", ledger.rows[c1ID])
	}
}

func TestPriorID_UnknownAndContestedReferencesAreIgnored(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	r := priorIDRound("sha-2", "a.go:1:000000000000")
	rep := publishRound(t, gh, ledger, r)
	if rep.InlinePosted != 1 {
		t.Fatalf("a prior_id the ledger does not hold changes nothing: %+v", rep)
	}

	gh, ledger = newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	r = priorIDRound("sha-3", c1ID)
	second := f(fid("a.go", 70, "Another distinct point about routing here."), "medium", "a.go", 70, "Another distinct point about routing here.")
	second.FindingContract.PriorID = c1ID
	r.Findings = append(r.Findings, second)
	r.Commentable["a.go"][70] = true
	rep = publishRound(t, gh, ledger, r)
	if rep.InlinePosted != 1 {
		t.Fatalf("the first finding keeps the row, the second posts on its own: %+v", rep)
	}
	if ledger.rows[c1ID].LastSeenSHA != "sha-3" {
		t.Fatalf("row = %+v", ledger.rows[c1ID])
	}
	if _, own := ledger.rows[second.ID]; !own {
		t.Fatal("the second claimant gets its own row")
	}
}

func TestPriorID_VerbatimCarrierOutranksAClaim(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	r := priorIDRound("sha-2", c1ID)
	r.Findings = append(r.Findings, f(c1ID, "critical", "a.go", 10, "Critical thing."))
	rep := publishRound(t, gh, ledger, r)
	if rep.InlinePosted != 1 {
		t.Fatalf("a finding still carrying the row's id keeps it; the claimant is new: %+v", rep)
	}
	if _, own := ledger.rows[fid("a.go", 60, unrelatedC1)]; !own {
		t.Fatal("the claimant gets its own row")
	}
}

func TestPriorID_PlacedFindingIsNotRematchedByText(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())
	// The claimant names the summary-only a.go:99 row while its wording sits
	// at a.go:60, a commentable line: the row is promoted inline under its
	// own id, and no text match may hand the finding a different row.
	r := priorIDRound("sha-2", m2ID)
	r.Findings = r.Findings[:3]
	rep := publishRound(t, gh, ledger, r)
	if rep.InlinePosted != 1 || rep.StillOpen != 3 {
		t.Fatalf("report = %+v", rep)
	}
	if row := ledger.rows[m2ID]; row.LastSeenSHA != "sha-2" || row.CommentID == 0 {
		t.Fatalf("the named row is refreshed and promoted: %+v", row)
	}
	if ledger.rows[c1ID].LastSeenSHA != "sha-round-1" {
		t.Fatalf("c1 must not be matched to the placed finding: %+v", ledger.rows[c1ID])
	}
	if _, fresh := ledger.rows[fid("a.go", 60, unrelatedC1)]; fresh {
		t.Fatal("the claimant must not get its own row")
	}
}

func TestPriorID_AFindingAlreadyOnTheLedgerKeepsItsOwnRow(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, ledgerRoundOne())

	r := ledgerRoundOne()
	r.HeadSHA, r.RoundNumber = "sha-2", 0
	r.Findings[1].FindingContract.PriorID = m2ID
	publishRound(t, gh, ledger, r)
	if row := ledger.rows[c1ID]; row.LastSeenSHA != "sha-2" || row.State != db.PublishedStateOpen {
		t.Fatalf("the verbatim row outranks the prior_id claim: %+v", row)
	}
	if row := ledger.rows[m2ID]; row.LastSeenSHA != "sha-2" || row.State != db.PublishedStateOpen {
		t.Fatalf("the named row is untouched by the mislabelled claim: %+v", row)
	}
	if len(gh.reviews) != 1 {
		t.Fatalf("no new root: reviews=%d", len(gh.reviews))
	}
}
