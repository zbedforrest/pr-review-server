package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedGenerateRequest struct {
	Contents []struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		ThinkingConfig *struct {
			ThinkingLevel string `json:"thinkingLevel"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

func TestGeminiThinkingLevelMapping(t *testing.T) {
	for input, expected := range map[string]string{
		"low":    "LOW",
		"medium": "MEDIUM",
		"high":   "HIGH",
	} {
		level, err := geminiThinkingLevel(input)
		require.NoError(t, err)
		assert.EqualValues(t, expected, level)
	}

	for _, invalid := range []string{"", "HIGH", "max"} {
		_, err := geminiThinkingLevel(invalid)
		assert.ErrorContains(t, err, "unsupported thinking level", "input %q", invalid)
	}
}

func TestGeminiThinkingClient_GetReviewSendsThinkingLevel(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{"content": {"parts": [{"text": "looks good"}], "role": "model"}}],
			"usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 11, "totalTokenCount": 18}
		}`))
	}))
	defer server.Close()

	client, err := newGeminiThinkingClient("test-key", "gemini-3.1-pro-preview", "medium", server.URL, false)
	require.NoError(t, err)

	review, promptTokens, candidateTokens, totalTokens, err := client.GetReview("hello")
	require.NoError(t, err)
	assert.Equal(t, "looks good", review)
	assert.Equal(t, int32(7), promptTokens)
	assert.Equal(t, int32(11), candidateTokens)
	assert.Equal(t, int32(18), totalTokens)

	assert.Contains(t, capturedPath, "gemini-3.1-pro-preview")
	var req capturedGenerateRequest
	require.NoError(t, json.Unmarshal(capturedBody, &req))
	require.NotNil(t, req.GenerationConfig.ThinkingConfig)
	assert.Equal(t, "MEDIUM", req.GenerationConfig.ThinkingConfig.ThinkingLevel)
	require.NotEmpty(t, req.Contents)
	require.NotEmpty(t, req.Contents[0].Parts)
	assert.Equal(t, "hello", req.Contents[0].Parts[0].Text)
}

func TestGeminiThinkingClient_GetReviewStreamSendsThinkingLevel(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"candidates":[{"content":{"parts":[{"text":"first "}],"role":"model"}}]}` + "\n\n" +
				`data: {"candidates":[{"content":{"parts":[{"text":"second"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}` + "\n\n"))
	}))
	defer server.Close()

	client, err := newGeminiThinkingClient("test-key", "gemini-3.1-pro-preview", "high", server.URL, false)
	require.NoError(t, err)

	review, promptTokens, candidateTokens, totalTokens, err := client.GetReviewStream("stream me", io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "first second", review)
	assert.Equal(t, int32(3), promptTokens)
	assert.Equal(t, int32(5), candidateTokens)
	assert.Equal(t, int32(8), totalTokens)

	var req capturedGenerateRequest
	require.NoError(t, json.Unmarshal(capturedBody, &req))
	require.NotNil(t, req.GenerationConfig.ThinkingConfig)
	assert.Equal(t, "HIGH", req.GenerationConfig.ThinkingConfig.ThinkingLevel)
}
