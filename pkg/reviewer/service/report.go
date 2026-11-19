package service

import (
	"fmt"
	"log"
	"time"

	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/types"
)

// GenerateHTMLReportContent generates HTML report content as bytes without writing to disk.
// withhold, when set, names the findings the publisher keeps off the PR.
// Returns nil if generation fails.
func GenerateHTMLReportContent(result *ReviewResult, prNumber int, owner string, repoName string, commitSHA string, modelName string, withhold func(types.LineComment) bool) []byte {
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repoName, prNumber)
	checks := make([]html.CheckRecord, 0, len(result.Checks.Records))
	for _, r := range result.Checks.Records {
		checks = append(checks, html.CheckRecord{
			ID: r.ID, Source: r.Source, Question: r.Question, TargetFile: r.TargetFile, Verdict: r.Verdict,
			Answer: r.Answer, EvidencePath: r.EvidencePath, EvidenceResolved: r.EvidenceResolved, Unresolved: r.Unresolved,
		})
	}
	report, err := html.GenerateReportFrom(html.ReportInput{
		Comments: result.Comments, Diff: result.Diff, PRNumber: prNumber, PRURL: prURL, PRTitle: result.PRTitle, PRBody: result.PRBody,
		Prompt: result.Prompt, CommitSHA: commitSHA, ModelName: modelName, PromptTokens: result.PromptTokenCount,
		CandidateTokens: result.CandidatesTokenCount, TotalTokens: result.TotalTokenCount, GeneratedAt: time.Now(),
		FileContents: result.FileContents, Checks: checks,
		Profile: result.ProfileTitle, ProfileDeviations: result.ProfileDeviations, Withhold: withhold,
	})
	if err != nil {
		log.Printf("Error generating HTML report: %v", err)
		return nil
	}
	return []byte(report)
}
