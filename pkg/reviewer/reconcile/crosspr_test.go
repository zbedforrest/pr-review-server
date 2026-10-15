package reconcile

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// The audited pair, anonymised. On the earlier PR the author called the
// purchaser-id fallback point intentional (the backfill is complete and
// the read switch is on); the next PR of the same author raised it again
// from another file as a new MEDIUM, and the author answered a fourth time.
const (
	earlierRootText = "The buyer lookup in `record_fanclub_bulk` hardcodes `purchaser_id`, but every other purchase read branches on `PURCHASER_READ_SWAP` and falls back to parent `user_id` (`Purchases._active`, `store/views.py` methods). The `purchaser_id` column can be null for rows written before it was added or before the backfill ran, so with the switch off the fanout lookup misses older buyers."
	earlierImpact   = "When the purchaser read swap switch is off and purchaser_id is not populated, fanout may write a FAN_CLUB unread row for a member who already bought the set."
	earlierReason   = "intentional. Same answer as the earlier threads on this. The purchaser_id backfill is done in prod (OPS-1821, zero nulls and zero mismatches verified) and purchaser_read_swap is on for both writes and the viewer ownership check. The switch-off state this describes doesn't exist anymore, and fanout writes already use purchaser_id, so reading it here matches what gets written."

	laterDeleteText     = "`_purchase_transaction_ids` filters on `purchaser_id` unconditionally, but `purchaser_id` is a `MirrorBigIntegerField` that was added with `null=True` and is only populated by a separate backfill (`backfill_purchaser`). While the feature switch `PURCHASER_READ_SWAP` is off, the Purchased feed resolves purchases through `user_id`, so a legacy row is listed but the delete lookup cannot find it and returns 400."
	laterDeleteHeadline = "Delete filters on un-backfilled purchaser_id"
	laterDeleteImpact   = "A user whose purchase row has a NULL purchaser_id (pre-backfill) still sees the set in the Purchased feed but gets a 400 on delete and no removal is written."

	laterLoopText     = "`_delete_item` runs one `SELECT`+`UPDATE` (and possibly an `INSERT`) per transaction inside the atomic block, creating an N+1 pattern for a public endpoint. Additionally, `PhotoSetRemoval` has no unique constraint, so overlapping delete requests can both observe zero rows and insert duplicates."
	laterLoopHeadline = "Per-transaction loop adds queries inside the write transaction"
	laterLoopImpact   = "Each purchase of the set adds a query inside the write transaction, holding the atomic block longer than necessary."
)

func earlierPurchaserPoint() EarlierPoint {
	return EarlierPoint{Subjects: []string{"purchaser_read_swap", "record_fanclub_bulk"}, Text: earlierRootText + " " + earlierImpact + " " + earlierReason}
}

func laterFinding(file string, line int, text, headline, impact string, subjects ...types.FindingSubject) payload.Finding {
	return payload.Finding{
		ID: payload.Fingerprint(file, line, text), File: file, Line: line, Severity: "medium", Comment: text, State: "confirmed", Active: true,
		FindingContractStatus: "valid",
		FindingContract: &types.FindingContract{FindingKind: "production_behavior", Materiality: "current_impact", Headline: headline, CurrentImpact: impact,
			Subjects: append([]types.FindingSubject{{Kind: "file", Path: file}}, subjects...)},
	}
}

func laterDeleteFinding() payload.Finding {
	return laterFinding("store/api/views.py", 280, laterDeleteText, laterDeleteHeadline, laterDeleteImpact,
		types.FindingSubject{Kind: "symbol", Path: "store/api/views.py", Name: "DeleteView._purchase_transaction_ids"},
		types.FindingSubject{Kind: "config_key", Name: "PURCHASER_READ_SWAP"})
}

func laterLoopFinding() payload.Finding {
	return laterFinding("store/api/views.py", 288, laterLoopText, laterLoopHeadline, laterLoopImpact,
		types.FindingSubject{Kind: "symbol", Path: "store/api/views.py", Name: "DeleteView._delete_item"})
}

func TestSameEarlierPoint_TheAuditedPairMatchesOnSwitchAndText(t *testing.T) {
	s, ok := SameEarlierPoint(laterDeleteFinding(), earlierPurchaserPoint())
	if !ok || s.Subject != "purchaser_read_swap" || s.Text < ExternalThreshold {
		t.Fatalf("the delete finding raises the settled purchaser point again: %+v ok=%v", s, ok)
	}
}

func TestSameEarlierPoint_TheOtherFindingOfThatPRIsNotThePoint(t *testing.T) {
	if s, ok := SameEarlierPoint(laterLoopFinding(), earlierPurchaserPoint()); ok {
		t.Fatalf("the per-transaction loop shares no subject with the settled point: %+v", s)
	}
}

func TestSameEarlierPoint_ASharedSubjectAloneIsNotThePoint(t *testing.T) {
	f := laterFinding("store/api/views.py", 300, "`PURCHASER_READ_SWAP` is read once at import time, so flipping it at runtime takes effect only after a restart.",
		"Switch read at import time", "Flipping the switch at runtime changes nothing until the process restarts.",
		types.FindingSubject{Kind: "config_key", Name: "PURCHASER_READ_SWAP"})
	if s, ok := SameEarlierPoint(f, earlierPurchaserPoint()); ok {
		t.Fatalf("one shared key with unrelated text is another point: %+v", s)
	}
}

func TestSameEarlierPoint_SharedTextAloneIsNotThePoint(t *testing.T) {
	f := laterDeleteFinding()
	f.FindingContract.Subjects = []types.FindingSubject{{Kind: "file", Path: f.File}, {Kind: "symbol", Path: f.File, Name: "DeleteView._purchase_transaction_ids"}}
	if s, ok := SameEarlierPoint(f, earlierPurchaserPoint()); ok {
		t.Fatalf("matching text with no shared subject is not enough: %+v", s)
	}
}

// Greptile's withdrawn thread on the earlier PR names no contract subject;
// the author's answer names the switch, which is the finding's key.
func TestSameEarlierPoint_ASubjectTheThreadNamesCountsForAThreadWithoutAContract(t *testing.T) {
	root := "`record_fanclub_bulk` reads buyers only from `purchaser_id`, even when the purchaser read switch is off. If older purchases still have a null `purchaser_id`, this query misses those buyers. The existing purchase reader uses `user_id` until those rows are backfilled. A member who bought before joining the collection toggle cohort has no purchase state row. Publishing that set for fan club members then creates a `FAN_CLUB` unread row, so the bought set shows NEW on Fan club instead of Purchased. Use `user_id` while the read switch is off, or fall back to it when `purchaser_id` is null."
	answer := "Not an issue. The purchaser_id backfill is complete in prod (OPS-1821 found 0 nulls and 0 purchaser_id / user_id mismatches), and new rows get it on save from the mirror field. purchaser_read_swap is already on in prod, and the viewer's ownership check reads purchaser_id without the switch too."
	p := EarlierPoint{Subjects: CodeIdentifiers(root), Text: root + " " + answer}
	s, ok := SameEarlierPoint(laterDeleteFinding(), p)
	if !ok || s.Subject != "purchaser_read_swap" {
		t.Fatalf("the answer names the switch the finding is keyed on: %+v ok=%v", s, ok)
	}
	p.Text = root + " Not an issue, the backfill is complete and the switch is on."
	if s, ok := SameEarlierPoint(laterDeleteFinding(), p); ok {
		t.Fatalf("a thread that names neither subject is another point: %+v", s)
	}
}

func TestSameEarlierPoint_SubjectsCompareByLastSegmentToo(t *testing.T) {
	p := EarlierPoint{Subjects: []string{"_purchase_transaction_ids"}, Text: laterDeleteText + " " + laterDeleteImpact}
	if _, ok := SameEarlierPoint(laterDeleteFinding(), p); !ok {
		t.Fatal("a qualified symbol matches its bare name")
	}
}

func TestCodeIdentifiers_ReadsTheSpansAnotherReviewerNamed(t *testing.T) {
	root := "`record_fanclub_bulk` reads buyers only from `purchaser_id`, even when the purchaser read switch is off. The existing purchase reader uses `user_id` until those rows are backfilled. Use `user_id` while the read switch is off, or `Purchases._active()`; see `collection/models.py` and `a b`."
	got := CodeIdentifiers(root)
	want := "[purchaser_id purchases._active record_fanclub_bulk user_id]"
	if s := sprint(got); s != want {
		t.Fatalf("identifiers = %s, want %s", s, want)
	}
}

func sprint(ss []string) string {
	out := "["
	for i, s := range ss {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out + "]"
}
