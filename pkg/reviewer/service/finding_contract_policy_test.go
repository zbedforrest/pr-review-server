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
