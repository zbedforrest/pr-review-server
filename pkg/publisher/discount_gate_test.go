package publisher

import (
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// auditedFalsePositives are anonymised copies of the six confirmed false
// positives from the comment-system audit: every one claimed current impact
// with an experiment, then conditioned the claim on something unchecked in
// its uncertainty line. The last entry carries its second-round wording;
// the posted first-round line is auditedFalsePositiveAsPosted below.
var auditedFalsePositives = []struct {
	name, severity, kind, impact, uncertainty string
}{
	{"preconditions-only update may erase case steps", "critical", "production_behavior",
		"Running the script to fix only a case's prerequisites can wipe that case's steps, and a version write clears the results on its run instances.",
		"Medium: depends on whether the v2 PUT keeps steps when the events list is absent, which the tool's own full-replace comment suggests it does not."},
	{"viewers bucketed by socket peer when the header is absent", "critical", "production_behavior",
		"When the proxy header is absent, every viewer is bucketed by the socket peer address, so all viewers resolve to the same player version instead of the intended per-viewer cohort.",
		"This depends on the framework's `IP()` using `RemoteIP()` as a fallback, which the new `fromRemote` test assumes it does not."},
	{"approved sets marked ineligible by positional evaluation choice", "medium", "production_behavior",
		"Approved sets can be marked ineligible when the chosen evaluation's snapshot does not reflect the composition at completion.",
		"Whether the evaluation model defines a default ordering that makes the positional selection deterministic."},
	{"connect timeouts retried despite deliberate timeout exclusion", "critical", "production_behavior",
		"Connect timeouts are classified as transient and retried, adding up to two extra attempts and a short sleep per chunk despite the documented intent to exclude timeouts.",
		"I could not execute commands in this checkout to confirm the installed client version, and the hierarchy I know makes ConnectTimeout a NetworkError; if the pinned version differs, this is latent instead of current."},
	{"double activation of the delete confirmation sends two deletes", "medium", "production_behavior",
		"A rapid double activation of the delete confirmation can dispatch two DELETE requests and surface a spurious error toast for the second response.",
		"Whether the simple popup unmounts synchronously enough to make a double activation harmless is unverified from the diff."},
	{"live query fetches even when the broadcast is over", "medium", "production_behavior",
		"Selecting or remaining on the LIVE filter after the broadcast has ended issues a pointless threads request that the query function then throws away.",
		"Whether the LIVE tab remains mounted while the broadcaster is offline is unknown, so the wasted fetch may only occur for part of the offline window."},
}

// auditedFalsePositiveAsPosted is the last audited false positive with the
// uncertainty line that was actually posted. It is not hedged, so no lexical
// gate hides it; the test pins that limit.
var auditedFalsePositiveAsPosted = struct{ name, severity, kind, impact, uncertainty string }{
	"live query fetches even when the broadcast is over", "medium", "production_behavior",
	"Selecting or remaining on the LIVE filter after the broadcast has ended issues a pointless threads request that the query function then throws away.",
	"Fairly confident; the skip expression makes availability irrelevant whenever the live filter is selected.",
}

func auditedFinding(id string, c struct{ name, severity, kind, impact, uncertainty string }) payload.Finding {
	x := fp(id, c.severity, "a.go", 10, c.name+".", "agent")
	x.FindingContract = falsifiableTestContract(c.kind, "current_impact", c.impact, c.uncertainty)
	x.FindingContract.Headline = c.name
	x.FindingContract.SeverityRationale = "Silent data loss in a paid flow."
	x.FindingContract.Subjects = []types.FindingSubject{{Kind: "file", Path: "a.go"}}
	x.FindingContractStatus = "valid"
	return x
}

func TestShown_AuditedFalsePositivesFailTheConditionGate(t *testing.T) {
	t.Setenv("PUBLISH_CONDITION_HEDGE_GATE", "true")
	for i, c := range auditedFalsePositives {
		x := auditedFinding("fp", c)
		if err := types.ValidateFindingContract(x.FindingContract); err != nil {
			t.Fatalf("%d %s: fixture contract invalid: %v", i, c.name, err)
		}
		if Shown(x) {
			t.Errorf("%d %s (%s): shown, want hidden", i, c.name, c.severity)
		}
		sel := Select([]payload.Finding{x}, nil, map[string]map[int]bool{"a.go": {10: true}}, DefaultPolicy())
		if len(sel.Inline) != 0 || len(sel.Annotations) != 0 {
			t.Errorf("%d %s: selected inline=%d annotations=%d, want nothing", i, c.name, len(sel.Inline), len(sel.Annotations))
		}
	}
}

func TestShown_AuditedFalsePositivesPassTheDefaultGate(t *testing.T) {
	for i, c := range auditedFalsePositives {
		if !Shown(auditedFinding("fp", c)) {
			t.Errorf("%d %s: the default gate has no rule that hides a condition hedge", i, c.name)
		}
	}
}

func TestShown_AuditedFalsePositiveAsPostedIsShownEvenWithTheConditionGate(t *testing.T) {
	t.Setenv("PUBLISH_CONDITION_HEDGE_GATE", "true")
	if !Shown(auditedFinding("fp", auditedFalsePositiveAsPosted)) {
		t.Fatal("an unhedged false positive is beyond a lexical gate; want shown")
	}
}

func TestShown_AuditedFalsePositivesWereShownBeforeTheGate(t *testing.T) {
	t.Setenv("PUBLISH_CONDITION_HEDGE_GATE", "true")
	t.Setenv("PUBLISH_DISCOUNT_GATE", "false")
	for i, c := range auditedFalsePositives {
		if !Shown(auditedFinding("fp", c)) {
			t.Errorf("%d %s: the gate-off path should still show it", i, c.name)
		}
	}
}

func TestShown_CriticalNeedsTheSameContractAsAMedium(t *testing.T) {
	commentable := map[string]map[int]bool{"a.go": {1: true, 2: true, 3: true, 4: true}}
	asserted := func(id, sev string, line int) payload.Finding {
		x := fp(id, sev, "a.go", line, "Breaks now.", "agent")
		x.FindingContract = falsifiableTestContract("production_behavior", "current_impact", "Every checkout returns a 500.", "Confident; reached on every request.")
		x.FindingContractStatus = "valid"
		return x
	}
	hedgedCritical := asserted("hedged", "critical", 2)
	hedgedCritical.FindingContract.Uncertainty = "May be deliberate: the legacy shape makes this path unreachable."
	unfalsifiableCritical := asserted("unfalsifiable", "critical", 3)
	unfalsifiableCritical.FindingContract.Falsifiability = "unknown"
	unfalsifiableCritical.FindingContract.FalsifiableCondition, unfalsifiableCritical.FindingContract.ExpectedObservable = nil, nil
	nilImpactCritical := asserted("nil", "critical", 4)
	nilImpactCritical.FindingContract.CurrentImpact = "None today; the flag is off."

	sel := Select([]payload.Finding{asserted("ok", "critical", 1), hedgedCritical, unfalsifiableCritical, nilImpactCritical}, nil, commentable, DefaultPolicy())
	if got := ids(sel.Inline); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("inline = %v, want [ok]", got)
	}
	if len(sel.Annotations) != 0 {
		t.Fatalf("discounted criticals must not reach the summary either: %v", ids(sel.Annotations))
	}
}
