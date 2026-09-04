package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Default Gemini model names. Override at runtime with the
// GEMINI_FLASH_MODEL / GEMINI_PRO_MODEL environment variables;
// the defaults below apply when those are unset or empty.
// Pro defaults to the 3.1 preview (the -latest alias still resolves to
// 2.5 Pro while no stable 3.x Pro exists). Flash stays on the stable
// alias — a 3.5-flash swap was evaluated and rejected: label flips were
// within single-model sampling noise.
const (
	DefaultFlashModel = "gemini-flash-latest"
	DefaultProModel   = "gemini-3.1-pro-preview"
)

// FlashModelName returns the flash model name, preferring the
// GEMINI_FLASH_MODEL environment variable over DefaultFlashModel.
func FlashModelName() string {
	return modelNameFromEnv("GEMINI_FLASH_MODEL", DefaultFlashModel)
}

// ProModelName returns the pro model name, preferring the
// GEMINI_PRO_MODEL environment variable over DefaultProModel.
func ProModelName() string {
	return modelNameFromEnv("GEMINI_PRO_MODEL", DefaultProModel)
}

func modelNameFromEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// IClient defines the interface for an LLM client.
type IClient interface {
	GetReview(prompt string) (string, int32, int32, int32, error)
	GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error)
	ValidateAPIKey() error
}

// LLMProvider represents the type of LLM provider
type LLMProvider string

const (
	ProviderGemini     LLMProvider = "gemini"
	ProviderClaude     LLMProvider = "claude"
	ProviderOpenRouter LLMProvider = "openrouter"
)

// Client is a wrapper for the Gemini API client.
type Client struct {
	genaiClient *genai.GenerativeModel
	verbose     bool
}

// NewClient creates a new LLM client based on the provider type.
func NewClient(provider LLMProvider, apiKey string, useFlash bool, verbose bool) IClient {
	switch provider {
	case ProviderClaude:
		model := DefaultClaudeModel
		if useFlash {
			model = ClaudeHaikuModel
		}
		return NewClaudeClient(apiKey, model, verbose)
	default:
		return NewGeminiClient(apiKey, useFlash, verbose)
	}
}

// NewGeminiClient creates a new Gemini API client. The model name resolves
// from the GEMINI_FLASH_MODEL / GEMINI_PRO_MODEL environment variables when
// set, falling back to the package defaults.
func NewGeminiClient(apiKey string, useFlash bool, verbose bool) *Client {
	modelName := ProModelName()
	if useFlash {
		modelName = FlashModelName()
	}
	return NewGeminiClientWithModel(apiKey, modelName, verbose)
}

// NewGeminiClientWithModel creates a new Gemini API client for an explicit
// model name.
func NewGeminiClientWithModel(apiKey, modelName string, verbose bool) *Client {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}

	color.White("Using %s model.", modelName)
	model := client.GenerativeModel(modelName)

	return &Client{genaiClient: model, verbose: verbose}
}

// ValidateAPIKey performs a minimal API call to verify the key is valid
func (c *Client) ValidateAPIKey() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Send minimal prompt to test API key validity
	resp, err := c.genaiClient.GenerateContent(ctx, genai.Text("test"))
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") ||
			strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") ||
			strings.Contains(errMsg, "unauthorized") {
			return fmt.Errorf("GEMINI_API_KEY validation failed - your API key is invalid or expired: %w", err)
		}
		return fmt.Errorf("Gemini API validation failed: %w", err)
	}

	if len(resp.Candidates) == 0 {
		return fmt.Errorf("Gemini API validation failed: received empty response")
	}

	return nil
}

// GetReview sends the prompt to the Gemini API and returns the review.
func (c *Client) GetReview(prompt string) (string, int32, int32, int32, error) {
	ctx := context.Background()
	resp, err := c.genaiClient.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		// Check if error is related to authentication/invalid API key
		errMsg := err.Error()
		if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") || strings.Contains(errMsg, "unauthorized") {
			return "", 0, 0, 0, fmt.Errorf("Gemini API authentication failed - your GEMINI_API_KEY may be invalid or expired: %w", err)
		}
		return "", 0, 0, 0, fmt.Errorf("Gemini API request failed: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", 0, 0, 0, fmt.Errorf("no response from AI")
	}

	// Assuming the response is text
	if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
		if len(resp.Candidates[0].Content.Parts) > 1 {
			color.Red("Multiple parts in response: %v", resp.Candidates[0].Content.Parts)
		}
		promptTokenCount := resp.UsageMetadata.PromptTokenCount
		candidatesTokenCount := resp.UsageMetadata.CandidatesTokenCount
		totalTokenCount := resp.UsageMetadata.TotalTokenCount
		return string(txt), promptTokenCount, candidatesTokenCount, totalTokenCount, nil
	}

	return "", 0, 0, 0, fmt.Errorf("unexpected response format from AI")
}

func (c *Client) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	ctx := context.Background()
	iter := c.genaiClient.GenerateContentStream(ctx, genai.Text(prompt))
	var reviewContent string
	var promptTokenCount int32
	var candidatesTokenCount int32
	var totalTokenCount int32

	for {
		resp, err := iter.Next()
		if c.verbose {
			// Pretty-print the response to get more insight into the stream
			jsonResp, jsonErr := json.MarshalIndent(resp, "", "  ")
			if jsonErr != nil {
				log.Printf("Failed to marshal stream response for verbose logging: %v", err)
			} else {
				log.Printf("Full response from API stream:\n%s", string(jsonResp))
			}
		}

		if err == iterator.Done {
			break
		}
		if err != nil {
			// Check if error is related to authentication/invalid API key
			errMsg := err.Error()
			if strings.Contains(errMsg, "API key") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "403") || strings.Contains(errMsg, "authentication") || strings.Contains(errMsg, "unauthorized") {
				return "", 0, 0, 0, fmt.Errorf("Gemini API authentication failed - your GEMINI_API_KEY may be invalid or expired: %w", err)
			}
			return "", 0, 0, 0, fmt.Errorf("Gemini API request failed: %w", err)
		}

		for _, cand := range resp.Candidates {
			if cand.Content != nil {
				// final values are accurate, so update with each chunk
				promptTokenCount = resp.UsageMetadata.PromptTokenCount
				candidatesTokenCount = resp.UsageMetadata.CandidatesTokenCount
				totalTokenCount = resp.UsageMetadata.TotalTokenCount
				for _, part := range cand.Content.Parts {
					if txt, ok := part.(genai.Text); ok {
						chunk := string(txt)
						reviewContent += chunk
					}
				}
			}
		}
	}

	return reviewContent, promptTokenCount, candidatesTokenCount, totalTokenCount, nil
}

// Close closes the underlying genai.Client.
// Note: The current genai.Client does not have a Close() method. This is a placeholder.
func (c *Client) Close() {
	// genaiClient doesn't have a Close() method.
	// If it did, it would be called here.
}
