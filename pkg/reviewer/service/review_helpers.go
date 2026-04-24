package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/types"
)

// prLogPrefix returns a consistent log prefix for PR-related messages
// Format: [repo#123] for easy identification in concurrent logs
func prLogPrefix(owner, repoName string, prNumber int) string {
	return fmt.Sprintf("[%s/%s#%d]", owner, repoName, prNumber)
}

// PRData holds all the data fetched for a PR review
type PRData struct {
	PR                      github.PR
	Diff                    string
	FileContext             string
	FileContents            map[string]string
	ExistingComments        []github.PRComment
	PreviousCommentsContext string
	CurrentUser             github.User
}

// ReviewExecutionResult holds the results of executing review requests
type ReviewExecutionResult struct {
	Comments             []types.LineComment
	PromptTokenCount     int32
	CandidatesTokenCount int32
	TotalTokenCount      int32
	SuccessCount         int64
	FirstError           error
}

// fetchPRData fetches all the data needed for a PR review
func (s *Service) fetchPRData(ctx context.Context, cfg PerformReviewConfig) (*PRData, error) {
	data := &PRData{}
	prefix := prLogPrefix(cfg.Owner, cfg.RepoName, cfg.PRNumber)

	// Fetch PR details
	color.White("%s Fetching PR details...", prefix)
	pr, err := s.githubClient.GetReviewPR(cfg.Token, cfg.Owner, cfg.RepoName, cfg.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("error fetching PR details: %v", err)
	}
	data.PR = pr
	color.Green("%s Successfully fetched PR details.", prefix)

	// Fetch context
	color.White("%s Fetching file context...", prefix)
	fileContext, fileContents, err := s.getPRContext(cfg.Token, cfg.Owner, cfg.RepoName, pr, cfg.PRNumber, cfg.Testing, cfg.Adjacent, cfg.SelectedContext)
	if err != nil {
		return nil, fmt.Errorf("error fetching PR context: %v", err)
	}
	data.FileContext = fileContext
	data.FileContents = fileContents
	color.Green("%s Successfully fetched file context.", prefix)

	// Fetch diff
	color.White("%s Fetching diff...", prefix)
	diff, err := s.githubClient.GetPRDiff(cfg.Token, cfg.Owner, cfg.RepoName, cfg.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("error fetching PR diff: %v", err)
	}
	data.Diff = diff
	color.Green("%s Successfully fetched diff.", prefix)

	// Fetch existing comments if requested
	if cfg.GitHub {
		color.White("%s Fetching existing comments...", prefix)
		existingComments, err := s.githubClient.GetPRComments(cfg.Token, cfg.Owner, cfg.RepoName, cfg.PRNumber)
		if err != nil {
			return nil, fmt.Errorf("error fetching existing PR comments: %v", err)
		}
		data.ExistingComments = existingComments
		color.Green("%s Successfully fetched %d existing comments.", prefix, len(existingComments))

		// Get current user to identify tool's own comments
		currentUser, err := s.githubClient.GetCurrentUser(cfg.Token)
		if err != nil {
			color.Yellow("%s Could not fetch current user info: %v. Including all comments in context.", prefix, err)
			currentUser = github.User{Login: ""}
		}
		data.CurrentUser = currentUser

		// Build previous comments context
		data.PreviousCommentsContext = s.buildPreviousCommentsContext(existingComments, currentUser.Login)
	} else {
		color.Yellow("%s Skipping GitHub comments (--github flag not provided)", prefix)
		data.ExistingComments = []github.PRComment{}
		data.CurrentUser = github.User{Login: ""}
		data.PreviousCommentsContext = "No GitHub comments context requested."
	}

	return data, nil
}

// buildPrompt constructs the appropriate prompt based on configuration
func (s *Service) buildPrompt(cfg PerformReviewConfig, data *PRData) string {
	if cfg.CustomPrompt != "" {
		return s.buildCustomReviewPrompt(cfg.CustomPrompt, data.PR.Body, cfg.ExtraContext, data.FileContext, data.Diff, data.PreviousCommentsContext)
	} else if cfg.Testing {
		return s.buildTestingReviewPrompt(data.PR.Body, cfg.ExtraContext, data.FileContext, data.Diff, data.PreviousCommentsContext)
	}
	return s.buildReviewPrompt(data.PR.Body, cfg.ExtraContext, data.FileContext, data.Diff, data.PreviousCommentsContext)
}

// Prompt is a fully-assembled LLM prompt, ready to send. Callers bake the
// static template and per-PR context together into Content.
type Prompt struct {
	Name    string // identifier for logs/errors, e.g. "standard#2", "custom"
	Content string // complete prompt string sent to the LLM
}

// Parser converts a raw LLM response into LineComments.
type Parser func(raw string) ([]types.LineComment, error)

// reviewResultMsg is the message type for successful review results
type reviewResultMsg struct {
	comments             []types.LineComment
	promptTokenCount     int32
	candidatesTokenCount int32
	totalTokenCount      int32
}

// reviewErrorMsg is the message type for review errors
type reviewErrorMsg struct {
	err    error
	reqNum int
	name   string
}

// rawResponseMsg is the message type for raw (unparseable) responses
type rawResponseMsg struct {
	content string
	reqNum  int
	name    string
}

// runPrompts sends each Prompt to the LLM concurrently (staggered 250ms when
// more than one), parses each response with parse, and aggregates the results.
// For a single prompt the first attempt streams to stdout.
func (s *Service) runPrompts(ctx context.Context, cfg PerformReviewConfig, prompts []Prompt, parse Parser) *ReviewExecutionResult {
	result := &ReviewExecutionResult{}
	prefix := prLogPrefix(cfg.Owner, cfg.RepoName, cfg.PRNumber)

	n := len(prompts)
	color.White("%s Making %d review request(s)...", prefix, n)
	var wg sync.WaitGroup
	var firstErrorMu sync.Mutex

	resultsChan := make(chan reviewResultMsg, n)
	errorChan := make(chan reviewErrorMsg, n)
	rawResponseChan := make(chan rawResponseMsg, n)

	for i, p := range prompts {
		wg.Add(1)
		go func(requestNum int, prompt Prompt) {
			defer wg.Done()
			s.runSinglePrompt(ctx, cfg, prompt, requestNum, n, parse, resultsChan, errorChan, rawResponseChan, result, &firstErrorMu)
		}(i+1, p)
		if n > 1 {
			time.Sleep(250 * time.Millisecond) // Stagger requests
		}
	}

	wg.Wait()
	close(resultsChan)
	close(errorChan)
	close(rawResponseChan)

	for r := range resultsChan {
		result.Comments = append(result.Comments, r.comments...)
		result.PromptTokenCount = r.promptTokenCount
		result.CandidatesTokenCount += r.candidatesTokenCount
		result.TotalTokenCount += r.totalTokenCount
	}
	for e := range errorChan {
		color.Red("%s Request %d (%s) failed: %v", prefix, e.reqNum, e.name, e.err)
	}
	for resp := range rawResponseChan {
		if cfg.WithComments {
			color.Yellow("%s Posting raw AI response as a general comment for request %d (%s).", prefix, resp.reqNum, resp.name)
			_ = s.githubClient.PostPRComment(cfg.Token, cfg.Owner, cfg.RepoName, cfg.PRNumber, fmt.Sprintf("Request %d (%s) failed to produce valid JSON. Raw response:\n\n%s", resp.reqNum, resp.name, resp.content))
		}
	}

	return result
}

// runSinglePrompt sends one Prompt to the LLM with a single retry and parses
// the response.
func (s *Service) runSinglePrompt(
	ctx context.Context,
	cfg PerformReviewConfig,
	prompt Prompt,
	requestNum int,
	totalRequests int,
	parse Parser,
	resultsChan chan<- reviewResultMsg,
	errorChan chan<- reviewErrorMsg,
	rawResponseChan chan<- rawResponseMsg,
	result *ReviewExecutionResult,
	firstErrorMu *sync.Mutex,
) {
	prefix := prLogPrefix(cfg.Owner, cfg.RepoName, cfg.PRNumber)
	color.White("%s Starting review request %d/%d (%s)", prefix, requestNum, totalRequests, prompt.Name)

	maxAttempts := 2
	var reviewContent string
	var promptTokenCount int32
	var candidatesTokenCount int32
	var totalTokenCount int32
	var errReview error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			errorChan <- reviewErrorMsg{err: ctx.Err(), reqNum: requestNum, name: prompt.Name}
			return
		default:
		}

		if attempt > 1 {
			color.Yellow("%s Retrying AI review (attempt %d/%d) for request %d (%s)...", prefix, attempt, maxAttempts, requestNum, prompt.Name)
		}

		var llmClient llm.IClient = s.smartLlmClient
		if cfg.Fast {
			llmClient = s.fastLlmClient
		}

		if cfg.Verbose {
			color.Yellow("%s LLM Client type: %T", prefix, llmClient)
		}

		if totalRequests == 1 && attempt == 1 {
			white.Printf("%s Streaming response:\n", prefix)
			reviewContent, promptTokenCount, candidatesTokenCount, totalTokenCount, errReview = llmClient.GetReviewStream(prompt.Content, os.Stdout)
			fmt.Println() // Newline after streaming
		} else {
			reviewContent, promptTokenCount, candidatesTokenCount, totalTokenCount, errReview = llmClient.GetReview(prompt.Content)
		}

		if errReview != nil {
			color.Red("%s Error getting review from LLM (attempt %d) on request %d (%s): %v", prefix, attempt, requestNum, prompt.Name, errReview)
			if attempt < maxAttempts {
				continue
			}
			color.Red("%s Failed to get review from LLM after %d attempts for request %d (%s). Skipping.", prefix, maxAttempts, requestNum, prompt.Name)
			errorChan <- reviewErrorMsg{err: errReview, reqNum: requestNum, name: prompt.Name}
			firstErrorMu.Lock()
			if result.FirstError == nil {
				result.FirstError = errReview
			}
			firstErrorMu.Unlock()
			return
		}

		comments, err := parse(reviewContent)
		if err == nil {
			color.Green("%s Successfully parsed AI review for request %d (%s).", prefix, requestNum, prompt.Name)
			atomic.AddInt64(&result.SuccessCount, 1)
			resultsChan <- reviewResultMsg{
				comments:             comments,
				promptTokenCount:     promptTokenCount,
				candidatesTokenCount: candidatesTokenCount,
				totalTokenCount:      totalTokenCount,
			}
			return
		}

		if attempt < maxAttempts {
			color.Yellow("%s AI returned non-parseable response for request %d (%s). Will retry.", prefix, requestNum, prompt.Name)
			continue
		}

		color.Red("%s Error parsing AI review after %d attempts for request %d (%s): %v", prefix, maxAttempts, requestNum, prompt.Name, err)
		rawResponseChan <- rawResponseMsg{content: reviewContent, reqNum: requestNum, name: prompt.Name}
	}
}

// summaryParser wraps a raw LLM response as a single SUMMARY line comment.
// Used for custom-prompt mode where responses are free-form text, not JSON.
func summaryParser(raw string) ([]types.LineComment, error) {
	return []types.LineComment{{
		FilePath:    "SUMMARY",
		LineNumber:  0,
		CommentBody: raw,
	}}, nil
}

// handlePostReviewProcessing handles classification, summary generation, and posting
func (s *Service) handlePostReviewProcessing(
	ctx context.Context,
	cfg PerformReviewConfig,
	data *PRData,
	allComments []types.LineComment,
) ([]types.LineComment, error) {
	prefix := prLogPrefix(cfg.Owner, cfg.RepoName, cfg.PRNumber)

	// Classify comment importance if we have comments and not a custom prompt
	if len(allComments) > 0 && cfg.CustomPrompt == "" {
		classifiedComments, err := s.classifyCommentImportance(cfg, allComments, data.PR.Body, data.FileContext, data.Diff)
		if err != nil {
			color.Yellow("%s Could not classify comment importance: %v", prefix, err)
		} else {
			allComments = classifiedComments
		}
	}

	// Convert tool's previous comments to LineComment format and include in final output
	previousToolComments := s.convertPreviousCommentsToLineComments(data.ExistingComments, data.CurrentUser.Login)

	// Combine previous tool comments with new comments
	allToolComments := append(previousToolComments, allComments...)

	// Generate comprehensive summary in testing mode
	if cfg.Testing && len(allToolComments) > 0 {
		color.White("%s Generating comprehensive summary of tool-generated review comments...", prefix)
		comprehensiveSummary, err := s.generateComprehensiveSummary(allToolComments, data.PR.Body, data.Diff)
		if err != nil {
			color.Yellow("%s Could not generate comprehensive summary: %v", prefix, err)
		} else {
			summaryComment := types.LineComment{
				FilePath:    "SUMMARY",
				LineNumber:  0,
				CommentBody: comprehensiveSummary,
			}
			allToolComments = append(allToolComments, summaryComment)
		}
	}

	return allToolComments, nil
}
