package publisher

import (
	"context"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// Regression tests built from the 2026-10-13 day-one audit, anonymised:
// classes 2 (severity text and internal ids), 6 (praise, reason-less
// notes, status line, no-op refresh, source footer), 9 (unverified
// library claims) and 10 (invented regressions).

// contractFinding is a confirmed finding with a valid contract, the shape
// every audited comment had.
func contractFinding(sev, file string, line int, comment string, c *types.FindingContract) payload.Finding {
	x := payload.Finding{Severity: sev, File: file, Line: line, Comment: comment, State: "confirmed", Active: true, Provenance: "agent"}
	x.FindingContract = c
	x.FindingContractStatus = types.ContractStatus(c)
	x.ID = payload.Fingerprint(file, line, comment)
	return x
}

func asserting(kind, impact, uncertainty, rationale, headline string) *types.FindingContract {
	c := falsifiableTestContract(kind, "current_impact", impact, uncertainty)
	c.SeverityRationale = rationale
	c.Headline = headline
	c.Subjects = []types.FindingSubject{{Kind: "file", Path: "x"}}
	return c
}

func opinion(impact, uncertainty, rationale, headline string) *types.FindingContract {
	return &types.FindingContract{
		SchemaVersion: 1, FindingKind: "design_opinion", Materiality: "no_user_impact", Falsifiability: "not_falsifiable",
		CurrentImpact: impact, Uncertainty: uncertainty, SeverityRationale: rationale, Headline: headline,
		Subjects: []types.FindingSubject{{Kind: "file", Path: "x"}},
	}
}

// Class 2: "Low; same pattern as A-1 and directly verifiable." rendered
// under a MEDIUM badge.
func TestRenderInline_ContractSentencesCarryNoConfidenceLabelOrFindingID(t *testing.T) {
	modifier := contractFinding("medium", "web/src/modal/AccessModal.scss", 190,
		"The `--desktop` modifier is nested inside the base `.AccessModal` block, so the stylesheet compiles this to the descendant selector `.AccessModal .AccessModal--desktop`. But the element gets both classes on the same element, so the selector never matches and the desktop width and padding are dead code.",
		asserting("production_behavior", "On desktop, the access modal renders with mobile geometry instead of the intended 369px desktop variant.",
			"Low; same pattern as A-1 and directly verifiable.", "Dead CSS silently drops the desktop sizing this PR exists to add.", "Desktop modal modifier selector never matches"))
	terms := contractFinding("medium", "web/src/modal/TermsModal.scss", 132,
		"The `--desktop` modifier is nested inside the base `.TermsModal` block, so the stylesheet compiles this to the descendant selector `.TermsModal .TermsModal--desktop`. The test only checks the class exists, so it won't catch this.",
		asserting("production_behavior", "On desktop, the terms modal keeps its mobile geometry because the modifier CSS never applies.",
			"Low; the nesting and the single-element class application are both directly visible in the code.", "A selector that cannot match makes the PR's desktop-layout change a no-op.", "Desktop terms modifier selector never matches"))

	body := RenderInline(modifier, SourceTagPrismOnly, "", "https://prism.example/badge", "")
	if strings.Contains(body, "Low;") || strings.Contains(body, "A-1") {
		t.Fatalf("a MEDIUM comment must not say Low or lean on an internal id:\n%s", body)
	}
	body = RenderInline(terms, SourceTagPrismOnly, "", "https://prism.example/badge", "")
	if strings.Contains(body, "Low;") || !strings.Contains(body, "\nThe nesting and the single-element class application are both directly visible in the code.\n") {
		t.Fatalf("the confidence label goes, the sentence stays:\n%s", body)
	}
}

func TestRenderInline_BodyNamesOtherFindingsInsteadOfTheirLabels(t *testing.T) {
	x := f("t1", "medium", "a.go", 10, "This test verifies the page contains the source strings rather than any behavior. It will pass with the `opt.selected` bug in A-1 and will break on any refactor (see A-2, A-3).")
	body := RenderInline(x, SourceTagPrismOnly, "", "", "")
	if MentionsFindingID(strings.ReplaceAll(body, FindingMarker("t1"), "")) {
		t.Fatalf("internal labels leaked:\n%s", body)
	}
	if !strings.Contains(body, "bug in another finding on this PR and will break on any refactor.") {
		t.Fatalf("a label the sentence leans on is named, an aside is dropped:\n%s", body)
	}
}

func TestRenderSummary_DisputedReasonNamesOtherFindingsInsteadOfTheirLabels(t *testing.T) {
	x := f("u1", "medium", "a.go", 10, "The consumer never calls getPermissions.")
	x.State = "unverified"
	x.Assessment = &types.Disposition{SourceID: "FP-3", State: "rejected", Reason: "Same claim as FP-2: the mobile consumer tears itself down on devicesFailed."}
	r := Round{Owner: "acme", Repo: "example", Number: 7, HeadSHA: "sha", RoundNumber: 1, ShowUnverified: true, Findings: []payload.Finding{x}}
	summary := RenderSummary(r, Selection{})
	if MentionsFindingID(summary) || !strings.Contains(summary, "Agent: Same claim as another finding on this PR: the mobile consumer") {
		t.Fatalf("summary:\n%s", summary)
	}
}

// Class 6: "Source: PRism · Both" under a root comment means nothing to a
// reader.
func TestRenderInline_NoSourceFooter(t *testing.T) {
	x := f("m1", "medium", "b.go", 20, "Medium thing.")
	if body := RenderInline(x, SourceTagBoth, "", "", ""); strings.Contains(body, "Source: PRism") {
		t.Fatalf("source footer still rendered:\n%s", body)
	}
}

// Class 6: a compliment used as the title of a LOW note.
func TestTitleSource_SkipsAPraiseOpener(t *testing.T) {
	comment := "The change correctly pairs `use_http2 = true` (port named `h2c`) with `HTTP2_ENABLED=true` so the container accepts h2c forwards. However, the deploy workflow also derives both the port name and this env var from the per-environment `SERVICE_HTTP2_ENABLED` variable. If that variable is unset for dev, a workflow deploy will revert the service to HTTP/1.1."
	c := &types.FindingContract{SchemaVersion: 1, FindingKind: "latent_hazard", Materiality: "future_condition_only", Falsifiability: "falsifiable",
		CurrentImpact: "Dev now accepts h2c as intended; no current user-visible impact, since the risk is a future deploy reverting the setting.",
		Uncertainty:   "Whether the dev variable is already set to true cannot be determined from this repository.", SeverityRationale: "Dev-only.",
		Subjects: []types.FindingSubject{{Kind: "file", Path: "x"}}}
	trigger, cond, obs := "A workflow deploy runs with the variable unset.", "Run the deploy workflow for dev with the variable unset.", "The port is renamed back to http1."
	c.CounterfactualTrigger, c.FalsifiableCondition, c.ExpectedObservable = &trigger, &cond, &obs
	x := contractFinding("low", "workspaces/dev/service.tf", 33, comment, c)
	if x.FindingContractStatus != "valid" {
		t.Fatal("fixture contract is invalid")
	}
	got := titleSource(x)
	want := "The deploy workflow also derives both the port name and this env var from the per-environment `SERVICE_HTTP2_ENABLED` variable."
	if got != want {
		t.Fatalf("title = %q, want the first sentence that reports something", got)
	}
	r := Round{Owner: "acme", Repo: "example", Number: 41, HeadSHA: "6b4506b", RoundNumber: 1, Findings: []payload.Finding{x}}
	if summary := RenderSummary(r, Selection{}); strings.Contains(summary, "correctly pairs") {
		t.Fatalf("praise listed as a finding:\n%s", summary)
	}
}

func TestIsPraise(t *testing.T) {
	for text, want := range map[string]bool{
		"The PR correctly pairs `a` with `b` so the container accepts forwards.": true,
		"The nested quoting is correct: the tool parses each value as CSV.":      true,
		"This looks good.": true,
		"The handler does not correctly release the lock on the error path.": false,
		"The change correctly guards the read but misses the write path.":    false,
		"Retry loop never backs off.":                                        false,
		"The `sized` filter drops tables missing `total_size_bytes`.":        false,
		"This change already drops the guard.":                               false,
		"The retry helper is correct and ignores the deadline.":              false,
		"Clean-room parse drops the lock.":                                   false,
		"Solid-state cache is unguarded.":                                    false,
		"The change correctly moves the logic, which regresses the cache.":   false,
	} {
		if got := IsPraise(text); got != want {
			t.Errorf("IsPraise(%q) = %v, want %v", text, got, want)
		}
	}
}

// Class 6: a bare-title note with no reason behind it is held, not listed.
func TestPublish_HoldsAReasonlessNote(t *testing.T) {
	note := contractFinding("low", "web/src/ui/overlay.tsx", 682,
		"The `FocusToggle` toggle is reused to gate autofocus behavior on the overlay, but this toggle already controls the signup form experiment via `SignupModal`. Verify whether this coupling is intentional.",
		opinion("None demonstrated; behavior is as described in the PR when the shared toggle is enabled.",
			"Medium; whether an independent toggle is desired is a product/rollout call I cannot infer from the diff alone.",
			"It is a toggle-scoping preference rather than a defect, so it is low severity.",
			"Login focus gated by signup-form toggle"))
	gap := contractFinding("low", "web/src/ui/overlay.test.ts", 28,
		"The test parameterization does not cover the mobile + toggle-off case, which exercises the fallback branch. Add the missing row.",
		&types.FindingContract{SchemaVersion: 1, FindingKind: "test_quality", Materiality: "no_user_impact", Falsifiability: "not_falsifiable",
			CurrentImpact: "No current user impact; the new test table omits one branch of the mobile override.", Uncertainty: "Low: the existing cases pin the primary behavior.",
			SeverityRationale: "Test-only gap with no production behavior change, so LOW.", Subjects: []types.FindingSubject{{Kind: "file", Path: "x"}}})
	if !ReasonlessNote(note) || ReasonlessNote(gap) {
		t.Fatalf("reasonless: note=%v gap=%v", ReasonlessNote(note), ReasonlessNote(gap))
	}
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := Round{Owner: "acme", Repo: "example", Number: 32664, HeadSHA: "d26ad1b", Findings: []payload.Finding{note, gap}}
	rep := publishRound(t, gh, ledger, r)
	summary := gh.issueCreates[0]
	if strings.Contains(summary, "Login focus gated") || !strings.Contains(summary, "1 folded note<") {
		t.Fatalf("the bare-title note must not be listed:\n%s", summary)
	}
	if len(rep.Held) != 1 || rep.Held[0].Reason != HoldReasonless || rep.Held[0].ID != note.ID {
		t.Fatalf("held = %+v", rep.Held)
	}
	if len(rep.Hygiene.Held) != 1 {
		t.Fatalf("a hold is a hygiene note: %+v", rep.Hygiene)
	}
}

// Class 6: "0 new · 0 still open · 0 fixed" above six folded notes.
func TestRenderSummary_StatusLineCountsFoldedNotes(t *testing.T) {
	r := Round{Owner: "acme", Repo: "example", Number: 32470, HeadSHA: "e1939b0", RoundNumber: 2}
	for i := 0; i < 6; i++ {
		r.Findings = append(r.Findings, contractFinding("low", "discovery/views.py", 10+i, "Note number "+string(rune('a'+i))+" about a stale comment.",
			opinion("No behavioral difference today; the pointer in the comment is stale.", "None.", "Comment-only staleness.", "")))
	}
	summary := RenderSummary(r, Selection{})
	if !strings.Contains(summary, "**Since last review:** 0 new · 0 still open · 0 fixed · 6 folded notes") {
		t.Fatalf("status line ignores the folded notes:\n%s", summary)
	}
	if d, ok := parseSinceLastReview(summary); !ok || d.Fixed != 0 {
		t.Fatalf("the refresh path must still parse the line: %+v %v", d, ok)
	}
	r.RoundNumber = 1
	if summary := RenderSummary(r, Selection{}); strings.Contains(summary, "Since last review") {
		t.Fatalf("round one has no status line:\n%s", summary)
	}
}

// Class 6: a summary re-edited with no new commit and nothing changed.
func TestRefreshSummary_DoesNotEditAnUnchangedSummary(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, filedRound())
	gh.existingIssueComments = []IssueComment{{ID: 501, Body: gh.issueCreates[0]}}
	editsBefore := len(gh.issueEdits)

	refresh := filedRound()
	refresh.RoundNumber, refresh.HeadSHA, refresh.Previous = 0, "", nil
	p := &Publisher{GH: gh, Ledger: ledger, Policy: DefaultPolicy()}
	if err := p.RefreshSummary(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	if len(gh.issueEdits) != editsBefore {
		t.Fatalf("an unchanged summary was edited:\n%s", gh.issueEdits[501])
	}
}

// Class 9: "I am reading the action's documented behavior from memory" and
// "shift defaults are certain" were both wrong.
func TestPublish_HoldsUnverifiedLibraryClaims(t *testing.T) {
	scan := contractFinding("medium", ".github/workflows/scan.yml", 28,
		"The job grants `security-events: write` to upload SARIF results, but the scanner action is not configured with `advanced-security: true`. Without this setting, the action only produces a workflow artifact and does not upload to code scanning.",
		asserting("production_behavior", "On the first run after merge the scan results are not uploaded to code scanning, so the alerts this PR intends to clear remain open.",
			"I am reading the action's documented behavior from memory and cannot inspect the pinned release in this checkout.",
			"Losing the upload silently reduces scanning visibility.", "Scan step may not upload findings to code scanning"))
	shift := contractFinding("medium", "web/src/tooltip/middlewares.ts", 60,
		"`shift` defaults to `mainAxis: true` and `crossAxis: false`, so it only repositions the tooltip along the vertical axis for a top placement. Enable cross-axis shifting.",
		asserting("production_behavior", "Tooltips wider than the horizontal room beside their icon can overflow the viewport edge instead of sliding to stay visible.",
			"Positioning-library shift defaults are certain; residual uncertainty is only whether the tooltip wrapper injects its own shift middleware.",
			"Clipped tooltips on narrow layouts.", "Shift middleware cannot slide tooltip horizontally"))
	read := contractFinding("medium", "web/src/tooltip/middlewares.ts", 70,
		"`node_modules/@floating-ui/core/src/middleware/shift.ts:41` sets `crossAxis = false` by default, so the tooltip never slides horizontally.",
		asserting("production_behavior", "Tooltips wider than the room beside their icon overflow the viewport edge.",
			"The default is read from the pinned source in node_modules.", "Clipped tooltips.", "Tooltip never slides horizontally"))
	if !UnverifiedLibraryClaim(scan) || !UnverifiedLibraryClaim(shift) || UnverifiedLibraryClaim(read) {
		t.Fatalf("library claim: scan=%v shift=%v read=%v", UnverifiedLibraryClaim(scan), UnverifiedLibraryClaim(shift), UnverifiedLibraryClaim(read))
	}
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := Round{Owner: "acme", Repo: "example", Number: 613, HeadSHA: "41435f1", Findings: []payload.Finding{scan, shift, read},
		Commentable: map[string]map[int]bool{".github/workflows/scan.yml": {28: true}, "web/src/tooltip/middlewares.ts": {60: true, 70: true}}}
	rep := publishRound(t, gh, ledger, r)
	if len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 1 || gh.reviews[0].comments[0].Line != 70 {
		t.Fatalf("only the claim read from the pinned source posts: %+v", gh.reviews)
	}
	summary := gh.issueCreates[0]
	if strings.Contains(summary, "code scanning") || strings.Contains(summary, "slide tooltip") {
		t.Fatalf("held claims must not be listed either:\n%s", summary)
	}
	if len(rep.Held) != 2 || rep.Held[0].Reason != HoldLibraryClaim || rep.Held[1].Reason != HoldLibraryClaim {
		t.Fatalf("held = %+v", rep.Held)
	}
	if row := ledger.get(db.PublishedKindFinding, scan.ID); row != nil {
		t.Fatalf("a held finding gets no row: %+v", row)
	}
}

func TestPublish_LibraryClaimHoldHasAKillSwitch(t *testing.T) {
	scan := contractFinding("medium", ".github/workflows/scan.yml", 28, "The scanner action is not configured with `advanced-security: true`.",
		asserting("production_behavior", "Scan results are not uploaded.", "I am reading the action's documented behavior from memory.", "Visibility.", "Scan step may not upload findings"))
	gh, ledger := newFakeGitHub(), newFakeLedger()
	pol := testPolicy()
	pol.LibraryClaimHold = false
	rep := publishWith(t, gh, ledger, Round{Owner: "acme", Repo: "example", Number: 613, HeadSHA: "41435f1", Findings: []payload.Finding{scan},
		Commentable: map[string]map[int]bool{".github/workflows/scan.yml": {28: true}}}, pol)
	if len(gh.reviews) != 1 || len(rep.Held) != 0 {
		t.Fatalf("with the hold off the claim posts: reviews=%d held=%+v", len(gh.reviews), rep.Held)
	}
}

// A hold keeps the ledger row of an earlier post alive: absence because of
// the gate is never a fix, and a thread that is open on GitHub is listed
// and counted as still open.
func TestPublish_HeldFindingKeepsItsOpenRowAndIsNeverFixed(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	scan := contractFinding("medium", ".github/workflows/scan.yml", 28, "The scanner action is not configured with `advanced-security: true`.",
		asserting("production_behavior", "Scan results are not uploaded.", "I am reading the action's documented behavior from memory.", "Visibility.", "Scan step may not upload findings"))
	r1 := Round{Owner: "acme", Repo: "example", Number: 613, HeadSHA: "sha-1", Findings: []payload.Finding{scan},
		Commentable: map[string]map[int]bool{".github/workflows/scan.yml": {28: true}}}
	pol := testPolicy()
	pol.LibraryClaimHold = false
	publishWith(t, gh, ledger, r1, pol)
	if ledger.get(db.PublishedKindFinding, scan.ID) == nil {
		t.Fatal("round one posted the claim")
	}

	r2 := r1
	r2.HeadSHA, r2.Previous = "sha-2", nil
	r2.Changes = changed(".github/workflows/scan.yml")
	rep := publishRound(t, gh, ledger, r2)
	row := ledger.get(db.PublishedKindFinding, scan.ID)
	if row.State != db.PublishedStateOpen || row.LastSeenSHA != "sha-2" || rep.Fixed != 0 || len(gh.replies) != 0 {
		t.Fatalf("a held finding stays open and silent: row=%+v fixed=%d replies=%d", row, rep.Fixed, len(gh.replies))
	}
	if !strings.Contains(gh.issueEdits[501], "0 new · 1 still open · 0 fixed") || !strings.Contains(gh.issueEdits[501], "Scan step may not upload findings") {
		t.Fatalf("an open thread is still open, held or not:\n%s", gh.issueEdits[501])
	}
}

// Class 10: "dropped the branded precedence (`active_toggles.X and SITE`)"
// when that line is a context line of the very hunk the finding cites.
const smallLogoHunk = `@@ -13,17 +13,23 @@
         {% endif %}
         <a href="/" class="logo-link">
           {% if toggles.BrandLogo and SITE and SITE.logo %}
-          <img src="{{ SITE.logo }}" class="logo" alt="Homepage" />
-          {% elif promo_logo_img %}
-          <img src="{{ promo_logo_img }}" class="logo" alt="Homepage" />
+            <img src="{{ SITE.logo }}" class="logo" alt="Homepage" />
+          {% elif promo_logos %}
+            <img src="{{ promo_logos.light }}" class="logo light" alt="Homepage" />
           {% else %}
-          <img src="{{ STATIC_URL }}images/logo.svg" class="logo light" alt="Homepage" />
+            <img src="{{ STATIC_URL }}images/logo.svg" class="logo light" alt="Homepage" />
           {% endif %}
           {% if wide_layout %}
-          <img src="{{ STATIC_URL }}images/logo-small.svg" class="logoSmall light" alt="Homepage" />
+            {% if promo_logos == None %}
+                <img src="{{ STATIC_URL }}images/logo-small.svg" class="logoSmall light dark" alt="Homepage" />
+            {% endif %}
           {% endif %}
         </a>`

func regressionFinding(comment, impact string) payload.Finding {
	x := contractFinding("medium", "templates/header.html", 26, comment,
		asserting("production_behavior", impact,
			"Medium: a branded small variant may be intentionally unsupported, but the full-size logo honors SITE while this rewritten block does not.",
			"Shows the wrong brand logo for branded users on narrow viewports.", ""))
	x.DiffHunk = smallLogoHunk
	return x
}

const inventedImpact = "On branded sites at narrow widths the header small logo shows the default logo instead of the branded logo."

func TestPublish_HoldsARegressionClaimTheHunkContradicts(t *testing.T) {
	invented := regressionFinding("The small-logo block dropped the branded precedence that the full-size logo keeps (`toggles.BrandLogo and SITE and SITE.logo`). On branded sites at narrow widths, the full logo is hidden and this block renders; it now falls back to the default logo instead of the site's own branded logo.", inventedImpact)
	cited := regressionFinding("The promo branch dropped the `promo_logo_img` fallback that the old block rendered, so a campaign configured through the legacy tag no longer shows its logo.",
		"A campaign configured through the legacy promo tag shows no logo at all.")
	if !UncitedRegressionClaim(invented) || UncitedRegressionClaim(cited) {
		t.Fatalf("uncited: invented=%v cited=%v", UncitedRegressionClaim(invented), UncitedRegressionClaim(cited))
	}
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := Round{Owner: "acme", Repo: "example", Number: 32509, HeadSHA: "8afc20a", Findings: []payload.Finding{invented, cited},
		Commentable: map[string]map[int]bool{"templates/header.html": {26: true}}}
	rep := publishRound(t, gh, ledger, r)
	if len(gh.reviews) != 1 || len(gh.reviews[0].comments) != 1 || !strings.Contains(gh.reviews[0].comments[0].Body, "promo_logo_img") {
		t.Fatalf("only the cited claim posts: %+v", gh.reviews)
	}
	if len(rep.Held) != 1 || rep.Held[0].Reason != HoldRegressionClaim {
		t.Fatalf("held = %+v", rep.Held)
	}
	if strings.Contains(gh.issueCreates[0], "branded precedence") || strings.Contains(gh.issueCreates[0], "narrow widths") {
		t.Fatalf("the held claim must not headline the summary:\n%s", gh.issueCreates[0])
	}
}

func TestUncitedRegressionClaim_LeavesClaimsTheHunkCannotJudge(t *testing.T) {
	elsewhere := regressionFinding("The port drops the `FrontendConfig` that enabled the redirect, so plain HTTP will no longer be redirected.", "Plain HTTP is not redirected.")
	elsewhere.DiffHunk = "@@ -1,2 +1,3 @@\n a: 1\n+b: 2\n c: 3\n"
	current := regressionFinding("The extractor drops any env-reading call whose first argument is not a string literal (`getEnv(key)`).", "Keys passed as constants are not compared.")
	future := regressionFinding("A future `onClose` that no longer removes the notification immediately would leak the listener (`isExiting`).", "A listener may leak.")
	for name, x := range map[string]payload.Finding{"elsewhere": elsewhere, "current": current, "future": future} {
		if UncitedRegressionClaim(x) {
			t.Errorf("%s: not a hold", name)
		}
	}
}

func TestRenderHygiene_KillSwitchRestoresTheOldRendering(t *testing.T) {
	t.Setenv(RenderHygieneEnv, "false")
	x := contractFinding("medium", "a.scss", 190, "Comment.", asserting("production_behavior", "Impact.", "Low; same pattern as A-1 and directly verifiable.", "Rationale.", "Headline"))
	body := RenderInline(x, SourceTagBoth, "", "", "")
	if !strings.Contains(body, "Low; same pattern as A-1") || !strings.Contains(body, "Source: PRism · Both") {
		t.Fatalf("kill switch ignored:\n%s", body)
	}
}

func TestContractSentence_KeepsHyphenatedSeverityWordsAndRealIdentifiers(t *testing.T) {
	for _, s := range []string{
		"High-cardinality keys blow up the index.",
		"Low-level callers skip the check.",
		"Critical-path rendering waits on the lock.",
		"Medium-sized payloads are fine.",
		"The P-256 curve is hard-coded.",
		"The schema-A-1 table has no index.",
	} {
		if got := ContractSentence(s); got != s {
			t.Errorf("ContractSentence(%q) = %q", s, got)
		}
	}
	for s, want := range map[string]string{
		"Low - the nesting is visible.":    "The nesting is visible.",
		"Medium: same pattern as A-1.":     "",
		"High confidence, verified in CI.": "Verified in CI.",
	} {
		if got := ContractSentence(s); got != want {
			t.Errorf("ContractSentence(%q) = %q, want %q", s, got, want)
		}
	}
	if got := StripFindingIDAsides("The schema-A-1 table mirrors A-2."); got != "The schema-A-1 table mirrors another finding on this PR." {
		t.Errorf("StripFindingIDAsides = %q", got)
	}
}

func TestUnverifiedLibraryClaim_HoldsTheInstructedPhraseNotAPlainClaim(t *testing.T) {
	instructed := contractFinding("low", "web/src/tooltip/middlewares.ts", 70,
		"`shift` defaults to `mainAxis: true` and `crossAxis: false`, so the tooltip never slides horizontally.",
		asserting("production_behavior", "Tooltips overflow the viewport edge.", "Unverified against the pinned version; the default may differ.", "Clipped tooltips.", "Tooltip never slides horizontally"))
	plain := contractFinding("medium", "web/src/tooltip/middlewares.ts", 70,
		"`shift` defaults to `mainAxis: true` and `crossAxis: false`, so the tooltip never slides horizontally.",
		asserting("production_behavior", "Tooltips overflow the viewport edge.", "The wrapper may inject its own shift middleware.", "Clipped tooltips.", "Tooltip never slides horizontally"))
	cited := contractFinding("medium", "web/src/tooltip/middlewares.ts", 70,
		"The library is documented to shift along the main axis only (docs/tooltips.md:12), so the tooltip never slides horizontally.",
		asserting("production_behavior", "Tooltips overflow the viewport edge.", "The repo doc may lag the pinned release.", "Clipped tooltips.", "Tooltip never slides horizontally"))
	if !UnverifiedLibraryClaim(instructed) || UnverifiedLibraryClaim(plain) || UnverifiedLibraryClaim(cited) {
		t.Fatalf("instructed=%v plain=%v cited=%v", UnverifiedLibraryClaim(instructed), UnverifiedLibraryClaim(plain), UnverifiedLibraryClaim(cited))
	}
}

func TestFirstFindingSentence_ReadsPastTheFirstLine(t *testing.T) {
	comment := "This looks good.\n\nHowever, the lock is never released on the error path.\n\n```suggestion\ndefer mu.Unlock()\n```"
	got, ok := FirstFindingSentence(comment)
	if !ok || got != "The lock is never released on the error path." {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	x := f("p1", "medium", "a.go", 10, comment)
	if AllPraise(x) {
		t.Fatal("a defect on a later line is not praise")
	}
	body := RenderInline(x, SourceTagPrismOnly, "", "", "")
	if strings.Count(body, "never released on the error path") != 1 || strings.Contains(body, "This looks good") {
		t.Fatalf("the reporting sentence titles the comment once and the praise is gone:\n%s", body)
	}
}

func TestTitleSource_StripsALabelFromEverySource(t *testing.T) {
	withHeadline := contractFinding("medium", "a.go", 10, "The lock is never released.",
		asserting("production_behavior", "Low; requests hang after the first error.", "The retry path is untested.", "Hangs.", "Medium: lock never released"))
	if got := titleSource(withHeadline); got != "Lock never released" {
		t.Fatalf("headline title = %q", got)
	}
	withImpact := contractFinding("medium", "a.go", 10, "The lock is never released.",
		asserting("production_behavior", "Low; requests hang after the first error.", "The retry path is untested.", "Hangs.", ""))
	body := RenderInline(withImpact, SourceTagPrismOnly, "", "", "")
	if strings.Contains(body, "Low;") || strings.Count(body, "equests hang after the first error") != 1 {
		t.Fatalf("the impact sentence is the title once, without its label:\n%s", body)
	}
	fromComment := f("c1", "medium", "a.go", 10, "Medium: the lock is never released.")
	if got := titleSource(fromComment); got != "The lock is never released." {
		t.Fatalf("comment title = %q", got)
	}
}

func TestUnverifiedLibraryClaim_IgnoresTechnicalProse(t *testing.T) {
	prose := contractFinding("medium", "a.go", 10, "The value is read from memory after the guard, so I am certain the race is reachable.",
		asserting("production_behavior", "Stale reads after a resize.", "The guard ordering is visible in the code.", "Data race.", "Stale read after resize"))
	admitted := contractFinding("medium", "a.go", 10, "The action uploads nothing without `advanced-security: true`.",
		asserting("production_behavior", "Scan results are not uploaded.", "I am recalling the default from memory.", "Visibility.", "Scan step may not upload findings"))
	if UnverifiedLibraryClaim(prose) || !UnverifiedLibraryClaim(admitted) {
		t.Fatalf("prose=%v admitted=%v", UnverifiedLibraryClaim(prose), UnverifiedLibraryClaim(admitted))
	}
}

func TestTitleFromComment_AgreesWithTitleSourceOnALabelOnlyHeadline(t *testing.T) {
	x := contractFinding("low", "a.go", 10, "The helper re-reads the file on every call. Cache the parsed result.",
		&types.FindingContract{SchemaVersion: 1, FindingKind: "design_opinion", Materiality: "unknown", Falsifiability: "not_falsifiable",
			CurrentImpact: "Unknown.", Uncertainty: "The call count is not measured.", SeverityRationale: "Performance preference.",
			Headline: "Same pattern as A-1", Subjects: []types.FindingSubject{{Kind: "file", Path: "a.go"}}})
	if !titleFromComment(x) || titleSource(x) != "The helper re-reads the file on every call." {
		t.Fatalf("fromComment=%v title=%q", titleFromComment(x), titleSource(x))
	}
	body := RenderInline(x, SourceTagPrismOnly, "", "", "")
	if strings.Count(body, "re-reads the file on every call") != 1 {
		t.Fatalf("the first sentence is the title once:\n%s", body)
	}
}

func TestPublishLegacy_HeldAnnotationIsNotCountedFixed(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	scan := contractFinding("medium", ".github/workflows/scan.yml", 28, "The scanner action is not configured with `advanced-security: true`.",
		asserting("production_behavior", "Scan results are not uploaded.", "I am reading the action's documented behavior from memory.", "Visibility.", "Scan step may not upload findings"))
	pol := testPolicy()
	pol.LegacyLedger = true
	pol.LibraryClaimHold = false
	r1 := Round{Owner: "acme", Repo: "example", Number: 613, HeadSHA: "sha-1", Findings: []payload.Finding{scan}}
	publishWith(t, gh, ledger, r1, pol)
	if ledger.get(db.PublishedKindAnnotation, scan.ID) == nil {
		t.Fatal("round one listed the note")
	}
	pol.LibraryClaimHold = true
	r2 := r1
	r2.HeadSHA, r2.Previous = "sha-2", nil
	rep := publishWith(t, gh, ledger, r2, pol)
	if rep.Fixed != 0 || !strings.Contains(gh.issueEdits[501], "0 fixed") {
		t.Fatalf("a held note is not fixed: fixed=%d\n%s", rep.Fixed, gh.issueEdits[501])
	}
}
