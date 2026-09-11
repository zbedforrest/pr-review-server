package llm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openRouterTestServer(t *testing.T, handler http.HandlerFunc) *OpenRouterClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewOpenRouterClient("test-key", server.URL, "openai/gpt-5.6-sol", false)
}

func TestOpenRouterGetReview_Success(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody openRouterChatRequest

	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "looks good"}}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46}
		}`))
	})

	content, promptTokens, completionTokens, totalTokens, err := client.GetReview("review this diff")
	require.NoError(t, err)
	assert.Equal(t, "looks good", content)
	assert.Equal(t, int32(12), promptTokens)
	assert.Equal(t, int32(34), completionTokens)
	assert.Equal(t, int32(46), totalTokens)

	assert.Equal(t, "/chat/completions", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "openai/gpt-5.6-sol", gotBody.Model)
	require.Len(t, gotBody.Messages, 1)
	assert.Equal(t, "user", gotBody.Messages[0].Role)
	assert.Equal(t, "review this diff", gotBody.Messages[0].Content)
}

func TestOpenRouterGetReviewStream_BuffersToWriter(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"content": "streamed review"}}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
		}`))
	})

	var buf bytes.Buffer
	content, promptTokens, completionTokens, totalTokens, err := client.GetReviewStream("prompt", &buf)
	require.NoError(t, err)
	assert.Equal(t, "streamed review", content)
	assert.Equal(t, "streamed review", buf.String())
	assert.Equal(t, int32(1), promptTokens)
	assert.Equal(t, int32(2), completionTokens)
	assert.Equal(t, int32(3), totalTokens)
}

func TestOpenRouterGetReview_AuthError(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"code": 401, "message": "No auth credentials found"}}`))
	})

	_, _, _, _, err := client.GetReview("prompt")
	require.Error(t, err)
	assert.ErrorContains(t, err, "OPENROUTER_API_KEY")
	assert.ErrorContains(t, err, "No auth credentials found")
}

func TestOpenRouterGetReview_HTTPError(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error": {"code": "rate_limited", "message": "slow down"}}`))
	})

	_, _, _, _, err := client.GetReview("prompt")
	require.Error(t, err)
	assert.ErrorContains(t, err, "status 429")
	assert.ErrorContains(t, err, "slow down")
}

func TestOpenRouterGetReview_ErrorInsideOKBody(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error": {"code": 502, "message": "provider unavailable"}}`))
	})

	_, _, _, _, err := client.GetReview("prompt")
	require.Error(t, err)
	assert.ErrorContains(t, err, "provider unavailable")
}

func TestOpenRouterGetReview_EmptyChoices(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices": []}`))
	})

	_, _, _, _, err := client.GetReview("prompt")
	require.Error(t, err)
	assert.ErrorContains(t, err, "no response from OpenRouter API")
}

func TestOpenRouterGetReview_EmptyContent(t *testing.T) {
	client := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices": [{"message": {"content": ""}}]}`))
	})

	_, _, _, _, err := client.GetReview("prompt")
	require.Error(t, err)
	assert.ErrorContains(t, err, "empty message content")
}

func TestOpenRouterValidateAPIKey(t *testing.T) {
	valid := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body openRouterChatRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, 16, body.MaxTokens)
		_, _ = w.Write([]byte(`{"choices": [{"message": {"content": ""}}]}`))
	})
	assert.NoError(t, valid.ValidateAPIKey())

	invalid := openRouterTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "bad key"}}`))
	})
	err := invalid.ValidateAPIKey()
	require.Error(t, err)
	assert.ErrorContains(t, err, "OpenRouter API validation failed")
	assert.ErrorContains(t, err, "OPENROUTER_API_KEY")
}

func TestNewOpenRouterClient_Defaults(t *testing.T) {
	client := NewOpenRouterClient("key", "", "", false)
	assert.Equal(t, DefaultOpenRouterBaseURL, client.baseURL)
	assert.Equal(t, DefaultOpenRouterModel, client.model)
}
