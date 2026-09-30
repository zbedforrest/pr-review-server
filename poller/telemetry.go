package poller

import (
	"pr-review-server/db"
)

// Event types sent to New Relic. One PrismReviewRun per finished review run;
// ensemble runs add one PrismEnsembleRun per agent run and one PrismLlmRequest
// for the merge author call.
const (
	eventReviewRun   = "PrismReviewRun"
	eventEnsembleRun = "PrismEnsembleRun"
	eventLLMRequest  = "PrismLlmRequest"
)

// recordReviewTelemetry emits the run's events. It reads only what the run
// already holds and never blocks the finalize path.
func (p *Poller) recordReviewTelemetry(exec *reviewExecution, patch db.ReviewRunPatch, durationMS int64) {
	if p.telemetry == nil {
		return
	}
	pr := exec.Job.PR
	base := map[string]any{
		"run_id":     exec.Job.RunID,
		"repo":       pr.Owner + "/" + pr.Repo,
		"pr_number":  pr.Number,
		"commit_sha": pr.CommitSHA,
		"profile":    exec.Job.Config.Effective.Profile,
		"trigger":    exec.Job.TriggerSource,
	}
	ev := copyAttrs(base)
	ev["status"] = deref(patch.Status)
	ev["terminal_code"] = deref(patch.TerminalCode)
	ev["failure_stage"] = deref(patch.FailureStage)
	ev["verdict"] = deref(patch.Verdict)
	ev["duration_ms"] = durationMS
	ev["queue_wait_ms"] = exec.QueueWait.Milliseconds()
	ev["prep_ms"] = exec.PrepMS
	ev["agent_ms"] = exec.AgentMS
	ev["publish"] = !exec.Job.SkipPublish
	if patch.CriticalCount != nil {
		ev["critical"], ev["medium"], ev["low"] = *patch.CriticalCount, derefInt(patch.MediumCount), derefInt(patch.LowCount)
	}
	cost, in, out := exec.attemptTotals()
	if r := exec.Ensemble; r != nil {
		ev["ensemble_launched"], ev["ensemble_valid"], ev["ensemble_merged"] = r.Launched, r.Valid, r.Merged
		ev["ensemble_relaunches"], ev["ensemble_stop_reason"], ev["ensemble_fallback"] = r.Relaunches, r.StopReason, r.Fallback
		ev["ensemble_quorum_ms"], ev["merge_method"], ev["merge_model"] = r.QuorumMS, r.Merge.Method, r.MergeModel
		ev["merge_reinserted"], ev["merge_clusters"] = len(r.Merge.Report.Reinserted), r.Merge.Clusters
		ev["merge_unfolded"] = len(r.Merge.Report.Unfolded)
		cost = max(cost, r.TotalCostUSD)
		for _, run := range r.Runs {
			re := copyAttrs(base)
			re["invocation"], re["status"], re["error"] = run.Invocation, run.Status, run.Error
			re["duration_ms"], re["findings"], re["cost_usd"] = run.DurationMS, run.Findings, run.CostUSD
			re["input_tokens"], re["output_tokens"], re["budget_units"] = run.InputTokens, run.OutputTokens, run.BudgetUnits
			re["relaunch"], re["merged"] = run.Relaunch, run.Merged
			re["requested_model"], re["served_model"] = run.RequestedFrom, run.ServedModel
			p.telemetry.Record(eventEnsembleRun, re)
		}
		if c := r.Merge.Call; c.RequestedModel != "" {
			le := copyAttrs(base)
			le["purpose"], le["requested_model"], le["served_model"], le["provider"] = "ensemble_merge", c.RequestedModel, c.ServedModel, c.Provider
			le["generation_id"], le["attempts"], le["latency_ms"] = c.GenerationID, c.Attempts, c.Latency.Milliseconds()
			le["prompt_tokens"], le["cached_tokens"], le["completion_tokens"], le["reasoning_tokens"] = c.PromptTokens, c.CachedTokens, c.CompletionTokens, c.ReasoningTokens
			le["cost_usd"], le["error"] = c.CostUSD, r.Merge.AuthorErr
			p.telemetry.Record(eventLLMRequest, le)
		}
	}
	ev["cost_usd"], ev["input_tokens"], ev["output_tokens"] = cost, in, out
	p.telemetry.Record(eventReviewRun, ev)
}

// attemptTotals sums the cost and tokens of the run's provider attempts.
func (e *reviewExecution) attemptTotals() (cost float64, in, out int64) {
	e.attemptsMu.Lock()
	defer e.attemptsMu.Unlock()
	for _, a := range e.providerAttempts {
		cost += a.CostUSD
		in += a.InputTokens
		out += a.OutputTokens
	}
	return cost, in, out
}

func copyAttrs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+16)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
