package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// StructuredRequest is one chat completion whose reply must match Schema.
type StructuredRequest struct {
	Model      string
	System     string
	User       string
	SchemaName string
	Schema     json.RawMessage
	MaxTokens  int
	// ProviderOrder is OpenRouter's provider preference; fallbacks stay on,
	// restricted to providers that honor the schema.
	ProviderOrder []string
}

// Call records what served one structured request, for telemetry.
type Call struct {
	RequestedModel   string        `json:"requested_model"`
	ServedModel      string        `json:"served_model"`
	Provider         string        `json:"provider"`
	GenerationID     string        `json:"generation_id"`
	PromptTokens     int           `json:"prompt_tokens"`
	CachedTokens     int           `json:"cached_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	ReasoningTokens  int           `json:"reasoning_tokens"`
	CostUSD          float64       `json:"cost_usd"`
	Latency          time.Duration `json:"latency_ns"`
	Attempts         int           `json:"attempts"`
}

type structuredBody struct {
	Model          string              `json:"model"`
	Messages       []openRouterMessage `json:"messages"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
	ResponseFormat responseFormat      `json:"response_format"`
	Provider       providerPrefs       `json:"provider"`
	Usage          usageOpts           `json:"usage"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type providerPrefs struct {
	Order             []string `json:"order,omitempty"`
	AllowFallbacks    bool     `json:"allow_fallbacks"`
	RequireParameters bool     `json:"require_parameters"`
}

type usageOpts struct {
	Include bool `json:"include"`
}

type structuredReply struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Choices  []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *openRouterError `json:"error"`
}

// structuredAttempts and structuredBackoff bound retries of rate limits,
// server errors and dropped connections.
var (
	structuredAttempts = 3
	structuredBackoff  = []time.Duration{time.Second, 3 * time.Second}
)

// CompleteStructured runs one schema-enforced completion and returns the
// reply text (JSON matching the schema) and what served it.
func (c *OpenRouterClient) CompleteStructured(ctx context.Context, req StructuredRequest) (string, Call, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	msgs := []openRouterMessage{{Role: "user", Content: req.User}}
	if req.System != "" {
		msgs = append([]openRouterMessage{{Role: "system", Content: req.System}}, msgs...)
	}
	payload, err := json.Marshal(structuredBody{
		Model:          model,
		Messages:       msgs,
		MaxTokens:      req.MaxTokens,
		ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: req.SchemaName, Strict: true, Schema: req.Schema}},
		Provider:       providerPrefs{Order: req.ProviderOrder, AllowFallbacks: true, RequireParameters: true},
		Usage:          usageOpts{Include: true},
	})
	if err != nil {
		return "", Call{}, fmt.Errorf("OpenRouter structured request encoding failed: %w", err)
	}
	call := Call{RequestedModel: model}
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= structuredAttempts; attempt++ {
		call.Attempts = attempt
		content, retry, err := c.structuredOnce(ctx, payload, &call)
		if err == nil {
			call.Latency = time.Since(start)
			return content, call, nil
		}
		lastErr = err
		if !retry || attempt == structuredAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", call, ctx.Err()
		case <-time.After(structuredBackoff[min(attempt-1, len(structuredBackoff)-1)]):
		}
	}
	call.Latency = time.Since(start)
	return "", call, lastErr
}

// structuredOnce sends one request; retry reports whether a failure is
// transient (rate limit, server error, network).
func (c *OpenRouterClient) structuredOnce(ctx context.Context, payload []byte, call *Call) (string, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("OpenRouter request creation failed: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", ctx.Err() == nil, fmt.Errorf("OpenRouter API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", true, fmt.Errorf("OpenRouter API response read failed: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return "", true, fmt.Errorf("OpenRouter API request failed: %s", errorSummary(body, resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("OpenRouter API request failed: %s", errorSummary(body, resp.StatusCode))
	}
	var r structuredReply
	if err := json.Unmarshal(body, &r); err != nil {
		return "", true, fmt.Errorf("OpenRouter API returned unparseable response: %w", err)
	}
	if r.Error != nil {
		return "", true, fmt.Errorf("OpenRouter API error: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == "" {
		return "", true, fmt.Errorf("OpenRouter API returned no content")
	}
	call.ServedModel, call.Provider, call.GenerationID = r.Model, r.Provider, r.ID
	call.PromptTokens, call.CompletionTokens = r.Usage.PromptTokens, r.Usage.CompletionTokens
	call.CachedTokens = r.Usage.PromptTokensDetails.CachedTokens
	call.ReasoningTokens = r.Usage.CompletionTokensDetails.ReasoningTokens
	call.CostUSD = r.Usage.Cost
	return r.Choices[0].Message.Content, false, nil
}
