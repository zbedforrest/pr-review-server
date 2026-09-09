package publisher

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func unverified(id, sev, file string, line int, comment string) payload.Finding {
	x := fp(id, sev, file, line, comment, "first-pass")
	x.FindingContract, x.FindingContractStatus = nil, ""
	x.State = "unverified"
	return x
}

func inactive(id, sev, file string, line int, comment, state string) payload.Finding {
	x := unverified(id, sev, file, line, comment)
	x.State, x.Active = state, false
	return x
}

func TestPublishable_InactiveIsNeverPublished(t *testing.T) {
	for _, prov := range []string{"agent", "required-check", "carried", ""} {
		x := fp("x", "critical", "a.go", 1, "c", prov)
		x.Active = false
		if Publishable(x) {
			t.Errorf("Publishable(provenance=%q, inactive) = true, want false", prov)
		}
	}
	if !Publishable(fp("x", "critical", "a.go", 1, "c", "agent")) {
		t.Fatal("an active agent finding stays publishable")
	}
}

func disputed(id, sev, file string, line int, comment, reason string) payload.Finding {
	x := unverified(id, sev, file, line, comment)
	x.Assessment = &types.Disposition{SourceID: "FP-1", State: "rejected", Reason: reason}
	return x
}

func TestUnverifiedNote_ActiveUnverifiedClaimsOnly(t *testing.T) {
	carried := unverified("b", "medium", "a.go", 1, "Claim.")
	carried.Provenance = "carried"
	narrative := f("h", "unknown", "SUMMARY", 0, "n")
	narrative.State = "unverified"
	cases := map[string]struct {
		f    payload.Finding
		want bool
	}{
		"first-pass unverified":      {unverified("a", "critical", "a.go", 1, "Claim."), true},
		"carried unverified":         {carried, true},
		"disputed":                   {disputed("c", "critical", "a.go", 1, "Claim.", "Not so."), true},
		"confirmed agent finding":    {fp("d", "medium", "a.go", 1, "c", "agent"), false},
		"inactive unverified record": {inactive("e", "low", "a.go", 1, "c", "unverified"), false},
		"rejected":                   {inactive("f", "medium", "a.go", 1, "c", "rejected"), false},
		"merged":                     {inactive("g", "medium", "a.go", 1, "c", "merged"), false},
		"narrative":                  {narrative, false},
		"raw bug-memory alert":       {unverified("i", "medium", "a.go", 0, "**Bug-memory alert: past failure pattern.** Text."), false},
	}
	for name, tc := range cases {
		if got := UnverifiedNote(tc.f); got != tc.want {
			t.Errorf("%s: UnverifiedNote = %v, want %v", name, got, tc.want)
		}
	}
}

func unverifiedRound() Round {
	return Round{Owner: "acme", Repo: "example", Number: 7, HeadSHA: "abc1234567", RoundNumber: 1, ShowUnverified: true,
		Findings: []payload.Finding{
			f("sum", "unknown", "SUMMARY", 0, "Verdict: approve with suggestions."),
			f("c1", "critical", "a.go", 10, "Confirmed critical."),
			unverified("u1", "critical", "pkg/db/conn.go", 12, "Connection is never closed on the error path."),
			disputed("d1", "critical", "pkg/auth/token.go", 40, "Token is logged in plaintext. The debug line prints the bearer token.",
				"The logger redacts the Authorization header before writing."),
			inactive("r1", "medium", "pkg/api/handler.go", 8, "Missing error check on Decode.", "rejected"),
			inactive("m1", "medium", "a.go", 10, "Restated critical from the first pass.", "merged"),
			inactive("n1", "low", "pkg/util/str.go", 3, "Unexamined low claim.", "unverified"),
		},
		Commentable: map[string]map[int]bool{"a.go": {10: true}, "pkg/db/conn.go": {12: true}, "pkg/auth/token.go": {40: true}},
	}
}

func TestRenderSummary_FoldsUnverifiedFirstPassNotes(t *testing.T) {
	r := unverifiedRound()
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))

	foldIdx := strings.Index(out, "<details><summary>2 unverified notes</summary>")
	if foldIdx < 0 {
		t.Fatalf("summary must fold the unverified notes:\n%s", out)
	}
	if strings.Contains(out[:foldIdx], "Connection is never closed") || strings.Contains(out[:foldIdx], "Token is logged") {
		t.Fatalf("unverified claims must not be top-level bullets:\n%s", out)
	}
	fold := out[foldIdx:]
	for _, want := range []string{
		"- **[CRITICAL]** **FIRST PASS · UNVERIFIED** Connection is never closed on the error path",
		"[`conn.go:12`](https://github.com/acme/example/blob/abc1234567/pkg/db/conn.go#L12)",
		"- **[CRITICAL]** **FIRST PASS · DISPUTED** Token is logged in plaintext",
		"\n  - Agent: The logger redacts the Authorization header before writing.\n",
	} {
		if !strings.Contains(fold, want) {
			t.Errorf("unverified fold missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Unexamined low claim") {
		t.Errorf("inactive unverified records stay off GitHub:\n%s", out)
	}
	if !strings.Contains(out, "merge confidence 3/5") {
		t.Errorf("unverified claims must not move the confidence score (one confirmed critical = 3):\n%s", out)
	}
}

func TestRenderSummary_UnverifiedFoldFollowsLowerSeverityNotes(t *testing.T) {
	r := unverifiedRound()
	r.Findings = append(r.Findings, withContract(f("l1", "low", "b.go", 9, "Nit."), "test_quality", "no_user_impact", "No user impact.", ""))
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	lower, unv := strings.Index(out, "1 lower-severity note</summary>"), strings.Index(out, "2 unverified notes</summary>")
	if lower < 0 || unv < 0 || unv < lower {
		t.Fatalf("unverified fold must follow the lower-severity fold:\n%s", out)
	}
}

func TestRenderSummary_UnverifiedFoldCappedLikeLowerSeverityNotes(t *testing.T) {
	r := unverifiedRound()
	r.DashboardURL = "https://prism.example/api/review/acme/example/7?format=html"
	r.Findings = r.Findings[:2]
	for i := 0; i < maxFoldedNotes+3; i++ {
		r.Findings = append(r.Findings, unverified(fmt.Sprintf("u%d", i), "medium", fmt.Sprintf("f%d.go", i), 1, fmt.Sprintf("Claim %d.", i)))
	}
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if !strings.Contains(out, "- ... 3 more on the [dashboard](https://prism.example/api/review/acme/example/7?format=html)") {
		t.Fatalf("unverified fold must cap at %d with a dashboard link:\n%s", maxFoldedNotes, out)
	}
	if strings.Count(out, "**FIRST PASS · UNVERIFIED**") != maxFoldedNotes {
		t.Fatalf("want %d unverified bullets:\n%s", maxFoldedNotes, out)
	}
}

func TestRenderSummary_UnverifiedFoldAbsentWhenDisabled(t *testing.T) {
	r := unverifiedRound()
	r.ShowUnverified = false
	out := RenderSummary(r, Select(r.Findings, nil, r.Commentable, DefaultPolicy()))
	if strings.Contains(out, "unverified first-pass") || strings.Contains(out, "Connection is never closed") || strings.Contains(out, "Token is logged") {
		t.Fatalf("unverified notes must be absent when the policy is off:\n%s", out)
	}
}

func TestPublish_PolicyShowUnverifiedControlsTheFold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
		want   bool
	}{
		{"default on", DefaultPolicy(), true},
		{"explicitly off", Policy{InlineCap: DefaultInlineCap, InlineMinSeverity: DefaultInlineMinSeverity}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, ledger := newFakeGitHub(), newFakeLedger()
			r := unverifiedRound()
			r.ShowUnverified = !tc.want
			p := &Publisher{GH: gh, Ledger: ledger, Policy: tc.policy}
			if _, err := p.Publish(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(gh.issueCreates[0], "unverified notes"); got != tc.want {
				t.Fatalf("fold present = %v, want %v:\n%s", got, tc.want, gh.issueCreates[0])
			}
		})
	}
}

func TestPolicy_ShowUnverifiedDefaultsOnAndIsLeftAsGiven(t *testing.T) {
	if !DefaultPolicy().ShowUnverified {
		t.Fatal("DefaultPolicy must show unverified notes")
	}
	if (Policy{InlineCap: -1}).withDefaults().ShowUnverified {
		t.Fatal("withDefaults must not turn ShowUnverified on")
	}
	if !(Policy{ShowUnverified: true}).withDefaults().ShowUnverified {
		t.Fatal("withDefaults must keep ShowUnverified on")
	}
}

func TestSelect_UnverifiedClaimsAreNeverInlineOrAnnotations(t *testing.T) {
	r := unverifiedRound()
	sel := Select(r.Findings, nil, r.Commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("inline = %v, want [c1]", got)
	}
	if len(sel.Annotations) != 0 {
		t.Fatalf("annotations = %v, want none", ids(sel.Annotations))
	}
}

func TestPublish_RejectedAndMergedRecordsNeverReachGitHub(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r := unverifiedRound()
	publishRound(t, gh, ledger, r)

	var posted []string
	posted = append(posted, gh.issueCreates...)
	for _, rv := range gh.reviews {
		for _, c := range rv.comments {
			posted = append(posted, c.Body)
		}
	}
	if len(posted) < 2 {
		t.Fatalf("expected a summary and an inline comment, got %d bodies", len(posted))
	}
	for _, body := range posted {
		for _, banned := range []string{"Missing error check on Decode", "Restated critical", "Unexamined low claim", "handler.go", "str.go"} {
			if strings.Contains(body, banned) {
				t.Errorf("inactive record %q leaked to GitHub:\n%s", banned, body)
			}
		}
	}
	for _, id := range []string{"r1", "m1", "n1", "u1", "d1"} {
		if _, ok := ledger.rows[id]; ok {
			t.Errorf("record %s must not be written to the ledger", id)
		}
	}
}

func TestPublishable_UnverifiedClaimsGoOnlyToTheUnverifiedFold(t *testing.T) {
	carried := fp("c", "medium", "a.go", 4, "carried from last round", "carried")
	carried.State, carried.Active = "unverified", true
	if Publishable(carried) {
		t.Fatal("an unverified claim (carried or first-pass) must not be a top-level or lower-severity bullet")
	}
	if !UnverifiedNote(carried) {
		t.Fatal("it belongs in the unverified fold instead")
	}
}

func TestPublish_ClaimMovedToTheUnverifiedFoldIsNeitherFixedNorResolved(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	publishRound(t, gh, ledger, roundOne())

	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 2
	for i := range r2.Findings {
		if r2.Findings[i].ID == "c1" {
			r2.Findings[i].Provenance, r2.Findings[i].State = "carried", "unverified"
		}
	}
	rep := publishRound(t, gh, ledger, r2)
	if rep.Fixed != 0 {
		t.Fatalf("a claim carried into the unverified fold is still held by the review, not fixed: %+v", rep)
	}
	if c1 := ledger.get(db.PublishedKindFinding, "c1"); c1 == nil || c1.State != db.PublishedStateOpen || c1.LastSeenSHA != "sha-2" {
		t.Fatalf("the ledger row must stay open and be refreshed: %+v", c1)
	}
}

func TestUnverifiedBullet_MarksCarriedClaimsAsCarriedAndBoundsTheReason(t *testing.T) {
	carried := fp("c", "medium", "a.go", 4, "carried claim", "carried")
	carried.State, carried.Active = "unverified", true
	disputed := fp("d", "medium", "b.go", 9, "disputed claim", "first-pass")
	disputed.State, disputed.Active = "unverified", true
	disputed.Assessment = &types.Disposition{State: "rejected", Reason: strings.Repeat("long reason ", 60)}
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1, ShowUnverified: true,
		Findings: []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "n"), carried, disputed}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if !strings.Contains(out, "CARRIED · UNVERIFIED") || strings.Contains(out, "FIRST PASS · UNVERIFIED** carried") {
		t.Errorf("carried claims must not be attributed to the first pass:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Agent:") && len(line) > 320 {
			t.Errorf("the disputed reason must be bounded: %d chars", len(line))
		}
	}
}

func TestRoundDiff_NewCountsOnlyWhatTheReaderCanSee(t *testing.T) {
	hidden := fp("u", "medium", "a.go", 4, "unverified claim", "first-pass")
	hidden.State, hidden.Active = "unverified", true
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 2, ShowUnverified: false,
		Findings: []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "n"), hidden}}
	if d := r.diff(); d.New != 0 {
		t.Fatalf("a claim the comment does not show cannot be announced as new: %+v", d)
	}
	r.ShowUnverified = true
	if d := r.diff(); d.New != 1 {
		t.Fatalf("once shown it counts: %+v", d)
	}
}

func TestRenderSummary_UnverifiedFoldLabelCoversCarriedClaims(t *testing.T) {
	carried := fp("c", "medium", "a.go", 4, "carried claim", "carried")
	carried.State, carried.Active = "unverified", true
	r := Round{Owner: "acme", Repo: "example", Number: 1, HeadSHA: "abc1234", RoundNumber: 1, ShowUnverified: true,
		Findings: []payload.Finding{f("sum", "unknown", "SUMMARY", 0, "n"), carried}}
	out := RenderSummary(r, Select(r.Findings, nil, nil, DefaultPolicy()))
	if !strings.Contains(out, "<summary>1 unverified note</summary>") || strings.Contains(out, "first-pass note") {
		t.Errorf("fold label must not call a carried claim first-pass:\n%s", out)
	}
}

func TestPublish_ConfirmedLowInTheFoldStaysPresent(t *testing.T) {
	gh, ledger := newFakeGitHub(), newFakeLedger()
	r1 := roundOne()
	publishRound(t, gh, ledger, r1)
	r2 := roundOne()
	r2.HeadSHA, r2.RoundNumber = "sha-2", 2
	for i := range r2.Findings {
		if r2.Findings[i].ID == "m1" {
			// The contract no longer clears the inline bar; the finding is still
			// confirmed and rendered in the lower-severity fold.
			r2.Findings[i].FindingContract.Materiality = "unknown"
		}
	}
	rep := publishRound(t, gh, ledger, r2)
	if rep.Fixed != 0 {
		t.Fatalf("a confirmed finding that moved to the lower-severity fold is not fixed: %+v", rep)
	}
	if m1 := ledger.get(db.PublishedKindFinding, "m1"); m1 == nil || m1.State != db.PublishedStateOpen {
		t.Fatalf("its ledger row must stay open: %+v", m1)
	}
}
