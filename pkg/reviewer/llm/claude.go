package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/fatih/color"
)

const (
	DefaultClaudeModel = "claude-sonnet-5"
	ClaudeHaikuModel   = "claude-3-5-haiku-latest"
	ClaudeMaxTokens    = 32000
)

// ClaudeClient is a wrapper for the Anthropic Claude API client.
type ClaudeClient struct {
	client  *anthropic.Client
	model   anthropic.Model
	verbose bool
}

// NewClaudeClient creates a new Claude API client for the given model.
// An empty model falls back to DefaultClaudeModel.
func NewClaudeClient(apiKey, model string, verbose bool) *ClaudeClient {
	if apiKey == "" {
		color.Red("Warning: Claude API key is empty!")
	} else if verbose {
		color.Yellow("Claude API key length: %d characters", len(apiKey))
	}
	if model == "" {
		model = DefaultClaudeModel
	}

	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	color.White("Using %s model.", model)

	return &ClaudeClient{
		client:  &client,
		model:   anthropic.Model(model),
		verbose: verbose,
	}
}

// ValidateAPIKey performs a minimal API call to verify the key is valid
func (c *ClaudeClient) ValidateAPIKey() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	message, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: 10,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("test")),
		},
	})
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") ||
			strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") ||
			strings.Contains(errMsg, "unauthorized") {
			return fmt.Errorf("ANTHROPIC_API_KEY validation failed - your API key is invalid or expired: %w", err)
		}
		return fmt.Errorf("Claude API validation failed: %w", err)
	}

	if len(message.Content) == 0 {
		return fmt.Errorf("Claude API validation failed: received empty response")
	}

	return nil
}

// GetReview sends the prompt to the Claude API and returns the review with token counts.
func (c *ClaudeClient) GetReview(prompt string) (string, int32, int32, int32, error) {
	// The SDK rejects non-streaming requests at this max_tokens; delegate to
	// the streaming path and discard incremental writes.
	return c.GetReviewStream(prompt, io.Discard)
}

// cachedPromptMessage marks the prompt block as an ephemeral cache breakpoint
// so parallel identical-prompt samples read the first sample's cached prefix.
func cachedPromptMessage(prompt string) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.ContentBlockParamUnion{
		OfText: &anthropic.TextBlockParam{
			Text:         prompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		},
	})
}

// GetReviewStream sends the prompt to the Claude API and streams the response with token counts.
func (c *ClaudeClient) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	ctx := context.Background()

	if c.verbose {
		color.Yellow("Starting Claude streaming request...")
		color.Yellow("Model: %s", c.model)
		color.Yellow("Prompt length: %d characters", len(prompt))
	}

	stream := c.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: ClaudeMaxTokens,
		Messages:  []anthropic.MessageParam{cachedPromptMessage(prompt)},
	})

	var reviewContent string
	var eventCount int
	var promptTokens, candidatesTokens, totalTokens int32

	for stream.Next() {
		event := stream.Current()
		eventCount++

		switch eventVariant := event.AsAny().(type) {
		case anthropic.MessageStartEvent:
			promptTokens = int32(eventVariant.Message.Usage.InputTokens)
			candidatesTokens = int32(eventVariant.Message.Usage.OutputTokens)
		case anthropic.ContentBlockDeltaEvent:
			switch deltaVariant := eventVariant.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				chunk := deltaVariant.Text
				reviewContent += chunk
				fmt.Fprint(w, chunk)
			default:
				if c.verbose {
					color.Yellow("Skipping non-text delta of type: %T", deltaVariant)
				}
			}
		case anthropic.MessageDeltaEvent:
			candidatesTokens = int32(eventVariant.Usage.OutputTokens)
		}
	}

	totalTokens = promptTokens + candidatesTokens

	if err := stream.Err(); err != nil {
		color.Red("Stream error occurred: %v", err)
		errMsg := err.Error()
		if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") || strings.Contains(errMsg, "unauthorized") {
			return "", 0, 0, 0, fmt.Errorf("Claude API authentication failed - your ANTHROPIC_API_KEY may be invalid or expired: %w", err)
		}
		return "", 0, 0, 0, fmt.Errorf("Claude API stream error: %w", err)
	}

	if reviewContent == "" {
		return "", 0, 0, 0, fmt.Errorf("stream completed but no text content was received (received %d events total)", eventCount)
	}

	return reviewContent, promptTokens, candidatesTokens, totalTokens, nil
}
