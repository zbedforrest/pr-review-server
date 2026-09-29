package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"google.golang.org/genai"
)

type ActivitySummaryModel struct {
	Provider, Model, APIKey, BaseURL string
	Client                           *http.Client
}

func (m ActivitySummaryModel) Summarize(ctx context.Context, activities []Activity) (string, error) {
	if len(activities) == 0 || m.APIKey == "" {
		return "", fmt.Errorf("activity summary unavailable")
	}
	if len(activities) > 8 {
		activities = activities[len(activities)-8:]
	}
	observed := make([]string, 0, len(activities))
	for _, activity := range activities {
		observed = append(observed, ActivitySummary(activity))
	}
	prompt := "Write one plain present-tense status line, at most ten words, describing this review agent's observed activity. Focus on the latest activity. Do not invent results, approval decisions, progress percentages, or tests. No markdown. Observed activities, oldest first:\n" + strings.Join(observed, "\n")
	var text string
	switch m.Provider {
	case "gemini":
		client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: m.APIKey, Backend: genai.BackendGeminiAPI})
		if err != nil {
			return "", err
		}
		zero := int32(0)
		response, err := client.Models.GenerateContent(ctx, m.Model, genai.Text(prompt), &genai.GenerateContentConfig{MaxOutputTokens: 64, ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: &zero}})
		if err != nil {
			return "", err
		}
		text = response.Text()
	case "openrouter":
		base := m.BaseURL
		if base == "" {
			base = "https://openrouter.ai/api/v1"
		}
		body, _ := json.Marshal(map[string]any{"model": m.Model, "max_tokens": 64, "temperature": 0, "messages": []any{map[string]string{"role": "user", "content": prompt}}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
		req.Header.Set("Content-Type", "application/json")
		client := m.Client
		if client == nil {
			client = http.DefaultClient
		}
		response, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("activity summary returned HTTP %d", response.StatusCode)
		}
		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&result); err != nil {
			return "", err
		}
		if len(result.Choices) != 1 {
			return "", fmt.Errorf("activity summary missing")
		}
		text = result.Choices[0].Message.Content
	default:
		return "", fmt.Errorf("activity summary provider unavailable")
	}
	return CleanActivitySummary(text)
}

func CleanActivitySummary(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 160 || len(strings.Fields(text)) > 10 || strings.ContainsAny(text, "<>`*\n\r") || strings.Contains(text, "://") || strings.ContainsFunc(text, unicode.IsControl) {
		return "", fmt.Errorf("invalid activity summary")
	}
	return text, nil
}
