package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/types"
)

// MockGithubClient is a mock implementation of the github.Client interface.
type MockGithubClient struct {
	PostPRCommentFunc      func(token, owner, repo string, prNumber int, body string) error
	PostLineCommentFunc    func(token, owner, repo string, prNumber int, commitSHA, body, path string, line int) error
	GetReviewPRFunc        func(token, owner, repo string, prNumber int) (github.PR, error)
	GetPRFilesFunc         func(token, owner, repo string, prNumber int) ([]github.PRFile, error)
	GetFileContentFunc     func(token, owner, repo, path, ref string) (string, error)
	GetPRDiffFunc          func(token, owner, repo string, prNumber int) (string, error)
	GetReleasesFunc        func(token, owner, repo string, nMonthsLookback int) ([]github.Release, error)
	FetchPRsFunc           func(token, owner, repo, state string, nMonthsLookback int) ([]github.PR, error)
	GetPRReviewsFunc       func(token, owner, repo string, prNumber int) ([]github.Review, error)
	GetPRApprovalsFunc     func(token, owner, repo string, prNumber int) (int, error)
	GetUserReviewStateFunc func(token, owner, repo string, prNumber int, username string) (string, error)
	GetPRCommentsFunc      func(token, owner, repo string, prNumber int) ([]github.PRComment, error)
	GetCurrentUserFunc     func(token string) (github.User, error)
}

func (m *MockGithubClient) PostPRComment(token, owner, repo string, prNumber int, body string) error {
	if m.PostPRCommentFunc != nil {
		return m.PostPRCommentFunc(token, owner, repo, prNumber, body)
	}
	return nil
}

func (m *MockGithubClient) PostLineComment(token, owner, repo string, prNumber int, commitSHA, body, path string, line int) error {
	if m.PostLineCommentFunc != nil {
		return m.PostLineCommentFunc(token, owner, repo, prNumber, commitSHA, body, path, line)
	}
	return nil
}

func (m *MockGithubClient) GetReviewPR(token, owner, repo string, prNumber int) (github.PR, error) {
	if m.GetReviewPRFunc != nil {
		return m.GetReviewPRFunc(token, owner, repo, prNumber)
	}
	return github.PR{}, nil
}

func (m *MockGithubClient) GetPRFiles(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
	if m.GetPRFilesFunc != nil {
		return m.GetPRFilesFunc(token, owner, repo, prNumber)
	}
	return nil, nil
}

func (m *MockGithubClient) GetFileContent(token, owner, repo, path, ref string) (string, error) {
	if m.GetFileContentFunc != nil {
		return m.GetFileContentFunc(token, owner, repo, path, ref)
	}
	return "", nil
}

func (m *MockGithubClient) GetPRDiff(token, owner, repo string, prNumber int) (string, error) {
	if m.GetPRDiffFunc != nil {
		return m.GetPRDiffFunc(token, owner, repo, prNumber)
	}
	return "", nil
}

func (m *MockGithubClient) GetReleases(token, owner, repo string, nMonthsLookback int) ([]github.Release, error) {
	if m.GetReleasesFunc != nil {
		return m.GetReleasesFunc(token, owner, repo, nMonthsLookback)
	}
	return nil, nil
}

func (m *MockGithubClient) FetchPRs(token, owner, repo, state string, nMonthsLookback int) ([]github.PR, error) {
	if m.FetchPRsFunc != nil {
		return m.FetchPRsFunc(token, owner, repo, state, nMonthsLookback)
	}
	return nil, nil
}

func (m *MockGithubClient) GetPRReviews(token, owner, repo string, prNumber int) ([]github.Review, error) {
	if m.GetPRReviewsFunc != nil {
		return m.GetPRReviewsFunc(token, owner, repo, prNumber)
	}
	return nil, nil
}

func (m *MockGithubClient) GetPRApprovals(token, owner, repo string, prNumber int) (int, error) {
	if m.GetPRApprovalsFunc != nil {
		return m.GetPRApprovalsFunc(token, owner, repo, prNumber)
	}
	return 0, nil
}

func (m *MockGithubClient) GetUserReviewState(token, owner, repo string, prNumber int, username string) (string, error) {
	if m.GetUserReviewStateFunc != nil {
		return m.GetUserReviewStateFunc(token, owner, repo, prNumber, username)
	}
	return "", nil
}

func (m *MockGithubClient) GetPRComments(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
	if m.GetPRCommentsFunc != nil {
		return m.GetPRCommentsFunc(token, owner, repo, prNumber)
	}
	return []github.PRComment{}, nil
}

func (m *MockGithubClient) GetCurrentUser(token string) (github.User, error) {
	if m.GetCurrentUserFunc != nil {
		return m.GetCurrentUserFunc(token)
	}
	return github.User{Login: "test-user"}, nil
}

// MockLLMClient is a mock implementation of the llm.IClient interface.
type MockLLMClient struct {
	GetReviewFunc       func(prompt string) (string, int32, int32, int32, error)
	GetReviewStreamFunc func(prompt string, w io.Writer) (string, int32, int32, int32, error)
	ValidateAPIKeyFunc  func() error
}

func (m *MockLLMClient) ValidateAPIKey() error {
	if m.ValidateAPIKeyFunc != nil {
		return m.ValidateAPIKeyFunc()
	}
	return nil
}

func (m *MockLLMClient) GetReview(prompt string) (string, int32, int32, int32, error) {
	if m.GetReviewFunc != nil {
		return m.GetReviewFunc(prompt)
	}
	return "", 0, 0, 0, nil
}

func (m *MockLLMClient) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	if m.GetReviewStreamFunc != nil {
		return m.GetReviewStreamFunc(prompt, w)
	}
	return "", 0, 0, 0, nil
}

func TestPerformReview_FallbackComment_WithComments_False(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}
	postCommentCalled := false
	mockGithub.PostPRCommentFunc = func(token, owner, repo string, prNumber int, body string) error {
		postCommentCalled = true
		return nil
	}

	mockLLM := &MockLLMClient{
		GetReviewStreamFunc: func(prompt string, w io.Writer) (string, int32, int32, int32, error) {
			return "this is not json", 0, 0, 0, nil // Simulate invalid JSON
		},
	}

	service := NewService(mockGithub, mockLLM, mockLLM)
	config := PerformReviewConfig{WithComments: false}

	// Act
	_, err := service.PerformReview(config)

	// Assert
	assert.Error(t, err)
	assert.False(t, postCommentCalled, "PostPRComment should not have been called")
}

func TestPerformReview_FallbackComment_WithComments_True(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}
	postCommentCalled := false
	mockGithub.PostPRCommentFunc = func(token, owner, repo string, prNumber int, body string) error {
		postCommentCalled = true
		return nil
	}

	mockLLM := &MockLLMClient{
		GetReviewStreamFunc: func(prompt string, w io.Writer) (string, int32, int32, int32, error) {
			return "this is not json", 0, 0, 0, nil // Simulate invalid JSON
		},
	}

	service := NewService(mockGithub, mockLLM, mockLLM)
	config := PerformReviewConfig{WithComments: true}

	// Act
	_, err := service.PerformReview(config)

	// Assert
	assert.Error(t, err)
	assert.True(t, postCommentCalled, "PostPRComment should have been called")
}

func TestPerformReview_LineComments_WithComments_False(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}
	lineCommentCalled := false
	mockGithub.PostLineCommentFunc = func(token, owner, repo string, prNumber int, commitSHA, body, path string, line int) error {
		lineCommentCalled = true
		return nil
	}

	mockSmartLLM := &MockLLMClient{
		GetReviewStreamFunc: func(prompt string, w io.Writer) (string, int32, int32, int32, error) {
			// Valid JSON to trigger the main path
			return `[{"file_path":"a/b.go","line_number":1,"comment_body":"test"}]`, 0, 0, 0, nil
		},
	}
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Valid classification response
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{WithComments: false}

	// Act
	_, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.False(t, lineCommentCalled, "PostLineComment should not have been called")
}

func TestPerformReview_LineComments_WithComments_True(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}
	lineCommentCalled := false
	mockGithub.PostLineCommentFunc = func(token, owner, repo string, prNumber int, commitSHA, body, path string, line int) error {
		lineCommentCalled = true
		return nil
	}

	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Valid JSON to trigger the main path
			return `[{"file_path":"a/b.go","line_number":1,"comment_body":"test"}]`, 0, 0, 0, nil
		},
	}
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Valid classification response
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{WithComments: true}

	// Act
	_, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.True(t, lineCommentCalled, "PostLineComment should have been called")
}

func TestPerformReview_ExtraContextIncludedInPrompt(t *testing.T) {
	const extra = "My special question here"

	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}

	promptCaptured := ""
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			promptCaptured = prompt
			// Minimal valid comment JSON so processing continues
			return `[{"file_path":"x.go","line_number":1,"comment_body":"ok"}]`, 0, 0, 0, nil
		},
	}

	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			return `{"RC-1":"LOW"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	cfg := PerformReviewConfig{ExtraContext: extra}

	// Act
	_, err := service.PerformReview(cfg)

	// Assert
	assert.NoError(t, err)
	assert.Contains(t, promptCaptured, extra, "extra context should be included in prompt sent to LLM")
}

func TestPerformReview_NRequests(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}

	reviewCallCount := 0
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			reviewCallCount++
			if reviewCallCount == 1 {
				return `[{"file_path":"a.go","line_number":1,"comment_body":"comment 1"}]`, 0, 0, 0, nil
			}
			return `[{"file_path":"b.go","line_number":2,"comment_body":"comment 2"}]`, 0, 0, 0, nil
		},
	}

	classificationPromptCaptured := ""
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			classificationPromptCaptured = prompt
			// Valid classification response for 2 comments
			return `{"RC-1":"MEDIUM", "RC-2": "CRITICAL"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 2, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 2, reviewCallCount, "GetReviewStream should be called twice")
	assert.Len(t, result.Comments, 3, "Should have collected 2 comments + 1 summary")

	// Check that the comments from both calls are present and classified
	assert.Equal(t, "a.go", result.Comments[0].FilePath)
	assert.Equal(t, "MEDIUM", result.Comments[0].Importance)
	assert.Equal(t, "b.go", result.Comments[1].FilePath)
	assert.Equal(t, "CRITICAL", result.Comments[1].Importance)

	// Check that the classification prompt contained both comments in the new format
	assert.Contains(t, classificationPromptCaptured, "Comment 1:")
	assert.Contains(t, classificationPromptCaptured, "File: a.go:1")
	assert.Contains(t, classificationPromptCaptured, "comment 1")
	assert.Contains(t, classificationPromptCaptured, "Comment 2:")
	assert.Contains(t, classificationPromptCaptured, "File: b.go:2")
	assert.Contains(t, classificationPromptCaptured, "comment 2")
}

func TestPerformReview_NRequests_SummaryDeduplication(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}

	reviewCallCount := 0
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			reviewCallCount++
			if reviewCallCount == 1 {
				// First request returns a summary and a regular comment
				return `[
					{"file_path":"SUMMARY","line_number":0,"comment_body":"First summary comment"},
					{"file_path":"a.go","line_number":1,"comment_body":"comment 1"}
				]`, 0, 0, 0, nil
			}
			// Second request returns a different summary and a different regular comment
			return `[
				{"file_path":"SUMMARY","line_number":0,"comment_body":"Second summary comment"},
				{"file_path":"b.go","line_number":2,"comment_body":"comment 2"}
			]`, 0, 0, 0, nil
		},
	}

	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Valid classification response for 3 comments (1 summary + 2 regular)
			return `{"RC-1":"MEDIUM", "RC-2": "CRITICAL", "RC-3": "LOW"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 2, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 2, reviewCallCount, "GetReviewStream should be called twice")

	// Should have all original comments + 1 comprehensive summary = 5 total
	// (2 original summaries + 2 regular comments + 1 new comprehensive summary)
	assert.Len(t, result.Comments, 5, "Should have 4 original comments + 1 comprehensive summary")

	// Check that we have our comprehensive summary at the end
	var comprehensiveSummary *types.LineComment
	summaryCount := 0
	for i, comment := range result.Comments {
		if comment.FilePath == "SUMMARY" && comment.LineNumber == 0 {
			summaryCount++
			// The last summary should be our comprehensive summary
			if i == len(result.Comments)-1 {
				comprehensiveSummary = &result.Comments[i]
			}
		}
	}

	assert.Equal(t, 3, summaryCount, "Should have 2 original summaries + 1 comprehensive summary")
	assert.NotNil(t, comprehensiveSummary, "Should have a comprehensive summary")
	// The comprehensive summary should be from our mock LLM, not the original comments
	assert.Contains(t, comprehensiveSummary.CommentBody, "RC-", "Comprehensive summary should be generated by fast LLM")

	// Check that both regular comments are present
	regularComments := 0
	for _, comment := range result.Comments {
		if comment.FilePath != "SUMMARY" && comment.LineNumber != 0 {
			regularComments++
		}
	}
	assert.Equal(t, 2, regularComments, "Should have 2 regular comments")
}

func TestBuildPreviousCommentsContext(t *testing.T) {
	service := NewService(nil, nil, nil)

	t.Run("No comments", func(t *testing.T) {
		var comments []github.PRComment
		result := service.buildPreviousCommentsContext(comments, "test-user")
		assert.Equal(t, "No previous comments found on this pull request.", result)
	})

	t.Run("With comments", func(t *testing.T) {
		comments := []github.PRComment{
			{
				ID:        1,
				Body:      "This looks good!",
				User:      github.User{Login: "reviewer1"},
				CreatedAt: parseTime("2023-01-01T10:00:00Z"),
			},
			{
				ID:        2,
				Body:      "Please fix this issue.",
				Path:      "file.go",
				Line:      42,
				User:      github.User{Login: "reviewer2"},
				CreatedAt: parseTime("2023-01-01T11:00:00Z"),
			},
		}

		result := service.buildPreviousCommentsContext(comments, "test-user")

		assert.Contains(t, result, "=== EXISTING COMMENTS (2 total) ===")
		assert.Contains(t, result, "Comment 1 by reviewer1 (2023-01-01 10:00)")
		assert.Contains(t, result, "This looks good!")
		assert.Contains(t, result, "Comment 2 by reviewer2 (2023-01-01 11:00)")
		assert.Contains(t, result, "Location: file.go:42")
		assert.Contains(t, result, "Please fix this issue.")
		assert.Contains(t, result, "=== END EXISTING COMMENTS ===")
	})

	t.Run("With tool's own comments", func(t *testing.T) {
		comments := []github.PRComment{
			{
				ID:        1,
				Body:      "Consider adding error handling here",
				Path:      "main.go",
				Line:      15,
				User:      github.User{Login: "test-user"}, // Tool's own comment
				CreatedAt: parseTime("2023-01-01T09:00:00Z"),
			},
			{
				ID:        2,
				Body:      "This looks good!",
				User:      github.User{Login: "reviewer1"}, // Other reviewer
				CreatedAt: parseTime("2023-01-01T10:00:00Z"),
			},
		}

		result := service.buildPreviousCommentsContext(comments, "test-user")

		assert.Contains(t, result, "=== EXISTING COMMENTS (2 total) ===")
		assert.Contains(t, result, "--- OTHER REVIEWERS' COMMENTS (1) ---")
		assert.Contains(t, result, "Comment 1 by reviewer1")
		assert.Contains(t, result, "--- YOUR PREVIOUS COMMENTS (1) ---")
		assert.Contains(t, result, "Your comment 1")
		assert.Contains(t, result, "Consider adding error handling here")
		assert.Contains(t, result, "Location: main.go:15")
	})
}

func parseTime(timeStr string) time.Time {
	t, _ := time.Parse(time.RFC3339, timeStr)
	return t
}

func TestPerformReview_WithPreviousComments(t *testing.T) {
	// Arrange
	existingComments := []github.PRComment{
		{
			ID:        1,
			Body:      "This function is missing error handling",
			Path:      "app.go",
			Line:      10,
			User:      github.User{Login: "reviewer1"},
			CreatedAt: parseTime("2023-01-01T10:00:00Z"),
		},
	}

	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
		GetPRCommentsFunc: func(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
			return existingComments, nil
		},
	}

	reviewCallCount := 0
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			reviewCallCount++
			// Verify that the prompt contains existing comments
			assert.Contains(t, prompt, "=== EXISTING COMMENTS (1 total) ===")
			assert.Contains(t, prompt, "This function is missing error handling")
			assert.Contains(t, prompt, "AVOID DUPLICATING EXISTING FEEDBACK")

			// Return a different comment that doesn't duplicate the existing one
			return `[{"file_path":"app.go","line_number":20,"comment_body":"Consider adding unit tests for this function"}]`, 0, 0, 0, nil
		},
	}

	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 1, GitHub: true, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 1, reviewCallCount, "GetReview should be called once")

	// Should have one new comment + 1 summary = 2 total
	assert.Len(t, result.Comments, 2, "Should have 1 new comment + 1 summary")
	assert.Equal(t, "app.go", result.Comments[0].FilePath)
	assert.Equal(t, 20, result.Comments[0].LineNumber)
	assert.Contains(t, result.Comments[0].CommentBody, "Consider adding unit tests")
}

func TestPerformReview_WithToolsPreviousComments(t *testing.T) {
	// Arrange - simulate a scenario where the tool previously made comments
	existingComments := []github.PRComment{
		{
			ID:        1,
			Body:      "Previous AI comment: This function lacks error handling",
			Path:      "app.go",
			Line:      10,
			User:      github.User{Login: "ai-bot"}, // Tool's own comment
			CreatedAt: parseTime("2023-01-01T09:00:00Z"),
		},
		{
			ID:        2,
			Body:      "Human reviewer: Looks good overall",
			User:      github.User{Login: "human-reviewer"},
			CreatedAt: parseTime("2023-01-01T10:00:00Z"),
		},
	}

	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
		GetPRCommentsFunc: func(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
			return existingComments, nil
		},
		GetCurrentUserFunc: func(token string) (github.User, error) {
			return github.User{Login: "ai-bot"}, nil
		},
	}

	reviewCallCount := 0
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			reviewCallCount++
			// Verify that the prompt contains both other reviewers' comments and tool's own previous comments
			assert.Contains(t, prompt, "--- OTHER REVIEWERS' COMMENTS (1) ---")
			assert.Contains(t, prompt, "Human reviewer: Looks good overall")
			assert.Contains(t, prompt, "--- YOUR PREVIOUS COMMENTS (1) ---")
			assert.Contains(t, prompt, "Previous AI comment: This function lacks error handling")
			assert.Contains(t, prompt, "AVOID DUPLICATING EXISTING FEEDBACK")

			// Return a new, different comment
			return `[{"file_path":"app.go","line_number":30,"comment_body":"Consider adding documentation for this new method"}]`, 0, 0, 0, nil
		},
	}

	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			return `{"RC-1":"LOW"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 1, GitHub: true, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 1, reviewCallCount, "GetReview should be called once")

	// Should have 1 previous tool comment + 1 new comment + 1 summary = 3 total
	assert.Len(t, result.Comments, 3, "Should have 3 comments: 1 previous + 1 new + 1 summary")

	// Find the new comment (not the previous one)
	var newComment *types.LineComment
	for i, comment := range result.Comments {
		if strings.Contains(comment.CommentBody, "Consider adding documentation") {
			newComment = &result.Comments[i]
			break
		}
	}

	assert.NotNil(t, newComment, "Should find the new comment")
	assert.Equal(t, "app.go", newComment.FilePath)
	assert.Equal(t, 30, newComment.LineNumber)
	assert.Contains(t, newComment.CommentBody, "Consider adding documentation")
}

func TestPerformReview_IncludesPreviousToolCommentsInOutput(t *testing.T) {
	// Arrange - the tool has made comments in previous executions
	existingComments := []github.PRComment{
		{
			ID:        1,
			Body:      "Previous AI comment: This function lacks error handling",
			Path:      "app.go",
			Line:      10,
			User:      github.User{Login: "ai-bot"}, // Tool's own comment
			CreatedAt: parseTime("2023-01-01T09:00:00Z"),
		},
		{
			ID:        2,
			Body:      "Previous AI comment: Missing unit tests",
			Path:      "test.go",
			Line:      5,
			User:      github.User{Login: "ai-bot"}, // Tool's own comment
			CreatedAt: parseTime("2023-01-01T10:00:00Z"),
		},
		{
			ID:        3,
			Body:      "Human reviewer: LGTM",
			User:      github.User{Login: "human-reviewer"}, // Other reviewer - should not be included in output
			CreatedAt: parseTime("2023-01-01T11:00:00Z"),
		},
	}

	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
		GetPRCommentsFunc: func(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
			return existingComments, nil
		},
		GetCurrentUserFunc: func(token string) (github.User, error) {
			return github.User{Login: "ai-bot"}, nil
		},
	}

	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Return one new comment
			return `[{"file_path":"app.go","line_number":25,"comment_body":"New AI comment: Consider adding logging"}]`, 0, 0, 0, nil
		},
	}

	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 1, GitHub: true, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)

	// Should include 2 previous tool comments + 1 new comment + 1 summary = 4 total
	assert.Len(t, result.Comments, 4, "Should have 4 comments: 2 previous + 1 new + 1 summary")

	// Check that previous tool comments are included
	var previousComments, newComments int
	for _, comment := range result.Comments {
		if strings.Contains(comment.CommentBody, "Previous AI comment") {
			previousComments++
		} else if strings.Contains(comment.CommentBody, "New AI comment") {
			newComments++
		}
	}

	assert.Equal(t, 2, previousComments, "Should have 2 previous tool comments")
	assert.Equal(t, 1, newComments, "Should have 1 new comment")

	// Verify the specific comments are present
	commentBodies := make([]string, len(result.Comments))
	for i, comment := range result.Comments {
		commentBodies[i] = comment.CommentBody
	}

	assert.Contains(t, commentBodies, "Previous AI comment: This function lacks error handling")
	assert.Contains(t, commentBodies, "Previous AI comment: Missing unit tests")
	assert.Contains(t, commentBodies, "New AI comment: Consider adding logging")

	// Human reviewer comment should NOT be included in output
	assert.NotContains(t, commentBodies, "Human reviewer: LGTM")
}

func TestConvertPreviousCommentsToLineComments(t *testing.T) {
	service := NewService(nil, nil, nil)

	t.Run("Converts only tool's own comments", func(t *testing.T) {
		comments := []github.PRComment{
			{
				ID:   1,
				Body: "Tool comment 1",
				Path: "file1.go",
				Line: 10,
				User: github.User{Login: "ai-bot"},
			},
			{
				ID:   2,
				Body: "Human comment",
				Path: "file2.go",
				Line: 20,
				User: github.User{Login: "human-reviewer"},
			},
			{
				ID:   3,
				Body: "Tool comment 2",
				Path: "file3.go",
				Line: 30,
				User: github.User{Login: "ai-bot"},
			},
		}

		result := service.convertPreviousCommentsToLineComments(comments, "ai-bot")

		assert.Len(t, result, 2, "Should convert only ai-bot's comments")
		assert.Equal(t, "Tool comment 1", result[0].CommentBody)
		assert.Equal(t, "file1.go", result[0].FilePath)
		assert.Equal(t, 10, result[0].LineNumber)
		assert.Equal(t, "Tool comment 2", result[1].CommentBody)
		assert.Equal(t, "file3.go", result[1].FilePath)
		assert.Equal(t, 30, result[1].LineNumber)
	})

	t.Run("Handles general PR comments", func(t *testing.T) {
		comments := []github.PRComment{
			{
				ID:   1,
				Body: "General PR comment",
				Path: "", // No specific file
				Line: 0,  // No specific line
				User: github.User{Login: "ai-bot"},
			},
		}

		result := service.convertPreviousCommentsToLineComments(comments, "ai-bot")

		assert.Len(t, result, 1)
		assert.Equal(t, "General PR comment", result[0].CommentBody)
		assert.Equal(t, "GENERAL", result[0].FilePath)
		assert.Equal(t, 0, result[0].LineNumber)
	})
}

func TestGenerateComprehensiveSummary(t *testing.T) {
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			// Verify the prompt contains all the expected sections
			assert.Contains(t, prompt, "=== ALL AI-GENERATED REVIEW COMMENTS ===")
			assert.Contains(t, prompt, "Comment 1:")
			assert.Contains(t, prompt, "File: app.go:10")
			assert.Contains(t, prompt, "Error handling needed")
			assert.Contains(t, prompt, "Comment 2:")
			assert.Contains(t, prompt, "Type: General PR Comment")
			assert.Contains(t, prompt, "Overall looks good")
			assert.Contains(t, prompt, "principal-level full‑stack engineer")
			assert.Contains(t, prompt, "testing summary")
			assert.Contains(t, prompt, "Test Coverage Analysis")
			assert.Contains(t, prompt, "AI-generated review comments")

			return "## Test Coverage Analysis\nMissing unit tests for critical functionality...", 0, 0, 0, nil
		},
	}

	service := NewService(nil, nil, mockFastLLM)

	comments := []types.LineComment{
		{
			FilePath:    "app.go",
			LineNumber:  10,
			CommentBody: "Error handling needed",
			Importance:  "CRITICAL",
		},
		{
			FilePath:    "GENERAL",
			LineNumber:  0,
			CommentBody: "Overall looks good",
			Importance:  "LOW",
		},
	}

	summary, err := service.generateComprehensiveSummary(comments, "PR body", "diff content")

	assert.NoError(t, err)
	assert.Contains(t, summary, "Test Coverage Analysis")
	assert.Contains(t, summary, "Missing unit tests")
}

func TestPerformReview_WithComprehensiveSummary(t *testing.T) {
	// Arrange - PR with existing tool comments
	existingComments := []github.PRComment{
		{
			ID:        1,
			Body:      "Previous comment: Add error handling",
			Path:      "app.go",
			Line:      10,
			User:      github.User{Login: "ai-bot"},
			CreatedAt: parseTime("2023-01-01T09:00:00Z"),
		},
	}

	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "sha123"},
				Body: "Test PR body",
			}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff content", nil
		},
		GetPRCommentsFunc: func(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
			return existingComments, nil
		},
		GetCurrentUserFunc: func(token string) (github.User, error) {
			return github.User{Login: "ai-bot"}, nil
		},
	}

	smartCallCount := 0
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			smartCallCount++
			return `[{"file_path":"app.go","line_number":20,"comment_body":"New comment: Add unit tests"}]`, 0, 0, 0, nil
		},
	}

	fastCallCount := 0
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			fastCallCount++
			if strings.Contains(prompt, "comprehensive summary") || strings.Contains(prompt, "testing summary") {
				// This is the summary generation call
				return "## Test Coverage Analysis\nPR has 2 testing-related concerns that need attention...", 0, 0, 0, nil
			}
			// This is the classification call
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{NRequests: 1, GitHub: true, Testing: true}

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 1, smartCallCount, "Smart LLM should be called once for review")
	assert.Equal(t, 2, fastCallCount, "Fast LLM should be called twice: classification + summary")

	// Should have: 1 previous + 1 new + 1 summary = 3 total
	assert.Len(t, result.Comments, 3, "Should have 3 comments: 1 previous + 1 new + 1 summary")

	// Check for summary comment
	var summaryComment *types.LineComment
	for i, comment := range result.Comments {
		if comment.FilePath == "SUMMARY" {
			summaryComment = &result.Comments[i]
			break
		}
	}

	assert.NotNil(t, summaryComment, "Should have a summary comment")
	assert.Equal(t, "SUMMARY", summaryComment.FilePath)
	assert.Equal(t, 0, summaryComment.LineNumber)
	assert.Contains(t, summaryComment.CommentBody, "Test Coverage Analysis")
	assert.Contains(t, summaryComment.CommentBody, "testing-related concerns")
}

func TestParseMarkdownResponse(t *testing.T) {
	service := NewService(nil, nil, nil)

	// Sample markdown content similar to what was in 22346-output.txt
	markdownContent := `File: frontend/react/src/components/roomlist/HomepagePaginatedRoomlist/HomepagePaginatedRoomlist.test.tsx:28
Rationale: The primary change in HomepagePaginatedRoomlist.tsx is the conditional rendering of SearchResultsHeader when search keywords are present.
Action: Add an integration test to verify that providing keywords via UrlState correctly renders the SearchResultsHeader.

File: frontend/react/src/components/roomlist/SearchResultsHeader/SearchResultsHeader.test.tsx:194
Rationale: The component SearchResultsHeader introduces new logic to display a specific message when there are no results.
Action: Add a dedicated unit test for the SrchEvlv variant that simulates this scenario.`

	comments, err := service.ParseMarkdownResponse(markdownContent)

	assert.NoError(t, err)
	assert.Len(t, comments, 2)

	// Check first comment
	assert.Equal(t, "frontend/react/src/components/roomlist/HomepagePaginatedRoomlist/HomepagePaginatedRoomlist.test.tsx", comments[0].FilePath)
	assert.Equal(t, 28, comments[0].LineNumber)
	assert.Contains(t, comments[0].CommentBody, "Rationale:")
	assert.Contains(t, comments[0].CommentBody, "conditional rendering of SearchResultsHeader")

	// Check second comment
	assert.Equal(t, "frontend/react/src/components/roomlist/SearchResultsHeader/SearchResultsHeader.test.tsx", comments[1].FilePath)
	assert.Equal(t, 194, comments[1].LineNumber)
	assert.Contains(t, comments[1].CommentBody, "Rationale:")
	assert.Contains(t, comments[1].CommentBody, "SrchEvlv variant")
}

func TestPerformReview_NoSummaryWhenNotInTestingMode(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
	}

	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			return `[{"file_path":"app.go","line_number":10,"comment_body":"Add error handling"}]`, 0, 0, 0, nil
		},
	}

	summaryCallCount := 0
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			if strings.Contains(prompt, "comprehensive summary") || strings.Contains(prompt, "testing summary") {
				summaryCallCount++
				return "This should not be called", 0, 0, 0, nil
			}
			// Classification call
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(mockGithub, mockSmartLLM, mockFastLLM)
	config := PerformReviewConfig{Testing: false} // Not in testing mode

	// Act
	result, err := service.PerformReview(config)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 0, summaryCallCount, "Summary should not be generated when Testing=false")

	// Should have only 1 comment (no summary)
	assert.Len(t, result.Comments, 1, "Should have 1 comment without summary")
	assert.Equal(t, "app.go", result.Comments[0].FilePath)

	// Verify no SUMMARY comment
	for _, comment := range result.Comments {
		assert.NotEqual(t, "SUMMARY", comment.FilePath, "Should not have SUMMARY comment when Testing=false")
	}
}

// ============================================================================
// Tests for review_helpers.go extracted functions
// ============================================================================

func TestFetchPRData_Success(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{
				Number: 123,
				Title:  "Test PR",
				Body:   "Test body",
				Head: struct {
					SHA string `json:"sha"`
				}{SHA: "abc123"},
			}, nil
		},
		GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
			return []github.PRFile{
				{Filename: "main.go", Status: "modified"},
			}, nil
		},
		GetFileContentFunc: func(token, owner, repo, path, ref string) (string, error) {
			return "package main", nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "+added line\n-removed line", nil
		},
	}

	service := NewService(mockGithub, nil, nil)
	ctx := context.Background()
	cfg := PerformReviewConfig{
		Token:    "token",
		Owner:    "owner",
		RepoName: "repo",
		PRNumber: 123,
	}

	// Act
	data, err := service.fetchPRData(ctx, cfg)

	// Assert
	assert.NoError(t, err)
	assert.NotNil(t, data)
	assert.Equal(t, 123, data.PR.Number)
	assert.Equal(t, "Test PR", data.PR.Title)
	assert.Equal(t, "+added line\n-removed line", data.Diff)
	assert.NotEmpty(t, data.FileContext)
}

func TestFetchPRData_PRNotFound(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{}, fmt.Errorf("PR not found")
		},
	}

	service := NewService(mockGithub, nil, nil)
	ctx := context.Background()
	cfg := PerformReviewConfig{
		Token:    "token",
		Owner:    "owner",
		RepoName: "repo",
		PRNumber: 999,
	}

	// Act
	data, err := service.fetchPRData(ctx, cfg)

	// Assert
	assert.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "error fetching PR details")
}

func TestFetchPRData_DiffFetchError(t *testing.T) {
	// Arrange
	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{Number: 123}, nil
		},
		GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
			return []github.PRFile{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "", fmt.Errorf("failed to fetch diff")
		},
	}

	service := NewService(mockGithub, nil, nil)
	ctx := context.Background()
	cfg := PerformReviewConfig{
		Token:    "token",
		Owner:    "owner",
		RepoName: "repo",
		PRNumber: 123,
	}

	// Act
	data, err := service.fetchPRData(ctx, cfg)

	// Assert
	assert.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "error fetching PR diff")
}

func TestFetchPRData_WithGitHubComments(t *testing.T) {
	// Arrange
	existingComments := []github.PRComment{
		{
			ID:   1,
			Body: "Looks good!",
			User: github.User{Login: "reviewer1"},
		},
	}

	mockGithub := &MockGithubClient{
		GetReviewPRFunc: func(token, owner, repo string, prNumber int) (github.PR, error) {
			return github.PR{Number: 123}, nil
		},
		GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
			return []github.PRFile{}, nil
		},
		GetPRDiffFunc: func(token, owner, repo string, prNumber int) (string, error) {
			return "diff", nil
		},
		GetPRCommentsFunc: func(token, owner, repo string, prNumber int) ([]github.PRComment, error) {
			return existingComments, nil
		},
		GetCurrentUserFunc: func(token string) (github.User, error) {
			return github.User{Login: "ai-bot"}, nil
		},
	}

	service := NewService(mockGithub, nil, nil)
	ctx := context.Background()
	cfg := PerformReviewConfig{
		Token:    "token",
		Owner:    "owner",
		RepoName: "repo",
		PRNumber: 123,
		GitHub:   true, // Enable GitHub comment fetching
	}

	// Act
	data, err := service.fetchPRData(ctx, cfg)

	// Assert
	assert.NoError(t, err)
	assert.NotNil(t, data)
	assert.Len(t, data.ExistingComments, 1)
	assert.Equal(t, "ai-bot", data.CurrentUser.Login)
	assert.Contains(t, data.PreviousCommentsContext, "Looks good!")
}

func TestBuildPrompt_StandardReview(t *testing.T) {
	service := NewService(nil, nil, nil)

	data := &PRData{
		PR: github.PR{
			Body: "PR description",
		},
		FileContext:             "file context here",
		Diff:                    "diff content",
		PreviousCommentsContext: "no comments",
	}

	cfg := PerformReviewConfig{
		Testing:      false,
		CustomPrompt: "",
	}

	// Act
	prompt := service.buildPrompt(cfg, data)

	// Assert
	assert.NotEmpty(t, prompt)
	// Standard review prompt includes file context, diff, and previous comments
	assert.Contains(t, prompt, "file context here")
	assert.Contains(t, prompt, "diff content")
	assert.Contains(t, prompt, "no comments")
	assert.Contains(t, prompt, "staff engineer") // Part of the standard review prompt
}

func TestBuildPrompt_TestingMode(t *testing.T) {
	service := NewService(nil, nil, nil)

	data := &PRData{
		PR: github.PR{
			Body: "PR description",
		},
		FileContext:             "file context",
		Diff:                    "diff",
		PreviousCommentsContext: "no comments",
	}

	cfg := PerformReviewConfig{
		Testing:      true,
		CustomPrompt: "",
	}

	// Act
	prompt := service.buildPrompt(cfg, data)

	// Assert
	assert.NotEmpty(t, prompt)
	// Testing mode prompt should include testing-specific instructions
	assert.Contains(t, prompt, "test")
}

func TestBuildPrompt_CustomPrompt(t *testing.T) {
	service := NewService(nil, nil, nil)

	data := &PRData{
		PR: github.PR{
			Body: "PR description",
		},
		FileContext:             "file context",
		Diff:                    "diff",
		PreviousCommentsContext: "no comments",
	}

	cfg := PerformReviewConfig{
		CustomPrompt: "My custom question about the code",
	}

	// Act
	prompt := service.buildPrompt(cfg, data)

	// Assert
	assert.NotEmpty(t, prompt)
	assert.Contains(t, prompt, "My custom question about the code")
}

func TestRunPrompts_HeterogeneousPrompts(t *testing.T) {
	var mu sync.Mutex
	seenContents := map[string]bool{}
	mockSmartLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			mu.Lock()
			seenContents[prompt] = true
			mu.Unlock()
			// Echo which prompt produced each comment so we can verify all three ran.
			return fmt.Sprintf(`[{"file_path":"%s.go","line_number":1,"comment_body":"from %s"}]`, prompt, prompt), 10, 5, 15, nil
		},
	}

	service := NewService(&MockGithubClient{}, mockSmartLLM, nil)
	ctx := context.Background()
	cfg := PerformReviewConfig{Fast: false}

	prompts := []Prompt{
		{Name: "security", Content: "security"},
		{Name: "testing", Content: "testing"},
		{Name: "correctness", Content: "correctness"},
	}

	result := service.runPrompts(ctx, cfg, prompts, service.parseAIResponse)

	assert.Equal(t, int64(3), result.SuccessCount)
	assert.Len(t, result.Comments, 3)
	assert.True(t, seenContents["security"], "security prompt should have been sent")
	assert.True(t, seenContents["testing"], "testing prompt should have been sent")
	assert.True(t, seenContents["correctness"], "correctness prompt should have been sent")
}

func TestHandlePostReviewProcessing_WithClassification(t *testing.T) {
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			if strings.Contains(prompt, "testing summary") || strings.Contains(prompt, "comprehensive summary") {
				return "## Summary\nThis PR needs attention.", 0, 0, 0, nil
			}
			// Classification response
			return `{"RC-1":"CRITICAL","RC-2":"LOW"}`, 0, 0, 0, nil
		},
	}

	service := NewService(nil, nil, mockFastLLM)
	ctx := context.Background()

	cfg := PerformReviewConfig{
		Testing: true,
	}

	data := &PRData{
		PR: github.PR{
			Body: "Test PR",
		},
		Diff:             "diff content",
		FileContext:      "file context",
		ExistingComments: []github.PRComment{},
		CurrentUser:      github.User{Login: ""},
	}

	comments := []types.LineComment{
		{FilePath: "main.go", LineNumber: 10, CommentBody: "Fix this bug"},
		{FilePath: "utils.go", LineNumber: 20, CommentBody: "Consider refactoring"},
	}

	// Act
	result, err := service.handlePostReviewProcessing(ctx, cfg, data, comments)

	// Assert
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// Should have 2 original comments + 1 summary
	assert.Len(t, result, 3)

	// Check that comments are classified
	assert.Equal(t, "CRITICAL", result[0].Importance)
	assert.Equal(t, "LOW", result[1].Importance)

	// Check summary exists
	var hasSummary bool
	for _, c := range result {
		if c.FilePath == "SUMMARY" {
			hasSummary = true
			assert.Contains(t, c.CommentBody, "Summary")
			break
		}
	}
	assert.True(t, hasSummary, "Should have a summary comment")
}

func TestHandlePostReviewProcessing_WithPreviousToolComments(t *testing.T) {
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			if strings.Contains(prompt, "testing summary") || strings.Contains(prompt, "comprehensive summary") {
				return "## Summary", 0, 0, 0, nil
			}
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(nil, nil, mockFastLLM)
	ctx := context.Background()

	cfg := PerformReviewConfig{
		Testing: true,
	}

	data := &PRData{
		PR: github.PR{
			Body: "Test PR",
		},
		Diff:        "diff content",
		FileContext: "file context",
		ExistingComments: []github.PRComment{
			{
				ID:   1,
				Body: "Previous tool comment",
				Path: "old.go",
				Line: 5,
				User: github.User{Login: "ai-bot"},
			},
		},
		CurrentUser: github.User{Login: "ai-bot"},
	}

	newComments := []types.LineComment{
		{FilePath: "new.go", LineNumber: 10, CommentBody: "New comment"},
	}

	// Act
	result, err := service.handlePostReviewProcessing(ctx, cfg, data, newComments)

	// Assert
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// Should have 1 previous comment + 1 new comment + 1 summary = 3
	assert.Len(t, result, 3)

	// Check that previous comment is included
	var hasPreviousComment bool
	for _, c := range result {
		if c.CommentBody == "Previous tool comment" {
			hasPreviousComment = true
			break
		}
	}
	assert.True(t, hasPreviousComment, "Should include previous tool comments")
}

func TestHandlePostReviewProcessing_NoSummaryWhenNotTesting(t *testing.T) {
	summaryCallCount := 0
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			if strings.Contains(prompt, "testing summary") || strings.Contains(prompt, "comprehensive summary") {
				summaryCallCount++
				return "## Summary", 0, 0, 0, nil
			}
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(nil, nil, mockFastLLM)
	ctx := context.Background()

	cfg := PerformReviewConfig{
		Testing: false, // Not in testing mode
	}

	data := &PRData{
		PR: github.PR{
			Body: "Test PR",
		},
		Diff:             "diff content",
		FileContext:      "file context",
		ExistingComments: []github.PRComment{},
		CurrentUser:      github.User{Login: ""},
	}

	comments := []types.LineComment{
		{FilePath: "main.go", LineNumber: 10, CommentBody: "Fix this"},
	}

	// Act
	result, err := service.handlePostReviewProcessing(ctx, cfg, data, comments)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 0, summaryCallCount, "Summary should not be generated when not in testing mode")

	// Should only have 1 comment (no summary)
	assert.Len(t, result, 1)

	// Verify no SUMMARY comment
	for _, c := range result {
		assert.NotEqual(t, "SUMMARY", c.FilePath)
	}
}

func TestHandlePostReviewProcessing_SkipsClassificationForCustomPrompt(t *testing.T) {
	classificationCallCount := 0
	mockFastLLM := &MockLLMClient{
		GetReviewFunc: func(prompt string) (string, int32, int32, int32, error) {
			if strings.Contains(prompt, "RC-") || strings.Contains(prompt, "CRITICAL") {
				classificationCallCount++
			}
			return `{"RC-1":"MEDIUM"}`, 0, 0, 0, nil
		},
	}

	service := NewService(nil, nil, mockFastLLM)
	ctx := context.Background()

	cfg := PerformReviewConfig{
		Testing:      false,
		CustomPrompt: "My custom question", // Custom prompt mode
	}

	data := &PRData{
		PR: github.PR{
			Body: "Test PR",
		},
		Diff:             "diff content",
		FileContext:      "file context",
		ExistingComments: []github.PRComment{},
		CurrentUser:      github.User{Login: ""},
	}

	comments := []types.LineComment{
		{FilePath: "main.go", LineNumber: 10, CommentBody: "Answer to custom prompt"},
	}

	// Act
	result, err := service.handlePostReviewProcessing(ctx, cfg, data, comments)

	// Assert
	assert.NoError(t, err)
	assert.Equal(t, 0, classificationCallCount, "Classification should be skipped for custom prompts")
	assert.Len(t, result, 1)
}

func TestGetPRContext_TestingModeFlag(t *testing.T) {
	t.Run("Fetches test files when testing mode is true", func(t *testing.T) {
		fileContentCallCount := 0
		filesPaths := []string{}

		mockGithub := &MockGithubClient{
			GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
				return []github.PRFile{
					{Filename: "app.go", Status: "modified"},
				}, nil
			},
			GetFileContentFunc: func(token, owner, repo, path, ref string) (string, error) {
				fileContentCallCount++
				filesPaths = append(filesPaths, path)
				return fmt.Sprintf("content of %s", path), nil
			},
		}

		service := NewService(mockGithub, nil, nil)
		pr := github.PR{
			Base: struct {
				Ref string `json:"ref"`
			}{Ref: "main"},
		}

		// Act with testing = true and includeFileContents = true
		context, fileContents, err := service.getPRContext("token", "owner", "repo", pr, 1, true, true, ContextDefinition{})

		// Assert
		assert.NoError(t, err)
		assert.NotEmpty(t, context)
		assert.NotEmpty(t, fileContents)

		// Should have fetched both the main file and test file
		assert.GreaterOrEqual(t, fileContentCallCount, 2, "Should fetch at least main file and test file")
		assert.Contains(t, filesPaths, "app.go", "Should fetch main file")
		// Check if any test file was attempted (app_test.go or other patterns)
		hasTestFile := false
		for _, path := range filesPaths {
			if strings.Contains(path, "_test.go") || strings.Contains(path, ".test.") {
				hasTestFile = true
				break
			}
		}
		assert.True(t, hasTestFile, "Should attempt to fetch test files")
	})

	t.Run("Skips test files when testing mode is false", func(t *testing.T) {
		fileContentCallCount := 0
		filesPaths := []string{}

		mockGithub := &MockGithubClient{
			GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
				return []github.PRFile{
					{Filename: "app.go", Status: "modified"},
				}, nil
			},
			GetFileContentFunc: func(token, owner, repo, path, ref string) (string, error) {
				fileContentCallCount++
				filesPaths = append(filesPaths, path)
				return fmt.Sprintf("content of %s", path), nil
			},
		}

		service := NewService(mockGithub, nil, nil)
		pr := github.PR{
			Base: struct {
				Ref string `json:"ref"`
			}{Ref: "main"},
		}

		// Act with testing = false and includeFileContents = true
		context, fileContents, err := service.getPRContext("token", "owner", "repo", pr, 1, false, true, ContextDefinition{})

		// Assert
		assert.NoError(t, err)
		assert.NotEmpty(t, context)
		assert.NotEmpty(t, fileContents)

		// Should have fetched only the main file, not test files
		assert.Equal(t, 1, fileContentCallCount, "Should fetch only main file when testing=false")
		assert.Contains(t, filesPaths, "app.go", "Should fetch main file")

		// Verify no test files were fetched
		for _, path := range filesPaths {
			assert.NotContains(t, path, "_test.go", "Should not fetch Go test files")
			assert.NotContains(t, path, ".test.", "Should not fetch test files")
		}
	})

	t.Run("Does not store file contents when includeFileContents is false", func(t *testing.T) {
		mockGithub := &MockGithubClient{
			GetPRFilesFunc: func(token, owner, repo string, prNumber int) ([]github.PRFile, error) {
				return []github.PRFile{
					{Filename: "app.go", Status: "modified"},
				}, nil
			},
			GetFileContentFunc: func(token, owner, repo, path, ref string) (string, error) {
				return "content of file", nil
			},
		}

		service := NewService(mockGithub, nil, nil)
		pr := github.PR{
			Base: struct {
				Ref string `json:"ref"`
			}{Ref: "main"},
		}

		// Act with includeFileContents = false
		context, fileContents, err := service.getPRContext("token", "owner", "repo", pr, 1, false, false, ContextDefinition{})

		// Assert
		assert.NoError(t, err)
		assert.NotEmpty(t, context, "Should still return context string")
		assert.Nil(t, fileContents, "Should not store file contents when includeFileContents=false")
	})
}
