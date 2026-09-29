package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testBudget struct {
	used   Usage
	refuse bool
}

func (b *testBudget) Reserve(ctx context.Context, u Usage) error {
	if b.refuse {
		return fmt.Errorf("budget_exhausted")
	}
	b.used = addUsage(b.used, u)
	return nil
}
func (b *testBudget) Settle(ctx context.Context, reserved, actual Usage) error {
	b.used.InputTokens += actual.InputTokens - reserved.InputTokens
	b.used.OutputTokens += actual.OutputTokens - reserved.OutputTokens
	b.used.ToolBytes += actual.ToolBytes - reserved.ToolBytes
	return nil
}
func TestNativeToolRoundTrip(t *testing.T) {
	for _, provider := range []string{"anthropic", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			s, a := validFixture()
			payload, _ := json.Marshal(a)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req["model"] != "pinned-model" {
					t.Error("model changed")
				}
				if len(req["tools"].([]any)) != 6 {
					t.Error("wrong tool inventory")
				}
				if provider == "anthropic" {
					content := []any{map[string]any{"type": "text", "text": string(payload)}}
					stop := "end_turn"
					if calls == 1 {
						content = []any{map[string]any{"type": "tool_use", "id": "read1", "name": "list_evidence", "input": map[string]any{}}}
						stop = "tool_use"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"model": "pinned-model", "stop_reason": stop, "content": content, "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}})
				} else {
					message := map[string]any{"role": "assistant", "content": string(payload)}
					finish := "stop"
					if calls == 1 {
						message = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "read1", "type": "function", "function": map[string]any{"name": "list_evidence", "arguments": "{}"}}}}
						finish = "tool_calls"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"model": "pinned-model", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 100}})
				}
			}))
			defer server.Close()
			b := &testBudget{}
			n := NativeInvestigator{Config: ModelConfig{Provider: provider, Model: "pinned-model", APIKey: "fixture", BaseURL: server.URL, Client: server.Client()}}
			got, err := n.Investigate(context.Background(), s, nil, b)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != "candidate" || calls != 2 || b.used.InputTokens != 200 || b.used.ToolCalls != 1 {
				t.Fatalf("unexpected roundtrip: %+v calls=%d budget=%+v", got, calls, b.used)
			}
		})
	}
}
func TestNativeRejectsMissingUsageAndChargesReservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"model","stop_reason":"end_turn","content":[]}`))
	}))
	defer server.Close()
	s, _ := validFixture()
	b := &testBudget{}
	n := NativeInvestigator{Config: ModelConfig{Provider: "anthropic", Model: "model", APIKey: "fixture", BaseURL: server.URL}}
	_, err := n.Investigate(context.Background(), s, nil, b)
	if err == nil || b.used.InputTokens != 100000 {
		t.Fatalf("missing usage must retain reservation: %v %+v", err, b.used)
	}
}
func TestBudgetAndCancellationPreventRequest(t *testing.T) {
	s, _ := validFixture()
	n := NativeInvestigator{Config: ModelConfig{Provider: "anthropic", Model: "model", APIKey: "fixture", BaseURL: "http://invalid.invalid"}}
	_, err := n.Investigate(context.Background(), s, nil, &testBudget{refuse: true})
	if err == nil || !strings.Contains(err.Error(), "budget_exhausted") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = n.Investigate(ctx, s, nil, &testBudget{})
	if err != context.Canceled {
		t.Fatal(err)
	}
}
func TestToolBoundary(t *testing.T) {
	s, _ := validFixture()
	for _, name := range []string{"bash", "write_file", "web"} {
		if _, err := dispatch(context.Background(), s, nil, name, json.RawMessage(`{}`)); err == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, p := range []string{"../secret", "/etc/passwd", "a/../../secret", "a\\secret"} {
		args, _ := json.Marshal(ReadRequest{Revision: s.Revision.Head, Path: p, StartLine: 1, EndLine: 2})
		if _, err := dispatch(context.Background(), s, nil, "read_file", args); err == nil {
			t.Fatal("accepted", p)
		}
	}
}
