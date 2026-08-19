package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/reviewer/types"
)

// DefaultAgentModel is the Claude model used when AgentConfig.Model is empty.
const DefaultAgentModel = "claude-opus-4-8"

// DefaultAgentEffort is the reasoning effort used when AgentConfig.Effort is
// empty. Kept at the historical hardcoded value for both backends.
const DefaultAgentEffort = "medium"

// AgentConfig holds runtime knobs for a single agent-review invocation.
type AgentConfig struct {
	CloneRootDir      string        // parent dir for per-invocation clones
	LogsDir           string        // parent dir for raw stream-json logs
	WallClock         time.Duration // hard wall-clock timeout
	MaxTurns          int           // abort after this many backend-specific turn-budget units
	GitHubToken       string        // optional; HTTPS clone auth
	Backend           string        // claude (default) or openrouter
	Model             string        // backend model id; defaults according to Backend
	Effort            string        // backend reasoning effort; defaults to DefaultAgentEffort
	OpenRouterAPIKey  string        // frozen deployment credential; injected into the Codex child environment
	OpenRouterBaseURL string        // optional OpenRouter API root; used only by the openrouter backend

	// BugMemory is the optional pattern library (nil = feature off). The
	// matcher excludes entries sourced from the PR under review; see
	// bugmemory.go.
	BugMemory *BugMemoryLibrary

	// RequiredChecks converts fired gates + matched memory entries into
	// forced-choice checks the agent must answer with evidence, enforced
	// deterministically post-parse (see checks.go). Off by default; when off
	// the prompt and outputs are byte-identical to a checkless run.
	RequiredChecks bool

	// FailureLogSink, when non-nil, is called synchronously with the raw
	// stream-json log path after a failed run, before the error returns —
	// LogsDir is ephemeral, so this is the log's only path off the instance.
	FailureLogSink func(logPath string)

	// AttemptObserver receives a started event immediately before Spawn and one
	// terminal event afterward under the same natural key. Clone and prompt-build
	// failures emit nothing because execution never reached a provider attempt;
	// ErrProviderAttemptAborted prevents the spawn entirely.
	AttemptObserver ProviderAttemptObserver
}

// AgentReview is the result of a successful agent run.
type AgentReview struct {
	Comments  []types.LineComment // parsed from the agent's final JSON response
	Gates     []types.LineComment // mechanical gate findings (advisory, provenance "mechanical")
	BugMemory BugMemoryMatch      // which memory entries were injected/excluded (telemetry)

	// Checks and CheckFindings carry the required-check enforcement output
	// (see checks.go): funnel telemetry, and the deterministic escalations
	// the caller merges as a lower-priority FindingSet. Both are zero-valued
	// when the feature is off.
	Checks        RequiredCheckTelemetry
	CheckFindings []types.LineComment

	RawFinal string // the agent's final result text (pre-parse, for debugging)
	CloneDir string // path to the per-invocation clone (kept for inspection)
	LogPath  string // where the raw stream-json was written (removed by then — /tmp hygiene)

	// Model verification: Claude reports the serving model in init + assistant
	// events. Codex JSONL does not, so OpenRouter reports its exact pinned
	// request model. ModelFallback means a reported model did not satisfy the
	// request — the review still publishes, but callers surface it loudly.
	RequestedModel string
	ServedModel    string
	// ObservedServedModels preserves every model identity reported by the
	// provider stream, including mid-run switches.
	ObservedServedModels []string
	ModelFallback        bool
	Backend              string
	Effort               string
	AssistantTurns       int
	DurationMS           int64
	// ServingModelVerified is true only when the agent stream explicitly
	// reported the model that served the request. Codex/OpenRouter currently
	// pins the requested model but does not expose the routed model in JSONL.
	ServingModelVerified bool
}

// Spawner abstracts subprocess creation so tests can stub the agent CLI.
type Spawner interface {
	Spawn(ctx context.Context, name string, args []string, dir string) (SpawnedProcess, error)
}

// EnvironmentSpawner is required by backends whose frozen credential must be
// injected into the child without placing it in argv. The slice is the complete
// child environment, already filtered and deduplicated by openRouterEnvironment.
type EnvironmentSpawner interface {
	SpawnWithEnv(ctx context.Context, name string, args []string, dir string, environment []string) (SpawnedProcess, error)
}

// SpawnedProcess is what a Spawner returns.
type SpawnedProcess interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
	Kill() error
}

// RunAgentReview clones the PR branch, spawns the configured agent CLI, parses
// its JSONL output, and returns the assembled markdown. On any failure
// (clone error, timeout, turn-cap hit, non-zero exit) it returns a descriptive
// error — caller is expected to surface it loud.
//
// defaultBranch is the name of the PR's base branch (e.g. "master") — used
// for the initial clone. The PR's head commit is then fetched via the
// `pull/<N>/head` refspec and checked out.
func RunAgentReview(
	ctx context.Context,
	agentCfg AgentConfig,
	spawner Spawner,
	owner, repo, defaultBranch string,
	prNumber int,
	commitSHA string,
	geminiComments []types.LineComment,
) (review *AgentReview, returnErr error) {
	if agentCfg.MaxTurns <= 0 {
		return nil, errors.New("agent: MaxTurns must be > 0")
	}
	if agentCfg.WallClock <= 0 {
		return nil, errors.New("agent: WallClock must be > 0")
	}
	runtime, err := resolveAgentRuntime(agentCfg)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(agentCfg.CloneRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create clone root: %w", err)
	}
	if err := os.MkdirAll(agentCfg.LogsDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: create logs dir: %w", err)
	}

	slug := fmt.Sprintf("%s__%s__pr%d__%d", owner, repo, prNumber, time.Now().UnixNano())
	cloneDir := filepath.Join(agentCfg.CloneRootDir, slug)
	logPath := filepath.Join(agentCfg.LogsDir, slug+".jsonl")

	logPrefix := fmt.Sprintf("[AGENT %s/%s#%d]", owner, repo, prNumber)
	log.Printf("%s starting (clone=%s, log=%s, wall_clock=%s, max_turns=%d, gemini_comments=%d)",
		logPrefix, cloneDir, logPath, agentCfg.WallClock, agentCfg.MaxTurns, len(geminiComments))

	// Single wall-clock budget covers BOTH the clone and the agent subprocess.
	// That way a slow clone can't burn the budget and leave nothing for thinking
	// (or worse, run unbounded under the outer context).
	runCtx, cancel := context.WithTimeout(ctx, agentCfg.WallClock)
	defer cancel()

	cloneStart := time.Now()
	cleanupClone, err := cloneForAgent(runCtx, agentCfg.CloneRootDir, cloneDir, owner, repo, defaultBranch, prNumber, commitSHA, agentCfg.GitHubToken)
	if err != nil {
		log.Printf("%s clone FAILED after %s: %v", logPrefix, time.Since(cloneStart), err)
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("agent: wall-clock timeout (%s) during clone", agentCfg.WallClock)
		}
		return nil, fmt.Errorf("agent: clone: %w", err)
	}
	// Remove the per-run worktree once we're done with it. On Cloud Run the
	// clones live on /tmp (= memory), so leaking a per-review worktree on
	// every successful run would OOM the instance after a handful of large-
	// repo reviews.
	defer func() {
		if cerr := cleanupClone(); cerr != nil {
			log.Printf("%s WARN: worktree cleanup failed: %v", logPrefix, cerr)
		}
	}()
	log.Printf("%s clone ok (%s) at %s", logPrefix, time.Since(cloneStart), cloneDir)

	// Parse the PR diff once; the same []diffFile feeds both the mechanical
	// gates and the bug-memory matcher, so offline dry-runs of either are
	// predictive of production behavior.
	diffFiles := diffFilesForWorktree(runCtx, cloneDir, defaultBranch, agentCfg.GitHubToken, owner+"/"+repo, prNumber)

	// Mechanical gates: cheap deterministic checks over the diff + worktree.
	// Their findings go into the prompt (the agent must address each) AND are
	// returned for the reconciliation merge, so they survive dismissal.
	var gates []types.LineComment
	if diffFiles != nil {
		gates = RunMechanicalGates(runCtx, cloneDir, diffFiles)
	}
	if len(gates) > 0 {
		log.Printf("%s mechanical gates fired: %d", logPrefix, len(gates))
	}

	// Bug memory: patterns from this deployment's past bugs, matched to the
	// touched areas. Entries sourced from this exact PR are excluded by the
	// matcher (structural leave-one-out).
	memEntries, memMatch := MatchBugMemory(agentCfg.BugMemory, diffFiles, owner, repo, prNumber)
	if len(memMatch.Matched) > 0 || len(memMatch.Excluded) > 0 {
		log.Printf("%s bug memory: injected=%v excluded_leakage=%v (version %s)",
			logPrefix, memMatch.Matched, memMatch.Excluded, memMatch.Version)
	}

	// Required checks: forced-choice questions derived from the gates and
	// memory entries above, answered in the findings JSON and enforced
	// post-parse. Feature-gated; an empty list leaves the prompt unchanged.
	var checks []RequiredCheck
	if agentCfg.RequiredChecks {
		checks = BuildRequiredChecks(gates, memEntries, diffFiles)
	}
	if len(checks) > 0 {
		ids := make([]string, len(checks))
		for i, c := range checks {
			ids[i] = c.ID
		}
		log.Printf("%s required checks issued: %v", logPrefix, ids)
	}

	prompt, err := buildAgentPromptContent(defaultBranch, diffFiles, geminiComments, gates, memEntries, checks)
	if err != nil {
		return nil, fmt.Errorf("agent: build prompt: %w", err)
	}

	args := runtime.args(prompt)

	// Log the argv without the full prompt (too big; promptAgentReview is static
	// and the comment list is in geminiComments count above).
	log.Printf("%s spawning %s (backend=%s, model=%s, effort=%s, prompt_chars=%d)",
		logPrefix, runtime.command, runtime.backend, runtime.model, runtime.effort, len(prompt))

	agentStartedAt := time.Now().UTC()
	startedEvent := ProviderAttemptEvent{
		Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
		Provider: agentProviderName(runtime.backend), Backend: runtime.backend,
		RequestedModel: runtime.model, ResolvedModel: runtime.model, Effort: runtime.effort,
		StartedAt: &agentStartedAt, Status: "started", MatcherVersion: "v1",
	}
	if observerErr := observeProviderAttempt(agentCfg.AttemptObserver, startedEvent); errors.Is(observerErr, ErrProviderAttemptAborted) {
		return nil, fmt.Errorf("agent: %w", observerErr)
	}
	var proc SpawnedProcess
	if runtime.backend == AgentBackendOpenRouter {
		envSpawner, ok := spawner.(EnvironmentSpawner)
		if !ok {
			err = errors.New("agent: OpenRouter spawner does not support frozen credential injection")
		} else {
			proc, err = envSpawner.SpawnWithEnv(runCtx, runtime.command, args, cloneDir,
				openRouterEnvironment(os.Environ(), agentCfg.OpenRouterAPIKey))
		}
	} else {
		proc, err = spawner.Spawn(runCtx, runtime.command, args, cloneDir)
	}
	if err != nil {
		log.Printf("%s spawn FAILED: %v", logPrefix, err)
		completedAt := time.Now().UTC()
		startedEvent.CompletedAt = &completedAt
		startedEvent.DurationMS = completedAt.Sub(agentStartedAt).Milliseconds()
		startedEvent.Status = "failed"
		startedEvent.StopReason, startedEvent.ErrorCode = agentAttemptFailure(err, runCtx)
		if runCtx.Err() == nil {
			startedEvent.StopReason = "spawn_error"
			startedEvent.ErrorCode = "spawn_failed"
		}
		startedEvent.ErrorSummary = err.Error()
		_ = observeProviderAttempt(agentCfg.AttemptObserver, startedEvent)
		return nil, fmt.Errorf("agent: spawn %s: %w", runtime.command, err)
	}
	log.Printf("%s %s spawned, streaming output to %s", logPrefix, runtime.command, logPath)
	var agentCompletedAt time.Time
	var parseResult *agentParseResult
	defer func() {
		completedAt := agentCompletedAt
		if completedAt.IsZero() {
			completedAt = time.Now().UTC()
		}
		event := ProviderAttemptEvent{
			Stage: "agent", InvocationNumber: 1, AttemptNumber: 1,
			Provider: agentProviderName(runtime.backend), Backend: runtime.backend,
			RequestedModel: runtime.model, ResolvedModel: runtime.model, Effort: runtime.effort,
			StartedAt: &agentStartedAt, CompletedAt: &completedAt,
			DurationMS: completedAt.Sub(agentStartedAt).Milliseconds(),
			Status:     "completed", StopReason: "completed", MatcherVersion: "v1",
		}
		if parseResult != nil {
			event.AssistantTurns = parseResult.assistantTurns
			event.ObservedServedModels = append([]string(nil), parseResult.servedModels...)
			event.PrimaryServedModel, event.ServedModelSource, event.ServingModelVerified,
				event.Fallback, event.FallbackReason = agentServingMetadata(runtime, parseResult.servedModels)
		}
		if review != nil {
			event.AssistantTurns = review.AssistantTurns
			event.ObservedServedModels = append([]string(nil), review.ObservedServedModels...)
			event.PrimaryServedModel = review.ServedModel
			event.ServingModelVerified = review.ServingModelVerified
			event.Fallback = review.ModelFallback
			if review.ServingModelVerified {
				event.ServedModelSource = "stream"
			} else if review.ServedModel != "" {
				event.ServedModelSource = "pinned_request"
			}
			if review.ModelFallback {
				event.FallbackReason = "observed served model did not match requested model"
			}
		}
		if returnErr != nil {
			event.Status = "failed"
			event.StopReason, event.ErrorCode = agentAttemptFailure(returnErr, runCtx)
			event.ErrorSummary = returnErr.Error()
		} else if review == nil {
			// A panic after Spawn runs defers with both named returns still nil.
			// Preserve the panic while ensuring durable telemetry cannot claim a
			// provider invocation completed successfully without a result.
			event.Status = "failed"
			event.StopReason = "panic"
			event.ErrorCode = "execution_panicked"
			event.ErrorSummary = "agent execution panicked before producing a result"
		}
		_ = observeProviderAttempt(agentCfg.AttemptObserver, event)
	}()

	logFile, err := os.Create(logPath)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("agent: create log: %w", err)
	}
	defer logFile.Close()

	// LogsDir is /tmp (= instance memory on Cloud Run): remove the jsonl once
	// it isn't the sole record of the run. No sink (local dev) = keep on failure.
	logRemovable := false
	defer func() {
		if logRemovable {
			_ = os.Remove(logPath)
		}
	}()

	// Drain stderr to a buffer so we can include it on failure. Safe to be
	// unbounded for now — dev use, sensible agent outputs.
	var stderrBuf strings.Builder
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		_, _ = io.Copy(&stderrBuf, proc.Stderr())
	}()

	// Stream stdout: tee to log file and parse turn-by-turn.
	var parseErr error
	parseResult, parseErr = runtime.parseStream(proc, logFile, agentCfg.MaxTurns)

	waitErr := proc.Wait()
	agentCompletedAt = time.Now().UTC()
	stderrWG.Wait()

	persistFailureLog := func() {
		if agentCfg.FailureLogSink == nil {
			return
		}
		_ = logFile.Sync()
		agentCfg.FailureLogSink(logPath)
		logRemovable = true
	}

	if parseErr != nil {
		// Turn-cap hit or parse error — subprocess already killed inside parser.
		persistFailureLog()
		return nil, fmt.Errorf("agent: %w (stderr: %s)", parseErr, truncate(stderrBuf.String(), 1000))
	}

	if runCtx.Err() == context.DeadlineExceeded {
		persistFailureLog()
		return nil, fmt.Errorf("agent: wall-clock timeout (%s) after %d turns (stderr: %s)",
			agentCfg.WallClock, parseResult.assistantTurns, truncate(stderrBuf.String(), 1000))
	}

	if waitErr != nil {
		persistFailureLog()
		return nil, fmt.Errorf("agent: %s exited with error: %w (stream: %s) (stderr: %s)",
			runtime.command, waitErr, truncate(parseResult.diagnostic(), 1000), truncate(stderrBuf.String(), 1000))
	}

	// The CLI can exit 0 after an error result event; ungated, the error text
	// would publish as a "successful" SUMMARY review.
	if parseResult.streamErr != "" {
		persistFailureLog()
		return nil, fmt.Errorf("agent: CLI reported error in stream: %s (stderr: %s)",
			truncate(parseResult.streamErr, 1000), truncate(stderrBuf.String(), 1000))
	}

	if parseResult.finalOutput == "" {
		log.Printf("%s %s finished with no final result after %d turn(s)", logPrefix, runtime.command, parseResult.assistantTurns)
		persistFailureLog()
		return nil, fmt.Errorf("agent: no final result emitted after %d turn(s) (stream: %s) (stderr: %s)",
			parseResult.assistantTurns, truncate(parseResult.diagnostic(), 1000), truncate(stderrBuf.String(), 1000))
	}

	comments, parseErr := parseAgentJSON(parseResult.finalOutput)
	if parseErr != nil {
		log.Printf("%s final output is not valid JSON (%v); wrapping as SUMMARY", logPrefix, parseErr)
		comments = []types.LineComment{{
			FilePath:    "SUMMARY",
			LineNumber:  0,
			CommentBody: parseResult.finalOutput,
		}}
	}

	// Required-check enforcement must run here, while the worktree still
	// exists — the evidence validator stats cited paths against it. A parse
	// failure above means no CHECK answers survive, so every issued check
	// escalates as unanswered (the safe default).
	var checkTel RequiredCheckTelemetry
	var checkFindings []types.LineComment
	if len(checks) > 0 {
		var answers []CheckAnswer
		comments, answers = ParseCheckAnswers(comments)
		diffPaths := make([]string, len(diffFiles))
		for i, f := range diffFiles {
			diffPaths[i] = f.Path
		}
		comments, gates, checkFindings, checkTel = EnforceRequiredChecks(checks, answers, comments, gates, diffPaths, cloneDir)
		log.Printf("%s required checks: issued=%d answered=%d violated=%d evidence_ok=%d escalated=%d",
			logPrefix, checkTel.ChecksIssued, checkTel.ChecksAnswered, checkTel.ChecksViolated,
			checkTel.ChecksEvidenceOK, len(checkFindings))
	}
	// Fallback if ANY served model fails to match — a transient fallback
	// that recovers mid-run still ran turns on the wrong model. ServedModel
	// reports the offender (or the primary model on a clean run).
	servedModel := ""
	modelFallback := false
	if len(parseResult.servedModels) > 0 {
		servedModel = parseResult.servedModels[0]
	} else if !runtime.reportsServingModel {
		// Codex's stable JSONL schema does not expose the response model. The
		// OpenRouter backend pins one exact model slug and does not configure a
		// model fallback list, so report that request while keeping the
		// telemetry limitation explicit in logs.
		servedModel = runtime.model
		log.Printf("%s serving model not present in %s stream; using pinned request model %s",
			logPrefix, runtime.command, runtime.model)
	} else {
		// Fail-open for a monitoring feature: make a silent regression in
		// the CLI's model reporting visible in logs.
		log.Printf("%s WARNING: stream reported no serving model — fallback detection skipped", logPrefix)
	}
	for _, m := range parseResult.servedModels {
		if !modelMatches(runtime.model, m) {
			servedModel = m
			modelFallback = true
			break
		}
	}
	if modelFallback {
		log.Printf("%s WARNING: MODEL FALLBACK: requested=%s served=%s (all seen: %v) — review ran on the wrong model",
			logPrefix, runtime.model, servedModel, parseResult.servedModels)
	}

	agentDuration := agentCompletedAt.Sub(agentStartedAt)
	log.Printf("%s complete in %s (turns=%d, comments=%d, model=%s)",
		logPrefix, agentDuration, parseResult.assistantTurns, len(comments), servedModel)

	logRemovable = true
	return &AgentReview{
		Comments:             comments,
		Gates:                gates,
		BugMemory:            memMatch,
		Checks:               checkTel,
		CheckFindings:        checkFindings,
		RawFinal:             parseResult.finalOutput,
		CloneDir:             cloneDir,
		LogPath:              logPath,
		RequestedModel:       runtime.model,
		ServedModel:          servedModel,
		ObservedServedModels: append([]string(nil), parseResult.servedModels...),
		ModelFallback:        modelFallback,
		Backend:              runtime.backend,
		Effort:               runtime.effort,
		AssistantTurns:       parseResult.assistantTurns,
		DurationMS:           agentDuration.Milliseconds(),
		ServingModelVerified: runtime.reportsServingModel && len(parseResult.servedModels) > 0,
	}, nil
}

func agentProviderName(backend string) string {
	if backend == AgentBackendOpenRouter {
		return "openrouter"
	}
	return "anthropic"
}

func agentServingMetadata(runtime agentRuntime, servedModels []string) (primary, source string, verified, fallback bool, fallbackReason string) {
	if len(servedModels) > 0 {
		primary = servedModels[0]
		source = "stream"
		verified = runtime.reportsServingModel
	} else if !runtime.reportsServingModel {
		primary = runtime.model
		source = "pinned_request"
	}
	for _, served := range servedModels {
		if !modelMatches(runtime.model, served) {
			primary = served
			fallback = true
			fallbackReason = "observed served model did not match requested model"
			break
		}
	}
	return
}

func agentAttemptFailure(err error, runCtx context.Context) (stopReason, errorCode string) {
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "wall_clock_timeout", "deadline_exceeded"
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		return "cancelled", "context_canceled"
	}
	if strings.Contains(err.Error(), "max-turns") {
		return "max_turns", "max_turns_exceeded"
	}
	if strings.Contains(err.Error(), "exited with error") {
		return "process_error", "process_exit_error"
	}
	if strings.Contains(err.Error(), "no final result") {
		return "missing_result", "missing_result"
	}
	if strings.Contains(err.Error(), "CLI reported error") {
		return "provider_error", "provider_error"
	}
	return "error", "agent_failed"
}

// parseAgentJSON extracts a []LineComment from the agent's final output.
// Defensive against three failure modes we've observed in agent transcripts:
//   - ```json or plain ``` code fences wrapping the array
//   - conversational prefix ("Here is the review:\n[...]")
//   - conversational suffix after the array
//
// Strategy: locate the outermost top-level JSON array bracket pair and
// parse only that span. If the model emits something that doesn't contain
// a valid array span, return the unmarshal error from the trimmed slice
// so the caller's diagnostic still points at the malformed content.
func parseAgentJSON(raw string) ([]types.LineComment, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	// If there's a top-level array, slice to it by BRACKET BALANCE from the
	// first `[` — not to the last `]` in the string. The last-`]` heuristic
	// broke whenever conversational suffix text contained a bracket (e.g. a
	// trailing "```suggestion … arr[i] …" block): the slice then spanned
	// prose, json.Unmarshal failed, and the whole review collapsed into a
	// single SUMMARY blob. Measured on benchmark runs this destroyed the
	// structured output of ~17-25%% of reviews under some configs.
	if start := strings.Index(trimmed, "["); start >= 0 {
		if end := matchBracket(trimmed, start); end > start {
			trimmed = trimmed[start : end+1]
		}
	}

	var comments []types.LineComment
	if err := json.Unmarshal([]byte(trimmed), &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// matchBracket returns the index of the `]` closing the `[` at start,
// tracking nesting and skipping bracket characters inside JSON strings
// (respecting backslash escapes). Returns -1 if unbalanced.
func matchBracket(s string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// prScopeSection names the PR's base branch and lists the changed files so
// the agent's own review pass diffs against the right base. Without it the
// agent has to guess (origin/HEAD, "master"), which on a stacked PR pulls
// parent-branch changes into scope. Empty inputs contribute nothing.
func prScopeSection(baseBranch string, files []diffFile) string {
	if baseBranch == "" && len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n--- PR SCOPE ---\n")
	if baseBranch != "" {
		fmt.Fprintf(&b, "The PR's base branch is `origin/%s`. The change under review is exactly `git diff origin/%s...HEAD`. Do not diff against origin/HEAD or any other branch — on a stacked PR that drags in parent-branch changes that are out of scope, and code the PR does not touch must not be flagged.\n", baseBranch, baseBranch)
	}
	if len(files) > 0 {
		b.WriteString("Changed files (+added/-removed lines):\n")
		// Cap the listing so a sprawling refactor can't crowd out the rest of
		// the prompt; the agent can still enumerate the tail via git.
		const maxListed = 100
		for i, f := range files {
			if i == maxListed {
				fmt.Fprintf(&b, "- ...and %d more (see the diff for the full list)\n", len(files)-maxListed)
				break
			}
			fmt.Fprintf(&b, "- %s (%s, +%d/-%d)\n", f.Path, f.Status, len(f.Added), len(f.Removed))
		}
	}
	return b.String()
}

// buildAgentPromptContent assembles the agent prompt: the static template,
// the PR-scope section (base branch + changed files, if known), the
// mechanical-gate alerts (if any), the bug-history section (if any
// memory entries matched), the required-checks block (if the feature issued
// any), then a JSON block of Gemini comments. With no scope, no gates, no
// matches and no checks the prompt is byte-identical to a memoryless,
// checkless build.
func buildAgentPromptContent(baseBranch string, diffFiles []diffFile, geminiComments, gates []types.LineComment, bugHistory []BugMemoryEntry, checks []RequiredCheck) (string, error) {
	commentsJSON, err := json.MarshalIndent(geminiComments, "", "  ")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(promptAgentReview)
	b.WriteString(prScopeSection(baseBranch, diffFiles))
	if len(gates) > 0 {
		b.WriteString("\n--- MECHANICAL ALERTS (deterministic checks; explicitly address each in your review) ---\n")
		for _, g := range gates {
			b.WriteString("- [" + g.FilePath + "] " + g.CommentBody + "\n")
		}
	}
	b.WriteString(bugMemorySection(bugHistory))
	b.WriteString(requiredChecksSection(checks))
	b.WriteString("\n--- GEMINI COMMENTS (JSON) ---\n")
	b.Write(commentsJSON)
	return b.String(), nil
}

// cacheMutexes serializes cache-repo work per (owner, repo). Each repo gets
// its own mutex so concurrent reviews of *different* repos don't block each
// other, but two reviews of the same repo share the cache fetch step.
var cacheMutexes sync.Map

func cacheLock(key string) *sync.Mutex {
	m, _ := cacheMutexes.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// cloneForAgent prepares a working directory for the agent by:
//
//  1. Ensuring a per-repo cache clone exists at <cloneRoot>/.cache/<owner>__<repo>.
//     First time: full shallow clone (the slow step). Subsequent: skipped.
//  2. Fetching the PR's head ref into the cache (cheap incremental fetch).
//  3. Creating a `git worktree` at the requested commit in <dir>. Worktrees
//     share the cache's object store, so this step is near-instant even on
//     monorepos.
//
// On success returns a cleanup function the caller MUST defer to remove the
// worktree (both the on-disk files and git's worktree registration). On
// failure cleanup is a no-op (the partial state is already cleaned by the
// failure path) and the returned error wraps git's output.
//
// The cache step holds a per-repo mutex so concurrent reviews of the same
// repo serialize their fetches; reviews of *different* repos run in parallel.
//
// Each step logs a START / DONE pair with a duration so it's obvious where
// any future slowness lives.
func cloneForAgent(ctx context.Context, cloneRoot, dir, owner, repo, defaultBranch string, prNumber int, commitSHA, token string) (cleanup func() error, err error) {
	noopCleanup := func() error { return nil }

	// Absolute paths everywhere — git's `worktree add <relative-path>` resolves
	// the path against the cmd's cwd, which would land worktrees inside the
	// cache dir. Make the cwd-binding moot.
	absCloneRoot, err := filepath.Abs(cloneRoot)
	if err != nil {
		return noopCleanup, fmt.Errorf("abs cloneRoot: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return noopCleanup, fmt.Errorf("abs worktree dir: %w", err)
	}

	cacheKey := fmt.Sprintf("%s/%s", owner, repo)
	cacheDir := filepath.Join(absCloneRoot, ".cache", fmt.Sprintf("%s__%s", owner, repo))
	sanitizedURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	logPrefix := fmt.Sprintf("[AGENT %s/%s#%d]", owner, repo, prNumber)

	mu := cacheLock(cacheKey)
	mu.Lock()
	defer mu.Unlock()

	// Step 1: ensure cache repo exists.
	cacheGitDir := filepath.Join(cacheDir, ".git")
	if _, err := os.Stat(cacheGitDir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
			return noopCleanup, fmt.Errorf("create cache parent: %w", err)
		}
		log.Printf("%s cache MISS — initial clone of %s -> %s (depth=200) START", logPrefix, sanitizedURL, cacheDir)
		t0 := time.Now()
		// Auth via http.extraheader rather than embedding in the URL: the
		// token stays out of .git/config (and so out of view of the agent's
		// Read tool), and out of the persisted clone URL git echoes on
		// failures. Argv exposure during the brief git invocation remains.
		cloneArgs := authHeaderArgs(token)
		// --quiet suppresses progress/banner output that CombinedOutput()
		// would otherwise buffer in memory for the duration of the clone.
		cloneArgs = append(cloneArgs, "clone", "--quiet", "--depth", "200")
		if defaultBranch != "" {
			cloneArgs = append(cloneArgs, "--branch", defaultBranch)
		}
		cloneArgs = append(cloneArgs, sanitizedURL, cacheDir)
		if out, err := runGit(ctx, "", cloneArgs...); err != nil {
			// Remove the partial directory so a future run won't see a half-clone
			// as a cache hit and try to fetch into a corrupt repo. Diagnostics
			// are in the returned error + log, not the on-disk leftovers.
			if rmErr := os.RemoveAll(cacheDir); rmErr != nil {
				log.Printf("%s WARN: failed to clean partial cache dir %s: %v", logPrefix, cacheDir, rmErr)
			}
			return noopCleanup, fmt.Errorf("git clone (cache init): %w (%s)", err, redactToken(out, token))
		}
		log.Printf("%s cache initial clone DONE in %s", logPrefix, time.Since(t0))
	} else if err != nil {
		return noopCleanup, fmt.Errorf("stat cache .git: %w", err)
	} else {
		log.Printf("%s cache HIT at %s", logPrefix, cacheDir)
	}

	// Step 2: fetch the PR head ref into the cache. Use a `+` refspec to force
	// update if the PR was rebased/force-pushed since last fetch. We also keep
	// it shallow at depth=200 to bound size.
	fetchSpec := fmt.Sprintf("+pull/%d/head:refs/agent-pr/%d", prNumber, prNumber)
	log.Printf("%s git fetch origin %s (in cache) START", logPrefix, fetchSpec)
	t1 := time.Now()
	fetchArgs := authHeaderArgs(token)
	fetchArgs = append(fetchArgs, "fetch", "--quiet", "--depth", "200", "origin", fetchSpec)
	if out, err := runGit(ctx, cacheDir, fetchArgs...); err != nil {
		return noopCleanup, fmt.Errorf("git fetch pr (cache): %w (%s)", err, redactToken(out, token))
	}
	log.Printf("%s git fetch DONE in %s", logPrefix, time.Since(t1))

	// Step 2b: fetch the PR's base branch into the cache. --depth implies
	// --single-branch, so the shared cache only tracks the branch it was
	// initialized with: without this, origin/<base> never exists for a PR
	// based on any other branch (stacked PRs — or every default-base PR when
	// the cache was initialized by a stacked one), and the deterministic
	// layer's diff (gates, bug memory, required checks) silently degrades to
	// "no signal". Also keeps origin/<base> fresh as the base moves. A
	// separate best-effort fetch, not folded into the PR fetch above: the
	// review itself only needs the PR head, and diffFilesForWorktree has its
	// own recovery path if this fails (e.g. a deleted base branch).
	if defaultBranch != "" {
		baseArgs := authHeaderArgs(token)
		baseArgs = append(baseArgs, "fetch", "--quiet", "--depth", "200", "origin",
			fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", defaultBranch, defaultBranch))
		if out, err := runGit(ctx, cacheDir, baseArgs...); err != nil {
			log.Printf("%s WARN: base-branch fetch (%s): %v (%s) — deterministic layer may find no diff base",
				logPrefix, defaultBranch, err, redactToken(out, token))
		}
	}

	// Step 3: create a worktree for this review at the requested commit.
	// Worktrees share the cache's object store, so this is near-instant.
	// Use the absolute target path so the worktree lands where the caller
	// expects regardless of git's cwd.
	log.Printf("%s git worktree add %s @ %s START", logPrefix, absDir, commitSHA)
	t2 := time.Now()
	if out, err := runGit(ctx, cacheDir, "worktree", "add", "--detach", absDir, commitSHA); err != nil {
		return noopCleanup, fmt.Errorf("git worktree add: %w (%s)", err, out)
	}
	log.Printf("%s git worktree add DONE in %s", logPrefix, time.Since(t2))

	// Cleanup: remove the worktree (files + git's worktree registration).
	// `worktree remove --force` handles both atomically; we use a fresh
	// background context because the run's ctx may already be expired by
	// the time the deferred cleanup runs.
	cleanup = func() error {
		log.Printf("%s git worktree remove %s START", logPrefix, absDir)
		t := time.Now()
		mu := cacheLock(cacheKey)
		mu.Lock()
		defer mu.Unlock()
		if out, err := runGit(context.Background(), cacheDir, "worktree", "remove", "--force", absDir); err != nil {
			// Fallback: nuke the dir directly + prune so the cache's worktree
			// list doesn't accumulate dead entries.
			_ = os.RemoveAll(absDir)
			_, _ = runGit(context.Background(), cacheDir, "worktree", "prune")
			return fmt.Errorf("git worktree remove: %w (%s)", err, out)
		}
		log.Printf("%s git worktree remove DONE in %s", logPrefix, time.Since(t))
		return nil
	}
	return cleanup, nil
}

// authHeaderArgs returns the leading `git -c http.extraheader=...` flags
// needed to authenticate to github.com using a GitHub installation token,
// or an empty slice if token is empty (public repo). Using the header
// instead of embedding the token in the clone URL keeps the token out of
// .git/config (which the agent's Read tool could otherwise scrape) and
// out of any URL git echoes back on errors.
//
// The token still appears briefly in argv during the git invocation, so
// `ps aux` from a sibling process during that window would see it. For our
// single-tenant Cloud Run container this is tolerable; the only sibling is
// the agent subprocess which we spawn ourselves.
func authHeaderArgs(token string) []string {
	if token == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"-c", "http.extraheader=Authorization: Basic " + auth}
}

// redactToken replaces any literal occurrence of token in s with "***".
// Used before including git output in error messages so an authenticated
// clone URL echoed by git can't leak the GitHub installation token.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseAgentStream reads stream-json events line-by-line from the subprocess,
// counts assistant turns, extracts the final result, and tees the raw bytes
// into a log file. Kills the subprocess when maxTurns is exceeded.
type agentParseResult struct {
	finalOutput    string
	assistantTurns int
	budgetUnits    int      // equals assistantTurns for Claude; counts completed work items for Codex
	streamErr      string   // error the CLI reported inside the stream (result event with error subtype)
	lastEvent      string   // raw last stream line, fallback diagnostic when no structured error arrived
	servedModels   []string // distinct models the stream reported, in first-seen order; empty if never reported
}

func openRouterEnvironment(base []string, apiKey string) []string {
	excluded := map[string]struct{}{
		"ANTHROPIC_API_KEY": {}, "DATABASE_URL": {}, "GEMINI_API_KEY": {},
		"GITHUB_APP_CLIENT_SECRET": {}, "GITHUB_APP_PRIVATE_KEY_PATH": {},
		"GITHUB_TOKEN": {}, "GOOGLE_APPLICATION_CREDENTIALS": {},
		"OPENROUTER_API_KEY": {}, "PRISM_TOKEN": {}, "SESSION_SECRET": {},
	}
	environment := make([]string, 0, len(base)+1)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, drop := excluded[key]; drop {
				continue
			}
		}
		environment = append(environment, entry)
	}
	return append(environment, "OPENROUTER_API_KEY="+apiKey)
}

// diagnostic returns the best available explanation of a failed run — under
// stream-json the CLI reports errors on stdout and stderr stays empty, so a
// bare exit status is uninformative.
func (r *agentParseResult) diagnostic() string {
	if r.streamErr != "" {
		return r.streamErr
	}
	if r.lastEvent != "" {
		return "last event: " + r.lastEvent
	}
	return "no stream output"
}

// noteServedModel records every distinct model the stream reports, in
// first-seen order — a transient mid-run fallback must not be masked by a
// later recovery to the original model. Error events carry the placeholder
// "<synthetic>" model — never a serving model.
func (r *agentParseResult) noteServedModel(v any) {
	s, ok := v.(string)
	if !ok || s == "" || strings.HasPrefix(s, "<") {
		return
	}
	for _, m := range r.servedModels {
		if m == s {
			return
		}
	}
	r.servedModels = append(r.servedModels, s)
}

// modelMatches reports whether the served model satisfies the requested one.
// Substring in either direction tolerates alias vs full id ("opus" vs
// "claude-opus-4-8") and dated vs undated ids in config or stream
// ("claude-fable-5-20260115" vs "claude-fable-5") without false alarms.
// Known blind spot: a same-family version swap where one id prefixes the
// other (requested "claude-opus-4", served "claude-opus-4-8") is NOT
// detected — configure full model ids to avoid it.
func modelMatches(requested, served string) bool {
	requested = strings.ToLower(requested)
	served = strings.ToLower(served)
	return strings.Contains(served, requested) || strings.Contains(requested, served)
}

func parseAgentStream(proc SpawnedProcess, logFile io.Writer, maxTurns int) (*agentParseResult, error) {
	result := &agentParseResult{}
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

		switch ev["type"] {
		case "system":
			if ev["subtype"] == "init" {
				result.noteServedModel(ev["model"])
			}
		case "assistant":
			if msg, ok := ev["message"].(map[string]any); ok {
				result.noteServedModel(msg["model"])
			}
			result.assistantTurns++
			result.budgetUnits++
			if result.budgetUnits%5 == 0 || result.budgetUnits == 1 {
				log.Printf("[AGENT] turn-budget unit %d/%d (assistant events)", result.budgetUnits, maxTurns)
			}
			if result.budgetUnits > maxTurns {
				_ = proc.Kill()
				return result, fmt.Errorf("exceeded max-turns (%d)", maxTurns)
			}
		case "result":
			if s, ok := ev["result"].(string); ok {
				result.finalOutput = s
			}
			subtype, _ := ev["subtype"].(string)
			isErr, _ := ev["is_error"].(bool)
			if isErr || (subtype != "" && subtype != "success") {
				msg := subtype
				for _, key := range []string{"error", "result", "message"} {
					if s, ok := ev[key].(string); ok && s != "" {
						if msg != "" {
							msg += ": "
						}
						msg += s
						break
					}
				}
				if msg == "" {
					msg = "unspecified error in result event"
				}
				result.streamErr = truncate(msg, 2000)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// Nothing drains stdout after this return; unkilled, proc.Wait()
		// stalls until the wall-clock watcher fires.
		_ = proc.Kill()
		return result, fmt.Errorf("read stdout: %w", err)
	}
	return result, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// DefaultSpawner is implemented per-platform; see agent_spawn_unix.go and
// agent_spawn_windows.go. The unix implementation uses Setpgid + group-kill
// so subprocesses spawned by an agent's shell tool are torn down with the
// parent rather than orphaned. The Windows stub exists only so the package
// compiles; agent reviews are not supported on Windows (no test, no deploy
// target).
