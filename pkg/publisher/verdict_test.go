package publisher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pr-review-server/db"
)

// auditedVerdictReplies are the 71 author replies the comment audit labelled
// intentional_behavior, anonymised (tickets, shas, products and people
// renamed, bodies cut to the sentences that matter). want says whether the
// fast path settles the reply; the rest still reach the model, a bare
// acknowledgement or the fix-claim path, exactly as before.
var auditedVerdictReplies = []struct {
	body string
	want bool
}{
	{"we already send these to the metrics vendor in other places.  good call out, I created the bot and yes i confirm i havent yet enabled you to look into the main repo to see where we do this!", false},
	{"This is acceptable. It's not often that a non-db error will cause this to fail, and returning stale values for 5 seconds isn't really a concern.", true},
	{"Import route is being deprecated. Marking this as resolved.", false},
	{"This is why the ttl of 180 seconds exists. The stale value will not be served forever.", false},
	{"This is intended behavior. Unregistered switches always read as false and are not editable in the database because they don't do anything and should be deleted or renamed to a registered switch.", true},
	{"This is intended behavior.", true},
	{"intended", true},
	{"This is deliberate behavior. It actually doesn't disable the entry, as you just said English users will still receive the entry.", true},
	{"Correct that the grants are now cluster-wide, and that's accepted for Q3. This PR is the first step of the RBAC ladder, which changes one thing per step.", false},
	{"Accepted, and it's why this is a dedicated ingest key (`ingest-key`) and not the cluster bundle's license key. An ingest key can only write telemetry to one account; it can't read or query anything.", true},
	{"By design; header corrected in dbfd32d. The gateway's catch-all 404s fade out over up to 15 minutes, so a late 2xx is the stable signal, and a strict end-of-window check would fail releases on one stray 404.", true},
	{"By design. The catalog's exit codes are fixed at 75, 2 and 76 (the subcommands copy that contract later), so there is no 77. A longer auth outage fails the step, which is the right signal for an outage.", true},
	{"By design for this PR. delete-environment is copied byte for byte from release-environment's (same 120s deadline, same missing continueOn and default); XO-1748 keeps the copies identical so a fix lands in both. The deadline-vs-retry gap is real and pre-existing", true},
	{"By design for this PR. prod-seed-snapshot-glue is release-environment-glue less unused code; run_action's replay on a 5xx is inherited unchanged and still runs that way in release-environment. Only delete_integration_environment goes through it here, where a duplicate is a no-op or failed run.", true},
	{"By design for Q3, same as the Greptile thread: the shared executor service account is the recorded gap until XO-1784 gives QA its own account and bucket grants. Verified its IAM: objectViewer and objectCreator on the artifacts bucket, no project-level roles.", true},
	{"This is intentional and in the description (line 11). every other http call in prod code already uses get_ssl_verification, the legacy path was the only verify=False left and it was hitting the same upstream endpoint the migrate command verifies.", true},
	{"This is intended, we require the user to have enough tokens for one minute of show, even with no minimum time set", true},
	{"intentional change is intentional, muting after the show ends would be weird", true},
	{"This would be correct behavior; if they are trying to join a show via an invalid entrypoint, we can't start to reconnect the show, so we prompt them until they decide end the show early. Without the reconnection parameter, the user could simply decline", true},
	{"Yep, known gap for this PR. Scoped in MSG-3282, which adds the cursor and a fill loop that keeps fetching from where the page stopped until it has 50 or every source is done.", false},
	{"This is intended: the ticket's arbitration rules say room leave clears both, room load clears a lingering toast, and deliberately leave the banner out of that. Not a bug, keeping as is.", true},
	{"Intentional. The gate is as narrow as we can make it. Every room-page bell journey today is a follow nudge, and the \"BROADCASTER is online\" bell is a test fixture. The explicit per-entry marker you describe is the real fix and belongs upstream", true},
	{"intentional phased state, following PRs will implement other video modes", true},
	{"Duplicates can't be created by the current purchase paths. The legacy path checks under TippingLock, and both paths write the sale row in the same atomic block as the purchase. The sale table is unique on (set, purchaser), so I removed the key dedupe on purpose.", false},
	{"This is intentional. Opt-in adds the paid review required for the vault without cancelling the existing messaging review. `test_free_task_does_not_skip_paid_review` covers this case.", true},
	{"The `common` runners are ephemeral: each job gets a fresh pod that is destroyed when the job ends, so nothing written to `~/.ssh` survives into a later job. This composite is a port of `actions/deploy-staging`; this line is carried over unchanged", false},
	{"that's fine", true},
	{"I will handle this in a follow up. The expected behavior is that these rejected tiles should be visible but not selectable.", false},
	{"Discussed previously, but yes, we do want rejected media to appear in the post modal. Changes to how this will be handled in the modal itself will be done in a separate PR.", false},
	{"This is intended. On reject, there is a tooltip that is activated on pill hover or click depending on device capability. Outside of reject, we allow the fall through to open the viewer.", true},
	{"This was intentional, it felt like it made more sense that a pill with a tooltip would not result in the viewer opening", true},
	{"This is intentional, there's a chat thread discussing this and agreeing that covering the title and description should be fine.", true},
	{"Will followup with https://tracker.example/browse/XO-11995", false},
	{"Profile tiles already handle missing previews with a matching legacy cover or placeholder. Messaging fallback handling is tracked in [MSG-3408](https://tracker.example/browse/MSG-3408) and must merge before the flag is enabled for creators with migrated media.", false},
	{"https://tracker.example/browse/TS-2513 will handle the backfill so that anyone that requires a passport will not have an empty `require_other_id_type` field.", false},
	{"https://tracker.example/browse/TS-2513 will handle the backfill so that anyone that requires a passport will not have an empty `require_other_id_type` field.", false},
	{"This was done on purpose to trigger a fresh fetch when switching from Live to All, which somewhat matches existing/previous behavior. This is in case the cache is stale when we're switching filters", true},
	{"It was not originally \"intended\" persay but, like you mentioned, it seemed to be an improvement so I decided not to gate it.", false},
	{"That is the goal/intent. These are new and don't affect existing users. Mediasets can't have their access settings changed once they're created so we can't go from undefined (legacy) to something (new).", true},
	{"That is the goal. I updated the description.", true},
	{"Current legacy behavior is that a failed message with media is wiped. This PR does not change that behavior.", false},
	{"This is intentional and fixes your earlier finding about misleading partial counts. The API's total_count counts media sets, not pictures or videos. We chose to hide incomplete counts until all pages load. Removing !hasMore would restore the original issue.", true},
	{"This is an edge case that we actually want to happen. Current users of the endpoint are changing the host so that the click throughs go to their whitelabel because the domain location is not a currently available feature. We don't want this addition to break their current workaround.", false},
	{"This is a new feature that has not rolled out so no one is actually using the `companion_asset` parameter so we can update it without worrying about consumers of the endpoint.", false},
	{"Not changing this here. It's the same 16px `CloseButton` the over-chat notification uses, and a bigger hit area should change both together.", true},
	{"yes", false},
	{"yes", false},
	{"Intentional, that's what Product asked.", true},
	{"Known and accepted per the ticket. The feed only room-gates fan club sets (purchases bypass it, recordings aren't gated), so this only affects a fan-club-only host whose room is blocked for the viewer.", true},
	{"Paid media sets, including DM ones, aren't live in prod yet. MSG-3307 makes DM sets viewable, and this list already includes their creators so it won't need a change then.", false},
	{"Intentional, it matches every messaging surface (check_gender_block=True), and the new test covers it.", true},
	{"This toggle should apply to tier 1 too", false},
	{"this feature is now dependent on the translation toggle too", false},
	{"yes showing h2 instead is ok behavior in that case", false},
	{"hashtag table names are required and no fallbacks should be provided", false},
	{"Won't fix: matches mobile.", true},
	{"Intended. The viewer's F2F publish negotiates mid-call and should still ramp r0 from the start cap. A call no longer clears r0's cap (8a4d4301e6), so the start cap is ramped like any other session.", true},
	{"Intended. A call keeps r0's current state. Held means the browser reports no estimate, so nothing confirms that uncapping r0 is safe, and doing it mid-broadcast risks the stack switching r0 off while live.", true},
	{"Not changed. A null MoM cell with no note only occurs when the same row's level cell is gated, and that level cell already carries the reason as a footnote (\"No data for …\" / \"Too few samples …\"). Every other null cell now has its own footnote.", true},
	{"Intentional. Personal cloud projects are not part of standard setup, so this page drops them. A team that still needs them can document it on its own page.", true},
	{"Deliberate: issuing-presence-first is the fix for the no-expiration-trump gap this ticket closes, a document that cannot prove when it was issued must not displace one that can, and the two regression tests pin that intent.", true},
	{"Deliberate, and legacy parity: the removed-payee gate is a person-wide freeze, `link_best_identity` skipped identically. If the freeze semantics should narrow, that is a policy change for its own ticket, not a gap introduced here.", true},
	{"Deliberate: name vouching accepts any non-expired document, aged-out included, matching the long-standing sibling convention. Tightening both touches every name-check flow, a policy decision for a follow-up if wanted.", true},
	{"By design, and matching the live glue, which retries a 401 on the token mint rather than treating it as final. The catalog template `entity-upsert-v0` makes the same call, re-minting once on a 401. On the last attempt summarize still records the row without relations, so the row is delayed, never lost.", true},
	{"Not changed here. The inline run-create-v0 logs the same line, and this PR ports that template exactly; the same inputs already show on the run in the catalog. Trimming it to property names needs the template changed too, noted as a follow-up on XO-1750.", true},
	{"Release test plans are per repository (only one repo has a test plan map), so one owner/repo is right for every number in the cell; linking only the confirming PR would drop the other numbers' links when the cell is rewritten. Left as is", false},
	{"Intentional correction.", true},
	{"Decision taken and recorded in 300a57dd. The two pre-merge checks were run: production Redis requires a password: **NO** (the chart owner, 2026-10-01 17:38Z; `requirepass` is supported but not enabled)", false},
	{"The maintainer then ruled (2026-10-01, decision 23) that the flip ships without the NetworkPolicy, since a per-deployment policy is what platform engineering asked to weigh fleet-wide first, and accepted the residual", false},
	{"Resolved by the Project Lead on the maintainer's decision, not as a disagreement with the finding.", false},
	{"Resolved by the Project Lead on the maintainer's decision, not as a disagreement with the finding.", false},
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
		"This is fine. Looking into or fixing the translation error will then surface the entry error.":              true,
		"not a bug here: the framework's `Ctx.IP()` only falls back to `RemoteIP()` when the proxy is untrusted.":    true,
		"False positive, checked this behavior locally.":                                                             true,
		"Not changing this. `usePurrAnchoredHost` re-resolves on `fullscreenchange`, forcing a re-render each time.": true,
		"Same finding as the earlier thread on this line, where the clearance works out to 4px. Not changing this.":  false,
		"This was intended what are you talking about":                                                               true,
		"For the 5th time this is intended behavior.":                                                                true,
		"That's the same comment as before.\n\nThis is intended. media_access = FREE is the new version.":            true,
		"This is intended and I will make a note in the PR description.":                                             true,
		"The author would be correct in considering this intended. Stop commenting on this test case.":               false,
		"This isn't really a finding or a risk, it's just intended behavior.":                                        false,
		"Yes, I'm aware. Will make a note in the PR description.":                                                    false,
		"This is how the previous translation worked, disregarding this comment":                                     false,
		"Out of scope for this ticket: the shown telemetry fires from existing code. Not touching it here.":          false,
		"Also fine, this is just error order, nothing falls through the cracks.":                                     false,
		"Please stop. I've addressed this in the previous comments.":                                                 false,

		"done, a same-selection click from the list or tables recenters now (c28b4ff)":                false,
		"Fair. Implemented error caching and stale reads up to a stale TTL":                           false,
		"Valid, fixed in 4dc9bc2. Rejecting a late first 200 would fail envs the old gate accepted.":  false,
		"Correct, the && short-circuit exempted room_exit from the visit-id check too. Split it.":     false,
		"Real, and reachable beyond room navigation: the host also re-resolves on fullscreen changes": false,
		"Agreed it wasn't doing anything useful. Removed it. d85e85d98b":                              false,
		"Should be fixed with the latest commits":                                                     false,
		"nice catch prism!": false,
		"seems reasonable":  false,
		"right again!":      false,
		"ok?":               false,

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
	if ledger.rows[0].Outcome != OutcomeSettledVerdict || *runs != 0 || len(gh.posted) != 0 || fmt.Sprint(gh.reactions) != "[101 101]" {
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
	if row := ledger.rows[0]; row.Outcome != OutcomeSettledVerdict || row.Action != ReplyActionReacted {
		t.Fatalf("row=%+v", row)
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
