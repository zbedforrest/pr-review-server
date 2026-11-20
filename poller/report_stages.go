package poller

import (
	"fmt"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/service"
)

// stageModels names the configured model of each stage that does not report
// the model it was served.
type stageModels struct {
	VerifyAgent, Gate, Rewrite string
}

func (p *Poller) stageModels() stageModels {
	_, verifyAgent := p.verifyAgentBackendModel()
	return stageModels{VerifyAgent: verifyAgent, Gate: p.cfg.GraderModel, Rewrite: p.cfg.PublishRewriteModel}
}

// reportStages is the generation block's per-stage account: the review runs
// (one agent or the ensemble and its merge), then verify, gate and rewrite
// when they ran.
func reportStages(ens *EnsembleReport, agent *service.AgentReview, agentModel string, configured stageModels) []html.StageRecord {
	if ens == nil {
		if agent == nil {
			return nil
		}
		return []html.StageRecord{{Name: "Review", Model: agentModel, Detail: fmt.Sprintf("%d findings", len(agent.Comments)), CostUSD: agent.CostUSD, DurationMS: agent.DurationMS}}
	}
	runsCost, runsMS, models := 0.0, int64(0), map[string]bool{}
	for _, r := range ens.Runs {
		runsCost += r.CostUSD
		if r.DurationMS > runsMS {
			runsMS = r.DurationMS
		}
		if m := firstNonEmpty(r.ServedModel, r.RequestedFrom); m != "" {
			models[m] = true
		}
	}
	stages := []html.StageRecord{{Name: "Review runs", Model: joinKeys(models),
		Detail: runsDetail(ens), CostUSD: runsCost, DurationMS: runsMS}}
	if ens.Fallback != "" {
		stages = append(stages, html.StageRecord{Name: "Fallback review", Model: agentModel, Detail: "profile " + ens.Fallback})
	} else {
		stages = append(stages, html.StageRecord{Name: "Merge", Model: ens.MergeModel, CostUSD: ens.Merge.Call.CostUSD, DurationMS: ens.Merge.Call.Latency.Milliseconds()})
	}
	if v := ens.Verify; v != nil && v.Sync {
		cost, model := v.AgentCostUSD, configured.VerifyAgent
		if v.Call != nil {
			cost += v.Call.CostUSD
			model = firstNonEmpty(v.Call.ServedModel, v.Call.RequestedModel)
		}
		stages = append(stages, html.StageRecord{Name: "Verify", Model: model, Detail: statusDetail(v.Status, v.Skip, v.Decisions, verifyWords), CostUSD: cost, DurationMS: v.DurationMS})
	}
	if g := ens.Gate; g != nil {
		stages = append(stages, html.StageRecord{Name: "Quality gate", Model: configured.Gate, Detail: modeDetail(g.Mode, statusDetail(g.Status, g.Skip, g.Grades, gateWords)), CostUSD: g.CostUSD, DurationMS: g.DurationMS})
	}
	if r := ens.Rewrite; r != nil {
		detail := statusDetail(r.Status, r.Skip, nil, nil)
		if r.Items > 0 {
			detail = fmt.Sprintf("%d of %d rewritten", r.Rewritten, r.Items)
		}
		stages = append(stages, html.StageRecord{Name: "Rewrite", Model: configured.Rewrite, Detail: modeDetail(r.Mode, detail), CostUSD: r.CostUSD, DurationMS: r.DurationMS})
	}
	return stages
}

// runsDetail accounts for every run launched: stopping at quorum cancels the
// stragglers by design, so those are not failures.
func runsDetail(ens *EnsembleReport) string {
	detail := fmt.Sprintf("%d of %d runs used", ens.Merged, ens.Launched)
	if spare := ens.Valid - ens.Merged; spare > 0 {
		detail += fmt.Sprintf(", %d finished but not needed", spare)
	}
	if unused := ens.Launched - ens.Valid; unused > 0 {
		switch ens.StopReason {
		case "quorum":
			detail += fmt.Sprintf(", %d cancelled at quorum", unused)
		case "straggler_cutoff":
			detail += fmt.Sprintf(", %d cut off as too slow", unused)
		default:
			detail += fmt.Sprintf(", %d failed", unused)
		}
	}
	return detail
}

// A stage's counts in the order and words a reader meets them elsewhere on
// the page.
var (
	verifyWords = [][2]string{{"keep", "kept"}, {"revise", "revised"}, {"hold", "held back"}, {"drop", "refuted"}, {"unavailable", "could not be checked"}}
	gateWords   = [][2]string{{"GOOD", "good"}, {"OK", "ok"}, {"BAD", "bad"}}
)

var skipWords = map[string]string{
	"not_publish_bound":  "this review was not going to post",
	"no_findings":        "no findings to check",
	"no_inline_findings": "no inline findings to rewrite",
	"over_budget":        "the day's budget was spent",
	"busy":               "another run held the slot",
	"deadline":           "too little time left in the run",
}

func statusDetail(status, skip string, counts map[string]int, words [][2]string) string {
	if skip != "" {
		if plain, ok := skipWords[skip]; ok {
			return "not run: " + plain
		}
		return "not run: " + strings.ReplaceAll(skip, "_", " ")
	}
	if len(counts) == 0 {
		return status
	}
	var parts []string
	named := map[string]bool{}
	for _, w := range words {
		named[w[0]] = true
		if n := counts[w[0]]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, w[1]))
		}
	}
	var rest []string
	for k, n := range counts {
		if !named[k] && n > 0 {
			rest = append(rest, fmt.Sprintf("%d %s", n, strings.ToLower(strings.ReplaceAll(k, "_", " "))))
		}
	}
	sort.Strings(rest)
	return strings.Join(append(parts, rest...), ", ")
}

func modeDetail(mode, detail string) string {
	if mode != "" && mode != gateModeOn {
		return detail + " (" + mode + ")"
	}
	return detail
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
