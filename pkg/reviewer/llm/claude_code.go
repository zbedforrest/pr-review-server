package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultClaudeCodeModel          = "claude-fable-5-1"
	defaultClaudeCodeCommand        = "claude"
	defaultClaudeCodeTimeoutSeconds = 900
	claudeCodeSystemPrompt          = "You are an expert code reviewer. Follow the instructions in the user message exactly and reply with the review only."
	claudeCodeDiagnosticLimit       = 500
)

// ClaudeCodeCommand returns the configured Claude Code executable name.
func ClaudeCodeCommand() string {
	if command := strings.TrimSpace(os.Getenv("CLAUDE_CODE_COMMAND")); command != "" {
		return command
	}
	return defaultClaudeCodeCommand
}

// ClaudeCodeClient runs a single-shot review through the Claude Code CLI.
type ClaudeCodeClient struct {
	command string
	model   string
	effort  string
	timeout time.Duration
	verbose bool

	// environment is a test seam for re-executing the Go test binary. A nil
	// value uses the production default-deny environment.
	environment []string
}

type claudeCodeResponse struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens              int32 `json:"input_tokens"`
		CacheCreationInputTokens int32 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int32 `json:"cache_read_input_tokens"`
		OutputTokens             int32 `json:"output_tokens"`
	} `json:"usage"`
}

func NewClaudeCodeClient(model, thinking string, verbose bool) *ClaudeCodeClient {
	timeoutSeconds := defaultClaudeCodeTimeoutSeconds
	if value := strings.TrimSpace(os.Getenv("FIRST_PASS_CLAUDE_CODE_TIMEOUT_SEC")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			timeoutSeconds = parsed
		}
	}
	return &ClaudeCodeClient{
		command: ClaudeCodeCommand(),
		model:   model,
		effort:  strings.ToLower(strings.TrimSpace(thinking)),
		timeout: time.Duration(timeoutSeconds) * time.Second,
		verbose: verbose,
	}
}

func (c *ClaudeCodeClient) GetReview(prompt string) (string, int32, int32, int32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	args := []string{
		"-p",
		"--model", c.model,
		"--tools", "",
		"--max-turns", "1",
		"--output-format", "json",
		"--no-session-persistence",
		"--system-prompt", claudeCodeSystemPrompt,
	}
	if c.effort != "" {
		args = append(args, "--effort", c.effort)
	}

	// A private project directory keeps any .claude/ settings, hooks or
	// CLAUDE.md left in the shared temp dir out of the first pass.
	workDir, err := os.MkdirTemp("", "claude-code-first-pass-")
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("create Claude Code working directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	cmd := exec.CommandContext(ctx, c.command, args...)
	cmd.Dir = workDir
	cmd.Env = c.environment
	if cmd.Env == nil {
		cmd.Env = ChildEnvironment(os.Environ(), "ANTHROPIC_API_KEY", "")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("prepare Claude Code stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", 0, 0, 0, fmt.Errorf("start Claude Code command %q: %w", c.command, err)
	}
	writeResult := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		closeErr := stdin.Close()
		if writeErr != nil {
			writeResult <- writeErr
			return
		}
		writeResult <- closeErr
	}()
	runErr := cmd.Wait()
	writeErr := <-writeResult
	if ctx.Err() == context.DeadlineExceeded {
		return "", 0, 0, 0, fmt.Errorf("Claude Code first pass timed out after %s", c.timeout)
	}
	if c.verbose {
		log.Printf("Claude Code first-pass stdout: %s", diagnosticSnippet(stdout.String()))
	}
	if writeErr != nil && runErr == nil {
		return "", 0, 0, 0, fmt.Errorf("write Claude Code prompt to stdin: %w", writeErr)
	}

	var response claudeCodeResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		// A crashed or misconfigured CLI usually reports on stderr with an
		// empty or non-JSON stdout; surface both so the failure is diagnosable.
		return "", 0, 0, 0, fmt.Errorf("decode Claude Code JSON output (exit_error=%v): %w; stdout=%q stderr=%q",
			runErr, err, diagnosticSnippet(stdout.String()), diagnosticSnippet(stderr.String()))
	}
	if runErr != nil || response.Type != "result" || response.IsError || response.Subtype != "success" || strings.TrimSpace(response.Result) == "" {
		return "", 0, 0, 0, fmt.Errorf(
			"Claude Code first pass failed (type=%q subtype=%q is_error=%t exit_error=%v): result=%q stderr=%q",
			response.Type, response.Subtype, response.IsError, runErr,
			diagnosticSnippet(response.Result), diagnosticSnippet(stderr.String()),
		)
	}

	promptTokens := response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens
	candidateTokens := response.Usage.OutputTokens
	return response.Result, promptTokens, candidateTokens, promptTokens + candidateTokens, nil
}

// GetReviewStream writes once after the CLI's non-incremental JSON response
// has completed; Claude Code JSON mode does not expose review text chunks.
func (c *ClaudeCodeClient) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	review, promptTokens, candidateTokens, totalTokens, err := c.GetReview(prompt)
	if err != nil {
		return "", 0, 0, 0, err
	}
	if _, err := io.WriteString(w, review); err != nil {
		return "", 0, 0, 0, fmt.Errorf("write Claude Code review: %w", err)
	}
	return review, promptTokens, candidateTokens, totalTokens, nil
}

// ValidateAPIKey checks local CLI availability. Claude Code owns its OAuth
// session, so validation deliberately performs no credential or network call.
func (c *ClaudeCodeClient) ValidateAPIKey() error {
	if _, err := exec.LookPath(c.command); err != nil {
		return fmt.Errorf("Claude Code command %q is not available on PATH: %w", c.command, err)
	}
	return nil
}

func diagnosticSnippet(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= claudeCodeDiagnosticLimit {
		return string(runes)
	}
	return string(runes[:claudeCodeDiagnosticLimit]) + "..."
}
