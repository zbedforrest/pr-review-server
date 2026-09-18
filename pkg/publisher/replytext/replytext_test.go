package replytext

import (
	"strings"
	"testing"
)

func TestStripAgreementOpener(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"You're right, the guard on retry.go:41 runs first.", "The guard on retry.go:41 runs first.", true},
		{"You're right. The guard on retry.go:41 runs first.", "The guard on retry.go:41 runs first.", true},
		{"You’re right that the guard on retry.go:41 runs first.", "The guard on retry.go:41 runs first.", true},
		{"Correct, and the fix is in at store.go:143.", "The fix is in at store.go:143.", true},
		{"Agreed. Good point, `store.go:143` caches the error.", "`store.go:143` caches the error.", true},
		{"Fair enough: nothing reads change.txt.", "Nothing reads change.txt.", true},
		{"The guard on retry.go:41 runs before the branch.", "The guard on retry.go:41 runs before the branch.", true},
		{"Right now the guard on retry.go:41 runs first.", "Right now the guard on retry.go:41 runs first.", true},
		{"Yesterday's change to retry.go:41 covers it.", "Yesterday's change to retry.go:41 covers it.", true},
		{"You're right about the guard, it runs first.", "You're right about the guard, it runs first.", true},
		{"You're right, retry.go:41 guards this.", "retry.go:41 guards this.", true},
		{"Correct, onRoomLoaded on X.tsx:82 only drops the toast.", "onRoomLoaded on X.tsx:82 only drops the toast.", true},
		{"You're right.", "", false},
		{"Agreed, good point.", "", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		got, ok := StripAgreementOpener(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q ok=%t, want %q ok=%t", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAssertsIntent(t *testing.T) {
	yes := []string{
		"This is intended: the ticket's arbitration rules say room leave clears both. Not a bug, keeping as is.",
		"This is intentional, the banner is a product decision.",
		"Working as designed.",
		"By design, we leave the toast as-is.",
		"This is intentional; nothing changed, keeping it as-is.",
		"This was added intentionally and is staying as-is.",
		"Defaced input is rejected on purpose; the commit message explains why.",
	}
	no := []string{
		"Fixed in 519f006, the guard is now on line 77.",
		"It was intentional at first but I changed it to clear the banner too.",
		"It is read by the deploy script, see hello.txt.",
		"Out of scope for this ticket, not touching it here.",
		"The intended caller is on a.go:1, so the guard runs first.",
		"Intentional originally, but the latest commit clears the banner too.",
	}
	for _, s := range yes {
		if !AssertsIntent(s) {
			t.Errorf("should assert intent: %q", s)
		}
	}
	for _, s := range no {
		if AssertsIntent(s) {
			t.Errorf("should not assert intent: %q", s)
		}
	}
}

func TestDefersAndTicketKeys(t *testing.T) {
	yes := []string{
		"Out of scope for this ticket: the shown telemetry fires from the bridge. Not touching it here.",
		"Will do this in a follow-up.",
		"Belongs in a separate PR.",
		"Tracking it separately in PROJ-42.",
	}
	for _, s := range yes {
		if !Defers(s) {
			t.Errorf("should defer: %q", s)
		}
	}
	if Defers("Fixed in 519f006, the guard is now on line 77.") {
		t.Errorf("a fix is not a deferral")
	}
	keys := TicketKeys("Tracked in PROJ-42 and PROJ-42 again; uses SHA-256 and UTF-8, see CVE-2024-1234 and RFC-7231.")
	if len(keys) != 1 || keys[0] != "PROJ-42" {
		t.Errorf("keys = %v", keys)
	}
	if got := TicketKeys("no keys here, just retry.go:41"); got != nil {
		t.Errorf("keys = %v", got)
	}
}

func TestFindingSeverity(t *testing.T) {
	cases := map[string]string{
		"<!-- prism:finding:v1:x -->\n**[MEDIUM] Nil deref.**":                 "medium",
		`<img alt="HIGH" src="https://example.test/badges/high.svg"> **Leak**`: "high",
		"no label": "",
	}
	for in, want := range cases {
		if got := FindingSeverity(in); got != want {
			t.Errorf("%q: severity=%q want %q", in, got, want)
		}
	}
	if !SeverityAtLeastMedium("critical") || !SeverityAtLeastMedium("medium") || SeverityAtLeastMedium("low") || SeverityAtLeastMedium("") {
		t.Errorf("severity bar is wrong")
	}
}

func TestRenderIntentPushbackAcknowledgesAndRecordsWithoutWithdrawing(t *testing.T) {
	ctx := Context{
		AuthorComment: "This is intended: the arbitration rules leave the banner out. Not a bug, keeping as is.",
		FindingBody:   "**[MEDIUM] Banner survives room entry.**",
		Decision:      "concede",
	}
	body := "You're right. That is the ticket's call. Keeping it means a banner raised in one room stays on screen after entering the next, since onRoomLoaded on PurrMediaExperiences.tsx:82 only drops the toast. Withdrawing this."
	got, ok := Render(body, ctx)
	if !ok {
		t.Fatal("rendered nothing")
	}
	want := "That is the ticket's call. Keeping it means a banner raised in one room stays on screen after entering the next, since onRoomLoaded on PurrMediaExperiences.tsx:82 only drops the toast. Should this be noted in the PR description as accepted risk?"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if Note(ctx) != NoteIntentAcknowledged {
		t.Errorf("note = %q", Note(ctx))
	}
	again, _ := Render(got, ctx)
	if again != got {
		t.Errorf("render is not idempotent: %q", again)
	}
	if _, ok := Render("You're right. Withdrawing this.", ctx); ok {
		t.Errorf("a withdrawal with nothing else must be rejected")
	}
	domain := "payments.go:12 withdraws funds before the authorization check. Keeping it means a double charge is possible."
	if got, _ := Render(domain, ctx); !strings.HasPrefix(got, domain) {
		t.Errorf("domain language is not a withdrawal: %q", got)
	}
}

func TestRenderIntentPushbackBelowMediumDoesNotAskAboutAcceptedRisk(t *testing.T) {
	ctx := Context{AuthorComment: "Intentional, keeping as is.", FindingBody: "**[LOW] Log line is noisy.**", Decision: "concede"}
	got, _ := Render("Your call. The line at log.go:12 prints once per request at info level.", ctx)
	if strings.Contains(got, "accepted risk") {
		t.Errorf("low severity must not ask: %q", got)
	}
	got, _ = Render("Your call. The line at log.go:12 prints once per request; worth a note in the PR description.", Context{AuthorComment: ctx.AuthorComment, FindingBody: "**[HIGH] x**", Decision: "concede"})
	if strings.Count(got, "description") != 1 {
		t.Errorf("must not ask twice: %q", got)
	}
	got, _ = Render("Withdrawing this.", Context{AuthorComment: ctx.AuthorComment, FindingBody: ctx.FindingBody, Decision: "hold"})
	if got != "Withdrawing this." {
		t.Errorf("only a concession takes the intent path: %q", got)
	}
}

func TestAppendixLenCountsOnlyWhatRenderAppended(t *testing.T) {
	body := "The guard on a.go:12 is gone."
	ctx := Context{AuthorComment: "Intentional, out of scope here, keeping as is.", FindingBody: "**[HIGH] x**", Decision: "concede"}
	got, _ := Render(body, ctx)
	if got == body || AppendixLen(got) != len([]rune(got))-len([]rune(body)) {
		t.Errorf("got %q appendix=%d", got, AppendixLen(got))
	}
	if AppendixLen(body) != 0 || AppendixLen("Tracked against PROJ-1 already.") != 0 {
		t.Errorf("a body with nothing appended has no appendix")
	}
	withKey, _ := Render(body, Context{AuthorComment: "Out of scope, see PROJ-42.", Decision: "hold"})
	if AppendixLen(withKey) != len([]rune(" Tracking this against PROJ-42.")) {
		t.Errorf("got %q appendix=%d", withKey, AppendixLen(withKey))
	}
}

func TestRenderDeferralAsksForOrRepeatsTheTicketKey(t *testing.T) {
	body := "The over-count is introduced by this component: purrBridge.ts:353 records the impression before PurrMediaExperiences.tsx:68 drops it."
	got, _ := Render(body, Context{AuthorComment: "Out of scope for this ticket, not touching it here.", Decision: "hold"})
	if !strings.HasSuffix(got, " "+TicketAsk) || strings.Count(got, "ticket") != 1 {
		t.Errorf("got %q", got)
	}
	again, _ := Render(got, Context{AuthorComment: "Out of scope for this ticket, not touching it here.", Decision: "hold"})
	if again != got {
		t.Errorf("render is not idempotent: %q", again)
	}
	got, _ = Render(body, Context{AuthorComment: "Out of scope here, tracked in PROJ-42.", Decision: "hold"})
	if !strings.HasSuffix(got, " Tracking this against PROJ-42.") {
		t.Errorf("got %q", got)
	}
	got, _ = Render(body+" PROJ-42 can carry it.", Context{AuthorComment: "Out of scope here, tracked in PROJ-42.", Decision: "hold"})
	if strings.Count(got, "PROJ-42") != 1 {
		t.Errorf("key must not be repeated twice: %q", got)
	}
	got, _ = Render(body, Context{AuthorComment: "Fixed in 519f006.", Decision: "concede"})
	if got != body {
		t.Errorf("a non-deferral must be left alone: %q", got)
	}
	got, _ = Render("The cache key on cache.go:12 remains shared between tenants.", Context{AuthorComment: "Out of scope, follow-up.", Decision: "hold"})
	if !strings.HasSuffix(got, TicketAsk) {
		t.Errorf("an unrelated key must not count as the ask: %q", got)
	}
	got, _ = Render("The cache key on cache.go:12 remains shared; if there is a ticket for this, its key would close the thread.", Context{AuthorComment: "Out of scope, follow-up.", Decision: "hold"})
	if strings.Contains(got, TicketAsk) {
		t.Errorf("an ask in the model's own words must not be doubled: %q", got)
	}
}
