package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/pkg/approval"
	"pr-review-server/pkg/reviewer/payload"
)

type approvalRunHistoryFixture struct {
	db.Database
	runs  []db.ReviewRun
	calls int
}

func (f *approvalRunHistoryFixture) ListReviewRuns(filter db.ReviewRunFilter) ([]db.ReviewRun, error) {
	f.calls++
	var rows []db.ReviewRun
	for _, run := range f.runs {
		if !filter.BeforeAcceptedAt.IsZero() && (run.AcceptedAt.After(filter.BeforeAcceptedAt) || (run.AcceptedAt.Equal(filter.BeforeAcceptedAt) && run.RunID >= filter.BeforeRunID)) {
			continue
		}
		rows = append(rows, run)
		if len(rows) == filter.Limit {
			break
		}
	}
	return rows, nil
}

func approvalPRismFixture(t *testing.T) (*Server, *approvalRunHistoryFixture, approval.Target, payload.Payload) {
	t.Helper()
	sha := strings.Repeat("a", 40)
	target := approval.Target{Owner: "acme", Repo: "example", Number: 7, ExpectedHeadSHA: sha}
	run := db.ReviewRun{RunID: "run-fixture", RepoOwner: "acme", RepoName: "example", PRNumber: 7, CommitSHA: sha, Status: db.ReviewRunStatusCompleted, AcceptedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)}
	run.JSONPath = gcs.ReviewRunJSONFileName(run.RepoOwner, run.RepoName, run.PRNumber, sha, run.RunID)
	history := &approvalRunHistoryFixture{runs: []db.ReviewRun{run}}
	s := &Server{db: history, cfg: &config.Config{ReviewsDir: t.TempDir()}}
	pl := payload.Payload{SchemaVersion: "2", Owner: "acme", Repo: "example", PRNumber: 7, CommitSHA: sha, ReviewRun: &payload.ReviewRunInfo{RunID: run.RunID, JSONPath: run.JSONPath, CompletedAt: time.Unix(2, 0)}, Findings: []payload.Finding{{ID: "old", File: "file.go", Line: 12, Comment: "missing validation", State: "rejected", Active: false}, {ID: "summary", File: "SUMMARY", Comment: "overall assessment", State: "confirmed", Active: true}, {ID: "check", File: "CHECK", Comment: "check outcome", State: "confirmed", Active: true}}}
	writeApprovalSidecar(t, s, run.JSONPath, pl)
	return s, history, target, pl
}

func writeApprovalSidecar(t *testing.T, s *Server, name string, pl payload.Payload) {
	t.Helper()
	file := filepath.Join(s.cfg.ReviewsDir, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalPRismPreservesInactiveAndSummaryEvidence(t *testing.T) {
	s, _, target, _ := approvalPRismFixture(t)
	sources, evidence, concerns, endpoints, err := s.collectApprovalPRism(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Completion != "completed" || len(evidence) != 4 || len(concerns) != 1 {
		t.Fatalf("lost evidence: sources=%+v evidence=%+v concerns=%+v", sources, evidence, concerns)
	}
	if concerns[0].Disposition != "" || !strings.Contains(concerns[0].ID, "run-fixture") {
		t.Fatal("inactive finding pre-resolved or lost run identity")
	}
	for _, endpoint := range endpoints {
		if !endpoint.Complete {
			t.Fatalf("incomplete %+v", endpoint)
		}
	}
}

func TestApprovalPRismRejectsMutableOrMismatchedSidecar(t *testing.T) {
	for _, mode := range []string{"mutable", "sha", "run", "missing", "partial"} {
		t.Run(mode, func(t *testing.T) {
			s, history, target, pl := approvalPRismFixture(t)
			switch mode {
			case "mutable":
				history.runs[0].JSONPath = "latest.json"
			case "sha":
				pl.CommitSHA = strings.Repeat("b", 40)
			case "run":
				pl.ReviewRun.RunID = "different"
			case "missing":
				history.runs[0].JSONPath = ""
			case "partial":
				pl.RequiredChecks = &payload.RequiredChecksInfo{Issued: 2, Answered: 1}
			}
			writeApprovalSidecar(t, s, gcs.ReviewRunJSONFileName("acme", "example", 7, target.ExpectedHeadSHA, "run-fixture"), pl)
			sources, _, _, _, err := s.collectApprovalPRism(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			if len(sources) != 1 || !sources[0].Incomplete {
				t.Fatalf("unsafe source accepted: %+v", sources)
			}
		})
	}
}

func TestApprovalPRismPaginatesAndRetainsRunningReview(t *testing.T) {
	s, history, target, _ := approvalPRismFixture(t)
	history.runs = nil
	for index := 101; index > 0; index-- {
		history.runs = append(history.runs, db.ReviewRun{RunID: strings.Repeat("x", index), RepoOwner: "acme", RepoName: "example", PRNumber: 7, CommitSHA: target.ExpectedHeadSHA, Status: db.ReviewRunStatusRunning, AcceptedAt: time.Unix(int64(index), 0)})
	}
	sources, _, _, endpoints, err := s.collectApprovalPRism(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if history.calls != 2 || len(sources) != 101 || !endpoints[0].Complete {
		t.Fatalf("pagination lost: calls=%d sources=%d endpoints=%+v", history.calls, len(sources), endpoints)
	}
	if sources[0].Completion != "running" {
		t.Fatal("running source lost")
	}
}
