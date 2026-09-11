package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const claudeStreamResponse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"looks "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"good"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func claudeTestClient(t *testing.T, requestBody *[]byte) *ClaudeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		*requestBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeStreamResponse))
	}))
	t.Cleanup(server.Close)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	return NewClaudeClient("test-key", "claude-sonnet-5", false)
}

func requireEphemeralCacheControl(t *testing.T, requestBody []byte) {
	t.Helper()
	var request struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				CacheControl *struct {
					Type string `json:"type"`
				} `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(requestBody, &request))
	assert.Equal(t, "claude-sonnet-5", request.Model)
	assert.True(t, request.Stream)
	require.Len(t, request.Messages, 1)
	assert.Equal(t, "user", request.Messages[0].Role)
	require.Len(t, request.Messages[0].Content, 1)
	block := request.Messages[0].Content[0]
	assert.Equal(t, "text", block.Type)
	assert.Equal(t, "review this diff", block.Text)
	require.NotNil(t, block.CacheControl, "prompt block must carry cache_control")
	assert.Equal(t, "ephemeral", block.CacheControl.Type)
}

func TestClaudeGetReviewStreamMarksPromptCacheControl(t *testing.T) {
	var requestBody []byte
	client := claudeTestClient(t, &requestBody)

	var buf bytes.Buffer
	content, promptTokens, candidatesTokens, totalTokens, err := client.GetReviewStream("review this diff", &buf)
	require.NoError(t, err)
	assert.Equal(t, "looks good", content)
	assert.Equal(t, "looks good", buf.String())
	assert.Equal(t, int32(10), promptTokens)
	assert.Equal(t, int32(5), candidatesTokens)
	assert.Equal(t, int32(15), totalTokens)

	requireEphemeralCacheControl(t, requestBody)
}

func TestClaudeGetReviewMarksPromptCacheControl(t *testing.T) {
	var requestBody []byte
	client := claudeTestClient(t, &requestBody)

	content, _, _, _, err := client.GetReview("review this diff")
	require.NoError(t, err)
	assert.Equal(t, "looks good", content)

	requireEphemeralCacheControl(t, requestBody)
}
