package service

import (
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

func EnforceAgentFindingContractPolicy(comments []types.LineComment) {
	EnforceFindingContractPolicy(comments)
	for index := range comments {
		comment := &comments[index]
		if comment.FilePath == "SUMMARY" || comment.FilePath == "CHECK" {
			continue
		}
		if types.ValidateFindingContract(comment.FindingContract) != nil &&
			strings.EqualFold(strings.TrimSpace(comment.Importance), "CRITICAL") {
			comment.Importance = "MEDIUM"
		}
	}
}

// EnforceFindingContractPolicy derives importance from a valid contract.
// Non-defect classes and findings with no present impact are LOW; a finding
// that discounts itself (see types.SelfDiscount) loses its current_impact
// claim and is LOW; an unknown-materiality CRITICAL is capped at MEDIUM.
func EnforceFindingContractPolicy(comments []types.LineComment) {
	discountGate := types.DiscountGateEnabled()
	for index := range comments {
		comment := &comments[index]
		if comment.FilePath == "SUMMARY" || comment.FilePath == "CHECK" {
			comment.FindingContract = nil
			continue
		}
		types.NormalizeFindingContract(comment.FindingContract)
		contract := comment.FindingContract
		if types.ValidateFindingContract(contract) != nil {
			continue
		}
		discounted := discountGate && types.SelfDiscount(contract) != types.DiscountNone
		if discounted && contract.Materiality == "current_impact" {
			contract.Materiality = "unknown"
		}
		if discounted ||
			contract.Materiality == "future_condition_only" ||
			contract.Materiality == "no_user_impact" ||
			contract.FindingKind == "design_opinion" ||
			contract.FindingKind == "description_drift" ||
			contract.FindingKind == "test_quality" {
			comment.Importance = "LOW"
		} else if contract.Materiality == "unknown" &&
			strings.EqualFold(strings.TrimSpace(comment.Importance), "CRITICAL") {
			comment.Importance = "MEDIUM"
		}
	}
}
