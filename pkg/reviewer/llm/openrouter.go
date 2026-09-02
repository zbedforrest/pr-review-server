package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fatih/color"
)

const (
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	DefaultOpenRouterModel   = "openai/gpt-5.6-sol"

	openRouterRequestTimeout  = 15 * time.Minute
	openRouterValidateTimeout = 60 * time.Second
)

// OpenRouterClient implements IClient with single-shot chat completions
// against an OpenRouter-compatible API.
type OpenRouterClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	verbose    bool
}

// NewOpenRouterClient creates a new OpenRouter API client. Empty baseURL and
// model fall back to DefaultOpenRouterBaseURL and DefaultOpenRouterModel.
func NewOpenRouterClient(apiKey, baseURL, model string, verbose bool) *OpenRouterClient {
	if apiKey == "" {
		color.Red("Warning: OpenRouter API key is empty!")
	}
	if baseURL == "" {
		baseURL = DefaultOpenRouterBaseURL
	}
	if model == "" {
		model = DefaultOpenRouterModel
	}
	color.White("Using %s model.", model)

	return &OpenRouterClient{
		httpClient: &http.Client{},
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		verbose:    verbose,
	}
}

type openRouterChatRequest struct {
	Model     string              `json:"model"`
	Messages  []openRouterMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
}

type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int32 `json:"prompt_tokens"`
		CompletionTokens int32 `json:"completion_tokens"`
		TotalTokens      int32 `json:"total_tokens"`
	} `json:"usage"`
	Error *openRouterError `json:"error"`
}

type openRouterError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// ValidateAPIKey performs a minimal API call to verify the key is valid
func (c *OpenRouterClient) ValidateAPIKey() error {
	ctx, cancel := context.WithTimeout(context.Background(), openRouterValidateTimeout)
	defer cancel()

	_, _, _, _, err := c.chatCompletion(ctx, openRouterChatRequest{
		Model:     c.model,
		Messages:  []openRouterMessage{{Role: "user", Content: "test"}},
		MaxTokens: 16,
	}, false)
	if err != nil {
		return fmt.Errorf("OpenRouter API validation failed: %w", err)
	}
	return nil
}

// GetReview sends the prompt to the OpenRouter API and returns the review
// with prompt, completion, and total token counts.
func (c *OpenRouterClient) GetReview(prompt string) (string, int32, int32, int32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openRouterRequestTimeout)
	defer cancel()

	return c.chatCompletion(ctx, openRouterChatRequest{
		Model:    c.model,
		Messages: []openRouterMessage{{Role: "user", Content: prompt}},
	}, true)
}

// GetReviewStream satisfies IClient by buffering the full completion and
// writing it to w once finished; OpenRouter SSE streaming is not used.
func (c *OpenRouterClient) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	reviewContent, promptTokens, completionTokens, totalTokens, err := c.GetReview(prompt)
	if err != nil {
		return "", 0, 0, 0, err
	}
	fmt.Fprint(w, reviewContent)
	return reviewContent, promptTokens, completionTokens, totalTokens, nil
}

func (c *OpenRouterClient) chatCompletion(ctx context.Context, reqBody openRouterChatRequest, requireContent bool) (string, int32, int32, int32, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter request encoding failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter request creation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API response read failed: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API authentication failed - your OPENROUTER_API_KEY may be invalid or expired: %s", errorSummary(body, resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API request failed: %s", errorSummary(body, resp.StatusCode))
	}

	var parsed openRouterChatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API returned unparseable response: %w", err)
	}
	// OpenRouter can report provider failures inside an HTTP 200 body.
	if parsed.Error != nil {
		return "", 0, 0, 0, fmt.Errorf("OpenRouter API error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", 0, 0, 0, fmt.Errorf("no response from OpenRouter API")
	}

	content := parsed.Choices[0].Message.Content
	if requireContent && content == "" {
		return "", 0, 0, 0, fmt.Errorf("unexpected response format from OpenRouter API: empty message content")
	}

	usage := parsed.Usage
	return content, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, nil
}

func errorSummary(body []byte, statusCode int) string {
	var parsed struct {
		Error *openRouterError `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return fmt.Sprintf("status %d: %s", statusCode, parsed.Error.Message)
	}
	return fmt.Sprintf("status %d", statusCode)
}
