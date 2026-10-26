package poller

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"pr-review-server/gcs"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/reviewer/verify"
)

const (
	verifyModeOff    = "off"
	verifyModeShadow = "shadow"
	verifyModeOn     = "on"
)

// VerifyReport is the review-level account of one verification.
type VerifyReport struct {
	Mode   string `json:"mode"`
	Sync   bool   `json:"sync"`
	Status string `json:"status"` // applied, recorded, pending, incomplete, failed, skipped
	// Skip is why verification did not run: no_findings, not_sampled,
	// over_budget, too_large, agent_off, busy, deadline.
	Skip            string         `json:"skip,omitempty"`
	Error           string         `json:"error,omitempty"`
	Findings        int            `json:"findings"`
	Decisions       map[string]int `json:"decisions,omitempty"`
	Missing         []string       `json:"missing,omitempty"`
	Invalid         []string       `json:"invalid,omitempty"`
	PacketItems     int            `json:"packet_items,omitempty"`
	EstimatedTokens int            `json:"estimated_tokens,omitempty"`
	Call            *llm.Call      `json:"call,omitempty"`
	Escalated       int            `json:"escalated,omitempty"`
	AgentDecisions  map[string]int `json:"agent_decisions,omitempty"`
	AgentCostUSD    float64        `json:"agent_cost_usd,omitempty"`
	AgentMS         int64          `json:"agent_ms,omitempty"`
	AgentError      string         `json:"agent_error,omitempty"`
	DurationMS      int64          `json:"duration_ms"`
	Artifact        string         `json:"artifact,omitempty"`
}

// verifyArtifact is what a background verification persists next to the
// run's sidecar: each finding as merged and as the verifier left it.
type verifyArtifact struct {
	RunID    string                `json:"run_id"`
	Owner    string                `json:"owner"`
	Repo     string                `json:"repo"`
	Number   int                   `json:"number"`
	Head     string                `json:"head"`
	Base     string                `json:"base"`
	Report   VerifyReport          `json:"report"`
	Findings []verifyArtifactEntry `json:"findings"`
}

type verifyArtifactEntry struct {
	Key      string             `json:"key"`
	Merged   types.LineComment  `json:"merged"`
	Verified *types.LineComment `json:"verified,omitempty"`
}

// verifySpend is the verifier's spend for the current UTC day. allow and
// add are separate, so concurrent verifications can each pass the check
// before any records its cost; the overshoot is bounded by the work in
// flight (about a dollar each), which the daily cap tolerates. The counter is
// per process: with N server instances the ceiling is N times the cap.
type verifySpend struct {
	mu  sync.Mutex
	day string
	usd float64
}

func (s *verifySpend) allow(now time.Time, capUSD float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(now)
	return capUSD <= 0 || s.usd < capUSD
}

func (s *verifySpend) add(now time.Time, usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(now)
	s.usd += usd
}

func (s *verifySpend) roll(now time.Time) {
	if day := now.UTC().Format("2006-01-02"); day != s.day {
		s.day, s.usd = day, 0
	}
}

// Tests replace these.
var (
	newVerifyGit       = func(dir string) verify.Git { return verify.RepoGit{Dir: dir} }
	newVerifyCompleter = func(apiKey, baseURL, model string) verify.Completer {
		return llm.NewOpenRouterClient(apiKey, baseURL, model, false)
	}
	verifySampleDraw = rand.Float64
	escalateVerifyFn = service.EscalateVerify
)

// verifyShadowBudgetShare is the part of the daily budget background
// (shadow) verifications may spend, so they cannot starve the publish-bound
// ones that change what posts.
const verifyShadowBudgetShare = 0.7

// verifyBackground bounds background verifications per process; one that
// finds it full is skipped rather than queued.
var verifyBackground = make(chan struct{}, 4)

// verifyTarget is the review a verification belongs to.
type verifyTarget struct {
	Owner, Repo, BaseRef, CloneRoot string
	Number                          int
}

// verifyEnsembleReview runs the repo-evidence verifier on a merged ensemble
// review. In on mode a publish-bound review waits for it and its decisions
// are applied; every other covered review is verified in the background and
// only recorded, so it changes nothing that posts and adds no latency.
func (p *Poller) verifyEnsembleReview(ctx context.Context, execution *reviewExecution, base service.AgentConfig, result *service.ReviewResult, review *service.AgentReview) *VerifyReport {
	if p.cfg == nil || p.cfg.EnsembleVerify == "" || p.cfg.EnsembleVerify == verifyModeOff {
		return nil
	}
	mode := p.cfg.EnsembleVerify
	var findings []verify.Finding
	var index []int
	for i, c := range review.Comments {
		if c.FilePath == "SUMMARY" || c.FilePath == "CHECK" || c.Inactive {
			continue
		}
		findings = append(findings, verify.Finding{Key: "F" + strconv.Itoa(len(findings)+1), Comment: c})
		index = append(index, i)
	}
	rep := &VerifyReport{Mode: mode, Findings: len(findings)}
	if len(findings) == 0 {
		rep.Status, rep.Skip = "skipped", "no_findings"
		return rep
	}
	job := execution.Job
	bound := p.reviewPublishBound(job)
	rep.Sync = mode == verifyModeOn && bound
	if !bound && verifySampleDraw() >= p.cfg.VerifyShadowSample {
		rep.Status, rep.Skip = "skipped", "not_sampled"
		return rep
	}
	in := verify.Input{Head: job.PR.CommitSHA, PRText: strings.TrimSpace(result.PRTitle + "\n\n" + result.PRBody), Findings: findings}
	if review.Scope != nil {
		in.Base = review.Scope.MergeBase
	}
	target := verifyTarget{Owner: job.PR.Owner, Repo: job.PR.Repo, Number: job.PR.Number, BaseRef: result.BaseRef, CloneRoot: base.CloneRootDir}
	wall := p.verifyWallClock()

	if !rep.Sync {
		rep.Status = "pending"
		rep.Artifact = verifyArtifactName(job)
		select {
		case verifyBackground <- struct{}{}:
			go func() {
				defer func() { <-verifyBackground }()
				p.recordVerification(job, target, in, wall, *rep)
			}()
		default:
			rep.Status, rep.Skip, rep.Artifact = "skipped", "busy", ""
		}
		return rep
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wall {
		rep.Status, rep.Skip = "skipped", "deadline"
		return rep
	}
	vctx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()
	verified := p.runVerify(vctx, target, in, rep)
	if rep.Status != "ok" {
		return rep
	}
	for i, c := range verified {
		c.Verification.Applied = true
		if c.Verification.Status == types.VerifyRevise {
			one := []types.LineComment{c}
			service.EnforceAgentFindingContractPolicy(one)
			c = one[0]
		}
		review.Comments[index[i]] = c
	}
	rep.Status = "applied"
	return rep
}

// runVerify builds the packet and makes the call, filling rep. Status is
// "ok" only when every finding got a decision.
func (p *Poller) runVerify(ctx context.Context, t verifyTarget, in verify.Input, rep *VerifyReport) []types.LineComment {
	start := time.Now()
	defer func() { rep.DurationMS = time.Since(start).Milliseconds() }()
	capUSD := p.cfg.VerifyDailyBudgetUSD
	if !rep.Sync {
		capUSD *= verifyShadowBudgetShare
	}
	if !p.verifySpend.allow(start, capUSD) {
		rep.Status, rep.Skip = "skipped", "over_budget"
		return nil
	}
	packetCall := p.cfg.VerifyPacketCall
	if packetCall && p.cfg.OpenRouterAPIKey == "" {
		packetCall = false
	}
	if !packetCall && !p.cfg.VerifyEscalate {
		rep.Status, rep.Skip = "skipped", "agent_off"
		return nil
	}
	pkt, err := verify.Build(ctx, newVerifyGit(service.AgentCacheDir(t.CloneRoot, t.Owner, t.Repo)), in, verify.DefaultLimits)
	if err != nil {
		rep.Status, rep.Error = "failed", err.Error()
		return nil
	}
	rep.PacketItems = len(pkt.Items)
	rep.EstimatedTokens = verify.EstimateTokens(pkt, in.Findings)
	if !packetCall {
		rep.EstimatedTokens = verify.EstimateAgentTokens(pkt, in.Findings)
	}
	if p.cfg.VerifyMaxInputTokens > 0 && rep.EstimatedTokens > p.cfg.VerifyMaxInputTokens {
		rep.Status, rep.Skip = "skipped", "too_large"
		return nil
	}
	if !packetCall {
		comments := make([]types.LineComment, 0, len(in.Findings))
		for _, f := range in.Findings {
			c := f.Comment
			c.Verification = &types.Verification{Status: types.VerifyUnavailable}
			comments = append(comments, c)
		}
		rep.Status = "ok"
		p.escalateVerify(ctx, t, in, pkt, comments, rep)
		return comments
	}
	v := verify.Verifier{Client: newVerifyCompleter(p.cfg.OpenRouterAPIKey, p.cfg.OpenRouterBaseURL, p.cfg.VerifyModel), Model: p.cfg.VerifyModel}
	res, err := v.Verify(ctx, pkt, in.Findings)
	p.verifySpend.add(time.Now(), res.Call.CostUSD)
	rep.Call = &res.Call
	if err != nil {
		rep.Status, rep.Error = "failed", err.Error()
		return nil
	}
	rep.Missing, rep.Invalid = res.Missing, res.Invalid
	rep.Decisions = map[string]int{}
	for _, c := range res.Comments {
		rep.Decisions[c.Verification.Status]++
	}
	if !res.Complete() {
		rep.Status = "incomplete"
		return nil
	}
	rep.Status = "ok"
	if p.cfg.VerifyEscalate {
		p.escalateVerify(ctx, t, in, pkt, res.Comments, rep)
	}
	return res.Comments
}

// escalateVerify runs the escalation agent over the findings the packet
// could not settle and folds its account into rep.
func (p *Poller) escalateVerify(ctx context.Context, t verifyTarget, in verify.Input, pkt *verify.Packet, comments []types.LineComment, rep *VerifyReport) {
	start := time.Now()
	capUSD := p.cfg.VerifyDailyBudgetUSD
	if !rep.Sync {
		capUSD *= verifyShadowBudgetShare
	}
	if !p.verifySpend.allow(start, capUSD) {
		rep.AgentError = "over_budget"
		return
	}
	res, err := escalateVerifyFn(ctx, p.verifyAgentConfig(ctx, t.CloneRoot), p.agentSpawner, service.VerifyAgentInput{
		Owner: t.Owner, Repo: t.Repo, DefaultBranch: t.BaseRef, PRNumber: t.Number,
		HeadSHA: in.Head, BaseSHA: in.Base, Packet: pkt, Findings: in.Findings, Slots: p.ensembleSlots,
	}, comments)
	rep.Escalated, rep.AgentMS = res.Escalated, res.DurationMS
	p.verifySpend.add(time.Now(), res.CostUSD)
	rep.AgentCostUSD = res.CostUSD
	if err != nil {
		rep.AgentError = err.Error()
		return
	}
	if res.Escalated == 0 {
		return
	}
	rep.AgentDecisions = res.Decisions
	rep.Decisions = map[string]int{}
	for _, c := range comments {
		rep.Decisions[c.Verification.Status]++
	}
}

func (p *Poller) verifyAgentConfig(ctx context.Context, cloneRoot string) service.AgentConfig {
	backend := p.cfg.VerifyAgentBackend
	if backend == "" {
		backend = p.cfg.AgentBackend
	}
	model := p.cfg.VerifyAgentModel
	if model == "" {
		model = "claude-opus-5-5"
		if strings.EqualFold(backend, service.AgentBackendOpenRouter) {
			model = "anthropic/claude-opus-5.5"
		}
	}
	token := ""
	if p.ghClientConcrete != nil {
		if tok, err := p.ghClientConcrete.CurrentToken(ctx); err == nil {
			token = tok
		}
	}
	return service.AgentConfig{
		CloneRootDir: cloneRoot, LogsDir: p.cfg.AgentLogsDir,
		WallClock: time.Duration(p.cfg.VerifyAgentWallClockSec) * time.Second, MaxTurns: p.cfg.VerifyAgentMaxTurns,
		GitHubToken: token, Backend: backend, Model: model, Effort: p.cfg.AgentEffort,
		AnthropicAPIKey: p.cfg.AnthropicAPIKey, OpenRouterAPIKey: p.cfg.OpenRouterAPIKey, OpenRouterBaseURL: p.cfg.OpenRouterBaseURL,
		FailureLogSink: p.persistAgentFailureLog,
	}
}

// verifyWallClock bounds one verification: the packet call, plus one agent
// wall clock per wave of parallel escalation batches when any finding can
// reach the agent.
func (p *Poller) verifyWallClock() time.Duration {
	wall := time.Duration(p.cfg.VerifyWallClockSec) * time.Second
	if p.cfg.VerifyEscalate {
		waves := (verify.MaxAgentBatches + service.AgentBatchParallel - 1) / max(1, service.AgentBatchParallel)
		wall += time.Duration(waves*p.cfg.VerifyAgentWallClockSec) * time.Second
	}
	return wall
}

// recordVerification verifies in the background and saves the decisions
// beside the run's sidecar without touching the review.
func (p *Poller) recordVerification(job ReviewJob, t verifyTarget, in verify.Input, wall time.Duration, rep VerifyReport) {
	ctx, cancel := context.WithTimeout(context.Background(), wall)
	defer cancel()
	verified := p.runVerify(ctx, t, in, &rep)
	if rep.Status == "ok" {
		rep.Status = "recorded"
	}
	art := verifyArtifact{RunID: job.RunID, Owner: job.PR.Owner, Repo: job.PR.Repo, Number: job.PR.Number, Head: in.Head, Base: in.Base, Report: rep}
	for i, f := range in.Findings {
		e := verifyArtifactEntry{Key: f.Key, Merged: f.Comment}
		if i < len(verified) {
			e.Verified = &verified[i]
		}
		art.Findings = append(art.Findings, e)
	}
	logVerify(job, rep)
	body, err := json.Marshal(art)
	if err != nil {
		log.Printf("[VERIFY %s/%s#%d] encode artifact: %v", job.PR.Owner, job.PR.Repo, job.PR.Number, err)
		return
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer saveCancel()
	if err := p.saveImmutableReviewArtifact(saveCtx, rep.Artifact, "application/json", body); err != nil {
		log.Printf("[VERIFY %s/%s#%d] save artifact: %v", job.PR.Owner, job.PR.Repo, job.PR.Number, err)
	}
}

func verifyArtifactName(job ReviewJob) string {
	return strings.TrimSuffix(gcs.ReviewRunJSONFileName(job.PR.Owner, job.PR.Repo, job.PR.Number, job.PR.CommitSHA, job.RunID), ".json") + ".verify.json"
}

// reviewPublishBound reports whether the review will try to post: publish
// is not skipped, the PR is not a draft (or the run may post on one) and the
// author passes the gate.
func (p *Poller) reviewPublishBound(job ReviewJob) bool {
	if job.SkipPublish || (job.PR.Draft && !p.publishesDraft(job)) {
		return false
	}
	gate, err := p.publishGate()
	return err == nil && gate.Allowed(job.PR.Author)
}

func logVerify(job ReviewJob, rep VerifyReport) {
	cost := 0.0
	if rep.Call != nil {
		cost = rep.Call.CostUSD
	}
	log.Printf("[VERIFY %s/%s#%d] mode=%s sync=%t status=%s skip=%s findings=%d decisions=%v missing=%d invalid=%d items=%d est_tokens=%d cost=$%.4f ms=%d err=%q escalated=%d agent=%v agent_cost=$%.4f agent_ms=%d agent_err=%q",
		job.PR.Owner, job.PR.Repo, job.PR.Number, rep.Mode, rep.Sync, rep.Status, rep.Skip, rep.Findings, rep.Decisions,
		len(rep.Missing), len(rep.Invalid), rep.PacketItems, rep.EstimatedTokens, cost, rep.DurationMS, rep.Error,
		rep.Escalated, rep.AgentDecisions, rep.AgentCostUSD, rep.AgentMS, rep.AgentError)
}

// verifyTimeoutAllowance is the time on mode adds to an ensemble review.
func (p *Poller) verifyTimeoutAllowance() time.Duration {
	if p.cfg == nil || p.cfg.EnsembleVerify != verifyModeOn {
		return 0
	}
	return p.verifyWallClock()
}
