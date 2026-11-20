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
		Launched: 5, Valid: 4, Merged: 4, StopReason: "quorum",
		Runs: []EnsembleRun{
			{CostUSD: 0.10, DurationMS: 60_000, ServedModel: "deepseek/flash"},
			{CostUSD: 0.20, DurationMS: 90_000, RequestedFrom: "deepseek/flash"},
		},
		MergeModel: "merge-model",
		Merge:      ensemble.MergeResult{Call: llm.Call{CostUSD: 0.05}},
		Verify:     &VerifyReport{Sync: true, Status: "applied", Decisions: map[string]int{"keep": 3, "drop": 1, "hold": 1, "revise": 2, "unavailable": 1, "odd_one": 1}, AgentCostUSD: 0.30, DurationMS: 40_000},
		Gate:       &GateReport{Mode: gateModeOn, Status: "applied", Grades: map[string]int{"GOOD": 2, "BAD": 1, "OK": 3}, CostUSD: 0.58},
		Rewrite:    &RewriteReport{Mode: gateModeOn, Items: 3, Rewritten: 2, CostUSD: 0.04},
	}
	stages := reportStages(ens, nil, "", stageModels{VerifyAgent: "verify-agent", Gate: "grader", Rewrite: "rewriter"})
	assert.Equal(t, []html.StageRecord{
		{Name: "Review runs", Model: "deepseek/flash", Detail: "4 of 5 runs used, 1 cancelled at quorum", CostUSD: 0.30000000000000004, DurationMS: 90_000},
		{Name: "Merge", Model: "merge-model", CostUSD: 0.05},
		{Name: "Verify", Model: "verify-agent", Detail: "3 kept, 2 revised, 1 held back, 1 refuted, 1 could not be checked, 1 odd one", CostUSD: 0.30, DurationMS: 40_000},
		{Name: "Quality gate", Model: "grader", Detail: "2 good, 3 ok, 1 bad", CostUSD: 0.58},
		{Name: "Rewrite", Model: "rewriter", Detail: "2 of 3 rewritten", CostUSD: 0.04},
	}, stages)
}

func TestReportStagesForASingleAgent(t *testing.T) {
	agent := &service.AgentReview{CostUSD: 1.25, DurationMS: 300_000}
	assert.Equal(t, []html.StageRecord{{Name: "Review", Model: "m", Detail: "0 findings", CostUSD: 1.25, DurationMS: 300_000}}, reportStages(nil, agent, "m", stageModels{}))
	assert.Nil(t, reportStages(nil, nil, "m", stageModels{}))
}

func TestRunsDetailSaysWhyARunWasNotUsed(t *testing.T) {
	cases := map[string]*EnsembleReport{
		"5 of 5 runs used": {Launched: 5, Valid: 5, Merged: 5, StopReason: "all_finished"},
		"3 of 5 runs used, 1 finished but not needed, 1 cancelled at quorum": {Launched: 5, Valid: 4, Merged: 3, StopReason: "quorum"},
		"3 of 5 runs used, 2 cut off as too slow":                            {Launched: 5, Valid: 3, Merged: 3, StopReason: "straggler_cutoff"},
		"4 of 5 runs used, 1 failed":                                         {Launched: 5, Valid: 4, Merged: 4, StopReason: "all_finished"},
	}
	for want, ens := range cases {
		assert.Equal(t, want, runsDetail(ens))
	}
}

func TestStatusDetailSaysWhyAStageDidNotRun(t *testing.T) {
	assert.Equal(t, "not run: this review was not going to post", statusDetail("skipped", "not_publish_bound", nil, nil))
	assert.Equal(t, "not run: legacy titles", statusDetail("skipped", "legacy_titles", nil, nil))
}
