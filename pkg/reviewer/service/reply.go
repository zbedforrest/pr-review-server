package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/reviewer/types"
)

// Reply decisions, mirrored by the publisher's constants.
const (
	ReplyDecisionConcede = "concede"
	ReplyDecisionHold    = "hold"
	ReplyDecisionAnswer  = "answer"
	ReplyDecisionAbstain = "abstain"

	replyMaxChars = 600
)

// ReplyMessage is one comment in the thread under a PRism inline finding.
type ReplyMessage struct {
	Author string
	Ours   bool
	Body   string
	At     time.Time
}

// ReplyInput is one author reply to answer, with the finding and thread it
// belongs to. HeadSHA is the PR head the reply was written against.
type ReplyInput struct {
	Owner         string
	Repo          string
	DefaultBranch string
	PRNumber      int
	HeadSHA       string
	Fingerprint   string
	FindingBody   string
	Thread        []ReplyMessage
	AuthorReply   string
	Class         string
}

// ReplyResult is the model's decision after validation: a hold whose evidence
// does not resolve in the checkout degrades to abstain, and cites that do not
// resolve are dropped from every decision (listed in Unresolved).
type ReplyResult struct {
	Decision       string
	Reply          string
	Cited          []types.EvidenceRef
	Unresolved     []types.EvidenceRef
	RequestedModel string
	ServedModel    string
	AssistantTurns int
	DurationMS     int64
}

type replyJSON struct {
	Decision string              `json:"decision"`
	Reply    string              `json:"reply"`
	Cited    []types.EvidenceRef `json:"cited"`
}

var replyDecisions = map[string]bool{ReplyDecisionConcede: true, ReplyDecisionHold: true, ReplyDecisionAnswer: true, ReplyDecisionAbstain: true}

// RunAgentReply checks out the PR head, hands the agent the finding, the
// thread and the author's reply, and returns its validated decision.
func RunAgentReply(ctx context.Context, cfg AgentConfig, spawner Spawner, in ReplyInput) (*ReplyResult, error) {
	if cfg.MaxTurns <= 0 || cfg.WallClock <= 0 {
		return nil, errors.New("reply: MaxTurns and WallClock must be > 0")
	}
	runtime, err := resolveAgentRuntime(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.CloneRootDir, 0o755); err != nil {
		return nil, fmt.Errorf("reply: create clone root: %w", err)
	}
	if err := os.MkdirAll(cfg.LogsDir, 0o755); err != nil {
		return nil, fmt.Errorf("reply: create logs dir: %w", err)
	}
	slug := fmt.Sprintf("%s__%s__pr%d__reply__%d", in.Owner, in.Repo, in.PRNumber, time.Now().UnixNano())
	cloneDir := filepath.Join(cfg.CloneRootDir, slug)
	logPath := filepath.Join(cfg.LogsDir, slug+".jsonl")
	logPrefix := fmt.Sprintf("[REPLY %s/%s#%d]", in.Owner, in.Repo, in.PRNumber)

	runCtx, cancel := context.WithTimeout(ctx, cfg.WallClock)
	defer cancel()
	started := time.Now()

	cleanupClone, err := cloneForAgent(runCtx, cfg.CloneRootDir, cloneDir, in.Owner, in.Repo, in.DefaultBranch, in.PRNumber, in.HeadSHA, cfg.GitHubToken)
	if err != nil {
		return nil, fmt.Errorf("reply: clone: %w", err)
	}
	defer func() {
		if cerr := cleanupClone(); cerr != nil {
			log.Printf("%s WARN: worktree cleanup failed: %v", logPrefix, cerr)
		}
	}()

	prompt, err := buildReplyPrompt(in)
	if err != nil {
		return nil, fmt.Errorf("reply: build prompt: %w", err)
	}
	credentialKey, credentialValue := "ANTHROPIC_API_KEY", cfg.AnthropicAPIKey
	if runtime.backend == AgentBackendOpenRouter {
		credentialKey, credentialValue = "OPENROUTER_API_KEY", cfg.OpenRouterAPIKey
	}
	log.Printf("%s spawning %s (model=%s, effort=%s, class=%s, prompt_chars=%d)", logPrefix, runtime.command, runtime.model, runtime.effort, in.Class, len(prompt))
	proc, err := spawner.SpawnWithEnv(runCtx, runtime.command, runtime.argsWithTools(prompt, replyAgentTools), cloneDir,
		agentChildEnvironment(os.Environ(), credentialKey, credentialValue))
	if err != nil {
		return nil, fmt.Errorf("reply: spawn %s: %w", runtime.command, err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("reply: create log: %w", err)
	}
	defer logFile.Close()
	// Keep the stream log on failure unless a sink captures it, like the
	// review agent does; a successful run leaves nothing on /tmp.
	succeeded := false
	defer func() {
		if succeeded {
			_ = os.Remove(logPath)
			return
		}
		if cfg.FailureLogSink != nil {
			_ = logFile.Sync()
			cfg.FailureLogSink(logPath)
			_ = os.Remove(logPath)
		}
	}()

	var stderrBuf strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&stderrBuf, proc.Stderr())
	}()
	parsed, parseErr := runtime.parseStream(proc, logFile, cfg.MaxTurns)
	waitErr := proc.Wait()
	wg.Wait()
	redact := func(s string) string { return truncate(redactToken(s, credentialValue), 600) }
	switch {
	case parseErr != nil:
		return nil, fmt.Errorf("reply: %w (stderr: %s)", parseErr, redact(stderrBuf.String()))
	case runCtx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("reply: wall-clock timeout (%s)", cfg.WallClock)
	case parsed.streamErr != "":
		return nil, fmt.Errorf("reply: CLI reported error: %s", redact(parsed.streamErr))
	case waitErr != nil:
		return nil, fmt.Errorf("reply: %s exited with error: %w (stderr: %s)", runtime.command, waitErr, redact(stderrBuf.String()))
	case parsed.finalOutput == "":
		return nil, fmt.Errorf("reply: no final result emitted (stream: %s)", redact(parsed.diagnostic()))
	}

	succeeded = true
	out := &ReplyResult{
		Decision: ReplyDecisionAbstain, RequestedModel: runtime.model,
		AssistantTurns: parsed.assistantTurns, DurationMS: time.Since(started).Milliseconds(),
	}
	out.ServedModel, _, _, _, _ = agentServingMetadata(runtime, parsed.servedModels)
	decision, ok := parseReplyJSON(parsed.finalOutput)
	if !ok || !replyDecisions[decision.Decision] {
		log.Printf("%s final output is not a reply decision; abstaining (%s)", logPrefix, truncate(parsed.finalOutput, 200))
		return out, nil
	}
	resolves := evidenceRefResolves(nil, cloneDir)
	for _, e := range decision.Cited {
		if resolves(e) {
			out.Cited = append(out.Cited, e)
		} else {
			out.Unresolved = append(out.Unresolved, e)
		}
	}
	// A hold and a concession both assert something about the code (a
	// concession dismisses the finding for good), so both need a file:line
	// the reader can open.
	if (decision.Decision == ReplyDecisionHold || decision.Decision == ReplyDecisionConcede) && len(out.Cited) == 0 {
		log.Printf("%s %s without resolving evidence (%d cited); abstaining", logPrefix, decision.Decision, len(decision.Cited))
		out.Cited = nil
		return out, nil
	}
	if decision.Decision != ReplyDecisionAbstain && decision.Reply == "" {
		return out, nil
	}
	if looksLikeSecret(decision.Reply) {
		log.Printf("%s reply text matches a credential pattern; abstaining", logPrefix)
		return out, nil
	}
	out.Decision = decision.Decision
	if out.Decision != ReplyDecisionAbstain {
		out.Reply = decision.Reply
	}
	return out, nil
}

// replyAgentTools omits Bash: the reply prompt carries author-written text
// and the answer is posted automatically, so the agent only reads.
const replyAgentTools = "Read,Grep,Glob"

var secretPatterns = regexp.MustCompile(`(?i)(sk-ant-[a-z0-9_-]{8,}|sk-[a-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|AIza[0-9A-Za-z_-]{30,}|api[_-]?key\s*[:=]\s*\S{12,})`)

// looksLikeSecret rejects reply text that resembles a credential, whatever
// prompted the model to include it.
func looksLikeSecret(s string) bool {
	return secretPatterns.MatchString(s)
}

var replySpaceRe = regexp.MustCompile(`\s+`)

// parseReplyJSON finds the decision object in the agent's final text. The
// reply is collapsed to one line: it is posted as a single paragraph.
func parseReplyJSON(raw string) (replyJSON, bool) {
	var d replyJSON
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return d, false
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &d); err != nil {
		if err2 := json.Unmarshal([]byte(escapeControlCharsInStrings(raw[start:end+1])), &d); err2 != nil {
			return d, false
		}
	}
	d.Decision = strings.ToLower(strings.TrimSpace(d.Decision))
	d.Reply = strings.TrimSpace(replySpaceRe.ReplaceAllString(d.Reply, " "))
	return d, true
}

func buildReplyPrompt(in ReplyInput) (string, error) {
	type message struct {
		Author string `json:"author"`
		Role   string `json:"role"`
		At     string `json:"at"`
		Body   string `json:"body"`
	}
	thread := make([]message, 0, len(in.Thread))
	for _, m := range in.Thread {
		role := "author"
		if m.Ours {
			role = "prism"
		}
		thread = append(thread, message{Author: m.Author, Role: role, At: m.At.UTC().Format(time.RFC3339), Body: stripMarkers(m.Body)})
	}
	payload, err := json.MarshalIndent(map[string]any{
		"finding_id":   in.Fingerprint,
		"finding":      stripMarkers(in.FindingBody),
		"thread":       thread,
		"author_reply": in.AuthorReply,
		"reply_class":  in.Class,
		"head_sha":     in.HeadSHA,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return promptAgentReply + "\n\n" + string(payload), nil
}

var htmlCommentRe = regexp.MustCompile(`<!--.*?-->\s*`)

func stripMarkers(s string) string {
	return strings.TrimSpace(htmlCommentRe.ReplaceAllString(s, ""))
}
