package service

import "pr-review-server/pkg/reviewer/types"

func EnforceFindingContractPolicy(comments []types.LineComment) {
	for index := range comments {
		comment := &comments[index]
		if comment.FilePath == "SUMMARY" || comment.FilePath == "CHECK" {
			comment.FindingContract = nil
			continue
		}
		contract := comment.FindingContract
		if types.ValidateFindingContract(contract) == nil && (contract.Materiality == "future_condition_only" ||
			contract.Materiality == "no_user_impact" ||
			contract.FindingKind == "design_opinion" ||
			contract.FindingKind == "description_drift" ||
			contract.FindingKind == "test_quality") {
			comment.Importance = "LOW"
		}
	}
}
