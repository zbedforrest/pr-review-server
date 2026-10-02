package types

import "testing"

func falsifiableContract(impact, uncertainty string) *FindingContract {
	condition := "Run the request twice."
	observable := "The second request fails."
	return &FindingContract{
		SchemaVersion: FindingContractSchemaVersion, FindingKind: "production_behavior", Materiality: "current_impact",
		CurrentImpact: impact, Falsifiability: "falsifiable", FalsifiableCondition: &condition, ExpectedObservable: &observable,
		Subjects: []FindingSubject{{Kind: "file", Path: "a.go"}}, Uncertainty: uncertainty, SeverityRationale: "Breaks a paid flow.",
	}
}

func TestSelfDiscount_AuditedHedgesAreDiscounted(t *testing.T) {
	cases := map[string]string{
		"confidence label then condition": "Medium: depends on whether the v2 PUT keeps steps when the events list is absent, which the tool's own full-replace comment suggests it does not.",
		"depends on an unread library":    "This depends on the framework's `IP()` using `RemoteIP()` as a fallback, which the new `fromRemote` test assumes it does not.",
		"opens with whether":              "Whether the evaluation model defines a default ordering that makes the positional selection deterministic.",
		"uncertain whether":               "Uncertain whether set tasks ever accumulate more than one level-1 evaluation, since creation sets the needed count to 1.",
		"could not run commands":          "I could not execute commands in this checkout to confirm the installed client version, and the hierarchy I know makes ConnectTimeout a NetworkError.",
		"unverified from the diff":        "Whether the simple popup unmounts synchronously enough to make a double activation harmless is unverified from the diff.",
		"unknown mount state":             "Whether the LIVE tab remains mounted while the broadcaster is offline is unknown, so the wasted fetch may only occur for part of the offline window.",
		"could not read the slice":        "I could not read the slice implementation (diff truncated and shell unavailable), so the replace-vs-merge behavior is inferred from the passing test.",
		"intent guess":                    "May be deliberate: the handler drops the header on purpose for internal callers.",
		"by design":                       "By design if the backend never emits an empty list.",
		"conditional on the backend":      "If the backend emits the legacy shape this path is unreachable.",
		"nil impact in uncertainty":       "None today; the flag is off in every environment.",
	}
	for name, uncertainty := range cases {
		if got := SelfDiscount(falsifiableContract("Users lose their steps.", uncertainty)); got != DiscountHedged && got != DiscountNilImpact {
			t.Errorf("%s: SelfDiscount = %q, want a discount", name, got)
		}
	}
}

func TestSelfDiscount_NilImpactOpeningInCurrentImpact(t *testing.T) {
	for _, impact := range []string{
		"None today; the setting is unused.",
		"No user impact: the branch is dead code.",
		"No current caller passes an empty slice.",
		"Likely intentional, the default was chosen for internal callers.",
	} {
		if got := SelfDiscount(falsifiableContract(impact, "Confident.")); got == DiscountNone {
			t.Errorf("%q: expected a discount", impact)
		}
	}
}

func TestSelfDiscount_UnfalsifiableIsDiscounted(t *testing.T) {
	c := falsifiableContract("Users see a 500.", "Confident.")
	c.Falsifiability, c.FalsifiableCondition, c.ExpectedObservable = "unknown", nil, nil
	if got := SelfDiscount(c); got != DiscountUnfalsified {
		t.Fatalf("SelfDiscount = %q, want %q", got, DiscountUnfalsified)
	}
}

func TestSelfDiscount_AssertedFindingsAreNotDiscounted(t *testing.T) {
	for name, uncertainty := range map[string]string{
		"confident":                     "Confident; the branch is reached on every request.",
		"fairly confident":              "Fairly confident; the skip expression makes availability irrelevant whenever the live filter is selected.",
		"low residual with later hedge": "Low: the only open question is whether retries mask it, which does not change the first failure.",
		"intentionally mid-sentence":    "The guard was intentionally removed in the previous commit, so the crash is reachable today.",
		"empty":                         "",
	} {
		if got := SelfDiscount(falsifiableContract("Every checkout request returns a 500.", uncertainty)); got != DiscountNone {
			t.Errorf("%s: SelfDiscount = %q, want none", name, got)
		}
	}
	if got := SelfDiscount(nil); got != DiscountNone {
		t.Fatalf("nil contract discount = %q", got)
	}
}

func TestDiscountGateEnabled_DefaultsOnAndFalseDisables(t *testing.T) {
	t.Setenv("PUBLISH_DISCOUNT_GATE", "")
	if !DiscountGateEnabled() {
		t.Fatal("gate should default on")
	}
	t.Setenv("PUBLISH_DISCOUNT_GATE", "false")
	if DiscountGateEnabled() {
		t.Fatal("PUBLISH_DISCOUNT_GATE=false should disable the gate")
	}
}
