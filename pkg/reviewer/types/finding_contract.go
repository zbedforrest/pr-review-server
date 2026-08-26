package types

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const FindingContractSchemaVersion = 1

var findingKinds = map[string]bool{
	"production_behavior": true,
	"security_risk":       true,
	"latent_hazard":       true,
	"design_opinion":      true,
	"description_drift":   true,
	"test_quality":        true,
	"operational_risk":    true,
}

var materialities = map[string]bool{
	"current_impact":        true,
	"future_condition_only": true,
	"no_user_impact":        true,
	"unknown":               true,
}

var falsifiabilities = map[string]bool{
	"falsifiable":     true,
	"not_falsifiable": true,
	"unknown":         true,
}

var subjectKinds = map[string]bool{
	"file":       true,
	"symbol":     true,
	"selector":   true,
	"config_key": true,
	"endpoint":   true,
	"workflow":   true,
	"other":      true,
}

type FindingSubject struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

type FindingContract struct {
	SchemaVersion         int              `json:"schema_version"`
	FindingKind           string           `json:"finding_kind"`
	Materiality           string           `json:"materiality"`
	CurrentImpact         string           `json:"current_impact"`
	CounterfactualTrigger *string          `json:"counterfactual_trigger"`
	Falsifiability        string           `json:"falsifiability"`
	FalsifiableCondition  *string          `json:"falsifiable_condition"`
	ExpectedObservable    *string          `json:"expected_observable"`
	Subjects              []FindingSubject `json:"subjects"`
	Uncertainty           string           `json:"uncertainty"`
	SeverityRationale     string           `json:"severity_rationale"`
}

func ValidateFindingContract(contract *FindingContract) error {
	if contract == nil {
		return fmt.Errorf("finding contract is missing")
	}
	if contract.SchemaVersion != FindingContractSchemaVersion {
		return fmt.Errorf("finding contract schema version is unsupported")
	}
	if !findingKinds[contract.FindingKind] {
		return fmt.Errorf("finding kind is invalid")
	}
	if !materialities[contract.Materiality] {
		return fmt.Errorf("materiality is invalid")
	}
	if !falsifiabilities[contract.Falsifiability] {
		return fmt.Errorf("falsifiability is invalid")
	}
	for name, value := range map[string]string{
		"current impact":     contract.CurrentImpact,
		"uncertainty":        contract.Uncertainty,
		"severity rationale": contract.SeverityRationale,
	} {
		if err := validateContractText(value, name, 500); err != nil {
			return err
		}
	}
	if contract.CounterfactualTrigger != nil {
		if err := validateContractText(*contract.CounterfactualTrigger, "counterfactual trigger", 500); err != nil {
			return err
		}
	}
	if contract.Materiality == "future_condition_only" && contract.CounterfactualTrigger == nil {
		return fmt.Errorf("future-only materiality requires a counterfactual trigger")
	}
	if contract.Materiality == "future_condition_only" && contract.FindingKind != "latent_hazard" && contract.FindingKind != "security_risk" {
		return fmt.Errorf("future-only materiality requires a latent hazard or security risk")
	}
	if contract.FindingKind == "latent_hazard" && contract.Materiality != "future_condition_only" {
		return fmt.Errorf("latent hazards require future-only materiality")
	}
	if contract.FindingKind == "design_opinion" || contract.FindingKind == "description_drift" {
		if contract.Falsifiability != "not_falsifiable" {
			return fmt.Errorf("design context must be non-falsifiable")
		}
		if contract.Materiality == "current_impact" {
			return fmt.Errorf("design context cannot claim current impact")
		}
	}
	if contract.FindingKind == "test_quality" && contract.Materiality == "current_impact" {
		return fmt.Errorf("test quality cannot claim demonstrated current impact")
	}
	if contract.Falsifiability == "falsifiable" {
		if contract.FalsifiableCondition == nil || contract.ExpectedObservable == nil {
			return fmt.Errorf("falsifiable findings require a condition and observable")
		}
		if err := validateContractText(*contract.FalsifiableCondition, "falsifiable condition", 500); err != nil {
			return err
		}
		if err := validateContractText(*contract.ExpectedObservable, "expected observable", 500); err != nil {
			return err
		}
	} else if contract.FalsifiableCondition != nil || contract.ExpectedObservable != nil {
		return fmt.Errorf("non-falsifiable findings cannot define an experiment")
	}
	if len(contract.Subjects) == 0 || len(contract.Subjects) > 8 {
		return fmt.Errorf("finding subjects are invalid")
	}
	for _, subject := range contract.Subjects {
		if !subjectKinds[subject.Kind] {
			return fmt.Errorf("finding subject kind is invalid")
		}
		if err := validateContractText(subject.Path, "finding subject path", 300); err != nil {
			return err
		}
		if subject.Kind != "file" {
			if err := validateContractText(subject.Name, "finding subject name", 200); err != nil {
				return err
			}
		} else if subject.Name != "" {
			return fmt.Errorf("file subjects cannot define a name")
		}
	}
	return nil
}

func ContractStatus(contract *FindingContract) string {
	if contract == nil {
		return "missing"
	}
	if ValidateFindingContract(contract) != nil {
		return "invalid"
	}
	return "valid"
}

func validateContractText(value, name string, maximum int) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}
