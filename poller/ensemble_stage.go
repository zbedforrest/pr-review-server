package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"pr-review-server/pkg/reviewer/ensemble"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

// Straggler and relaunch policy for ensemble runs. Slow runs catch no more
// defects than fast ones in replay, so once enough runs are valid the review
// stops waiting at ensembleStragglerFactor times the first valid run's
// duration (never sooner than ensembleStragglerFloor). A run that fails
// within ensembleRelaunchWindow is relaunched once, up to ensembleMaxRelaunches
// per review.
const (
	ensembleStragglerFactor = 2
	ensembleStragglerFloor  = 120 * time.Second
	ensembleMaxRelaunches   = 2
	ensembleCancelDrain     = 30 * time.Second
)

var ensembleRelaunchWindow = 60 * time.Second

// ensembleFallbackMergeModel writes the merge when the configured merge model
// fails; the deterministic merge follows if both do.
const ensembleFallbackMergeModel = runconfig.EnsembleModel

// EnsembleRun is one agent run's outcome, for telemetry.
type EnsembleRun struct {
	Invocation    int     `json:"invocation"`
	Status        string  `json:"status"` // valid, valid_unmerged, parse_fallback, failed, cancelled
	Error         string  `json:"error,omitempty"`
	DurationMS    int64   `json:"duration_ms"`
	Findings      int     `json:"findings"`
	CostUSD       float64 `json:"cost_usd"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	Relaunch      bool    `json:"relaunch,omitempty"`
	Merged        bool    `json:"merged"`
	BudgetUnits   int     `json:"budget_units"`
	ServedModel   string  `json:"served_model,omitempty"`
	RequestedFrom string  `json:"requested_model,omitempty"`
	// Comments are the run's own findings, kept in the sidecar so a merge
	// can be compared with the runs it merged.
	Comments []ensembleComment `json:"comments,omitempty"`
}

type ensembleComment struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Comment  string `json:"comment"`
}

// EnsembleReport is the review-level account of an ensemble run.
type EnsembleReport struct {
	Runs         []EnsembleRun        `json:"runs"`
	Launched     int                  `json:"launched"`
	Valid        int                  `json:"valid"`
	Merged       int                  `json:"merged"`
	Relaunches   int                  `json:"relaunches"`
	StopReason   string               `json:"stop_reason"` // quorum, straggler_cutoff, all_finished, fallback
	QuorumMS     int64                `json:"quorum_ms"`
	Fallback     string               `json:"fallback,omitempty"`
	Merge        ensemble.MergeResult `json:"merge"`
	MergeModel   string               `json:"merge_model"`
	TotalCostUSD float64              `json:"total_cost_usd"`
}

type ensembleOutcome struct {
	invocation int
	relaunch   bool
	review     *service.AgentReview
	err        error
	started    time.Time
	finished   time.Time
}

var errEnsembleTooFewValid = errors.New("ensemble: too few valid runs")

// runEnsembleAgent runs one agent of an ensemble, and prepareEnsembleCheckout
// makes the worktree its runs share; tests replace both.
var (
	runEnsembleAgent        = service.RunAgentReview
	prepareEnsembleCheckout = service.PrepareSharedCheckout
)

// runEnsembleStage runs the ensemble and returns its merged review in the
// shape of a single agent's output, so the rest of the pipeline is unchanged.
// With too few valid runs it runs the fallback profile's single agent.
func (p *Poller) runEnsembleStage(ctx context.Context, execution *reviewExecution, base service.AgentConfig, result *service.ReviewResult) (*service.AgentReview, error) {
	pr := execution.Job.PR
	ens := execution.Job.Config.Effective.Ensemble
	started := time.Now()
	review, report, err := p.runEnsembleAgents(ctx, ens, base, pr.Owner, pr.Repo, result.BaseRef, pr.Number, pr.CommitSHA)
	defer func() { execution.Ensemble = &report }()
	if errors.Is(err, errEnsembleTooFewValid) {
		report.StopReason, report.Fallback = "fallback", ens.FallbackProfile
		logEnsembleReport(pr.Owner, pr.Repo, pr.Number, report)
		fallback, ferr := p.ensembleFallbackConfig(ens.FallbackProfile, base)
		if ferr != nil {
			return nil, fmt.Errorf("ensemble fallback: %w", ferr)
		}
		out, runErr := service.RunAgentReview(ctx, fallback, p.agentSpawner, pr.Owner, pr.Repo, result.BaseRef, pr.Number, pr.CommitSHA, result.Comments)
		if runErr == nil {
			out.CostUSD += report.TotalCostUSD
			out.DurationMS = time.Since(started).Milliseconds()
			if runconfig.LiteHygieneEnabled() && ens.MinSupportCritical > 1 {
				report.Merge.Report.SupportDowngraded += capLoneRunCriticals(out.Comments)
			}
		}
		return out, runErr
	}
	if err != nil {
		return nil, err
	}
	logEnsembleReport(pr.Owner, pr.Repo, pr.Number, report)
	return review, nil
}

// capLoneRunCriticals applies the ensemble's support rule to the fallback
// run, which by construction has support 1 behind every CRITICAL.
func capLoneRunCriticals(comments []types.LineComment) int {
	downgraded := 0
	for i := range comments {
		if comments[i].FilePath != "SUMMARY" && strings.EqualFold(strings.TrimSpace(comments[i].Importance), "CRITICAL") {
			comments[i].Importance = "MEDIUM"
			downgraded++
		}
	}
	return downgraded
}

// ensembleFallbackConfig is the single-agent config of the fallback profile,
// keeping the run's clone, credentials and observers.
func (p *Poller) ensembleFallbackConfig(profile string, base service.AgentConfig) (service.AgentConfig, error) {
	eff, err := runconfig.Expand(profile, runconfig.Effective{})
	if err != nil {
		return base, err
	}
	cfg := base
	cfg.SharedCheckout = ""
	cfg.Backend, cfg.Model, cfg.Effort = eff.Agent.Backend, eff.Agent.Model, eff.Agent.Effort
	cfg.Tools, cfg.Prompt, cfg.MaxTurns = eff.Agent.Tools, eff.Agent.Prompt, eff.Agent.MaxTurns
	cfg.WallClock = time.Duration(eff.Agent.WallClockSeconds) * time.Second
	cfg.CappedDiffWallClock = time.Duration(runconfig.CappedDiffWallClockSeconds(eff)) * time.Second
	cfg.Invocation = 0
	return cfg, nil
}

func (p *Poller) runEnsembleAgents(ctx context.Context, ens *runconfig.Ensemble, base service.AgentConfig, owner, repo, baseRef string, number int, sha string) (*service.AgentReview, EnsembleReport, error) {
	start := time.Now()
	// One read-only worktree serves every run (the OpenRouter agents run in
	// a read-only sandbox). If it cannot be made, each run clones its own.
	if dir, cleanup, err := prepareEnsembleCheckout(ctx, base, owner, repo, baseRef, number, sha); err == nil {
		base.SharedCheckout = dir
		defer func() {
			if cerr := cleanup(); cerr != nil {
				log.Printf("[ENSEMBLE %s/%s#%d] shared checkout cleanup failed: %v", owner, repo, number, cerr)
			}
		}()
	} else {
		log.Printf("[ENSEMBLE %s/%s#%d] shared checkout failed, runs clone their own: %v", owner, repo, number, err)
	}
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	results := make(chan ensembleOutcome, ens.Runs+ensembleMaxRelaunches)
	cancels := map[int]context.CancelFunc{}
	var report EnsembleReport
	launch := func(invocation int, relaunch bool) {
		runCtx, cancel := context.WithCancel(ctx)
		cancels[invocation] = cancel
		report.Launched++
		go func() {
			o := ensembleOutcome{invocation: invocation, relaunch: relaunch, started: time.Now()}
			defer func() { o.finished = time.Now(); results <- o }()
			if p.ensembleSlots != nil {
				select {
				case p.ensembleSlots <- struct{}{}:
					defer func() { <-p.ensembleSlots }()
				case <-runCtx.Done():
					o.err = runCtx.Err()
					return
				}
			}
			cfg := base
			cfg.Invocation = invocation
			o.review, o.err = runEnsembleAgent(runCtx, cfg, p.agentSpawner, owner, repo, baseRef, number, sha, nil)
		}()
	}
	for i := 1; i <= ens.Runs; i++ {
		launch(i, false)
	}
	next := ens.Runs + 1
	var valid, all []ensembleOutcome
	var cutoff <-chan time.Time
	pending := report.Launched
	report.StopReason = "all_finished"
wait:
	for pending > 0 {
		select {
		case o := <-results:
			pending--
			all = append(all, o)
			if o.err == nil && o.review != nil && !o.review.ParseFallback {
				valid = append(valid, o)
				if len(valid) == 1 {
					first := o.finished.Sub(start)
					cutoff = time.After(max(ensembleStragglerFloor, ensembleStragglerFactor*first) - first)
				}
				if len(valid) >= ens.Quorum {
					report.StopReason = "quorum"
					break wait
				}
				continue
			}
			if report.Relaunches < ensembleMaxRelaunches && o.finished.Sub(o.started) < ensembleRelaunchWindow && ctx.Err() == nil {
				report.Relaunches++
				launch(next, true)
				next++
				pending++
			}
		case <-cutoff:
			if len(valid) >= ens.MinValid {
				report.StopReason = "straggler_cutoff"
				break wait
			}
		case <-ctx.Done():
			break wait
		}
	}
	report.QuorumMS = time.Since(start).Milliseconds()
	cancelled := map[int]bool{}
	for inv, cancel := range cancels {
		if !containsInvocation(all, inv) {
			cancelled[inv] = true
			cancel()
		}
	}
	all = drainEnsemble(results, all, pending)
	report.Valid = len(valid)
	sort.Slice(valid, func(a, b int) bool { return valid[a].invocation < valid[b].invocation })
	if len(valid) > ens.Quorum {
		valid = valid[:ens.Quorum]
	}
	if len(valid) < ens.MinValid {
		report.Runs, report.TotalCostUSD = ensembleRuns(all, nil, cancelled)
		if ctx.Err() != nil {
			return nil, report, ctx.Err()
		}
		return nil, report, errEnsembleTooFewValid
	}
	report.Merged = len(valid)
	report.Runs, report.TotalCostUSD = ensembleRuns(all, valid, cancelled)
	runs := make([][]types.LineComment, len(valid))
	for i, o := range valid {
		runs[i] = o.review.Comments
	}
	merge, model := p.mergeEnsemble(ctx, ens, runs)
	report.Merge, report.MergeModel = merge, model
	report.TotalCostUSD += merge.Call.CostUSD
	return ensembleReview(valid, merge, report, time.Since(start)), report, nil
}

// mergeEnsemble merges with the configured model, then the fallback model,
// then deterministically; it returns the result and the model that wrote it.
func (p *Poller) mergeEnsemble(ctx context.Context, ens *runconfig.Ensemble, runs [][]types.LineComment) (ensemble.MergeResult, string) {
	opts := ensemble.Options{}
	if runconfig.LiteHygieneEnabled() {
		opts.MinSupportCritical = ens.MinSupportCritical
	}
	if p.cfg.OpenRouterAPIKey == "" {
		return ensemble.Merge(ctx, runs, nil, opts), "deterministic"
	}
	var last ensemble.MergeResult
	for _, m := range []string{ens.MergeModel, ensembleFallbackMergeModel} {
		if m == "" {
			continue
		}
		client := llm.NewOpenRouterClient(p.cfg.OpenRouterAPIKey, p.cfg.OpenRouterBaseURL, m, false)
		last = ensemble.Merge(ctx, runs, &ensemble.LLMAuthor{Client: client, Model: m}, opts)
		if last.Method == "author" || last.AuthorErr == "" {
			return last, m
		}
		log.Printf("[ENSEMBLE] merge author %s failed: %s", m, last.AuthorErr)
	}
	return last, "deterministic"
}

// ensembleReview presents the merge as one agent's output. Run-level fields
// (diff source, cited files, clone) come from the merged runs.
func ensembleReview(valid []ensembleOutcome, merge ensemble.MergeResult, report EnsembleReport, elapsed time.Duration) *service.AgentReview {
	first := valid[0].review
	out := &service.AgentReview{
		Comments:          append(append([]types.LineComment(nil), merge.Findings...), types.LineComment{FilePath: "SUMMARY", Summary: merge.Summary}),
		DiffSource:        first.DiffSource,
		CloneDir:          first.CloneDir,
		LogPath:           first.LogPath,
		RequestedModel:    first.RequestedModel,
		ServedModel:       first.ServedModel,
		Backend:           first.Backend,
		Effort:            first.Effort,
		DurationMS:        elapsed.Milliseconds(),
		CostUSD:           report.TotalCostUSD,
		CitedFileContents: map[string]string{},
	}
	for _, o := range valid {
		r := o.review
		out.InputTokens += r.InputTokens
		out.OutputTokens += r.OutputTokens
		out.AssistantTurns += r.AssistantTurns
		out.BudgetUnitsUsed += r.BudgetUnitsUsed
		out.PrepDurationMS = max(out.PrepDurationMS, r.PrepDurationMS)
		for k, v := range r.CitedFileContents {
			out.CitedFileContents[k] = v
		}
	}
	return out
}

// drainEnsemble collects runs still in flight after cancellation, waiting a
// bounded time so their worktrees are cleaned up before the job moves on.
func drainEnsemble(results <-chan ensembleOutcome, all []ensembleOutcome, pending int) []ensembleOutcome {
	deadline := time.After(ensembleCancelDrain)
	for ; pending > 0; pending-- {
		select {
		case o := <-results:
			all = append(all, o)
		case <-deadline:
			return all
		}
	}
	return all
}

// ensembleRuns labels every run: merged ("valid"), valid but finished after
// the quorum ("valid_unmerged"), unparseable ("parse_fallback"), stopped by
// the ensemble ("cancelled", whatever error the cancellation surfaced as),
// or "failed".
func ensembleRuns(all, merged []ensembleOutcome, cancelled map[int]bool) ([]EnsembleRun, float64) {
	var runs []EnsembleRun
	total := 0.0
	for _, o := range all {
		r := EnsembleRun{Invocation: o.invocation, Relaunch: o.relaunch, DurationMS: o.finished.Sub(o.started).Milliseconds()}
		switch {
		case containsInvocation(merged, o.invocation):
			r.Status, r.Merged = "valid", true
		case o.err == nil && o.review != nil && o.review.ParseFallback:
			r.Status = "parse_fallback"
		case o.err == nil && o.review != nil:
			r.Status = "valid_unmerged"
		case cancelled[o.invocation] || errors.Is(o.err, context.Canceled):
			r.Status = "cancelled"
		default:
			r.Status, r.Error = "failed", errString(o.err)
		}
		if o.review != nil {
			r.Findings = len(o.review.Comments)
			r.CostUSD, r.InputTokens, r.OutputTokens = o.review.CostUSD, o.review.InputTokens, o.review.OutputTokens
			r.BudgetUnits, r.ServedModel, r.RequestedFrom = o.review.BudgetUnitsUsed, o.review.ServedModel, o.review.RequestedModel
			total += o.review.CostUSD
			for _, c := range o.review.Comments {
				r.Comments = append(r.Comments, ensembleComment{File: c.FilePath, Line: c.LineNumber, Severity: c.Importance, Comment: c.CommentBody})
			}
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].Invocation < runs[b].Invocation })
	return runs, total
}

func containsInvocation(os []ensembleOutcome, inv int) bool {
	for _, o := range os {
		if o.invocation == inv {
			return true
		}
	}
	return false
}

func logEnsembleReport(owner, repo string, number int, r EnsembleReport) {
	b, _ := json.Marshal(r)
	log.Printf("[ENSEMBLE %s/%s#%d] %s", owner, repo, number, b)
}

// ensembleSidecar is the report as stored in the review sidecar: the merged
// findings themselves are already the sidecar's findings.
type ensembleSidecar struct {
	Runs         []EnsembleRun   `json:"runs"`
	Launched     int             `json:"launched"`
	Valid        int             `json:"valid"`
	Merged       int             `json:"merged"`
	Relaunches   int             `json:"relaunches"`
	StopReason   string          `json:"stop_reason"`
	QuorumMS     int64           `json:"quorum_ms"`
	Fallback     string          `json:"fallback,omitempty"`
	MergeMethod  string          `json:"merge_method"`
	MergeModel   string          `json:"merge_model"`
	MergeError   string          `json:"merge_error,omitempty"`
	MergeCall    llm.Call        `json:"merge_call"`
	Clusters     int             `json:"clusters"`
	Guard        ensemble.Report `json:"guard"`
	Support      []int           `json:"support"`
	TotalCostUSD float64         `json:"total_cost_usd"`
}

// sidecar returns the compact form, or nil (no sidecar field) for a nil report.
func (r *EnsembleReport) sidecar() any {
	if r == nil {
		return nil
	}
	return ensembleSidecar{
		Runs: r.Runs, Launched: r.Launched, Valid: r.Valid, Merged: r.Merged, Relaunches: r.Relaunches,
		StopReason: r.StopReason, QuorumMS: r.QuorumMS, Fallback: r.Fallback,
		MergeMethod: r.Merge.Method, MergeModel: r.MergeModel, MergeError: r.Merge.AuthorErr, MergeCall: r.Merge.Call,
		Clusters: r.Merge.Clusters, Guard: r.Merge.Report, Support: r.Merge.Support, TotalCostUSD: r.TotalCostUSD,
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
