package server

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/approval"
)

type approvalProgressResponse struct {
	ScanID    string `json:"scan_id"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Finished  int    `json:"finished"`
	Running   int    `json:"running"`
	Queued    int    `json:"queued"`
	Summary   string `json:"summary"`
	ToolCalls int    `json:"tool_calls"`
}

func (s *Server) handleApprovalProgress(w http.ResponseWriter, scan db.ApprovalScan) {
	rows, err := s.approvalStore().ListApprovalTargets(scan.UserID, scan.ID, db.MaxApprovalTargetsPerScan, "")
	if err != nil {
		approvalError(w, err)
		return
	}
	progress := approvalProgressResponse{ScanID: scan.ID, Status: scan.Status, Total: scan.Total, Summary: "Waiting for an available investigator"}
	for _, target := range rows {
		progress.ToolCalls += target.ToolCalls
		switch target.ExecutionStatus {
		case "completed", "failed", "timed_out", "cancelled":
			progress.Finished++
		case "queued":
			progress.Queued++
		default:
			progress.Running++
			if progress.Running == 1 {
				progress.Summary = approval.ActivitySummary(approval.Activity{Stage: target.ExecutionStatus})
				if summary, err := approval.CleanActivitySummary(target.Summary); err == nil {
					progress.Summary = summary
				}
			}
		}
	}
	if scan.CancelRequested {
		progress.Summary = "Stopping investigators and cancelling remaining pull requests"
	} else if progress.Finished == progress.Total {
		progress.Summary = "Investigation finished; review the evidence and results below"
	}
	writeV1JSON(w, http.StatusOK, progress)
}

func (s *Server) approvalProgressSummarizer() func(context.Context, []approval.Activity) (string, error) {
	if s.approvalExecution != nil && s.approvalExecution.summarize != nil {
		return s.approvalExecution.summarize
	}
	if s.cfg == nil || os.Getenv("APPROVAL_CANDIDATES_PROGRESS_SUMMARIES") == "false" {
		return nil
	}
	model := approval.ActivitySummaryModel{Model: os.Getenv("APPROVAL_CANDIDATES_PROGRESS_MODEL")}
	switch {
	case s.cfg.GeminiAPIKey != "":
		model.Provider, model.APIKey = "gemini", s.cfg.GeminiAPIKey
		if model.Model == "" {
			model.Model = "gemini-2.5-flash-lite"
		}
	case s.cfg.OpenRouterAPIKey != "":
		model.Provider, model.APIKey = "openrouter", s.cfg.OpenRouterAPIKey
		if model.Model == "" {
			model.Model = "google/gemini-2.5-flash-lite"
		}
	default:
		return nil
	}
	return model.Summarize
}

func (s *Server) startApprovalProgress(parent context.Context, target db.ApprovalTarget) (func(approval.Activity), func()) {
	ctx, cancel := context.WithCancel(parent)
	events := make(chan approval.Activity, 1)
	done := make(chan struct{})
	report := func(activity approval.Activity) {
		select {
		case <-events:
		default:
		}
		select {
		case events <- activity:
		default:
		}
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		s.runApprovalProgress(ctx, target, events, ticker.C, s.approvalProgressSummarizer())
	}()
	var once sync.Once
	return report, func() { once.Do(cancel); <-done }
}

func (s *Server) runApprovalProgress(ctx context.Context, target db.ApprovalTarget, events <-chan approval.Activity, ticks <-chan time.Time, summarize func(context.Context, []approval.Activity) (string, error)) {
	current := approval.Activity{Stage: "collecting"}
	history := []approval.Activity{current}
	summary, saved := approval.ActivitySummary(current), ""
	nextSummary := time.Now().Add(10 * time.Second)
	lastSummarized := approval.Activity{}
	calls := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			select {
			case activity := <-events:
				if activity != current {
					current = activity
					history = append(history, current)
					if len(history) > 8 {
						history = history[len(history)-8:]
					}
					summary = approval.ActivitySummary(current)
				}
			default:
			}
			if summary != saved {
				if err := s.approvalStore().SetApprovalTargetProgress(target.ID, target.LeaseToken, summary, time.Now()); err != nil {
					return
				}
				saved = summary
			}
			if summarize == nil || calls >= 18 || current == lastSummarized || now.Before(nextSummary) {
				continue
			}
			calls++
			lastSummarized = current
			nextSummary = now.Add(10 * time.Second)
			callCtx, stop := context.WithTimeout(ctx, 4*time.Second)
			generated, err := summarize(callCtx, history)
			stop()
			if err == nil {
				if clean, err := approval.CleanActivitySummary(generated); err == nil {
					summary = clean
				}
			}
		}
	}
}
