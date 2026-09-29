package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestActivitySummaryUsesOnlyKnownActivity(t *testing.T) {
	for _, activity := range []Activity{{Stage: "collecting"}, {Stage: "repository"}, {Stage: "model"}, {Stage: "citations"}, {Stage: "validating"}, {Stage: "tool", Tool: "read_file"}, {Stage: "tool", Tool: "private source content"}, {Stage: "injected instructions"}} {
		summary := ActivitySummary(activity)
		_, err := CleanActivitySummary(summary)
		require.NoError(t, err)
		require.NotContains(t, summary, "private source content")
		require.NotContains(t, summary, "injected instructions")
	}
}

func TestActivitySummaryModelBoundsInputAndOutput(t *testing.T) {
	var response atomic.Value
	response.Store("Inspecting code behind existing review concerns")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		var body struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, 64, body.MaxTokens)
		require.NotContains(t, body.Messages[0].Content, "untrusted file name")
		require.Less(t, len(body.Messages[0].Content), 1200)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, response.Load().(string))
	}))
	defer remote.Close()
	model := ActivitySummaryModel{Provider: "openrouter", APIKey: "fixture", Model: "fixture", BaseURL: remote.URL}
	activities := make([]Activity, 100)
	for i := range activities {
		activities[i] = Activity{Stage: "tool", Tool: "untrusted file name"}
	}
	summary, err := model.Summarize(context.Background(), activities)
	require.NoError(t, err)
	require.Equal(t, response.Load().(string), summary)
	response.Store(strings.Repeat("word ", 11))
	_, err = model.Summarize(context.Background(), activities)
	require.Error(t, err)
}

func TestActivitySummaryModelRespectsCancellation(t *testing.T) {
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer remote.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (ActivitySummaryModel{Provider: "openrouter", APIKey: "fixture", Model: "fixture", BaseURL: remote.URL}).Summarize(ctx, []Activity{{Stage: "model"}})
	require.Error(t, err)
}

func TestCleanActivitySummaryRejectsMarkupAndOversizedText(t *testing.T) {
	for _, text := range []string{"", "<script>hello</script>", "**Inspecting code**", "First line\nsecond line", strings.Repeat("a", 161), strings.Repeat("word ", 11), "Read https://example.com"} {
		_, err := CleanActivitySummary(text)
		require.Error(t, err, text)
	}
}
