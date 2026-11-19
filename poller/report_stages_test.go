package poller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"pr-review-server/pkg/reviewer/ensemble"
	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/service"
)

func TestReportStagesAccountsForEachStageThatRan(t *testing.T) {
	ens := &EnsembleReport{
		Launched: 5, Valid: 5, Merged: 4,
		Runs: []EnsembleRun{
			{CostUSD: 0.10, DurationMS: 60_000, ServedModel: "deepseek/flash"},
			{CostUSD: 0.20, DurationMS: 90_000, RequestedFrom: "deepseek/flash"},
		},
		MergeModel: "merge-model",
		Merge:      ensemble.MergeResult{Call: llm.Call{CostUSD: 0.05}},
		Verify:     &VerifyReport{Sync: true, Status: "applied", Decisions: map[string]int{"keep": 3, "drop": 1}, AgentCostUSD: 0.30, DurationMS: 40_000},
		Gate:       &GateReport{Mode: gateModeOn, Status: "applied", Grades: map[string]int{"GOOD": 2}, CostUSD: 0.58},
		Rewrite:    &RewriteReport{Mode: gateModeOn, Items: 3, Rewritten: 2, CostUSD: 0.04},
	}
	stages := reportStages(ens, nil, "")
	assert.Equal(t, []html.StageRecord{
		{Name: "Review runs", Model: "deepseek/flash", Detail: "5 launched, 5 valid, 4 merged", CostUSD: 0.30000000000000004, DurationMS: 90_000},
		{Name: "Merge", Model: "merge-model", CostUSD: 0.05},
		{Name: "Verify", Detail: "1 drop, 3 keep", CostUSD: 0.30, DurationMS: 40_000},
		{Name: "Quality gate", Detail: "2 good", CostUSD: 0.58},
		{Name: "Rewrite", Detail: "2 of 3 rewritten", CostUSD: 0.04},
	}, stages)
}

func TestReportStagesForASingleAgent(t *testing.T) {
	agent := &service.AgentReview{CostUSD: 1.25, DurationMS: 300_000}
	assert.Equal(t, []html.StageRecord{{Name: "Review", Model: "m", Detail: "0 findings", CostUSD: 1.25, DurationMS: 300_000}}, reportStages(nil, agent, "m"))
	assert.Nil(t, reportStages(nil, nil, "m"))
}
