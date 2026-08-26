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
