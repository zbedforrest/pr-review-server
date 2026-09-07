package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	claudeCodeHelperFlag     = "GO_WANT_CLAUDE_CODE_HELPER_PROCESS"
	claudeCodeHelperScenario = "CLAUDE_CODE_HELPER_SCENARIO"
)

func TestMain(m *testing.M) {
	if os.Getenv(claudeCodeHelperFlag) == "1" {
		runClaudeCodeHelperProcess()
		return
	}
	os.Exit(m.Run())
}

func runClaudeCodeHelperProcess() {
	scenario := os.Getenv(claudeCodeHelperScenario)
	if scenario == "timeout" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	response := claudeCodeResponse{Type: "result", Subtype: "success"}
	cwd, _ := os.Getwd()
	response.Result = fmt.Sprintf("stdin=%d cwd=%s api_key=%s argv=%s", len(prompt), cwd, os.Getenv("ANTHROPIC_API_KEY"), strings.Join(os.Args[1:], "|"))
	response.Usage.InputTokens = 11
	response.Usage.CacheCreationInputTokens = 3
	response.Usage.CacheReadInputTokens = 5
	response.Usage.OutputTokens = 7
	switch scenario {
	case "is_error":
		response.IsError = true
		response.Subtype = "error_during_execution"
		response.Result = "provider rejected the request"
	case "non_success":
		response.Subtype = "max_turns"
		response.Result = "turn budget exhausted"
	case "invalid_json":
		fmt.Fprint(os.Stdout, "definitely not json")
		os.Exit(0)
	case "nonzero":
		response.Result = "process failed"
		fmt.Fprint(os.Stderr, "fake stderr details")
		_ = json.NewEncoder(os.Stdout).Encode(response)
		os.Exit(9)
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}

func newClaudeCodeHelperClient(t *testing.T, scenario, thinking string) *ClaudeCodeClient {
	t.Helper()
	t.Setenv("CLAUDE_CODE_COMMAND", os.Args[0])
	client := NewClaudeCodeClient("claude-test-model", thinking, false)
	client.environment = append(
		ChildEnvironment(os.Environ(), "ANTHROPIC_API_KEY", ""),
		claudeCodeHelperFlag+"=1",
		claudeCodeHelperScenario+"="+scenario,
	)
	return client
}

func TestClaudeCodeClientSuccessUsesStdinAndCountsTokens(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "metered-key-must-not-pass")
	client := newClaudeCodeHelperClient(t, "success", "high")
	prompt := strings.Repeat("large diff contents ", 100)

	review, promptTokens, candidateTokens, totalTokens, err := client.GetReview(prompt)
	require.NoError(t, err)
	assert.Contains(t, review, "stdin="+strconv.Itoa(len(prompt)))
	assert.NotContains(t, review, prompt)
	assert.Contains(t, review, "cwd="+os.TempDir())
	assert.Contains(t, review, "api_key= argv=")
	assert.Contains(t, review, "argv=-p|")
	assert.Contains(t, review, "--model|claude-test-model")
	assert.Contains(t, review, "--tools||")
	assert.Contains(t, review, "--max-turns|1")
	assert.Contains(t, review, "--output-format|json")
	assert.Contains(t, review, "--no-session-persistence")
	assert.Contains(t, review, "--system-prompt|"+claudeCodeSystemPrompt)
	assert.Contains(t, review, "--effort|high")
	assert.EqualValues(t, 19, promptTokens)
	assert.EqualValues(t, 7, candidateTokens)
	assert.EqualValues(t, 26, totalTokens)
}

func TestClaudeCodeClientOmitsEffortWhenThinkingUnset(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "success", "")
	review, _, _, _, err := client.GetReview("review me")
	require.NoError(t, err)
	assert.NotContains(t, review, "--effort")
}

func TestClaudeCodeClientReportsStructuredFailures(t *testing.T) {
	for _, scenario := range []string{"is_error", "non_success"} {
		t.Run(scenario, func(t *testing.T) {
			client := newClaudeCodeHelperClient(t, scenario, "")
			_, _, _, _, err := client.GetReview("review me")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "subtype=")
		})
	}
}

func TestClaudeCodeClientReportsInvalidJSON(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "invalid_json", "")
	_, _, _, _, err := client.GetReview("review me")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "definitely not json")
}

func TestClaudeCodeClientReportsNonZeroExit(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "nonzero", "")
	_, _, _, _, err := client.GetReview("review me")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subtype=\"success\"")
	assert.Contains(t, err.Error(), "fake stderr details")
}

func TestClaudeCodeClientTimeout(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "timeout", "")
	client.timeout = time.Second
	started := time.Now()
	_, _, _, _, err := client.GetReview("review me")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "timed out")
	assert.Less(t, time.Since(started), 5*time.Second)
}

func TestClaudeCodeClientGetReviewStreamWritesCompleteResult(t *testing.T) {
	client := newClaudeCodeHelperClient(t, "success", "")
	var output strings.Builder
	review, promptTokens, candidateTokens, totalTokens, err := client.GetReviewStream("review me", &output)
	require.NoError(t, err)
	assert.Equal(t, review, output.String())
	assert.EqualValues(t, 19, promptTokens)
	assert.EqualValues(t, 7, candidateTokens)
	assert.EqualValues(t, 26, totalTokens)
}

func TestClaudeCodeClientValidateAPIKeyChecksCommandOnly(t *testing.T) {
	client := &ClaudeCodeClient{command: os.Args[0]}
	require.NoError(t, client.ValidateAPIKey())
	client.command = "definitely-not-a-real-claude-command"
	assert.ErrorContains(t, client.ValidateAPIKey(), "not available on PATH")
}

func TestNewClaudeCodeClientTimeoutConfiguration(t *testing.T) {
	t.Setenv("FIRST_PASS_CLAUDE_CODE_TIMEOUT_SEC", "17")
	assert.Equal(t, 17*time.Second, NewClaudeCodeClient("model", "", false).timeout)

	for _, invalid := range []string{"", "0", "-1", "soon"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("FIRST_PASS_CLAUDE_CODE_TIMEOUT_SEC", invalid)
			assert.Equal(t, 900*time.Second, NewClaudeCodeClient("model", "", false).timeout)
		})
	}
}
