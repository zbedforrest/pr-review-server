// verifyreplay runs the repo-evidence verifier offline over a directory of
// review sidecars and writes, per sidecar, the review as the verifier in on
// mode would leave it: held and dropped findings removed (or kept with their
// decision under --keep-suppressed), revisions applied.
// Scoring the output beside the input (benchmark/score.py, clean_fp.py)
// measures what verification costs in recall and saves in false positives.
//
// Usage:
//
//	OPENROUTER_API_KEY=... verifyreplay --in RUN_DIR --out OUT_DIR --clones CLONE_ROOT [--par 4] [--max-usd 40] [--model anthropic/claude-fable-5.1]
//
// CLONE_ROOT is an agent clone root (its .cache/owner__repo holds the
// objects at every sidecar's head and merge-base). OUT_DIR also gets
// decisions.jsonl, one line per finding, and summary.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/reviewer/verify"
)

type decisionLine struct {
	Case     string              `json:"case"`
	Key      string              `json:"key"`
	File     string              `json:"file"`
	Line     int                 `json:"line"`
	Severity string              `json:"severity"`
	Comment  string              `json:"comment"`
	Verified *types.Verification `json:"verification"`
	Revised  string              `json:"revised_comment,omitempty"`
}

type caseResult struct {
	Case         string         `json:"case"`
	Status       string         `json:"status"`
	Error        string         `json:"error,omitempty"`
	Findings     int            `json:"findings"`
	Decisions    map[string]int `json:"decisions,omitempty"`
	Items        int            `json:"packet_items"`
	EstTokens    int            `json:"estimated_tokens"`
	CostUSD      float64        `json:"cost_usd"`
	Escalated    int            `json:"escalated,omitempty"`
	AgentCostUSD float64        `json:"agent_cost_usd,omitempty"`
	AgentMS      int64          `json:"agent_ms,omitempty"`
	AgentError   string         `json:"agent_error,omitempty"`
	DurationMS   int64          `json:"duration_ms"`
}

func main() {
	in := flag.String("in", "", "directory of review sidecars (*.json)")
	out := flag.String("out", "", "output directory")
	clones := flag.String("clones", "", "agent clone root holding .cache/owner__repo")
	par := flag.Int("par", 4, "concurrent verify calls")
	maxUSD := flag.Float64("max-usd", 40, "stop starting new cases once this much is spent")
	model := flag.String("model", "anthropic/claude-fable-5.1", "verifier model")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-case timeout")
	dry := flag.Bool("dry", false, "build packets and report their size without calling the model")
	flag.BoolVar(&agentAll, "agent-all", false, "skip the packet call and send every finding to the verify agent (implies --escalate)")
	flag.IntVar(&verify.MaxAgentFindings, "agent-max-findings", verify.MaxAgentFindings, "findings per escalation run")
	flag.BoolVar(&escalate, "escalate", false, "send findings the packet check marks unavailable to the read-only verify agent")
	agentBackend := flag.String("agent-backend", service.AgentBackendClaude, "escalation agent backend (claude runs the Claude CLI; openrouter runs codex, which re-sends uncached context every turn)")
	agentModel := flag.String("agent-model", "claude-opus-5-5", "escalation agent model")
	agentTurns := flag.Int("agent-turns", 40, "escalation agent turn budget")
	agentWall := flag.Duration("agent-wall", 7*time.Minute, "escalation agent wall clock")
	flag.BoolVar(&keepSuppressed, "keep-suppressed", false, "keep held and dropped findings in the output with their applied Verification, as production does, for a publisher pass")
	flag.Parse()
	key := os.Getenv("OPENROUTER_API_KEY")
	if *in == "" || *out == "" || *clones == "" || (key == "" && !*dry) {
		log.Fatal("need --in, --out, --clones and OPENROUTER_API_KEY")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(*in, "*.json"))
	if err != nil {
		log.Fatal(err)
	}
	var cases []string
	for _, p := range paths {
		if base := filepath.Base(p); base != "summary.json" && !strings.HasSuffix(base, ".verify.json") {
			cases = append(cases, p)
		}
	}
	sort.Strings(cases)
	client := llm.NewOpenRouterClient(key, os.Getenv("OPENROUTER_BASE_URL"), *model, false)
	v := verify.Verifier{Client: client, Model: *model}
	agentCfg = service.AgentConfig{
		CloneRootDir: *clones, LogsDir: filepath.Join(*out, "agent-logs"), WallClock: *agentWall, MaxTurns: *agentTurns,
		Backend: *agentBackend, Model: *agentModel, OpenRouterAPIKey: key, OpenRouterBaseURL: os.Getenv("OPENROUTER_BASE_URL"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"), GitHubToken: os.Getenv("GITHUB_TOKEN"),
	}
	if *dry {
		v.Client = nil
	}

	var mu sync.Mutex
	var spent float64
	var results []caseResult
	var lines []decisionLine
	sem := make(chan struct{}, *par)
	var wg sync.WaitGroup
	for _, path := range cases {
		mu.Lock()
		over := spent >= *maxUSD
		mu.Unlock()
		if over {
			log.Printf("budget $%.2f reached; %s and later cases not run", *maxUSD, filepath.Base(path))
			mu.Lock()
			results = append(results, caseResult{Case: filepath.Base(path), Status: "not_run_budget"})
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(path string) {
			defer func() { <-sem; wg.Done() }()
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			res, dl := replay(ctx, v, *clones, *out, path)
			mu.Lock()
			spent += res.CostUSD
			results = append(results, res)
			lines = append(lines, dl...)
			mu.Unlock()
			log.Printf("%s %s findings=%d decisions=%v cost=$%.3f %dms %s escalated=%d agent=$%.3f %dms %s", res.Case, res.Status, res.Findings, res.Decisions, res.CostUSD, res.DurationMS, res.Error, res.Escalated, res.AgentCostUSD, res.AgentMS, res.AgentError)
		}(path)
	}
	wg.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Case < results[b].Case })
	sort.Slice(lines, func(a, b int) bool {
		if lines[a].Case != lines[b].Case {
			return lines[a].Case < lines[b].Case
		}
		return lines[a].Key < lines[b].Key
	})
	writeJSONL(filepath.Join(*out, "decisions.jsonl"), lines)
	totals := map[string]int{}
	for _, r := range results {
		for k, n := range r.Decisions {
			totals[k] += n
		}
	}
	tokens := 0
	for _, r := range results {
		tokens += r.EstTokens
	}
	summary := map[string]any{"model": *model, "cases": len(results), "spent_usd": spent, "estimated_input_tokens": tokens, "decisions": totals, "per_case": results}
	body, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "verify_summary.json"), body, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("cases=%d spent=$%.2f est_input_tokens=%d decisions=%v\n", len(results), spent, tokens, totals)
}

// keepSuppressed writes held and dropped findings instead of removing them;
// removal is what a recall scorer, which counts active claims, needs.
var keepSuppressed bool

var (
	agentAll bool
	escalate bool
	agentCfg service.AgentConfig
)

// replay verifies one sidecar and writes its on-mode result. A case that
// fails is written unchanged, as production would publish it.
func replay(ctx context.Context, v verify.Verifier, clones, outDir, path string) (out caseResult, _ []decisionLine) {
	start := time.Now()
	res := caseResult{Case: filepath.Base(path)}
	defer func() { out.DurationMS = time.Since(start).Milliseconds() }()
	raw, err := os.ReadFile(path)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res, nil
	}
	write := func(body []byte) {
		if err := os.WriteFile(filepath.Join(outDir, res.Case), body, 0o644); err != nil {
			log.Printf("%s: write: %v", res.Case, err)
		}
	}
	pl, err := payload.Decode(raw)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		write(raw)
		return res, nil
	}
	comments := pl.ToLineComments()
	var findings []verify.Finding
	var index []int
	for i, c := range comments {
		if c.FilePath == "SUMMARY" || c.FilePath == "CHECK" || c.Inactive {
			continue
		}
		findings = append(findings, verify.Finding{Key: fmt.Sprintf("F%d", len(findings)+1), Comment: c})
		index = append(index, i)
	}
	res.Findings = len(findings)
	if len(findings) == 0 {
		res.Status = "no_findings"
		write(raw)
		return res, nil
	}
	input := verify.Input{Head: pl.CommitSHA, Findings: findings}
	if pl.Scope != nil {
		input.Base = pl.Scope.MergeBase
	}
	git := verify.RepoGit{Dir: service.AgentCacheDir(clones, pl.Owner, pl.Repo)}
	pkt, err := verify.Build(ctx, git, input, verify.DefaultLimits)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		write(raw)
		return res, nil
	}
	res.Items, res.EstTokens = len(pkt.Items), verify.EstimateTokens(pkt, findings)
	if v.Client == nil {
		res.Status = "dry"
		for _, id := range pkt.ByFinding {
			for _, i := range id {
				if it, ok := pkt.Item(i); ok && it.Kind == verify.KindNote {
					res.Error += it.Text + "; "
				}
			}
		}
		return res, nil
	}
	var vr verify.Result
	if agentAll {
		for _, f := range findings {
			c := f.Comment
			c.Verification = &types.Verification{Status: types.VerifyUnavailable}
			vr.Comments = append(vr.Comments, c)
		}
	} else {
		vr, err = v.Verify(ctx, pkt, findings)
	}
	res.CostUSD = vr.Call.CostUSD
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		write(raw)
		return res, nil
	}
	if (escalate || agentAll) && vr.Complete() {
		in := service.VerifyAgentInput{Owner: pl.Owner, Repo: pl.Repo, DefaultBranch: defaultBranch(ctx, clones, pl.Owner, pl.Repo),
			PRNumber: pl.PRNumber, HeadSHA: input.Head, BaseSHA: input.Base, Packet: pkt, Findings: findings}
		er, err := service.EscalateVerify(ctx, agentCfg, service.DefaultSpawner{}, in, vr.Comments)
		res.Escalated, res.AgentCostUSD, res.AgentMS = er.Escalated, er.CostUSD, er.DurationMS
		res.CostUSD += er.CostUSD
		if err != nil {
			res.AgentError = err.Error()
		}
	}
	res.Decisions = map[string]int{}
	var lines []decisionLine
	for i, c := range vr.Comments {
		res.Decisions[c.Verification.Status]++
		f := findings[i]
		dl := decisionLine{Case: res.Case, Key: f.Key, File: f.Comment.FilePath, Line: f.Comment.LineNumber,
			Severity: f.Comment.Importance, Comment: f.Comment.CommentBody, Verified: c.Verification}
		if c.Verification.Status == types.VerifyRevise {
			dl.Revised = c.CommentBody
		}
		lines = append(lines, dl)
	}
	if !vr.Complete() {
		res.Status, res.Error = "incomplete", "missing "+strings.Join(vr.Missing, ",")
		write(raw)
		return res, lines
	}
	res.Status = "ok"
	drop := map[int]bool{}
	for i, c := range vr.Comments {
		c.Verification.Applied = true
		at := index[i]
		if c.Verification.Suppresses() {
			if keepSuppressed {
				pl.Findings[at].Verification = c.Verification
			} else {
				drop[at] = true
			}
			continue
		}
		if c.Verification.Status == types.VerifyRevise {
			one := []types.LineComment{c}
			service.EnforceAgentFindingContractPolicy(one)
			c = one[0]
		}
		f := &pl.Findings[at]
		f.Line, f.Comment, f.Severity = c.LineNumber, c.CommentBody, strings.ToLower(c.Importance)
		f.FindingContract, f.Verification = c.FindingContract, c.Verification
	}
	kept := pl.Findings[:0]
	for i, f := range pl.Findings {
		if !drop[i] {
			kept = append(kept, f)
		}
	}
	pl.Findings = kept
	body, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		write(raw)
		return res, lines
	}
	write(body)
	return res, lines
}

// defaultBranch reads the clone cache's origin HEAD, falling back to master.
func defaultBranch(ctx context.Context, clones, owner, repo string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", service.AgentCacheDir(clones, owner, repo), "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return "master"
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

func writeJSONL(path string, lines []decisionLine) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			log.Fatal(err)
		}
	}
}
