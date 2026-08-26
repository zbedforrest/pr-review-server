package service

import (
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

func EnforceAgentFindingContractPolicy(comments []types.LineComment) {
	for index := range comments {
		comment := &comments[index]
		if comment.FilePath == "SUMMARY" || comment.FilePath == "CHECK" {
			continue
		}
		types.NormalizeFindingContract(comment.FindingContract)
		if types.ValidateFindingContract(comment.FindingContract) != nil &&
			strings.EqualFold(strings.TrimSpace(comment.Importance), "CRITICAL") {
			comment.Importance = "MEDIUM"
		}
	}
	EnforceFindingContractPolicy(comments)
}

func EnforceFindingContractPolicy(comments []types.LineComment) {
	for index := range comments {
		comment := &comments[index]
		if comment.FilePath == "SUMMARY" || comment.FilePath == "CHECK" {
			comment.FindingContract = nil
			continue
		}
		contract := comment.FindingContract
		if types.ValidateFindingContract(contract) != nil {
			continue
		}
		if contract.Materiality == "future_condition_only" ||
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
