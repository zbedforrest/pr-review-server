package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testBudget struct {
	used        Usage
	refuse      bool
	settlements int
}

func (b *testBudget) Reserve(ctx context.Context, u Usage) error {
	if b.refuse {
		return fmt.Errorf("budget_exhausted")
	}
	b.used = addUsage(b.used, u)
	return nil
}
func (b *testBudget) Settle(ctx context.Context, reserved, actual Usage) error {
	b.settlements++
	b.used.InputTokens += actual.InputTokens - reserved.InputTokens
	b.used.OutputTokens += actual.OutputTokens - reserved.OutputTokens
	b.used.ToolBytes += actual.ToolBytes - reserved.ToolBytes
	return nil
}

type modelTurn struct {
	text     string
	calls    []modelTurnCall
	thinking []any
}

type modelTurnCall struct {
	id, name string
	args     map[string]any
}

const cleanAnswer = `{"summary":"Current completed review has no concerns.","no_concerns":["E1"]}`

func answerTurn(text string) modelTurn { return modelTurn{text: text} }

func toolTurn(calls ...modelTurnCall) modelTurn { return modelTurn{calls: calls} }

func fakeModel(t *testing.T, provider string, turn func(call int, request map[string]any) modelTurn) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		next := turn(calls, request)
		model, _ := request["model"].(string)
		if provider == "anthropic" {
			content := append([]any{}, next.thinking...)
			stop := "end_turn"
			if next.text != "" {
				content = append(content, map[string]any{"type": "text", "text": next.text})
			}
			for _, call := range next.calls {
				content = append(content, map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": call.args})
				stop = "tool_use"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "stop_reason": stop, "content": content, "usage": map[string]int{"input_tokens": 100, "output_tokens": 100}})
			return
		}
		message := map[string]any{"role": "assistant", "content": next.text}
		finish := "stop"
		if len(next.calls) > 0 {
			var toolCalls []any
			for _, call := range next.calls {
				args, _ := json.Marshal(call.args)
				toolCalls = append(toolCalls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": string(args)}})
			}
			message["tool_calls"] = toolCalls
			finish = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 100, "completion_tokens_details": map[string]int{"reasoning_tokens": 60}}})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func fakeInvestigator(provider string, server *httptest.Server) NativeInvestigator {
	return NativeInvestigator{Config: ModelConfig{Provider: provider, Model: "pinned-model", APIKey: "fixture", BaseURL: server.URL, Client: server.Client()}, ToolRounds: MaxToolRounds}
}

func TestNativeDefaultInvestigatorAnswersWithToolsClosed(t *testing.T) {
	s, _ := validFixture()
	server, _ := fakeModel(t, "openrouter", func(_ int, request map[string]any) modelTurn {
		if request["tool_choice"] != "none" {
			t.Errorf("default investigator must request the answer with tools closed, got tool_choice %v", request["tool_choice"])
		}
		return answerTurn(cleanAnswer)
	})
	n := fakeInvestigator("openrouter", server)
	n.ToolRounds = 0
	if _, err := n.Investigate(context.Background(), s, nil, &testBudget{}); err != nil {
		t.Fatal(err)
	}
}

func requestText(request map[string]any) string {
	raw, _ := json.Marshal(request["messages"])
	return string(raw)
}

func firstUserText(request map[string]any) string {
	for _, message := range request["messages"].([]any) {
		if m := message.(map[string]any); m["role"] == "user" {
			text, _ := m["content"].(string)
			return text
		}
	}
	return ""
}

func toolsDisabled(request map[string]any) bool {
	choice, _ := json.Marshal(request["tool_choice"])
	return strings.Contains(string(choice), "none")
}

func TestNativeAnswersFromThePreloadInOneRequest(t *testing.T) {
	for _, provider := range []string{"anthropic", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			s, _ := validFixture()
			server, calls := fakeModel(t, provider, func(_ int, request map[string]any) modelTurn {
				if request["model"] != "pinned-model" {
					t.Error("model changed")
				}
				if len(request["tools"].([]any)) != 4 {
					t.Errorf("wrong tool inventory: %v", request["tools"])
				}
				if toolsDisabled(request) {
					t.Error("tools disabled on the first request")
				}
				if max, _ := request["max_tokens"].(float64); int(max) != CallOutputTokens {
					t.Errorf("max_tokens = %v", request["max_tokens"])
				}
				if preload := firstUserText(request); !strings.Contains(preload, "=== E1 ") || !strings.Contains(preload, "No concerns.") {
					t.Errorf("first message lacks the preloaded evidence: %s", preload)
				}
				return answerTurn(cleanAnswer)
			})
			b := &testBudget{}
			got, err := fakeInvestigator(provider, server).Investigate(context.Background(), s, nil, b)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != "candidate" || *calls != 1 || b.used.InputTokens != 100 || got.Usage.Rounds != 1 || len(got.CoverageGaps) != 0 {
				t.Fatalf("unexpected result: %+v calls=%d budget=%+v", got, *calls, b.used)
			}
			if got.Model != "pinned-model" || got.Artifacts[0].Classification != "non_actionable" {
				t.Fatalf("unexpected assessment fields: %+v", got)
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
	if err == nil || b.used.InputTokens != CallInputTokens {
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

func TestNativeRecoversRejectedReadAndServesOldEvidenceCalls(t *testing.T) {
	s, _ := validFixture()
	s.Evidence = append(s.Evidence, Evidence{ID: "second", Body: "Additional reviewer context"})
	s.Digest = SnapshotDigest(s)
	server, _ := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
		if call == 1 {
			return toolTurn(modelTurnCall{"bad", "read_file", map[string]any{"revision": "H", "path": "../secret", "start_line": 1, "end_line": 1}}, modelTurnCall{"batch", "read_evidence", map[string]any{"evidence_ids": []string{"E1", "E2"}}})
		}
		if !strings.Contains(requestText(request), "Read rejected") {
			t.Error("read failure was not returned to investigator")
		}
		if !strings.Contains(requestText(request), "Additional reviewer context") {
			t.Error("old-style evidence read did not resolve aliases")
		}
		return answerTurn(`{"summary":"Current completed review has no concerns.","no_concerns":["E1","E2"]}`)
	})
	budget := &testBudget{}
	result, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != "candidate" || budget.used.ToolCalls != 2 {
		t.Fatalf("unexpected result %s with %+v", result.Decision, budget.used)
	}
}

func TestNativeReportsBudgetCeilingAsInvestigationLimit(t *testing.T) {
	s, _ := validFixture()
	n := NativeInvestigator{Config: ModelConfig{Provider: "anthropic", Model: "model", APIKey: "fixture"}, InitialUsage: Usage{Rounds: MaxRounds}}
	_, err := n.Investigate(context.Background(), s, nil, &testBudget{})
	if !errors.Is(err, ErrInvestigationLimit) {
		t.Fatalf("expected resource limit, got %v", err)
	}
}

func TestNativeFailsOversizedEvidenceBeforeAnyModelCall(t *testing.T) {
	s, _ := validFixture()
	s.Evidence[0].Body = strings.Repeat("x", PreloadEvidenceBytes+1)
	s.Digest = SnapshotDigest(s)
	b := &testBudget{}
	n := NativeInvestigator{Config: ModelConfig{Provider: "anthropic", Model: "model", APIKey: "fixture", BaseURL: "http://invalid.invalid"}}
	_, err := n.Investigate(context.Background(), s, nil, b)
	if LimitCode(err) != LimitEvidence || b.used != (Usage{}) {
		t.Fatalf("err=%v budget=%+v", err, b.used)
	}
}

func TestNativeSettlesOnlyValidatedReportedLimitUsage(t *testing.T) {
	for _, provider := range []string{"anthropic", "openrouter"} {
		for _, test := range []struct {
			name          string
			input, output int
			model, stop   string
			settled       bool
		}{
			{"input overage", CallInputTokens + 1, 100, "model", "", true},
			{"output overage", 100, CallOutputTokens + 1, "model", "", true},
			{"truncated output", 100, CallOutputTokens, "model", "limit", true},
			{"wrong model", CallInputTokens + 1, 100, "unexpected", "", false},
			{"missing input", 0, 100, "model", "", false},
			{"hostile integer", int(^uint(0) >> 1), 100, "model", "", false},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if provider == "anthropic" {
						stop := "end_turn"
						if test.stop == "limit" {
							stop = "max_tokens"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"model": test.model, "stop_reason": stop, "content": []any{map[string]any{"type": "text", "text": "{}"}}, "usage": map[string]int{"input_tokens": test.input, "output_tokens": test.output}})
					} else {
						stop := "stop"
						if test.stop == "limit" {
							stop = "length"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"model": test.model, "choices": []any{map[string]any{"finish_reason": stop, "message": map[string]any{"role": "assistant", "content": "{}"}}}, "usage": map[string]int{"prompt_tokens": test.input, "completion_tokens": test.output}})
					}
				}))
				defer remote.Close()
				s, _ := validFixture()
				budget := &testBudget{}
				n := NativeInvestigator{Config: ModelConfig{Provider: provider, Model: "model", APIKey: "fixture", BaseURL: remote.URL}}
				_, err := n.Investigate(context.Background(), s, nil, budget)
				if err == nil {
					t.Fatal("invalid model completion accepted")
				}
				if test.settled {
					if !errors.Is(err, ErrInvestigationLimit) || budget.settlements != 1 || budget.used.InputTokens != test.input || budget.used.OutputTokens != test.output {
						t.Fatalf("usage lost: %v %+v", err, budget)
					}
				} else if budget.settlements != 0 || budget.used.InputTokens != CallInputTokens || budget.used.OutputTokens != CallOutputTokens {
					t.Fatalf("untrusted usage settled: %+v", budget)
				}
			})
		}
	}
}

func TestNativeAsksOnceForMissingVerdictsAndMergesTheReply(t *testing.T) {
	s, _ := validFixture()
	s.Evidence[0].Body = "The retry loop never stops after a permanent failure."
	s.Evidence[0].ConcernIDs = []string{"retry"}
	s.Evidence = append(s.Evidence, Evidence{ID: "second", Body: "Thanks for the update."})
	s.Concerns = []Concern{{ID: "retry", EvidenceIDs: []string{"review"}, Claim: s.Evidence[0].Body, OriginalSeverity: "high", Impact: "correctness", OriginalRevision: s.Revision.Head}}
	s.Digest = SnapshotDigest(s)
	server, calls := fakeModel(t, "openrouter", func(call int, request map[string]any) modelTurn {
		if call == 1 {
			return answerTurn(`{"summary":"The retry concern needs a look."}`)
		}
		messages := request["messages"].([]any)
		followUp, _ := messages[len(messages)-1].(map[string]any)["content"].(string)
		if !strings.Contains(followUp, "C1") || !strings.Contains(followUp, "E2") || strings.Contains(followUp, "E1") {
			t.Errorf("follow-up should name exactly the missing items: %s", followUp)
		}
		if !toolsDisabled(request) {
			t.Error("tools stay closed for the follow-up")
		}
		if max, _ := request["max_tokens"].(float64); int(max) != FollowUpOutputTokens {
			t.Errorf("follow-up max_tokens = %v", request["max_tokens"])
		}
		return answerTurn(`{"verdicts":[{"concern":"C1","disposition":"Unresolved","rationale":"The loop still retries forever."}],"no_concerns":["E2"]}`)
	})
	got, err := fakeInvestigator("openrouter", server).Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || got.Decision != "needs_attention" || got.Summary != "The retry concern needs a look." || len(got.CoverageGaps) != 0 {
		t.Fatalf("calls=%d result=%+v", *calls, got)
	}
	if c := got.Concerns[0]; c.Disposition != "unresolved" || c.Rationale != "The loop still retries forever." || c.Claim != s.Concerns[0].Claim {
		t.Fatalf("follow-up verdict not merged: %+v", c)
	}
}

func TestNativeSkipsTheFollowUpWhenAnUnresolvedVerdictAlreadyBlocks(t *testing.T) {
	s, _ := validFixture()
	s.Evidence[0].Body = "The retry loop never stops after a permanent failure."
	s.Evidence[0].ConcernIDs = []string{"retry"}
	s.Evidence = append(s.Evidence, Evidence{ID: "second", Body: "Thanks for the update."})
	s.Concerns = []Concern{{ID: "retry", EvidenceIDs: []string{"review"}, Claim: s.Evidence[0].Body, OriginalSeverity: "high", Impact: "correctness", OriginalRevision: s.Revision.Head}}
	s.Digest = SnapshotDigest(s)
	server, calls := fakeModel(t, "openrouter", func(int, map[string]any) modelTurn {
		return answerTurn(`{"summary":"The retry concern needs a look.","verdicts":[{"concern":"C1","disposition":"unresolved","rationale":"Still retries."}]}`)
	})
	got, err := fakeInvestigator("openrouter", server).Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil || *calls != 1 || got.Decision != "needs_attention" {
		t.Fatalf("calls=%d decision=%s err=%v", *calls, got.Decision, err)
	}
}

func TestNativeSettlesItemsStillMissingAfterTheFollowUp(t *testing.T) {
	s, _ := validFixture()
	s.Concerns = []Concern{{ID: "retry", EvidenceIDs: []string{"review"}, Claim: "Retries never stop.", OriginalSeverity: "low", Impact: "style", OriginalRevision: s.Revision.Head}}
	s.Digest = SnapshotDigest(s)
	server, calls := fakeModel(t, "anthropic", func(int, map[string]any) modelTurn {
		return answerTurn(`{"summary":"Nothing to add."}`)
	})
	got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || got.Decision != "needs_attention" || got.Concerns[0].Disposition != "unresolved" {
		t.Fatalf("calls=%d result=%+v", *calls, got)
	}
}

func TestNativeAsksOnceForJSONThenFails(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		t.Run(fmt.Sprint("recovers=", recovers), func(t *testing.T) {
			s, _ := validFixture()
			server, calls := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
				if call == 2 && !strings.Contains(requestText(request), "was not the requested JSON object") {
					t.Error("follow-up did not explain the problem")
				}
				if call == 2 && recovers {
					return answerTurn(cleanAnswer)
				}
				return answerTurn("I think this looks fine.")
			})
			got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
			if *calls != 2 {
				t.Fatalf("calls=%d", *calls)
			}
			if recovers && (err != nil || got.Decision != "candidate") {
				t.Fatalf("recovered answer rejected: %+v %v", got, err)
			}
			if !recovers && (err == nil || !strings.Contains(err.Error(), "invalid_assessment")) {
				t.Fatalf("unparseable answers accepted: %v", err)
			}
		})
	}
}

func TestNativeClosesToolsAfterTheRoundCap(t *testing.T) {
	for _, keepsAsking := range []bool{false, true} {
		t.Run(fmt.Sprint("keeps_asking=", keepsAsking), func(t *testing.T) {
			s, _ := validFixture()
			server, calls := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
				if disabled := toolsDisabled(request); disabled != (call > MaxToolRounds) {
					t.Errorf("call %d: tools disabled=%t", call, disabled)
				}
				if closed := strings.Contains(requestText(request), "Tools are now closed"); closed != (call > MaxToolRounds) {
					t.Errorf("call %d: closing notice=%t", call, closed)
				}
				if call == MaxToolRounds+2 && !keepsAsking {
					if !strings.Contains(requestText(request), "Tools are closed") {
						t.Error("late tool call was not answered")
					}
					return answerTurn("```json\n" + cleanAnswer + "\n```")
				}
				return toolTurn(modelTurnCall{fmt.Sprint("list", call), "list_files", map[string]any{"revision": "H", "cursor": fmt.Sprint(call)}})
			})
			got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
			if keepsAsking {
				if !errors.Is(err, ErrInvestigationLimit) || *calls != MaxToolRounds+2 {
					t.Fatalf("calls=%d err=%v", *calls, err)
				}
				return
			}
			if err != nil || got.Decision != "candidate" || *calls != MaxToolRounds+2 {
				t.Fatalf("late tool call should be answered and the fenced answer accepted: %+v %v calls=%d", got, err, *calls)
			}
		})
	}
}

type recordingRepository struct {
	reads []ReadRequest
	text  string
}

func (r *recordingRepository) Read(_ context.Context, name string, req ReadRequest) (ReadResult, error) {
	if name == "read_diff" {
		return ReadResult{}, nil
	}
	if name == "read_file" && req.StartLine == preloadFullLines {
		return ReadResult{}, fmt.Errorf("invalid line range")
	}
	r.reads = append(r.reads, req)
	return ReadResult{Text: r.text}, nil
}

func TestNativeCapsToolCallsPerRoundAndResolvesRevisionAliases(t *testing.T) {
	s, _ := validFixture()
	repo := &recordingRepository{text: "file.txt\n"}
	server, _ := fakeModel(t, "openrouter", func(call int, request map[string]any) modelTurn {
		if call == 2 {
			if strings.Count(requestText(request), "Too many tool calls in one round") != 1 {
				t.Errorf("the call past the cap should get an error result: %s", requestText(request))
			}
			return answerTurn(cleanAnswer)
		}
		var calls []modelTurnCall
		for i := 0; i <= MaxToolCallsPerRound; i++ {
			calls = append(calls, modelTurnCall{fmt.Sprint("call", i), "list_files", map[string]any{"revision": "H", "path": fmt.Sprintf("dir%d", i)}})
		}
		return toolTurn(calls...)
	})
	budget := &testBudget{}
	if _, err := fakeInvestigator("openrouter", server).Investigate(context.Background(), s, repo, budget); err != nil {
		t.Fatal(err)
	}
	if len(repo.reads) != MaxToolCallsPerRound || budget.used.ToolCalls != MaxToolCallsPerRound {
		t.Fatalf("reads=%d budget=%+v", len(repo.reads), budget.used)
	}
	for _, read := range repo.reads {
		if read.Revision != s.Revision.Head {
			t.Fatalf("alias not resolved: %+v", read)
		}
	}
}

func TestInvestigationLimitsNameTheirCeiling(t *testing.T) {
	err := investigationLimit(LimitConversation, "conversation reached %d bytes", 250000)
	if !errors.Is(err, ErrInvestigationLimit) || LimitCode(err) != LimitConversation {
		t.Fatalf("err=%v code=%s", err, LimitCode(err))
	}
	if LimitCode(fmt.Errorf("wrapped: %w", err)) != LimitConversation {
		t.Fatal("wrapping lost the limit code")
	}
	if LimitCode(&ModelUsageLimitError{}) != LimitBudget {
		t.Fatal("a provider usage limit should report the token budget")
	}
}

func TestNativeReplaysThinkingBlocksWithToolResults(t *testing.T) {
	s, _ := validFixture()
	thinking := map[string]any{"type": "thinking", "thinking": "check the review", "signature": "sig"}
	server, calls := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
		if call == 1 {
			return modelTurn{thinking: []any{thinking}, calls: []modelTurnCall{{"read1", "list_files", map[string]any{"revision": "H"}}}}
		}
		replayed, _ := json.Marshal(request["messages"].([]any)[1].(map[string]any)["content"])
		if !strings.Contains(string(replayed), `"signature":"sig"`) {
			t.Errorf("thinking block not replayed: %s", replayed)
		}
		return modelTurn{thinking: []any{map[string]any{"type": "redacted_thinking", "data": "opaque"}}, text: cleanAnswer}
	})
	got, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, nil, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "candidate" || *calls != 2 {
		t.Fatalf("unexpected result: %+v calls=%d", got, *calls)
	}
}

type oversizedRepository struct{}

func (oversizedRepository) Read(_ context.Context, name string, _ ReadRequest) (ReadResult, error) {
	if name != "search_code" {
		return ReadResult{}, fmt.Errorf("unavailable")
	}
	return ReadResult{Text: strings.Repeat("x", 70000)}, nil
}

func TestNativeReturnsOversizedToolResultToTheModel(t *testing.T) {
	s, _ := validFixture()
	server, _ := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
		if call == 1 {
			return toolTurn(modelTurnCall{"search", "search_code", map[string]any{"revision": "H", "query": "x"}})
		}
		if !strings.Contains(requestText(request), "narrow the request") {
			t.Error("oversized result was not returned to the investigator")
		}
		return answerTurn(cleanAnswer)
	})
	result, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, oversizedRepository{}, &testBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != "candidate" {
		t.Fatalf("unexpected decision %s", result.Decision)
	}
}

func TestNativeAnswersARepeatedToolCallWithAPointer(t *testing.T) {
	s, _ := validFixture()
	var replayed string
	server, _ := fakeModel(t, "anthropic", func(call int, request map[string]any) modelTurn {
		if call == 1 {
			return toolTurn(modelTurnCall{"list1", "list_files", map[string]any{"revision": "H"}}, modelTurnCall{"list2", "list_files", map[string]any{"revision": "H"}})
		}
		replayed = requestText(request)
		return answerTurn(cleanAnswer)
	})
	if _, err := fakeInvestigator("anthropic", server).Investigate(context.Background(), s, &recordingRepository{text: "file.txt\n"}, &testBudget{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(replayed, "Identical to the result of an earlier call") != 1 {
		t.Fatalf("the second identical call should get a pointer, not the listing again: %s", replayed)
	}
}

func TestOpenRouterRequestBoundsReasoning(t *testing.T) {
	for _, budget := range []int{0, 6000} {
		t.Run(fmt.Sprint("budget=", budget), func(t *testing.T) {
			s, _ := validFixture()
			server, _ := fakeModel(t, "openrouter", func(_ int, request map[string]any) modelTurn {
				reasoning, present := request["reasoning"].(map[string]any)
				if max, _ := reasoning["max_tokens"].(float64); present != (budget > 0) || present && (int(max) != budget || reasoning["exclude"] != true) {
					t.Errorf("reasoning setting = %v", request["reasoning"])
				}
				if max, _ := request["max_tokens"].(float64); int(max) != CallOutputTokens {
					t.Errorf("max_tokens = %v", request["max_tokens"])
				}
				return answerTurn(cleanAnswer)
			})
			n := fakeInvestigator("openrouter", server)
			n.Config.ReasoningTokens = budget
			if _, err := n.Investigate(context.Background(), s, nil, &testBudget{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStripCodeFence(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}```":       `{"a":1}`,
		`{"a":1}`:                 `{"a":1}`,
	} {
		if got := stripCodeFence(in); got != want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}
