package service

import (
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestEnforceFindingContractPolicyCapsFutureOnlyFindingsAfterMerge(t *testing.T) {
	trigger := "A later caller omits the setting."
	condition := "The setting is absent."
	observable := "The request returns an error."
	contract := &types.FindingContract{
		SchemaVersion:         types.FindingContractSchemaVersion,
		FindingKind:           "latent_hazard",
		Materiality:           "future_condition_only",
		CurrentImpact:         "No current caller omits the setting.",
		CounterfactualTrigger: &trigger,
		Falsifiability:        "falsifiable",
		FalsifiableCondition:  &condition,
		ExpectedObservable:    &observable,
		Subjects: []types.FindingSubject{{
			Kind: "config_key",
			Path: "config.go",
			Name: "required_setting",
		}},
		Uncertainty:       "Future callers are not known.",
		SeverityRationale: "The current change creates no active failure.",
	}
	agent := FindingSet{Provenance: "agent", Comments: []types.LineComment{{
		FilePath:        "config.go",
		LineNumber:      12,
		CommentBody:     "A later caller could omit the setting.",
		Importance:      "LOW",
		FindingContract: contract,
	}}}
	firstPass := FindingSet{Provenance: "first-pass", Comments: []types.LineComment{{
		FilePath:    "config.go",
		LineNumber:  12,
		CommentBody: "The missing setting would return an error.",
		Importance:  "CRITICAL",
	}}}

	comments := MergeFindings(agent, firstPass)
	if comments[0].Importance != "MEDIUM" {
		t.Fatalf("merged importance = %q", comments[0].Importance)
	}
	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "LOW" {
		t.Fatalf("policy importance = %q", comments[0].Importance)
	}
}

func TestEnforceFindingContractPolicyDoesNotTrustInvalidContracts(t *testing.T) {
	comments := []types.LineComment{{
		FilePath:    "config.go",
		LineNumber:  12,
		CommentBody: "A current failure.",
		Importance:  "CRITICAL",
		FindingContract: &types.FindingContract{
			SchemaVersion: types.FindingContractSchemaVersion,
			FindingKind:   "latent_hazard",
			Materiality:   "future_condition_only",
		},
	}}

	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "CRITICAL" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
}

func TestEnforceFindingContractPolicyCapsNonDefectClasses(t *testing.T) {
	for _, contract := range []*types.FindingContract{
		validPolicyContract("production_behavior", "no_user_impact", "falsifiable"),
		validPolicyContract("design_opinion", "unknown", "not_falsifiable"),
		validPolicyContract("description_drift", "no_user_impact", "not_falsifiable"),
		validPolicyContract("test_quality", "unknown", "falsifiable"),
	} {
		comments := []types.LineComment{{
			FilePath:        "example.go",
			Importance:      "CRITICAL",
			FindingContract: contract,
		}}
		EnforceFindingContractPolicy(comments)
		if comments[0].Importance != "LOW" {
			t.Fatalf("%s/%s importance = %q", contract.FindingKind, contract.Materiality, comments[0].Importance)
		}
	}
}

func TestEnforceFindingContractPolicyCapsUnknownMaterialityAtMedium(t *testing.T) {
	contract := validPolicyContract("production_behavior", "unknown", "falsifiable")
	contract.CurrentImpact = "Checkout returns a 500 for carts with a removed item."
	comments := []types.LineComment{{
		FilePath:        "example.go",
		Importance:      "CRITICAL",
		FindingContract: contract,
	}}

	EnforceFindingContractPolicy(comments)

	if comments[0].Importance != "MEDIUM" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
}

func TestEnforceFindingContractPolicyNormalizesContractsFromAnyProducer(t *testing.T) {
	contract := validPolicyContract(" design_opinion ", " unknown ", " not_falsifiable ")
	contract.Subjects[0].Kind = " file "
	comments := []types.LineComment{{
		FilePath:        "example.go",
		Importance:      "CRITICAL",
		FindingContract: contract,
	}}

	EnforceFindingContractPolicy(comments)

	if comments[0].Importance != "LOW" {
		t.Fatalf("importance = %q", comments[0].Importance)
	}
	if contract.FindingKind != "design_opinion" || contract.Materiality != "unknown" || contract.Falsifiability != "not_falsifiable" || contract.Subjects[0].Kind != "file" {
		t.Fatalf("contract was not normalized: %#v", contract)
	}
}

func validPolicyContract(kind, materiality, falsifiability string) *types.FindingContract {
	condition := "The candidate fails."
	observable := "Compare the response status."
	contract := &types.FindingContract{
		SchemaVersion:     types.FindingContractSchemaVersion,
		FindingKind:       kind,
		Materiality:       materiality,
		CurrentImpact:     "No current user impact is demonstrated.",
		Falsifiability:    falsifiability,
		Subjects:          []types.FindingSubject{{Kind: "file", Path: "example.go"}},
		Uncertainty:       "The report covers the changed file.",
		SeverityRationale: "The observation does not establish a current defect.",
	}
	if falsifiability == "falsifiable" {
		contract.FalsifiableCondition = &condition
		contract.ExpectedObservable = &observable
	}
	return contract
}

func TestEnforceFindingContractPolicyDemotesSelfDiscountedFindings(t *testing.T) {
	t.Setenv("PUBLISH_CONDITION_HEDGE_GATE", "true")
	for name, uncertainty := range map[string]string{
		"condition":  "Medium: depends on whether the PUT keeps steps when the events list is absent.",
		"intent":     "May be deliberate; the default was chosen for internal callers.",
		"nil impact": "None today; the flag is off everywhere.",
		"unread":     "I could not execute commands in this checkout to confirm the installed client version.",
	} {
		contract := validPolicyContract("production_behavior", "current_impact", "falsifiable")
		contract.CurrentImpact = "Running the script to fix only a case's prerequisites wipes that case's steps."
		contract.Uncertainty = uncertainty
		comments := []types.LineComment{{FilePath: "example.go", Importance: "CRITICAL", FindingContract: contract}}
		EnforceFindingContractPolicy(comments)
		if comments[0].Importance != "LOW" || contract.Materiality != "unknown" {
			t.Errorf("%s: importance = %q materiality = %q, want LOW/unknown", name, comments[0].Importance, contract.Materiality)
		}
		if err := types.ValidateFindingContract(contract); err != nil {
			t.Errorf("%s: demoted contract is no longer valid: %v", name, err)
		}
	}
}

func TestEnforceFindingContractPolicyDemotesUnfalsifiableCurrentImpact(t *testing.T) {
	contract := validPolicyContract("production_behavior", "current_impact", "unknown")
	contract.CurrentImpact = "Every checkout request returns a 500."
	contract.Uncertainty = "Confident."
	comments := []types.LineComment{{FilePath: "example.go", Importance: "CRITICAL", FindingContract: contract}}
	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "LOW" || contract.Materiality != "unknown" {
		t.Fatalf("importance = %q materiality = %q, want LOW/unknown", comments[0].Importance, contract.Materiality)
	}
}

func TestEnforceFindingContractPolicyKeepsAssertedCurrentImpact(t *testing.T) {
	contract := validPolicyContract("production_behavior", "current_impact", "falsifiable")
	contract.CurrentImpact = "Every checkout request returns a 500."
	contract.Uncertainty = "Confident; the branch is reached on every request."
	comments := []types.LineComment{{FilePath: "example.go", Importance: "CRITICAL", FindingContract: contract}}
	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "CRITICAL" || contract.Materiality != "current_impact" {
		t.Fatalf("importance = %q materiality = %q, want CRITICAL/current_impact", comments[0].Importance, contract.Materiality)
	}
}

func TestEnforceFindingContractPolicyConditionGateOffKeepsConditionHedges(t *testing.T) {
	contract := validPolicyContract("production_behavior", "current_impact", "falsifiable")
	contract.CurrentImpact = "Users lose their steps."
	contract.Uncertainty = "Medium: depends on whether the PUT keeps steps."
	comments := []types.LineComment{{FilePath: "example.go", Importance: "CRITICAL", FindingContract: contract}}
	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "CRITICAL" || contract.Materiality != "current_impact" {
		t.Fatalf("importance = %q materiality = %q, want CRITICAL/current_impact with the condition gate off", comments[0].Importance, contract.Materiality)
	}
}

func TestEnforceFindingContractPolicyDiscountGateOffKeepsOldBehaviour(t *testing.T) {
	t.Setenv("PUBLISH_DISCOUNT_GATE", "false")
	t.Setenv("PUBLISH_CONDITION_HEDGE_GATE", "true")
	contract := validPolicyContract("production_behavior", "current_impact", "falsifiable")
	contract.CurrentImpact = "Users lose their steps."
	contract.Uncertainty = "Medium: depends on whether the PUT keeps steps."
	comments := []types.LineComment{{FilePath: "example.go", Importance: "CRITICAL", FindingContract: contract}}
	EnforceFindingContractPolicy(comments)
	if comments[0].Importance != "CRITICAL" || contract.Materiality != "current_impact" {
		t.Fatalf("importance = %q materiality = %q, want CRITICAL/current_impact with the gate off", comments[0].Importance, contract.Materiality)
	}
}
