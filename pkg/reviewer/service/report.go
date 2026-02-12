package service

import (
	"fmt"
	"log"
	"time"

	"pr-review-server/pkg/reviewer/html"
)

// GenerateHTMLReportContent generates HTML report content as bytes without writing to disk.
// Returns nil if generation fails.
func GenerateHTMLReportContent(result *ReviewResult, prNumber int, owner string, repoName string, commitSHA string, modelName string) []byte {
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repoName, prNumber)
	report, err := html.GenerateReportWithContext(result.Comments, result.Diff, prNumber, prURL, result.PRBody, result.Prompt, commitSHA, modelName, result.PromptTokenCount, result.CandidatesTokenCount, result.TotalTokenCount, time.Now(), result.FileContents)
	if err != nil {
		log.Printf("Error generating HTML report: %v", err)
		return nil
	}
	return []byte(report)
}
