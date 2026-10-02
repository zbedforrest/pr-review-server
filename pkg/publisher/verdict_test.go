package publisher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pr-review-server/db"
)

// auditedVerdictReplies mirror the 71 author replies the comment audit
// labelled intentional_behavior, paraphrased and anonymised (fake tickets,
// shas and products, bodies cut to the clauses the classifier reads). want
// says whether the fast path settles the reply; the rest still reach the
// model, a bare acknowledgement or the fix-claim path, exactly as before.
var auditedVerdictReplies = []struct {
	body string
	want bool
}{
	{"We already send these to the vendor from other call sites. Good catch, and I have not yet given the bot access to the other repo where that happens.", false},
	{"This is acceptable. A non-db error rarely fails this, and serving a stale value for a few seconds is not a concern.", true},
	{"That route is being retired. Marking this resolved.", false},
	{"That is what the 180 second ttl is for. The stale value is not served forever.", false},
	{"This is intended behavior. A switch nobody registered always reads false and cannot be edited, because it does nothing and should be removed or renamed.", true},
	{"This is intended behavior.", true},
	{"intended", true},
	{"This is deliberate behavior. It does not actually disable the entry; as you said, default-locale users still receive it.", true},
	{"Correct that the roles are now cluster-wide, and that is accepted for the quarter. This PR is the first rung of the ladder, one change per step.", false},
	{"Accepted, and it is why this is a dedicated ingest key and not the bundle's license key. An ingest key can only write telemetry to one account.", true},
	{"By design; header corrected in 1a2b3c4. The catch-all 404s fade out over a few minutes, so a late 2xx is the stable signal, and a strict end-of-window check would fail releases on one stray 404.", true},
	{"By design. The exit codes are fixed at three values, so there is no fourth. A longer auth outage fails the step, which is the right signal.", true},
	{"By design for this PR. The delete action is copied byte for byte from the release action; ACME-1748 keeps the copies identical so a fix lands in both. The deadline-vs-retry gap is real and pre-existing", true},
	{"By design for this PR. The seed action is the release action less unused code; the replay on a 5xx is inherited unchanged. Only the delete call goes through it here, where a duplicate is a no-op.", true},
	{"By design for this quarter, same as the other bot's thread: the shared executor account is the recorded gap until ACME-1784 gives QA its own account. Verified its roles: object viewer and creator on the bucket, nothing project-level.", true},
	{"This is intentional and in the description. Every other http call already uses the shared verification helper; the legacy path was the only verify=False left.", true},
	{"This is intended, we require the user to hold enough credit for one minute even with no minimum set", true},
	{"intentional change is intentional, muting once the session is over would be weird", true},
	{"This would be correct behavior; if they join through an invalid entrypoint we cannot reconnect, so we prompt them until they decide to end early.", true},
	{"Yep, known gap for this PR. Scoped in ACME-3282, which adds the cursor and a fill loop.", false},
	{"This is intended: the ticket's rules say leaving the room clears both, loading it clears a lingering toast, and the banner stays out of that on purpose. Not a bug, keeping as is.", true},
	{"Intentional. The gate is as narrow as we can make it. Every bell journey on this page today is a follow nudge, and the online bell is a test fixture. The per-entry marker you describe is the real fix and belongs upstream", true},
	{"intentional phased state, following PRs add the other modes", true},
	{"Duplicates cannot be created by the current purchase paths. Both paths write the sale row in the same atomic block, and the table is unique on the pair, so I removed the key dedupe on purpose.", false},
	{"This is intentional. Opting in adds the paid check without cancelling the existing one. The new test covers this case.", true},
	{"The shared runners are ephemeral: each job gets a fresh pod, so nothing written to the home dir survives. This composite is a port of the staging action; the line is carried over unchanged", false},
	{"that's fine", true},
	{"I will handle this in a follow-up PR. The expected behavior is that rejected tiles show but cannot be selected.", false},
	{"Discussed previously, but yes, we want rejected items to appear in the modal. How the modal handles them changes in a separate PR.", false},
	{"This is intended. On reject, a tooltip is shown on hover or tap depending on device. Otherwise we fall through to open the viewer.", true},
	{"This was intentional, it felt like a pill with a tooltip should not also open the viewer", true},
	{"This is intentional, there is a chat thread agreeing that covering the title and description should be fine.", true},
	{"Will follow up in https://tracker.example/browse/ACME-11995", false},
	{"Tiles on the profile already handle a missing preview with a placeholder. The fallback is tracked in [ACME-3408](https://tracker.example/browse/ACME-3408) and must merge before the flag is enabled.", false},
	{"https://tracker.example/browse/ACME-2513 will handle the backfill so nobody ends up with an empty id-type field.", false},
	{"https://tracker.example/browse/ACME-2513 will handle the backfill so nobody ends up with an empty id-type field.", false},
	{"This was done on purpose so switching filters triggers a fresh fetch, which matches the previous behavior. It covers the case where the cache is stale", true},
	{"It was not originally intended as such but, as you mentioned, it seemed an improvement so I decided not to gate it.", false},
	{"That is the goal/intent. These are new rows and leave existing users alone. Access settings cannot change once created so we cannot go from undefined to something.", true},
	{"That is the goal. I updated the PR description.", true},
	{"Current legacy behavior wipes a failed message that carries media. This PR does not change that.", false},
	{"This is intentional and fixes your earlier finding about misleading partial counts. The total counts sets, not items. We chose to hide incomplete counts until all pages load.", true},
	{"This is an edge case we actually want. Current users of the endpoint change the host so click-throughs go to their own domain, and we do not want to break that workaround.", false},
	{"This is a new feature behind a flag, so nobody uses the parameter yet and we can change it without worrying about consumers.", false},
	{"Not changing this here. It is the same 16px close button the other notification uses, and a bigger hit area should change both together.", true},
	{"yes", false},
	{"yes", false},
	{"Intentional, that's what the product owner asked.", true},
	{"Known and accepted per the ticket. Only club sets are room-gated, so this only affects a club-only host whose room is blocked for the viewer.", true},
	{"Paid sets, including direct ones, are not live yet. ACME-3307 makes them viewable, and this list already includes their creators so it will not need a change then.", false},
	{"Intentional, it matches every other surface (the same check flag), and the new test covers it.", true},
	{"This toggle should cover tier 1 as well", false},
	{"this feature now depends on the translation toggle too", false},
	{"yes rendering an h2 instead is fine in that case", false},
	{"the table names are required, no fallback should be provided", false},
	{"Won't fix: matches mobile.", true},
	{"Intended. The viewer's publish negotiates mid-call and should still ramp from the start cap. A call no longer clears that cap (1b2c3d4), so it is ramped like any other session.", true},
	{"Intended. A call keeps the current state. Held means the browser reports no estimate, so nothing confirms that uncapping is safe, and doing it mid-session risks the stack switching the layer off while live.", true},
	{"Not changed. A null cell with no note only occurs when the same row's level cell is gated, and that cell already carries the reason as a footnote. Every other null cell now has its own footnote.", true},
	{"Intentional. Personal projects are not part of standard setup, so this page drops them. A team that still needs them can document it on its own page.", true},
	{"Deliberate: issued-first is the fix for the gap this ticket closes, a document that cannot prove when it was issued must not displace one that can, and two regression tests pin that intent.", true},
	{"Deliberate, and parity with legacy: the removed-payee gate freezes the whole person, and the sibling helper skipped identically. Narrowing the freeze is a policy change for its own ticket.", true},
	{"Deliberate: vouching accepts any unexpired document, aged-out included, matching the sibling convention. Tightening both touches every name-check flow, a policy decision for a follow-up if wanted.", true},
	{"By design, matching the live glue: a 401 on the token mint is retried rather than treated as final. The template makes the same call. On the last attempt the row is still recorded without relations, so it is delayed, never lost.", true},
	{"Not changed here. The inline template logs the same line, and this PR ports it exactly; the same inputs already show on the run. Trimming it needs the template changed too, noted as a follow-up on ACME-1750.", true},
	{"Plans are per repository (one repo holds the map), so one owner/repo is right for every number in the cell; linking only the confirming PR would drop the other links when the cell is rewritten. Left as is", false},
	{"Intentional correction.", true},
	{"Decision taken and recorded in 3a4b5c6. The two pre-merge checks were run: the production cache requires a password: NO (the chart owner confirmed; the option is supported but not enabled)", false},
	{"The maintainer then ruled (decision 23) that the flip ships without the network policy, since a per-deployment policy is what the platform team asked to weigh fleet-wide first, and accepted the residual", false},
	{"Closed by the lead on the maintainer's ruling, not as a disagreement with the finding.", false},
	{"Closed by the lead on the maintainer's ruling, not as a disagreement with the finding.", false},
}

func TestIsVerdictOnTheAuditedIntentionalReplies(t *testing.T) {
	settled := 0
	for i, c := range auditedVerdictReplies {
		got := IsVerdict(c.body)
		if got != c.want {
			t.Errorf("#%d IsVerdict(%q) = %t, want %t", i, c.body, got, c.want)
		}
		if got {
			settled++
		}
	}
	if settled != 42 {
		t.Errorf("fast path settles %d of %d audited verdict replies, the table expects 42", settled, len(auditedVerdictReplies))
	}
}

// Verdicts the audit filed under other classes (pushback, frustration, ack)
// and the replies that must never read as one: every audited fix claim
// opener, the pushback that argues rather than rules, and the guards.
func TestIsVerdictOutsideTheAuditedClass(t *testing.T) {
	cases := map[string]bool{
		"This is fine. Chasing or fixing the locale error would then surface the entry error.":                    true,
		"not a bug here: the framework's `Ctx.IP()` only falls back to `RemoteIP()` when the proxy is untrusted.": true,
		"False positive, verified the behavior locally.":                                                          true,
		"Not changing this. `useAnchoredHost` re-resolves on `fullscreenchange`, forcing a re-render each time.":  true,
		"Same point as the earlier thread on this line, where the gap works out to 4px. Not changing this.":       false,
		"This was intended, what are you on about":                                                                true,
		"For the fourth time this is intended behavior.":                                                          true,
		"That's the same comment as before.\n\nThis is intended. media_access = FREE is the new version.":         true,
		"This is intended and I will make a note of it in the PR description.":                                    true,
		"The author is right to consider this intended. Stop commenting on this test.":                            false,
		"This isn't really a finding or a risk, just intended behavior.":                                          false,
		"Yes, I'm aware. Will note it in the PR description.":                                                     false,
		"This is how the old translation behaved, disregarding this comment":                                      false,
		"Out of scope for this ticket: that telemetry fires from existing code. Not touching it here.":            false,
		"Also fine, this is only error ordering, nothing slips through.":                                          false,
		"Please stop. I've answered this in the earlier comments.":                                                false,

		"done, a repeated click from the list recenters now (1a2b3c4)":                                  false,
		"Fair. Added error caching and stale reads up to a TTL":                                         false,
		"Valid, fixed in 1a2b3c4. Rejecting a late first 200 would fail envs the old gate let through.": false,
		"Correct, the && short-circuit exempted the exit event from the id check too. Split it.":        false,
		"Real, and reachable beyond navigation: the host also re-resolves on fullscreen changes":        false,
		"Agreed it did nothing useful. Removed it. 1a2b3c4":                                             false,
		"Should be fixed by the latest commits":                                                         false,
		"nice catch prism!":                                                                             false,
		"seems reasonable":                                                                              false,
		"right again!":                                                                                  false,
		"ok?":                                                                                           false,

		"Is this intentional?":                                                   false,
		"Not intentional, fixing.":                                               false,
		"This wasn't intended, fixed in abc1234.":                                false,
		"I think this is by design but let me double check.":                     false,
		"This is intended to be replaced in the follow-up PR.":                   false,
		"Not fixed yet. This is intended to be done in a follow-up":              false,
		"Not intentional. Intended fix is in abc1234.":                           false,
		"That's fine, I'll fix it anyway.":                                       false,
		"The timeout is intentional, but the retry count is a bug, fixing that.": false,
		"Intended fix is in abc1234.":                                            false,
		"> This is intentional\n\nNo it is not, the guard runs twice.":           false,
		"Deliberate: fixed the test instead.":                                    false,
		"I did not do it on purpose":                                             false,
		"I didn't do this intentionally":                                         false,
		"we did not intentionally skip it":                                       false,
		"I never meant to do that deliberately":                                  false,
		"Accepted":                                                               false,
		"Accepted the suggestion":                                                false,
		"Accepted your suggestion, thanks":                                       false,
		"Accepted, applied in abc1234":                                           false,
		"Expected. Fixed in abc1234":                                             false,
		"The ordering is intentional. Fixed the retry count though.":             false,
		"Not a bug, I'll push a fix for the other thing":                         false,
		"this is intentional, i think":                                           false,
		"This is intentional, but it should be fixed.":                           false,
		"Intended, though we should fix it in a follow-up":                       false,
		"By design. This needs to be changed before release.":                    false,
		"This is expected behavior. The new path is broken.":                     false,
		"This is expected. The failure is a real bug in the caller.":             false,
		"Intentional. The deadline gap is real and pre-existing":                 true,
		"Intentional, a bigger hit area should change both together.":            true,
		"The `if` branch is intentional, see the design doc.":                    true,
		"No, this is intentional: the cache is invalidated by the writer.":       true,
		"<!-- template -->\n**By design.** The gateway retries once.":            true,
		"@reviewer by design, the catalog has no code 77.":                       true,
		"We intentionally skip the lookup when the flag is off.":                 true,
		"I left this in deliberately so the migration can be replayed.":          true,
		"It is expected that the backend sends both fields.":                     true,
		"Accepted risk, documented in the runbook.":                              true,
		"Working as intended.":                                                   true,
		"wontfix":                                                                true,
		"Keeping this as-is, the other call sites rely on it.":                   true,
		"this is a known gap, the follow-up adds the cursor":                     true,
		"This is a deliberate choice: one owner per step.":                       true,
	}
	for body, want := range cases {
		if got := IsVerdict(body); got != want {
			t.Errorf("IsVerdict(%q) = %t, want %t", body, got, want)
		}
	}
}

func TestClassifyReplyRanksVerdictsAfterQuestionsAndFixClaims(t *testing.T) {
	cases := map[string]ReplyClass{
		"This is intentional.":                        ReplyVerdict,
		"Won't fix: matches mobile.":                  ReplyVerdict,
		"intended":                                    ReplyVerdict,
		"Not changing this here.":                     ReplyVerdict,
		"Is this intentional?":                        ReplyQuestion,
		"Fixed, it was intentional before but moved.": ReplyResolution,
		"Not intentional, fixing.":                    ReplyPushback,
		"I believe this is intentional, see the doc.": ReplyPushback,
		"that's fine":                                 ReplyVerdict,
		"ok":                                          ReplyOther,
	}
	for body, want := range cases {
		if got := ClassifyReply(body); got != want {
			t.Errorf("ClassifyReply(%q) = %s, want %s", body, got, want)
		}
	}
}

func TestTextEligibilityStopsAtVerdicts(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	verdict := AuthorReply{RootCommentID: 100, Class: ReplyVerdict, Body: "This is intentional.", CreatedAt: now}
	if got := TextEligibility(verdict, nil, 0, now, DefaultTextPolicy()); got != "verdict" {
		t.Errorf("a verdict never gets text: reason=%q", got)
	}
	pushback := AuthorReply{RootCommentID: 100, Class: ReplyPushback, Body: "The guard still runs twice on the retry path.", CreatedAt: now}
	settled := []db.PublishedReply{{Class: string(ReplyVerdict), Action: ReplyActionReacted, Outcome: OutcomeSettledVerdict}}
	if got := TextEligibility(pushback, settled, 0, now, DefaultTextPolicy()); got != "settled" {
		t.Errorf("a thread the author settled gets no more text: reason=%q", got)
	}
}

func verdictFixture(mode string, t *testing.T) (*ReplyReactor, *fakeReplyGH, *fakeReplyLedger, *int) {
	t.Helper()
	runs := 0
	r, gh, ledger := respondFixture(mode, func(_ context.Context, req ReplyRequest) (ReplyDecision, error) {
		runs++
		return ReplyDecision{Decision: DecisionHold, Reply: "The guard is on the other branch; line 12 reaches here with nil.", Cited: holdCite, React: false}, nil
	})
	gh.threads["acme/example#7"][1].Body = "This is intentional, the caller is trusted and the value is set at startup."
	return r, gh, ledger, &runs
}

func TestReplyReactor_VerdictDismissesTheFindingWithAReactionAndNoModelReply(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeRespond, t)
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *runs != 0 || len(gh.posted) != 0 {
		t.Fatalf("the model must not run and nothing is posted: runs=%d posted=%q", *runs, gh.posted)
	}
	if fmt.Sprint(gh.reactions) != "[101]" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed {
		t.Fatalf("reactions=%v states=%v", gh.reactions, ledger.states)
	}
	row := ledger.rows[0]
	if row.Class != string(ReplyVerdict) || row.Action != ReplyActionReacted || row.Outcome != OutcomeSettledVerdict || row.Decision != "" || row.AuthorID != 42 || row.Fingerprint != "a.go:1:abc" {
		t.Fatalf("row=%+v", row)
	}
	if rep.Reacted != 1 || rep.Recorded != 1 || rep.Responded != 0 || rep.Dispatched != 0 || len(rep.Handled) != 1 {
		t.Fatalf("rep=%+v", rep)
	}
	if len(rep.Verdicts) != 1 || rep.Verdicts[0] != (SettledVerdict{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "a.go:1:abc", RootCommentID: 100, AuthorCommentID: 101, Kind: VerdictKindReply}) {
		t.Fatalf("verdicts=%+v", rep.Verdicts)
	}

	gh.reactions = nil
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || rep.AlreadyHandled != 1 || len(rep.Verdicts) != 0 || *runs != 0 || len(gh.posted) != 0 {
		t.Fatalf("a second scan is a no-op: reactions=%v rep=%+v runs=%d", gh.reactions, rep, *runs)
	}
}

func TestReplyReactor_VerdictUnderReactDismissesLikeAThumbsDownContests(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeReact, t)
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.reactions) != "[101]" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || *runs != 0 || len(gh.posted) != 0 {
		t.Fatalf("reactions=%v states=%v runs=%d posted=%q", gh.reactions, ledger.states, *runs, gh.posted)
	}
	if row := ledger.rows[0]; row.Outcome != OutcomeSettledVerdict || len(rep.Verdicts) != 1 {
		t.Fatalf("row=%+v rep=%+v", row, rep)
	}
}

func TestReplyReactor_VerdictRecordedByAnotherLeaderIsReportedOnce(t *testing.T) {
	r, gh, ledger, _ := verdictFixture(ReplyModeRespond, t)
	ledger.recordExists = true
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Verdicts) != 0 || rep.Recorded != 0 || len(rep.Handled) != 0 || len(gh.posted) != 0 {
		t.Fatalf("the leader that recorded the row reports the verdict: rep=%+v", rep)
	}
}

func TestReplyReactor_VerdictSettlementIsFinishedByTheNextScanAfterAFailedWrite(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeRespond, t)
	ledger.failOutcomeOnce = true
	rep, err := r.Run(context.Background())
	if err != nil || len(rep.Errors) != 1 {
		t.Fatalf("the failed write is reported and the scan goes on: err=%v rep=%+v", err, rep)
	}
	if ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || ledger.rows[0].Outcome != "" {
		t.Fatalf("the dismissal happened, the outcome did not: states=%v row=%+v", ledger.states, ledger.rows[0])
	}
	rep, err = r.Run(context.Background())
	if err != nil || len(rep.Errors) != 0 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if ledger.rows[0].Outcome != OutcomeSettledVerdict || *runs != 0 || len(gh.posted) != 0 || fmt.Sprint(gh.reactions) != "[101]" {
		t.Fatalf("the next scan finishes the settlement without the model: row=%+v runs=%d posted=%q reactions=%v", ledger.rows[0], *runs, gh.posted, gh.reactions)
	}
}

func TestReplyReactor_VerdictUnderObserveIsRecordedAndSettledWhenTheModeRises(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeObserve, t)
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || ledger.states["a.go:1:abc"] != "" || len(rep.Verdicts) != 0 {
		t.Fatalf("observe neither reacts nor dismisses: reactions=%v states=%v rep=%+v", gh.reactions, ledger.states, rep)
	}
	if row := ledger.rows[0]; row.Class != string(ReplyVerdict) || row.Action != ReplyActionObserved || row.Outcome != "" {
		t.Fatalf("row=%+v", row)
	}

	r.Mode = ReplyModeRespond
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := ledger.rows[0]
	if fmt.Sprint(gh.reactions) != "[101]" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || row.Action != ReplyActionReacted || row.Outcome != OutcomeSettledVerdict {
		t.Fatalf("respond settles the observed verdict: reactions=%v states=%v row=%+v", gh.reactions, ledger.states, row)
	}
	if *runs != 0 || len(gh.posted) != 0 || len(rep.Verdicts) != 1 || rep.Recorded != 0 || len(rep.Handled) != 1 {
		t.Fatalf("runs=%d posted=%q rep=%+v", *runs, gh.posted, rep)
	}
}

func TestReplyReactor_VerdictUnderShadowReactsButChangesNoFindingState(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeShadow, t)
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.reactions) != "[101]" || ledger.states["a.go:1:abc"] != "" || len(rep.Verdicts) != 0 || *runs != 0 {
		t.Fatalf("reactions=%v states=%v rep=%+v runs=%d", gh.reactions, ledger.states, rep, *runs)
	}
	if row := ledger.rows[0]; row.Outcome != OutcomeShadowedVerdict || row.Action != ReplyActionReacted {
		t.Fatalf("row=%+v", row)
	}
	if _, err := r.Run(context.Background()); err != nil || fmt.Sprint(gh.reactions) != "[101]" {
		t.Fatalf("a second shadow scan does not react again: err=%v reactions=%v", err, gh.reactions)
	}

	r.Mode = ReplyModeRespond
	rep, err = r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := ledger.rows[0]
	if ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || row.Outcome != OutcomeSettledVerdict || fmt.Sprint(gh.reactions) != "[101]" {
		t.Fatalf("respond finishes a verdict seen under shadow without reacting again: states=%v row=%+v reactions=%v", ledger.states, row, gh.reactions)
	}
	if *runs != 0 || len(gh.posted) != 0 || len(rep.Verdicts) != 1 {
		t.Fatalf("runs=%d posted=%q rep=%+v", *runs, gh.posted, rep)
	}
}

func TestReplyReactor_VerdictWaitsWhileAnotherHolderClaimsThePendingRow(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeRespond, t)
	r.LastScanned = map[string]time.Time{}
	claimedAt := r.now().Add(-time.Minute)
	ledger.rows = []db.PublishedReply{{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, RootCommentID: 100, AuthorCommentID: 101,
		Fingerprint: "a.go:1:abc", AuthorID: 42, Class: string(ReplyPushback), Action: ReplyActionPending,
		ClaimedBy: "old-build", ClaimedAt: &claimedAt,
	}}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 0 || ledger.states["a.go:1:abc"] != "" || ledger.rows[0].Outcome != "" || *runs != 0 || len(rep.Verdicts) != 0 {
		t.Fatalf("a row another holder claims is left alone: reactions=%v states=%v row=%+v runs=%d", gh.reactions, ledger.states, ledger.rows[0], *runs)
	}
	if len(r.LastScanned) != 0 {
		t.Fatalf("the PR stays unsettled so the next scan retries: %v", r.LastScanned)
	}

	expired := r.now().Add(-time.Hour)
	ledger.rows[0].ClaimedAt = &expired
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gh.reactions) != "[101]" || ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || ledger.rows[0].Outcome != OutcomeSettledVerdict {
		t.Fatalf("an expired claim is settled: reactions=%v states=%v row=%+v", gh.reactions, ledger.states, ledger.rows[0])
	}
}

func TestReplyReactor_VerdictFastPathOffHandsTheVerdictToTheModel(t *testing.T) {
	for name, configure := range map[string]func(*ReplyReactor){
		"flag off": func(r *ReplyReactor) { r.NoVerdicts = true },
		"legacy":   func(r *ReplyReactor) { r.Legacy = true },
	} {
		r, gh, ledger, runs := verdictFixture(ReplyModeRespond, t)
		configure(r)
		rep, err := r.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if *runs != 1 || len(gh.posted) != 1 || rep.Responded != 1 || len(rep.Verdicts) != 0 {
			t.Fatalf("%s: the model answers the verdict as pushback: runs=%d posted=%d rep=%+v", name, *runs, len(gh.posted), rep)
		}
		if row := ledger.rows[0]; row.Class != string(ReplyPushback) || row.Outcome != "posted" {
			t.Fatalf("%s: row=%+v", name, row)
		}
		if ledger.states["a.go:1:abc"] == db.PublishedStateDismissed {
			t.Fatalf("%s: a hold does not dismiss: states=%v", name, ledger.states)
		}
	}
}

func TestReplyReactor_VerdictOnAPROutsideThePublishGateChangesNothing(t *testing.T) {
	r, gh, ledger, runs := verdictFixture(ReplyModeRespond, t)
	r.Allowed = func(string) bool { return false }
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.PRsSkipped["not_allowlisted"] != 1 || len(gh.reactions) != 0 || len(ledger.rows) != 0 || len(ledger.states) != 0 || *runs != 0 {
		t.Fatalf("rep=%+v reactions=%v rows=%d states=%v runs=%d", rep, gh.reactions, len(ledger.rows), ledger.states, *runs)
	}
}

func TestReplyReactor_VerdictAfterAThumbsDownMovesContestedToDismissed(t *testing.T) {
	r, gh, ledger, _ := verdictFixture(ReplyModeRespond, t)
	ledger.states = map[string]string{"a.go:1:abc": db.PublishedStateContested}
	ledger.findings = []db.PublishedFinding{{Fingerprint: "a.go:1:abc", State: db.PublishedStateContested}}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ledger.states["a.go:1:abc"] != db.PublishedStateDismissed || len(gh.posted) != 0 {
		t.Fatalf("states=%v posted=%q", ledger.states, gh.posted)
	}
}

func TestReplyReactor_ThumbsDownOnTheRootIsReportedAsAVerdict(t *testing.T) {
	r, gh, _ := reactorFixture(ReplyModeReact)
	gh.threads["acme/example#7"][0].ThumbsDown = 1
	gh.given = map[int64][]Reaction{100: {{UserID: 42, Content: "-1"}}}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := SettledVerdict{RepoOwner: "acme", RepoName: "example", PRNumber: 7, Fingerprint: "a.go:1:abc", RootCommentID: 100, Kind: VerdictKindThumbsDown}
	if len(rep.Verdicts) != 1 || rep.Verdicts[0] != want {
		t.Fatalf("verdicts=%+v", rep.Verdicts)
	}
}
