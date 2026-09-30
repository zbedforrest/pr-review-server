package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCompleteStructuredSendsTheSchemaAndReportsWhatServedIt(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		_, _ = io.WriteString(w, `{"id":"gen-1","model":"vendor/model-20260910","provider":"ProviderA",
			"choices":[{"message":{"content":"{\"findings\":[]}"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0012,
			"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	}))
	defer srv.Close()
	c := NewOpenRouterClient("key", srv.URL, "vendor/model", false)
	out, call, err := c.CompleteStructured(context.Background(), StructuredRequest{
		System: "sys", User: "merge these", SchemaName: "merge",
		Schema: json.RawMessage(`{"type":"object"}`), ProviderOrder: []string{"providera"},
	})
	if err != nil || out != `{"findings":[]}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	rf := sent["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	prov := sent["provider"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true || js["name"] != "merge" || prov["require_parameters"] != true {
		t.Fatalf("request did not enforce the schema: %v", sent)
	}
	want := Call{RequestedModel: "vendor/model", ServedModel: "vendor/model-20260910", Provider: "ProviderA", GenerationID: "gen-1",
		PromptTokens: 100, CachedTokens: 80, CompletionTokens: 20, ReasoningTokens: 5, CostUSD: 0.0012, Attempts: 1}
	call.Latency = 0
	if call != want {
		t.Fatalf("call = %+v, want %+v", call, want)
	}
}

func TestCompleteStructuredRetriesRateLimitsButNotBadRequests(t *testing.T) {
	structuredBackoff = []time.Duration{time.Millisecond}
	for _, tc := range []struct {
		first    int
		attempts int
		ok       bool
	}{{http.StatusTooManyRequests, 2, true}, {http.StatusBadGateway, 2, true}, {http.StatusBadRequest, 1, false}} {
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				w.WriteHeader(tc.first)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
		}))
		_, call, err := NewOpenRouterClient("key", srv.URL, "m", false).CompleteStructured(context.Background(), StructuredRequest{Schema: json.RawMessage(`{}`)})
		srv.Close()
		if (err == nil) != tc.ok || call.Attempts != tc.attempts {
			t.Errorf("status %d: err=%v attempts=%d, want ok=%v attempts=%d", tc.first, err, call.Attempts, tc.ok, tc.attempts)
		}
	}
}
