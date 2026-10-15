package publisher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
	"pr-review-server/pkg/reviewer/types"
)

// The audited pair, anonymised. On pull request 596 the author called the
// purchaser-id fallback point intentional (the backfill is complete, the
// read switch is on); Greptile had raised and withdrawn it four days
// before. Pull request 668 of the same author, one day later, raised it
// again from another file as a new MEDIUM, and the author answered a
// fourth time. The second MEDIUM of that round is a distinct point.
const (
	earlierFile     = "store/api/views.py"
	earlierRootText = "The buyer lookup in `record_fanclub_bulk` hardcodes `purchaser_id`, but every other purchase read branches on `PURCHASER_READ_SWAP` and falls back to parent `user_id` (`Purchases._active`, `store/views.py` methods). The `purchaser_id` column can be null for rows written before it was added or before the backfill ran, so with the switch off the fanout lookup misses older buyers."
	earlierImpact   = "When the purchaser read swap switch is off and purchaser_id is not populated, fanout may write a FAN_CLUB unread row for a member who already bought the set."
	earlierReason   = "intentional. Same answer as the earlier threads on this. The purchaser_id backfill is done in prod (OPS-1821, zero nulls and zero mismatches verified) and purchaser_read_swap is on for both writes and the viewer ownership check. The switch-off state this describes doesn't exist anymore, and fanout writes already use purchaser_id, so reading it here matches what gets written."

	laterFile           = "store/api/views.py"
	laterDeleteText     = "`_purchase_transaction_ids` filters on `purchaser_id` unconditionally, but `purchaser_id` is a `MirrorBigIntegerField` that was added with `null=True` and is only populated by a separate backfill (`backfill_purchaser`). While the feature switch `PURCHASER_READ_SWAP` is off, the Purchased feed resolves purchases through `user_id`, so a legacy row is listed but the delete lookup cannot find it and returns 400."
	laterDeleteHeadline = "Delete filters on un-backfilled purchaser_id"
	laterDeleteImpact   = "A user whose purchase row has a NULL purchaser_id (pre-backfill) still sees the set in the Purchased feed but gets a 400 on delete and no removal is written."
	laterLoopText       = "`_delete_item` runs one `SELECT`+`UPDATE` (and possibly an `INSERT`) per transaction inside the atomic block, creating an N+1 pattern for a public endpoint."
	laterLoopHeadline   = "Per-transaction loop adds queries inside the write transaction"
	laterLoopImpact     = "Each purchase of the set adds a query inside the write transaction, holding the atomic block longer than necessary."

	greptileRootTitle = "Older buyers get wrong badges"
	greptileRootBody  = "`record_fanclub_bulk` reads buyers only from `purchaser_id`, even when the purchaser read switch is off. If older purchases still have a null `purchaser_id`, this query misses those buyers. The existing purchase reader uses `user_id` until those rows are backfilled.\n\nA member who bought before joining the collection toggle cohort has no purchase state row. Publishing that set for fan club members then creates a `FAN_CLUB` unread row, so the bought set shows NEW on Fan club instead of Purchased. Use `user_id` while the read switch is off, or fall back to it when `purchaser_id` is null."
	// Greptile's root on pull request 668 itself, withdrawn 34 minutes
	// before PRism posted the same point there.
	samePRRootTitle  = "Older purchases cannot be deleted"
	samePRRootBody   = "If older purchases have not been backfilled and `PURCHASER_READ_SWAP` is off, the feed shows them through `user_id`, but this lookup cannot find them because `purchaser_id` is null. Deleting them returns 400 and leaves them visible.\n\nFollow the feed's switch-controlled buyer lookup while keeping the read on `tipping`. Add a test for an unbackfilled purchase."
	samePRAnswer     = "The backfill is done in prod. OPS-1821 measured 0 null purchaser_id rows and 0 mismatches against user_id, and purchaser_read_swap is on. With the switch on the feed reads purchaser_id too, so the feed and this lookup always agree. The user_id fallback in the feed is the legacy lever for the switch, not a data state we can hit."
	samePRWithdraw   = "That production data and switch state address the concern. With the read swap enabled, the feed and the delete view both resolve purchases through `purchaser_id`, and OPS-1821 confirms there are no null or mismatched rows to create a divergence. I'm withdrawing the finding."
	greptileWithdraw = "That addresses the concern. With the `purchaser_id` backfill verified complete, no remaining nulls or mismatches, and the read swap enabled for both new writes and the viewer ownership check, using `purchaser_id` here is correct, so I'm withdrawing the finding."
)

var (
	settledAt   = time.Date(2026, 10, 14, 15, 3, 52, 0, time.UTC)
	laterRound  = time.Date(2026, 10, 14, 21, 58, 51, 0, time.UTC)
	earlierRoot = int64(4001)
)

func subjectFinding(file string, line int, text, headline, impact string, subjects ...types.FindingSubject) payload.Finding {
	x := payload.Finding{ID: payload.Fingerprint(file, line, text), Severity: "medium", File: file, Line: line, Comment: text, State: "confirmed", Active: true,
		FindingContractStatus: "valid"}
	c := falsifiableTestContract("production_behavior", "current_impact", impact, "")
	c.Headline = headline
	c.Subjects = append([]types.FindingSubject{{Kind: "file", Path: file}}, subjects...)
	x.FindingContract = c
	return x
}

func laterDeleteFinding() payload.Finding {
	return subjectFinding(laterFile, 280, laterDeleteText, laterDeleteHeadline, laterDeleteImpact,
		types.FindingSubject{Kind: "symbol", Path: laterFile, Name: "DeleteView._purchase_transaction_ids"},
		types.FindingSubject{Kind: "config_key", Name: "PURCHASER_READ_SWAP"})
}

func laterLoopFinding() payload.Finding {
	return subjectFinding(laterFile, 288, laterLoopText, laterLoopHeadline, laterLoopImpact,
		types.FindingSubject{Kind: "symbol", Path: laterFile, Name: "DeleteView._delete_item"})
}

// earlierVerdictPoint is what the reply path records when the author's
// verdict settles the PRism thread on pull request 596.
func earlierVerdictPoint() db.SettledPoint {
	row := db.PublishedFinding{RepoOwner: "acme", RepoName: "example", PRNumber: 596, Kind: db.PublishedKindFinding,
		Fingerprint: payload.Fingerprint(earlierFile, 241, earlierRootText), CommentID: earlierRoot, State: db.PublishedStateDismissed,
		CommentText: earlierRootText + " " + earlierImpact, FindingKind: "production_behavior", Subjects: "purchaser_read_swap,record_fanclub_bulk"}
	return SettledPointFromRow(row, "dev-ana", db.SettledSourceVerdict, earlierReason, settledAt)
}

// fakeSettledLedger is a fakeLedger that also remembers settled points.
type fakeSettledLedger struct {
	*fakeLedger
	points   []db.SettledPoint
	recorded []db.SettledPoint
	listErr  error
}

func newFakeSettledLedger(points ...db.SettledPoint) *fakeSettledLedger {
	return &fakeSettledLedger{fakeLedger: newFakeLedger(), points: points}
}

func (l *fakeSettledLedger) RecordSettledPoint(p *db.SettledPoint) error {
	for i, have := range l.recorded {
		if have.PRNumber == p.PRNumber && have.CommentID == p.CommentID && have.Fingerprint == p.Fingerprint {
			l.recorded[i] = *p
			for j := range l.points {
				if l.points[j].PRNumber == p.PRNumber && l.points[j].CommentID == p.CommentID && l.points[j].Fingerprint == p.Fingerprint {
					l.points[j] = *p
				}
			}
			return nil
		}
	}
	l.recorded = append(l.recorded, *p)
	l.points = append(l.points, *p)
	return nil
}

func (l *fakeSettledLedger) ListSettledPoints(_, _, author string, since, until time.Time) ([]db.SettledPoint, error) {
	if l.listErr != nil {
		return nil, l.listErr
	}
	var out []db.SettledPoint
	for _, p := range l.points {
		if strings.EqualFold(p.Author, author) && p.SettledAt.After(since) && !p.SettledAt.After(until) {
			out = append(out, p)
		}
	}
	return out, nil
}

func laterPRRound() Round {
	return Round{
		Owner: "acme", Repo: "example", Number: 668, HeadSHA: "fea2a25", Author: "dev-ana",
		Findings:    []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative."), laterDeleteFinding(), laterLoopFinding()},
		Commentable: map[string]map[int]bool{laterFile: {280: true, 288: true}},
	}
}

func publishLater(t *testing.T, gh *fakeThreadGitHub, ledger *fakeSettledLedger, r Round, pol Policy) Report {
	t.Helper()
	p := &Publisher{GH: gh, Ledger: ledger, Policy: pol, Now: func() time.Time { return laterRound }}
	rep, err := p.Publish(context.Background(), r)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return rep
}

func TestCrossPR_PointTheAuthorSettledOnAnEarlierPRIsNotRaisedAgain(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
	rep := publishLater(t, gh, ledger, laterPRRound(), testPolicy())
	if rep.InlinePosted != 1 || rep.CrossPRSettled != 1 || len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 1 {
		t.Fatalf("only the per-transaction loop is posted: %+v reviews=%+v", rep, gh.reviews)
	}
	if posted := gh.reviews[0].comments[0]; posted.Line != 288 || strings.Contains(posted.Body, "un-backfilled") {
		t.Fatalf("the settled point got no root: %+v", posted)
	}
	row := ledger.get(db.PublishedKindAnnotation, laterDeleteFinding().ID)
	if row == nil || row.State != db.PublishedStateSettled || row.SettledPRNumber != 596 || row.SettledCommentID != earlierRoot || row.CommentID != 0 {
		t.Fatalf("a settled row names the earlier thread and owns no comment: %+v", row)
	}
	summary := gh.issueCreates[0]
	want := fmt.Sprintf("Settled earlier on [#596](%s)", SettledThreadURL("acme", "example", 596, earlierRoot))
	if !strings.Contains(summary, "1 point settled earlier by the author") || !strings.Contains(summary, want) || !strings.Contains(summary, "un-backfilled") {
		t.Fatalf("the summary folds the point under the earlier thread:\n%s", summary)
	}
	if strings.Count(summary, "un-backfilled") != 1 {
		t.Fatalf("the point is listed once, never as a bullet too:\n%s", summary)
	}
	if rep.Confidence != 4 {
		t.Fatalf("the settled point does not weigh on the score: confidence %d", rep.Confidence)
	}
}

func TestCrossPR_TheSettledRowStaysSilentOnLaterRounds(t *testing.T) {
	gh, ledger := newFakeSettledLedger(earlierVerdictPoint()), newFakeThreadGitHub()
	publishLater(t, ledger, gh, laterPRRound(), testPolicy())
	r2 := laterPRRound()
	r2.HeadSHA = "0b0b0b0"
	rep := publishLater(t, ledger, gh, r2, testPolicy())
	if rep.InlinePosted != 0 || rep.CrossPRSettled != 0 || rep.StillOpen != 1 || len(ledger.reviews) != 1 {
		t.Fatalf("the second round posts nothing new and counts the loop as still open: %+v", rep)
	}
	row := gh.get(db.PublishedKindAnnotation, laterDeleteFinding().ID)
	if row == nil || row.State != db.PublishedStateSettled || row.SettledPRNumber != 596 {
		t.Fatalf("the settled row is untouched: %+v", row)
	}
	summary := ledger.issueEdits[501]
	if !strings.Contains(summary, "Settled earlier on [#596]") || !strings.Contains(summary, "0 new · 1 still open · 0 fixed") {
		t.Fatalf("the refreshed summary still names the earlier thread and never counts the point:\n%s", summary)
	}
}

func TestCrossPR_OffPostsThePointAgain(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
	pol := testPolicy()
	pol.CrossPRSettled = false
	rep := publishLater(t, gh, ledger, laterPRRound(), pol)
	if rep.InlinePosted != 2 || rep.CrossPRSettled != 0 || ledger.get(db.PublishedKindAnnotation, laterDeleteFinding().ID) != nil {
		t.Fatalf("the kill switch restores the repeat: %+v", rep)
	}
}

func TestCrossPR_OutsideTheWindowOrAnotherAuthorPostsAgain(t *testing.T) {
	stale := earlierVerdictPoint()
	stale.SettledAt = laterRound.Add(-SettledWindow - time.Hour)
	other := earlierVerdictPoint()
	other.Author = "dev-bo"
	for name, point := range map[string]db.SettledPoint{"thirty-one days old": stale, "another author": other} {
		gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(point)
		if rep := publishLater(t, gh, ledger, laterPRRound(), testPolicy()); rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
			t.Fatalf("%s: the point is not this author's recent settlement: %+v", name, rep)
		}
	}
}

func TestCrossPR_ThePointSettledOnThisPRIsTheLedgersBusiness(t *testing.T) {
	own := earlierVerdictPoint()
	own.PRNumber = 668
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(own)
	if rep := publishLater(t, gh, ledger, laterPRRound(), testPolicy()); rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
		t.Fatalf("a point of this PR is matched by the ledger tiers, not here: %+v", rep)
	}
}

func TestCrossPR_ACriticalFindingIsExempt(t *testing.T) {
	for _, kind := range []string{"security_risk", "production_behavior"} {
		r := laterPRRound()
		r.Findings[1].Severity = "critical"
		r.Findings[1].FindingContract.FindingKind = kind
		gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
		if rep := publishLater(t, gh, ledger, r, testPolicy()); rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
			t.Fatalf("a critical %s is raised again whatever the author settled: %+v", kind, rep)
		}
	}
}

func TestCrossPR_APointSettledInAnotherFileIsAnotherPoint(t *testing.T) {
	point := earlierVerdictPoint()
	point.File = "store/models.py"
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(point)
	if rep := publishLater(t, gh, ledger, laterPRRound(), testPolicy()); rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
		t.Fatalf("the same symbol and prose in another file is raised again: %+v", rep)
	}
}

func TestCrossPR_ABareShortSubjectCarriesNoMatch(t *testing.T) {
	for _, c := range []struct {
		subject, file string
		generic       bool
	}{
		{"start", "a/b.ts", true}, {"output", "x/models.py", true}, {"creators", "collection/api/creators.py", true}, {"", "x.py", true},
		{"v2", "a.go", true}, {"s3", "a.go", true}, {"md5", "a.go", true}, {"utf8", "a.go", true}, {"sha256", "a.go", true},
		{"user_id", "a.py", true}, {"fan_club", "a.py", true}, {"created_at", "a.py", false}, {"purchaser_read_swap", "a.py", false},
		{"companion_asset", "forms.py", false}, {"showtipmenu", "x.tsx", false}, {"DeleteView._purchase_ids", "views.py", false},
	} {
		if got := genericSubject(c.subject, c.file); got != c.generic {
			t.Errorf("genericSubject(%q, %q) = %v", c.subject, c.file, got)
		}
	}
	bare := subjectFinding(laterFile, 280, laterDeleteText, laterDeleteHeadline, laterDeleteImpact, types.FindingSubject{Kind: "symbol", Path: laterFile, Name: "swap"})
	point := earlierVerdictPoint()
	point.Subjects = "swap"
	if _, score, ok := ScoreSettledElsewhere(bare, []db.SettledPoint{point}); ok {
		t.Fatalf("a point whose only shared subject is a bare word is raised again: %+v", score)
	}
	if _, score, ok := ScoreSettledElsewhere(laterDeleteFinding(), []db.SettledPoint{earlierVerdictPoint()}); !ok || score.Subject != "purchaser_read_swap" {
		t.Fatalf("a qualified subject carries the match: ok=%v %+v", ok, score)
	}
}

func TestCrossPR_ALaterDeferralSupersedesTheStand(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001}}
	publishLater(t, gh, ledger, earlierPRRound(greptileThread(true)), testPolicy())
	if len(ledger.recorded) != 1 || ledger.recorded[0].Source != db.SettledSourceWithdrawn {
		t.Fatalf("recorded = %+v", ledger.recorded)
	}
	comments := greptileThread(true)
	comments = append(comments, reconcile.ExternalComment{ID: 3004, Author: "dev-ana", Path: earlierFile, Line: 241, InReplyToID: 3001,
		Body: "Actually, we'll rework this in a follow-up PR.", CreatedAt: comments[2].CreatedAt.Add(time.Hour)})
	again := earlierPRRound(comments)
	again.HeadSHA = "bbe8764"
	publishLater(t, gh, ledger, again, testPolicy())
	if len(ledger.recorded) != 1 || ledger.recorded[0].Source != db.SettledSourceDeferred {
		t.Fatalf("the deferral replaces the stand: %+v", ledger.recorded)
	}
	later, laterLedger := newFakeThreadGitHub(), newFakeSettledLedger(ledger.points...)
	if rep := publishLater(t, later, laterLedger, laterPRRound(), testPolicy()); rep.CrossPRSettled != 0 || rep.InlinePosted != 2 {
		t.Fatalf("a deferred point settles nothing: %+v", rep)
	}
}

func TestCrossPR_ADeferralSettlesNothing(t *testing.T) {
	for _, body := range []string{
		"Fair. I'm actually gonna pull this line until the next PRs, as the next PRs should do the work of fixing this.",
		"Intentionally left to expire. This will be cleaned up in a followup PR.",
		"Featured logos as a feature will be dark until the two followup PRs.",
		"Not an issue for now, the backfill lands in a separate PR.",
		"made a jira ticket to handle this in a followup",
	} {
		if !Deferred(body) {
			t.Errorf("%q defers the point", body)
		}
		gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
		gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001, Resolved: true}}
		comments := greptileThread(true)
		comments[1].Body = body
		publishLater(t, gh, ledger, earlierPRRound(comments), testPolicy())
		if len(ledger.recorded) != 1 || ledger.recorded[0].Source != db.SettledSourceDeferred {
			t.Errorf("a deferral the reviewer withdrew or resolved is recorded as deferred, not settled: %q -> %+v", body, ledger.recorded)
		}
		later, laterLedger := newFakeThreadGitHub(), newFakeSettledLedger(ledger.points...)
		if rep := publishLater(t, later, laterLedger, laterPRRound(), testPolicy()); rep.CrossPRSettled != 0 || rep.InlinePosted != 2 {
			t.Errorf("a deferred point settles nothing: %q -> %+v", body, rep)
		}
	}
	for _, body := range []string{
		"This would be correct behavior; we prompt them until they decide to end the show early.",
		"Not an issue. The purchaser_id backfill is complete in prod.",
		"Intentional, the two flags are read by different workers.",
		"Known drift, on purpose. The backend settled on purchased.",
	} {
		if Deferred(body) {
			t.Errorf("%q stands by the code", body)
		}
	}
}

func TestCrossPR_AFoldedNoteIsSettledToo(t *testing.T) {
	r := laterPRRound()
	r.Findings[1].Severity = "low"
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
	rep := publishLater(t, gh, ledger, r, testPolicy())
	if rep.CrossPRSettled != 1 || rep.InlinePosted != 1 {
		t.Fatalf("%+v", rep)
	}
	if summary := gh.issueCreates[0]; strings.Contains(summary, "folded note") || !strings.Contains(summary, "Settled earlier on [#596]") {
		t.Fatalf("the note is named under the earlier thread, not folded:\n%s", summary)
	}
}

func TestCrossPR_AStoreErrorSkipsThePassAndPosts(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
	ledger.listErr = fmt.Errorf("db down")
	if rep := publishLater(t, gh, ledger, laterPRRound(), testPolicy()); rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
		t.Fatalf("a store failure never fails or silences the round: %+v", rep)
	}
}

func TestCrossPR_WithoutAStoreNothingChanges(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeLedger()
	rep := publishThreads(t, gh, ledger, laterPRRound())
	if rep.InlinePosted != 2 || rep.CrossPRSettled != 0 {
		t.Fatalf("%+v", rep)
	}
}

// greptileThread is Greptile's root on pull request 596, answered by the
// author and withdrawn a minute later, as the round lists it.
func greptileThread(withdrawn bool) []reconcile.ExternalComment {
	root := reconcile.ExternalComment{ID: 3001, Author: "greptile-apps[bot]", Path: earlierFile, Line: 241,
		Body: `<img alt="P1" src="https://example.invalid/p1.svg"> **` + greptileRootTitle + "**\n\n" + greptileRootBody, CreatedAt: settledAt.Add(-4 * 24 * time.Hour)}
	answer := reconcile.ExternalComment{ID: 3002, Author: "dev-ana", Path: earlierFile, Line: 241, InReplyToID: 3001, Body: "Not an issue. The purchaser_id backfill is complete in prod (OPS-1821 found 0 nulls and 0 purchaser_id / user_id mismatches), and new rows get it on save from the mirror field. purchaser_read_swap is already on in prod, and the viewer's ownership check reads purchaser_id without the switch too.", CreatedAt: root.CreatedAt.Add(time.Minute)}
	out := []reconcile.ExternalComment{root, answer}
	if withdrawn {
		out = append(out, reconcile.ExternalComment{ID: 3003, Author: "greptile-apps[bot]", Path: earlierFile, Line: 241, InReplyToID: 3001, Body: greptileWithdraw, CreatedAt: answer.CreatedAt.Add(30 * time.Second)})
	}
	return out
}

func earlierPRRound(comments []reconcile.ExternalComment) Round {
	roots := reconcile.ParseExternalComments(comments, nil)
	return Round{
		Owner: "acme", Repo: "example", Number: 596, HeadSHA: "8c858a5", Author: "dev-ana",
		Findings:        []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "Narrative.")},
		External:        roots,
		ExternalThreads: reconcile.BuildExternalThreads(comments, roots, "dev-ana"),
	}
}

func TestCrossPR_AWithdrawnThreadTheAuthorAnsweredIsRemembered(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001}}
	publishLater(t, gh, ledger, earlierPRRound(greptileThread(true)), testPolicy())
	if len(ledger.recorded) != 1 {
		t.Fatalf("recorded = %+v", ledger.recorded)
	}
	p := ledger.recorded[0]
	if p.Source != db.SettledSourceWithdrawn || p.CommentID != 3001 || p.PRNumber != 596 || p.Author != "dev-ana" || !p.SettledAt.Equal(settledAt.Add(-4*24*time.Hour).Add(90*time.Second)) {
		t.Fatalf("point = %+v", p)
	}
	if p.Subjects != "fan_club,purchaser_id,record_fanclub_bulk,user_id" || p.Headline != greptileRootTitle || !strings.Contains(p.Text, "backfill is complete") || !strings.Contains(p.Reason, "Not an issue") {
		t.Fatalf("point = %+v", p)
	}
	again := earlierPRRound(greptileThread(true))
	again.HeadSHA = "bbe8764"
	publishLater(t, gh, ledger, again, testPolicy())
	if len(ledger.recorded) != 1 {
		t.Fatalf("a thread is recorded once: %+v", ledger.recorded)
	}

	// The next PR of the author raises the point PRism itself never posted.
	later, laterLedger := newFakeThreadGitHub(), newFakeSettledLedger(ledger.points...)
	rep := publishLater(t, later, laterLedger, laterPRRound(), testPolicy())
	if rep.CrossPRSettled != 1 || rep.InlinePosted != 1 {
		t.Fatalf("the point is keyed on what was settled, not on who raised it: %+v", rep)
	}
	if summary := later.issueCreates[0]; !strings.Contains(summary, fmt.Sprintf("Settled earlier on [#596](%s)", SettledThreadURL("acme", "example", 596, 3001))) {
		t.Fatalf("the summary links Greptile's thread:\n%s", summary)
	}
}

func TestCrossPR_AResolvedThreadTheAuthorAnsweredIsRemembered(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001, Resolved: true}}
	publishLater(t, gh, ledger, earlierPRRound(greptileThread(false)), testPolicy())
	if len(ledger.recorded) != 1 || ledger.recorded[0].Source != db.SettledSourceResolved {
		t.Fatalf("recorded = %+v", ledger.recorded)
	}
}

func TestCrossPR_AThreadTheAuthorFixedAndResolvedIsNotSettled(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001, Resolved: true}}
	comments := greptileThread(false)
	comments[1].Body = "Fixed in a82796ac, the reader now falls back to user_id."
	publishLater(t, gh, ledger, earlierPRRound(comments), testPolicy())
	if len(ledger.recorded) != 0 {
		t.Fatalf("a fix the author resolved settles nothing: %+v", ledger.recorded)
	}
}

func TestThreadSettled_ResolvedThreadsCountOnlyWhenTheAuthorStoodByTheCode(t *testing.T) {
	cases := []struct {
		reply               string
		resolved, withdrawn bool
	}{
		{"Fixed in a82796ac", false, false},
		{"Fixed, the desktop block now overrides the mobile one.", false, false},
		{"Removed", false, false},
		{"I fixed this in 3adf5849 along with the loop.", false, false},
		{"Added store/testdb_integration_test.go: against a scratch database it covers the cleanup path.", false, false},
		{"ok", false, false},
		{"will fix", false, false},
		{"Is this still reachable after the guard?", false, false},
		{"I will handle this in a follow-up PR, but the items in the Content Vault should match.", false, false},
		{"I made it so that if the items already exist in the store, to skip the fetch.", false, false},
		{"Updated, the reader now falls back to user_id.", false, false},
		{"intentional", true, true},
		{"Not an issue. The purchaser_id backfill is complete in prod.", true, true},
		// A stand in prose settles a thread the reviewer withdrew; under a
		// resolve that anyone may have clicked it does not.
		{"this just adds bloat imo", false, true},
		{"the new page is behind MyCollUpgrd, so the old gate is never hit.", false, true},
		{"The feed's own queries don't grow. Every query grabs stuff for the whole page at once.", false, true},
	}
	for _, c := range cases {
		th := reconcile.ExternalThread{AuthorReplies: []string{c.reply}}
		if got := ThreadSettled(th, true); got != c.resolved {
			t.Errorf("resolved thread answered %q: settled=%v, want %v", c.reply, got, c.resolved)
		}
		if ThreadSettled(th, false) {
			t.Errorf("an open thread answered %q is not settled", c.reply)
		}
		th.Withdrawn = true
		if got := ThreadSettled(th, false); got != c.withdrawn {
			t.Errorf("withdrawn thread answered %q: settled=%v, want %v", c.reply, got, c.withdrawn)
		}
	}
	if ThreadSettled(reconcile.ExternalThread{Withdrawn: true}, true) {
		t.Error("a thread nobody answered is not settled")
	}
	for _, body := range []string{"I withdrew the finding.", "Withdrawing this one.", "No change is needed here.", "Finding withdrawn.", "I'll withdraw this.", greptileWithdraw, samePRWithdraw} {
		if !reconcile.IsWithdrawal(body) {
			t.Errorf("%q is a withdrawal", body)
		}
	}
	for _, body := range []string{"I would not withdraw the migration yet.", "This shouldn't be withdrawn.", "This doesn't need to be withdrawn, the race is real.", "there's no need to withdraw it", "the fix is not necessary here", "this is not necessary to merge, but the null branch should still be fixed", "nothing to change in the caller, the callee is wrong", "The withdrawal flow still double-charges."} {
		if reconcile.IsWithdrawal(body) {
			t.Errorf("%q is not a withdrawal", body)
		}
	}
}

func TestCrossPR_AThreadNobodyAnsweredOrStillOpenIsNotSettled(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3001", RootCommentID: 3001}}
	publishLater(t, gh, ledger, earlierPRRound(greptileThread(false)), testPolicy())
	unanswered := earlierPRRound(greptileThread(true)[:1])
	unanswered.HeadSHA = "bbe8764"
	publishLater(t, gh, ledger, unanswered, testPolicy())
	if len(ledger.recorded) != 0 {
		t.Fatalf("an open answered thread and an unanswered withdrawal settle nothing: %+v", ledger.recorded)
	}
}

func TestCrossPR_RefreshKeepsTheSettledPointOutOfTheSummary(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger(earlierVerdictPoint())
	publishLater(t, gh, ledger, laterPRRound(), testPolicy())
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}
	r := laterPRRound()
	r.RoundNumber = 1
	p := &Publisher{GH: gh, Ledger: ledger, Policy: testPolicy(), Now: func() time.Time { return laterRound }}
	if err := p.RefreshSummary(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if body, ok := gh.issueEdits[501]; ok && (strings.Count(body, "un-backfilled") != 1 || !strings.Contains(body, "Settled earlier on [#596]")) {
		t.Fatalf("refresh:\n%s", body)
	}
}

// samePRThread is Greptile's thread on pull request 668 as the round lists
// it: posted, answered by the author and withdrawn before PRism's round.
func samePRThread() []reconcile.ExternalComment {
	root := reconcile.ExternalComment{ID: 3101, Author: "greptile-apps[bot]", Path: laterFile, Line: 278,
		Body: `<img alt="P1" src="https://example.invalid/p1.svg"> **` + samePRRootTitle + "**\n\n" + samePRRootBody, CreatedAt: laterRound.Add(-38 * time.Minute)}
	answer := reconcile.ExternalComment{ID: 3102, Author: "dev-ana", Path: laterFile, Line: 278, InReplyToID: 3101, Body: samePRAnswer, CreatedAt: root.CreatedAt.Add(4 * time.Minute)}
	withdraw := reconcile.ExternalComment{ID: 3103, Author: "greptile-apps[bot]", Path: laterFile, Line: 278, InReplyToID: 3101, Body: samePRWithdraw, CreatedAt: answer.CreatedAt.Add(23 * time.Second)}
	return []reconcile.ExternalComment{root, answer, withdraw}
}

func TestCrossPR_AThreadWithdrawnOnThisPRSettlesAShownFindingInTheSameRound(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3101", RootCommentID: 3101, Resolved: true}}
	r := laterPRRound()
	comments := samePRThread()
	r.External = reconcile.ParseExternalComments(comments, nil)
	r.ExternalThreads = reconcile.BuildExternalThreads(comments, r.External, "dev-ana")
	rep := publishLater(t, gh, ledger, r, testPolicy())
	if rep.CrossPRSettled != 1 || rep.InlinePosted != 1 || rep.ExternalMatched != 0 || len(ledger.recorded) != 1 {
		t.Fatalf("the withdrawn thread settles the delete finding before it posts: %+v recorded=%d", rep, len(ledger.recorded))
	}
	row := ledger.get(db.PublishedKindAnnotation, laterDeleteFinding().ID)
	if row == nil || row.State != db.PublishedStateSettled || row.SettledPRNumber != 668 || row.SettledCommentID != 3101 {
		t.Fatalf("row = %+v", row)
	}
	if summary := gh.issueCreates[0]; !strings.Contains(summary, fmt.Sprintf("Settled on [#668](%s)", SettledThreadURL("acme", "example", 668, 3101))) {
		t.Fatalf("the summary links the withdrawn thread:\n%s", summary)
	}
}

func TestCrossPR_AThreadWithdrawnOnThisPRLeavesNotesToTheNoteReconcile(t *testing.T) {
	gh, ledger := newFakeThreadGitHub(), newFakeSettledLedger()
	gh.extra = []ReviewThread{{NodeID: "E3101", RootCommentID: 3101, Resolved: true}}
	r := laterPRRound()
	r.Findings[1].Severity = "low"
	comments := samePRThread()
	r.External = reconcile.ParseExternalComments(comments, nil)
	r.ExternalThreads = reconcile.BuildExternalThreads(comments, r.External, "dev-ana")
	rep := publishLater(t, gh, ledger, r, testPolicy())
	if rep.CrossPRSettled != 0 || rep.ExternalSettled != 1 {
		t.Fatalf("a note against the resolved thread is an external row, as before: %+v", rep)
	}
}
