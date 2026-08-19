package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
)

const (
	// AgentBackendClaude preserves the original Claude Code review runner.
	AgentBackendClaude = "claude"

	// AgentBackendOpenRouter runs Codex CLI against OpenRouter's Responses API.
	AgentBackendOpenRouter = "openrouter"

	// DefaultOpenRouterAgentModel is the exact OpenRouter slug used when an
	// OpenRouter review does not explicitly set AgentConfig.Model.
	DefaultOpenRouterAgentModel = "openai/gpt-5.6-sol"

	// DefaultOpenRouterBaseURL is OpenRouter's OpenAI-compatible API root.
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
)

type agentStreamParser func(SpawnedProcess, io.Writer, int) (*agentParseResult, error)

// agentRuntime is the fully resolved, validated backend configuration for one
// review. Keeping this separate from AgentConfig lets us fail before cloning a
// repository when a backend is unknown or its credential is missing.
type agentRuntime struct {
	backend             string
	command             string
	model               string
	effort              string
	openRouterBaseURL   string
	parseStream         agentStreamParser
	reportsServingModel bool
}

func resolveAgentRuntime(cfg AgentConfig) (agentRuntime, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.Backend))
	if backend == "" {
		backend = AgentBackendClaude
	}

	effort := strings.TrimSpace(cfg.Effort)
	if effort == "" {
		effort = DefaultAgentEffort
	}

	switch backend {
	case AgentBackendClaude:
		model := strings.TrimSpace(cfg.Model)
		if model == "" {
			model = DefaultAgentModel
		}
		return agentRuntime{
			backend:             backend,
			command:             "claude",
			model:               model,
			effort:              effort,
			parseStream:         parseAgentStream,
			reportsServingModel: true,
		}, nil

	case AgentBackendOpenRouter:
		if strings.TrimSpace(cfg.OpenRouterAPIKey) == "" {
			return agentRuntime{}, errors.New("agent: OPENROUTER_API_KEY is required when AGENT_BACKEND=openrouter")
		}
		model := strings.TrimSpace(cfg.Model)
		if model == "" {
			model = DefaultOpenRouterAgentModel
		}
		baseURL := strings.TrimSpace(cfg.OpenRouterBaseURL)
		if baseURL == "" {
			baseURL = DefaultOpenRouterBaseURL
		}
		return agentRuntime{
			backend:             backend,
			command:             "codex",
			model:               model,
			effort:              effort,
			openRouterBaseURL:   baseURL,
			parseStream:         parseCodexStream,
			reportsServingModel: false,
		}, nil

	default:
		return agentRuntime{}, fmt.Errorf("agent: unsupported backend %q (want %q or %q)",
			cfg.Backend, AgentBackendClaude, AgentBackendOpenRouter)
	}
}

func (r agentRuntime) args(prompt string) []string {
	if r.backend == AgentBackendClaude {
		return []string{
			"-p", prompt,
			"--model", r.model,
			"--effort", r.effort,
			"--tools", "Read,Grep,Glob,Bash",
			"--permission-mode", "bypassPermissions",
			"--output-format", "stream-json",
			"--verbose", // required by `claude` when combining --print + stream-json
		}
	}

	// Every provider setting is an explicit CLI override so a developer's
	// ~/.codex/config.toml cannot silently route a review somewhere else.
	// env_key names the environment variable; the secret itself never appears
	// in argv or the raw JSONL log.
	return []string{
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--sandbox", "read-only",
		"-c", `approval_policy="never"`,
		"-c", `model_provider="openrouter"`,
		"-c", `model_providers.openrouter.name="OpenRouter"`,
		"-c", "model_providers.openrouter.base_url=" + strconv.Quote(r.openRouterBaseURL),
		"-c", `model_providers.openrouter.env_key="OPENROUTER_API_KEY"`,
		"-c", `model_providers.openrouter.wire_api="responses"`,
		"-c", "model_reasoning_effort=" + strconv.Quote(r.effort),
		// These settings affect model-invoked shell commands, not the Codex
		// process itself. Codex can still authenticate to OpenRouter, while its
		// shell tool cannot inspect credentials inherited from the server.
		"-c", `shell_environment_policy.ignore_default_excludes=false`,
		"-c", `shell_environment_policy.exclude=["DATABASE_URL","GOOGLE_APPLICATION_CREDENTIALS","GITHUB_TOKEN","GEMINI_API_KEY","OPENROUTER_API_KEY","ANTHROPIC_API_KEY","GITHUB_APP_PRIVATE_KEY_PATH","GITHUB_APP_CLIENT_SECRET","SESSION_SECRET"]`,
		"-m", r.model,
		prompt,
	}
}

// parseCodexStream parses `codex exec --json` JSONL. Codex reports the final
// response as a completed agent_message item and failures as turn.failed or
// error events. Unlike Claude's stream, it does not currently expose the
// serving model, so model verification is handled by the pinned runtime model.
func parseCodexStream(proc SpawnedProcess, logFile io.Writer, maxTurns int) (*agentParseResult, error) {
	result := &agentParseResult{}
	consecutiveExemptItems := 0
	exemptItemCeiling := 2*maxTurns + 16
	scanner := bufio.NewScanner(proc.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = logFile.Write(line)
		_, _ = logFile.Write([]byte{'\n'})
		if len(bytes.TrimSpace(line)) > 0 {
			result.lastEvent = truncate(string(line), 2000)
		}

		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		eventType, _ := ev["type"].(string)
		switch eventType {
		case "item.completed":
			item, _ := ev["item"].(map[string]any)
			itemType, _ := item["type"].(string)
			if itemType == "reasoning" || itemType == "agent_message" {
				consecutiveExemptItems++
				if consecutiveExemptItems > exemptItemCeiling {
					_ = proc.Kill()
					return result, fmt.Errorf("exceeded consecutive budget-exempt item ceiling (%d)", exemptItemCeiling)
				}
			} else if itemType != "" {
				consecutiveExemptItems = 0
			}
			// Preserve the completed terminal response. It represents the result of
			// prior work, not another work item, so it does not consume budget.
			if itemType == "agent_message" {
				result.assistantTurns++
				if text, ok := item["text"].(string); ok {
					result.finalOutput = text
				}
			}
			// Count completed concrete work items so MaxTurns remains a meaningful
			// work bound. Provider-internal reasoning and the terminal answer are
			// excluded because neither represents another tool/file operation.
			if itemType != "" && itemType != "reasoning" && itemType != "agent_message" {
				result.budgetUnits++
				if result.budgetUnits%5 == 0 || result.budgetUnits == 1 {
					log.Printf("[AGENT] turn-budget unit %d/%d (completed non-reasoning items)", result.budgetUnits, maxTurns)
				}
				if result.budgetUnits > maxTurns {
					_ = proc.Kill()
					return result, fmt.Errorf("exceeded max-turns (%d)", maxTurns)
				}
			}
		case "turn.failed":
			result.streamErr = codexErrorMessage(ev["error"], "turn failed")

		case "error":
			result.streamErr = codexErrorMessage(ev, "unspecified Codex error")
		}
	}

	if err := scanner.Err(); err != nil {
		_ = proc.Kill()
		return result, fmt.Errorf("read stdout: %w", err)
	}
	return result, nil
}

func codexErrorMessage(v any, fallback string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return truncate(s, 2000)
	}
	if obj, ok := v.(map[string]any); ok {
		if s, ok := obj["message"].(string); ok && strings.TrimSpace(s) != "" {
			return truncate(s, 2000)
		}
		if nested, exists := obj["error"]; exists {
			if msg := codexErrorMessage(nested, ""); msg != "" {
				return msg
			}
		}
	}
	if b, err := json.Marshal(v); err == nil && string(b) != "null" && string(b) != "{}" {
		return truncate(string(b), 2000)
	}
	return fallback
}
