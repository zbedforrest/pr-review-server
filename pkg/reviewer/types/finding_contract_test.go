package types

import "testing"

func contractText(value string) *string {
	return &value
}

func validFindingContract() *FindingContract {
	return &FindingContract{
		SchemaVersion:        FindingContractSchemaVersion,
		FindingKind:          "production_behavior",
		Materiality:          "current_impact",
		CurrentImpact:        "Affected requests return an error.",
		Falsifiability:       "falsifiable",
		FalsifiableCondition: contractText("The candidate returns an error while the control succeeds."),
		ExpectedObservable:   contractText("Compare the exact response status in both arms."),
		Subjects: []FindingSubject{{
			Kind: "symbol",
			Path: "internal/handler.go",
			Name: "HandleRequest",
		}},
		Uncertainty:       "The report covers one authenticated request state.",
		SeverityRationale: "The changed path blocks affected requests.",
	}
}

func TestValidateFindingContract(t *testing.T) {
	if err := ValidateFindingContract(validFindingContract()); err != nil {
		t.Fatal(err)
	}
	if got := ContractStatus(validFindingContract()); got != "valid" {
		t.Fatalf("ContractStatus() = %q", got)
	}
}

func TestValidateFindingContractRejectsContradictions(t *testing.T) {
	tests := map[string]func(*FindingContract){
		"latent current impact": func(value *FindingContract) {
			value.FindingKind = "latent_hazard"
		},
		"future trigger missing": func(value *FindingContract) {
			value.FindingKind = "latent_hazard"
			value.Materiality = "future_condition_only"
		},
		"future production behavior": func(value *FindingContract) {
			value.Materiality = "future_condition_only"
			value.CounterfactualTrigger = contractText("A later caller omits the setting.")
		},
		"design experiment": func(value *FindingContract) {
			value.FindingKind = "design_opinion"
			value.Materiality = "no_user_impact"
		},
		"test quality current impact": func(value *FindingContract) {
			value.FindingKind = "test_quality"
		},
		"non-falsifiable observable": func(value *FindingContract) {
			value.Falsifiability = "not_falsifiable"
		},
		"subject missing name": func(value *FindingContract) {
			value.Subjects[0].Name = ""
		},
		"multiline impact": func(value *FindingContract) {
			value.CurrentImpact = "Line one.\nLine two."
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			value := validFindingContract()
			change(value)
			if err := ValidateFindingContract(value); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateFindingContractAcceptsFutureOnlyHazard(t *testing.T) {
	value := validFindingContract()
	value.FindingKind = "latent_hazard"
	value.Materiality = "future_condition_only"
	value.CurrentImpact = "No current production path triggers the behavior."
	value.CounterfactualTrigger = contractText("A later caller omits the required setting.")
	if err := ValidateFindingContract(value); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFindingContractAcceptsFutureOnlySecurityRisk(t *testing.T) {
	value := validFindingContract()
	value.FindingKind = "security_risk"
	value.Materiality = "future_condition_only"
	value.CurrentImpact = "The current deployment does not expose credentials."
	value.CounterfactualTrigger = contractText("A later deployment enables public diagnostics.")
	if err := ValidateFindingContract(value); err != nil {
		t.Fatal(err)
	}
}

func TestContractStatusDistinguishesMissingAndInvalid(t *testing.T) {
	if got := ContractStatus(nil); got != "missing" {
		t.Fatalf("missing status = %q", got)
	}
	value := validFindingContract()
	value.FindingKind = "bug"
	if got := ContractStatus(value); got != "invalid" {
		t.Fatalf("invalid status = %q", got)
	}
}
