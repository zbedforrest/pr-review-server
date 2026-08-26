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
		if types.ValidateFindingContract(contract) == nil && contract.Materiality == "future_condition_only" {
			comment.Importance = "LOW"
		}
	}
}
