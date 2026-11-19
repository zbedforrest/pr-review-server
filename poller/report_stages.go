package poller

import (
	"fmt"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/service"
)

// reportStages is the generation block's per-stage account: the review runs
// (one agent or the ensemble and its merge), then verify, gate and rewrite
// when they ran.
func reportStages(ens *EnsembleReport, agent *service.AgentReview, agentModel string) []html.StageRecord {
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
		Detail: fmt.Sprintf("%d launched, %d valid, %d merged", ens.Launched, ens.Valid, ens.Merged), CostUSD: runsCost, DurationMS: runsMS}}
	if ens.Fallback != "" {
		stages = append(stages, html.StageRecord{Name: "Fallback review", Model: agentModel, Detail: "profile " + ens.Fallback})
	} else {
		stages = append(stages, html.StageRecord{Name: "Merge", Model: ens.MergeModel, CostUSD: ens.Merge.Call.CostUSD, DurationMS: ens.Merge.Call.Latency.Milliseconds()})
	}
	if v := ens.Verify; v != nil && v.Sync {
		cost, model := v.AgentCostUSD, ""
		if v.Call != nil {
			cost += v.Call.CostUSD
			model = firstNonEmpty(v.Call.ServedModel, v.Call.RequestedModel)
		}
		stages = append(stages, html.StageRecord{Name: "Verify", Model: model, Detail: statusDetail(v.Status, v.Skip, v.Decisions), CostUSD: cost, DurationMS: v.DurationMS})
	}
	if g := ens.Gate; g != nil {
		stages = append(stages, html.StageRecord{Name: "Quality gate", Detail: modeDetail(g.Mode, statusDetail(g.Status, g.Skip, g.Grades)), CostUSD: g.CostUSD, DurationMS: g.DurationMS})
	}
	if r := ens.Rewrite; r != nil {
		detail := statusDetail(r.Status, r.Skip, nil)
		if r.Items > 0 {
			detail = fmt.Sprintf("%d of %d rewritten", r.Rewritten, r.Items)
		}
		stages = append(stages, html.StageRecord{Name: "Rewrite", Detail: modeDetail(r.Mode, detail), CostUSD: r.CostUSD, DurationMS: r.DurationMS})
	}
	return stages
}

func statusDetail(status, skip string, counts map[string]int) string {
	if skip != "" {
		return "skipped: " + strings.ReplaceAll(skip, "_", " ")
	}
	if len(counts) == 0 {
		return status
	}
	parts := make([]string, 0, len(counts))
	for k, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(k)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
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
