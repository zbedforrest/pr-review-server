package poller

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/ensemble"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/telemetry/newrelic"
)

func TestReviewTelemetryReportsTheRunItsEnsembleAndTheMergeCall(t *testing.T) {
	var mu sync.Mutex
	var events []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, _ := gzip.NewReader(r.Body)
		var batch []map[string]any
		_ = json.NewDecoder(zr).Decode(&batch)
		mu.Lock()
		events = append(events, batch...)
		mu.Unlock()
	}))
	defer srv.Close()
	p := &Poller{telemetry: newrelic.New(newrelic.Config{AccountID: "1", LicenseKey: "k", Endpoint: srv.URL})}
	exec := &reviewExecution{
		Job: ReviewJob{RunID: "run-1", TriggerSource: "auto_review", PR: github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "abc"},
			Config: runconfig.Snapshot{Effective: runconfig.Effective{Profile: runconfig.ProfileLite}}},
		providerAttempts: map[string]service.ProviderAttemptEvent{"a": {CostUSD: 0.03, InputTokens: 100, OutputTokens: 10}},
		Ensemble: &EnsembleReport{
			Launched: 5, Valid: 4, Merged: 4, StopReason: "quorum", MergeModel: "vendor/merge", TotalCostUSD: 0.15,
			Runs:  []EnsembleRun{{Invocation: 1, Status: "valid", Merged: true, CostUSD: 0.03}, {Invocation: 5, Status: "cancelled"}},
			Merge: ensemble.MergeResult{Method: "author", Clusters: 3, Call: llm.Call{RequestedModel: "vendor/merge", ServedModel: "vendor/merge-1", Provider: "P", CostUSD: 0.007}},
		},
	}
	status, verdict, crit := "completed", "approve_suggestions", 0
	p.recordReviewTelemetry(exec, db.ReviewRunPatch{Status: &status, Verdict: &verdict, CriticalCount: &crit}, 120000)
	p.telemetry.Close()

	mu.Lock()
	defer mu.Unlock()
	by := map[string][]map[string]any{}
	for _, e := range events {
		by[e["eventType"].(string)] = append(by[e["eventType"].(string)], e)
	}
	if len(by[eventReviewRun]) != 1 || len(by[eventEnsembleRun]) != 2 || len(by[eventLLMRequest]) != 1 {
		t.Fatalf("events = %v", events)
	}
	run := by[eventReviewRun][0]
	if run["profile"] != "lite" || run["repo"] != "acme/example" || run["ensemble_stop_reason"] != "quorum" || run["cost_usd"] != 0.15 || run["verdict"] != verdict {
		t.Fatalf("review event = %v", run)
	}
	if call := by[eventLLMRequest][0]; call["served_model"] != "vendor/merge-1" || call["provider"] != "P" {
		t.Fatalf("merge call event = %v", call)
	}
}

func TestReviewTelemetryIsANoOpWithoutASink(t *testing.T) {
	(&Poller{}).recordReviewTelemetry(&reviewExecution{}, db.ReviewRunPatch{}, 0)
}
