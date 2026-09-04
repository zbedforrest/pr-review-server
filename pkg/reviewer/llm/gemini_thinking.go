package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fatih/color"
	"google.golang.org/genai"
)

// GeminiThinkingClient is a Gemini client that pins an explicit thinking level
// on every generate request. It uses the google.golang.org/genai SDK because
// the legacy generative-ai-go SDK predates thinking and cannot express
// ThinkingConfig; deployments with no level configured keep the legacy client
// and its unchanged requests.
type GeminiThinkingClient struct {
	client  *genai.Client
	model   string
	level   genai.ThinkingLevel
	verbose bool
}

func geminiThinkingLevel(level string) (genai.ThinkingLevel, error) {
	switch level {
	case ThinkingLow:
		return genai.ThinkingLevelLow, nil
	case ThinkingMedium:
		return genai.ThinkingLevelMedium, nil
	case ThinkingHigh:
		return genai.ThinkingLevelHigh, nil
	default:
		return "", fmt.Errorf("unsupported thinking level %q (expected low, medium, or high)", level)
	}
}

// NewGeminiThinkingClient creates a Gemini client that requests the given
// thinking level (low, medium, or high).
func NewGeminiThinkingClient(apiKey, modelName, level string, verbose bool) (*GeminiThinkingClient, error) {
	return newGeminiThinkingClient(apiKey, modelName, level, "", verbose)
}

func newGeminiThinkingClient(apiKey, modelName, level, baseURL string, verbose bool) (*GeminiThinkingClient, error) {
	resolved, err := geminiThinkingLevel(level)
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      apiKey,
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: baseURL},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}
	color.White("Using %s model (thinking=%s).", modelName, level)
	return &GeminiThinkingClient{client: client, model: modelName, level: resolved, verbose: verbose}, nil
}

func (c *GeminiThinkingClient) generateConfig() *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: c.level},
	}
}

func classifyGeminiError(err error) error {
	errMsg := err.Error()
	if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") ||
		strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") ||
		strings.Contains(errMsg, "unauthorized") {
		return fmt.Errorf("Gemini API authentication failed - your GEMINI_API_KEY may be invalid or expired: %w", err)
	}
	return fmt.Errorf("Gemini API request failed: %w", err)
}

// ValidateAPIKey performs a minimal API call to verify the key is valid.
func (c *GeminiThinkingClient) ValidateAPIKey() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.client.Models.GenerateContent(ctx, c.model, genai.Text("test"), c.generateConfig())
	if err != nil {
		return classifyGeminiError(err)
	}
	if len(resp.Candidates) == 0 {
		return fmt.Errorf("Gemini API validation failed: received empty response")
	}
	return nil
}

// GetReview sends the prompt to the Gemini API and returns the review.
func (c *GeminiThinkingClient) GetReview(prompt string) (string, int32, int32, int32, error) {
	resp, err := c.client.Models.GenerateContent(context.Background(), c.model, genai.Text(prompt), c.generateConfig())
	if err != nil {
		return "", 0, 0, 0, classifyGeminiError(err)
	}
	text := resp.Text()
	if text == "" {
		return "", 0, 0, 0, fmt.Errorf("no response from AI")
	}
	var promptTokens, candidateTokens, totalTokens int32
	if resp.UsageMetadata != nil {
		promptTokens = resp.UsageMetadata.PromptTokenCount
		candidateTokens = resp.UsageMetadata.CandidatesTokenCount
		totalTokens = resp.UsageMetadata.TotalTokenCount
	}
	return text, promptTokens, candidateTokens, totalTokens, nil
}

// GetReviewStream streams the response, accumulating the full review text.
func (c *GeminiThinkingClient) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	var content strings.Builder
	var promptTokens, candidateTokens, totalTokens int32
	for resp, err := range c.client.Models.GenerateContentStream(context.Background(), c.model, genai.Text(prompt), c.generateConfig()) {
		if err != nil {
			return "", 0, 0, 0, classifyGeminiError(err)
		}
		if resp.UsageMetadata != nil {
			promptTokens = resp.UsageMetadata.PromptTokenCount
			candidateTokens = resp.UsageMetadata.CandidatesTokenCount
			totalTokens = resp.UsageMetadata.TotalTokenCount
		}
		chunk := resp.Text()
		content.WriteString(chunk)
		fmt.Fprint(w, chunk)
	}
	return content.String(), promptTokens, candidateTokens, totalTokens, nil
}
