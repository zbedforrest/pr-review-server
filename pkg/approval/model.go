package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type ModelConfig struct {
	Provider string
	Model    string
	APIKey   string
	BaseURL  string
	Client   *http.Client
}
type modelCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}
type modelReply struct {
	Text  string
	Calls []modelCall
	Usage Usage
	Raw   json.RawMessage
}

func (c ModelConfig) Available() error {
	if c.Provider != "anthropic" && c.Provider != "openrouter" {
		return fmt.Errorf("unsupported native provider")
	}
	if c.APIKey == "" || c.Model == "" {
		return fmt.Errorf("native model credentials and model required")
	}
	return nil
}
func (c ModelConfig) call(ctx context.Context, messages []any, maxOutput int) (modelReply, error) {
	var reply modelReply
	if err := c.Available(); err != nil {
		return reply, err
	}
	defs := toolDefinitions()
	var body map[string]any
	endpoint := strings.TrimRight(c.BaseURL, "/")
	if c.Provider == "anthropic" {
		if endpoint == "" {
			endpoint = "https://api.anthropic.com"
		}
		endpoint += "/v1/messages"
		body = map[string]any{"model": c.Model, "max_tokens": maxOutput, "system": investigatorPrompt, "messages": messages, "tools": defs}
	}
	if c.Provider == "openrouter" {
		if endpoint == "" {
			endpoint = "https://openrouter.ai/api"
		}
		endpoint += "/v1/chat/completions"
		tools := []any{}
		for _, d := range defs {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": d.Name, "description": d.Description, "parameters": d.Schema}})
		}
		msgs := append([]any{map[string]any{"role": "system", "content": investigatorPrompt}}, messages...)
		body = map[string]any{"model": c.Model, "max_tokens": maxOutput, "messages": msgs, "tools": tools, "provider": map[string]any{"allow_fallbacks": false, "require_parameters": true}}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return reply, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Provider == "anthropic" {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := boundedClient.Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return reply, err
	}
	if len(raw) > 2*1024*1024 {
		return reply, fmt.Errorf("model response too large")
	}
	if resp.StatusCode != 200 {
		return reply, fmt.Errorf("model HTTP status %d", resp.StatusCode)
	}
	if c.Provider == "anthropic" {
		var r struct {
			Model   string `json:"model"`
			Stop    string `json:"stop_reason"`
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage *struct {
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
				CacheRead  int `json:"cache_read_input_tokens"`
				CacheWrite int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return reply, err
		}
		if r.Model != c.Model || r.Usage == nil || r.Usage.Input <= 0 || r.Usage.Output <= 0 || r.Usage.CacheRead < 0 || r.Usage.CacheWrite < 0 {
			return reply, fmt.Errorf("invalid model provenance or usage")
		}
		if r.Stop != "end_turn" && r.Stop != "tool_use" {
			return reply, fmt.Errorf("model stopped without completion")
		}
		reply.Usage = Usage{InputTokens: r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite, OutputTokens: r.Usage.Output, Rounds: 1}
		for _, b := range r.Content {
			switch b.Type {
			case "text":
				reply.Text += b.Text
			case "tool_use":
				reply.Calls = append(reply.Calls, modelCall{b.ID, b.Name, b.Input})
			default:
				return reply, fmt.Errorf("unsupported model content")
			}
		}
		var decoded map[string]json.RawMessage
		_ = json.Unmarshal(raw, &decoded)
		reply.Raw = decoded["content"]
	} else {
		var r struct {
			Model string `json:"model"`
			Usage *struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Finish  string `json:"finish_reason"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
					Calls   []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return reply, err
		}
		if r.Model != c.Model || r.Usage == nil || r.Usage.Input <= 0 || r.Usage.Output <= 0 || len(r.Choices) != 1 {
			return reply, fmt.Errorf("invalid model provenance or usage")
		}
		ch := r.Choices[0]
		if ch.Finish != "stop" && ch.Finish != "tool_calls" {
			return reply, fmt.Errorf("model stopped without completion")
		}
		reply.Text = ch.Message.Content
		reply.Usage = Usage{InputTokens: r.Usage.Input, OutputTokens: r.Usage.Output, Rounds: 1}
		for _, call := range ch.Message.Calls {
			if call.Type != "function" {
				return reply, fmt.Errorf("unsupported tool type")
			}
			reply.Calls = append(reply.Calls, modelCall{call.ID, call.Function.Name, json.RawMessage(call.Function.Arguments)})
		}
		ch.Message.Role = "assistant"
		message, _ := json.Marshal(ch.Message)
		var continuation map[string]any
		_ = json.Unmarshal(message, &continuation)
		continuation["role"] = "assistant"
		reply.Raw, _ = json.Marshal(continuation)
	}
	if reply.Usage.InputTokens > 100000 || reply.Usage.OutputTokens > maxOutput {
		return reply, fmt.Errorf("model exceeded reserved usage")
	}
	return reply, nil
}
