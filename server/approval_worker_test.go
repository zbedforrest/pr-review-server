package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pr-review-server/db"
	gh "pr-review-server/github"
	"pr-review-server/pkg/approval"
)

type approvalCollectorFunc func(context.Context, approval.Target, approval.Viewer) (approval.Snapshot, error)

func (f approvalCollectorFunc) Collect(ctx context.Context, t approval.Target, v approval.Viewer) (approval.Snapshot, error) {
	return f(ctx, t, v)
}

type approvalInvestigatorFunc func(context.Context, approval.Snapshot, approval.Repository, approval.Budget) (approval.Assessment, error)

func (f approvalInvestigatorFunc) Investigate(ctx context.Context, s approval.Snapshot, r approval.Repository, b approval.Budget) (approval.Assessment, error) {
	return f(ctx, s, r, b)
}

func approvalWorkerFixture(t *testing.T) (*Server, *db.GormDB, db.ApprovalTarget) {
	t.Helper()
	t.Setenv("APPROVAL_CANDIDATES_ENABLED", "true")
	t.Setenv("APPROVAL_CANDIDATES_PROVIDER", "anthropic")
	t.Setenv("APPROVAL_CANDIDATES_MODEL", "fixture-model")
	t.Setenv("APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS", "6000000")
	t.Setenv("APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS", "120000")
	s, store := newTestServer(t, "alice")
	t.Cleanup(func() { _ = store.Close() })
	s.cfg.AnthropicAPIKey = "fixture-key"
	user := createTestUser(t, store, "alice")
	head := strings.Repeat("a", 40)
	require.NoError(t, store.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 123, LastCommitSHA: head, Status: "completed", Title: "Improve retry handling", Author: "bob", PRState: "open"}))
	pr, err := store.GetPR("acme", "example", 123)
	require.NoError(t, err)
	ensureUserPRView(t, store, user.ID, pr.ID, false)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":123,"state":"open","head":{"sha":%q},"base":{"sha":%q,"repo":{"id":42}},"user":{"login":"bob"}}`, head, strings.Repeat("b", 40))
	}))
	t.Cleanup(remote.Close)
	s.ghClient = gh.NewTestClient(remote.URL, "alice")
	now := time.Now()
	limits := approvalJSON(map[string]any{"provider": "anthropic", "model": "fixture-model", "policy_version": approval.PolicyVersion, "prompt_version": approval.PromptVersion, "runtime_version": approval.RuntimeVersion})
	_, _, err = store.AdmitApprovalScan(db.ApprovalAdmission{Scan: db.ApprovalScan{ID: "worker-scan", UserID: user.ID, Kind: "full", IdempotencyKey: "worker-key", RequestHash: "worker-hash", LimitsJSON: limits}, Targets: []db.ApprovalTarget{{ID: "worker-target", Owner: "acme", Repo: "example", Number: 123, ExpectedHeadSHA: head, RepositoryID: 42}}, Now: now, DailyInputLimit: 6000000, DailyOutputLimit: 120000, TargetInputLimit: 600000, TargetOutputLimit: 12000})
	require.NoError(t, err)
	target, err := store.ClaimApprovalTarget(db.ApprovalClaim{Worker: "fixture", Now: now, LeaseDuration: time.Minute, TargetDuration: 180 * time.Second, MaxSlots: 2})
	require.NoError(t, err)
	require.NotNil(t, target)
	s.approvalExecution = &approvalExecution{repository: func(context.Context, approval.Snapshot) (approval.Repository, func(), error) {
		return nil, func() {}, nil
	}, collector: approvalCollectorFunc(func(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
		snapshot := approval.Snapshot{Target: target, Viewer: viewer, RepositoryID: 42, Revision: approval.Revision{Head: head, Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}, Eligible: true, Manifest: approval.Manifest{Complete: true, Endpoints: []approval.Endpoint{{Name: "fixture", Complete: true}}}, Sources: []approval.Source{{ID: "review", Provider: "prism", Verified: true, Completion: "completed", ReviewedSHA: head, FileCoverage: "not_reported"}}, Checks: []approval.Check{{Name: "test", State: "success", SHA: head}}}
		approval.CanonicalizeSnapshot(&snapshot)
		return snapshot, nil
	})}
	return s, store, *target
}

func TestApprovalWorkerNativeRoundTripAndRevalidation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_%t", changed), func(t *testing.T) {
			s, store, target := approvalWorkerFixture(t)
			collector := s.approvalExecution.collector
			calls := 0
			s.approvalExecution.collector = approvalCollectorFunc(func(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
				snapshot, err := collector.Collect(ctx, target, viewer)
				calls++
				if changed && calls > 1 {
					snapshot.Checks[0].State = "failure"
					approval.CanonicalizeSnapshot(&snapshot)
				}
				return snapshot, err
			})
			modelCalls := 0
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelCalls++
				content := []any{map[string]any{"type": "text", "text": `{"summary":"Current review evidence is complete","artifacts":[],"concerns":[],"coverage_gaps":[],"citations":[]}`}}
				stop := "end_turn"
				if modelCalls == 1 {
					content = []any{map[string]any{"type": "tool_use", "id": "evidence", "name": "list_evidence", "input": map[string]any{}}}
					stop = "tool_use"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "fixture-model", "stop_reason": stop, "content": content, "usage": map[string]int{"input_tokens": 100, "output_tokens": 50}})
			}))
			defer model.Close()
			s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
				return approval.NativeInvestigator{Config: approval.ModelConfig{Provider: "anthropic", Model: "fixture-model", APIKey: "fixture", BaseURL: model.URL, Client: model.Client()}}
			}
			s.investigateApproval(context.Background(), target)
			result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
			require.NoError(t, err)
			require.Equal(t, "completed", result.ExecutionStatus)
			require.Equal(t, "candidate", result.Decision)
			require.Equal(t, 2, modelCalls)
			require.EqualValues(t, 200, result.InputTokens)
			require.EqualValues(t, 100, result.OutputTokens)
			require.Equal(t, 1, result.ToolCalls)
			if changed {
				require.Equal(t, "stale", result.Freshness)
			} else {
				require.Equal(t, "current", result.Freshness)
				require.NotNil(t, result.ValidUntil)
			}
			scan, err := store.GetApprovalScan(target.UserID, target.ScanID)
			require.NoError(t, err)
			require.Equal(t, "completed", scan.Status)
		})
	}
}

func TestApprovalWorkerCancellationFencesFinalResult(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	entered := make(chan struct{})
	done := make(chan struct{})
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
		return approvalInvestigatorFunc(func(ctx context.Context, _ approval.Snapshot, _ approval.Repository, _ approval.Budget) (approval.Assessment, error) {
			close(entered)
			<-ctx.Done()
			return approval.Assessment{}, ctx.Err()
		})
	}
	go func() { s.investigateApproval(context.Background(), target); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not enter model")
	}
	require.NoError(t, store.CancelApprovalScan(target.UserID, target.ScanID, time.Now()))
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("cancellation failed to stop model")
	}
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", result.ExecutionStatus)
	require.Empty(t, result.AssessmentJSON)
}

func TestApprovalWorkerRecoveryKeepsCumulativeBudget(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
		return approvalInvestigatorFunc(func(ctx context.Context, _ approval.Snapshot, _ approval.Repository, b approval.Budget) (approval.Assessment, error) {
			require.NoError(t, b.Reserve(ctx, approval.Usage{InputTokens: 100000, OutputTokens: 4096, Rounds: 1}))
			return approval.Assessment{}, errors.New("unknown provider usage")
		})
	}
	s.investigateApproval(context.Background(), target)
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", result.ExecutionStatus)
	require.EqualValues(t, 100000, result.InputTokens)
	require.EqualValues(t, 4096, result.OutputTokens)
	require.NotEqual(t, "candidate", result.Decision)
}

func TestApprovalWorkerRejectsConfigurationDriftBeforeModel(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	modelCalls := 0
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator { modelCalls++; return nil }
	t.Setenv("APPROVAL_CANDIDATES_MODEL", "different-model")
	s.investigateApproval(context.Background(), target)
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, 0, modelCalls)
	require.Equal(t, "completed", result.ExecutionStatus)
	require.Equal(t, "insufficient_evidence", result.Decision)
	require.Equal(t, "stale", result.Freshness)
}

func TestApprovalRevalidationChecksConfigurationAndAssessmentVersions(t *testing.T) {
	for _, mode := range []string{"model_changed", "policy_changed", "prompt_changed", "runtime_changed", "credentials_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			s, store, target := approvalWorkerFixture(t)
			user, err := store.GetUserByID(target.UserID)
			require.NoError(t, err)
			viewer := approval.Viewer{ID: user.ID, Login: user.GitHubUsername}
			requested := approval.Target{Owner: target.Owner, Repo: target.Repo, Number: target.Number, ExpectedHeadSHA: target.ExpectedHeadSHA}
			snapshot, err := s.collectApprovalSnapshot(context.Background(), requested, viewer)
			require.NoError(t, err)
			require.NoError(t, store.SaveApprovalSnapshot(target.ID, target.LeaseToken, approvalJSON(snapshot), time.Now()))
			assessment := approval.Assessment{SchemaVersion: "1", PolicyVersion: approval.PolicyVersion, PromptVersion: approval.PromptVersion, RuntimeVersion: approval.RuntimeVersion, Model: "fixture-model", Decision: "candidate", SnapshotDigest: snapshot.Digest, SnapshotID: snapshot.ID}
			switch mode {
			case "policy_changed":
				assessment.PolicyVersion = "old-policy"
			case "prompt_changed":
				assessment.PromptVersion = "old-prompt"
			case "runtime_changed":
				assessment.RuntimeVersion = "old-runtime"
			}
			now := time.Now()
			until := now.Add(time.Minute)
			require.NoError(t, store.FinalizeApprovalTarget(target.ID, target.LeaseToken, now, db.ApprovalFinalization{ExecutionStatus: "completed", Decision: "candidate", Freshness: "current", AssessmentJSON: approvalJSON(assessment), ValidatedAt: &now, ValidUntil: &until}))
			lease, err := store.ClaimApprovalValidation(user.ID, target.ID, time.Now(), 90*time.Second)
			require.NoError(t, err)
			require.NotNil(t, lease)
			if mode == "model_changed" {
				t.Setenv("APPROVAL_CANDIDATES_MODEL", "new-model")
			}
			if mode == "credentials_unavailable" {
				s.cfg.AnthropicAPIKey = ""
			}
			s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator { t.Fatal("revalidation invoked model"); return nil }
			s.revalidateApproval(target, *lease, viewer)
			result, err := store.GetApprovalTarget(user.ID, target.ScanID, target.ID)
			require.NoError(t, err)
			if mode == "credentials_unavailable" {
				require.Equal(t, "current", result.Freshness)
				require.True(t, result.ValidUntil.After(until))
			} else {
				require.Equal(t, "stale", result.Freshness)
				require.False(t, result.ValidUntil.After(until))
			}
		})
	}
}

func TestApprovalWorkerPartialValidationExpiresAssessment(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	collector := s.approvalExecution.collector
	calls := 0
	s.approvalExecution.collector = approvalCollectorFunc(func(ctx context.Context, target approval.Target, viewer approval.Viewer) (approval.Snapshot, error) {
		snapshot, err := collector.Collect(ctx, target, viewer)
		calls++
		if calls > 1 {
			snapshot.Manifest.Complete = false
			approval.CanonicalizeSnapshot(&snapshot)
		}
		return snapshot, err
	})
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
		return approvalInvestigatorFunc(func(context.Context, approval.Snapshot, approval.Repository, approval.Budget) (approval.Assessment, error) {
			return approval.Assessment{SchemaVersion: "1", PolicyVersion: approval.PolicyVersion, PromptVersion: approval.PromptVersion, RuntimeVersion: approval.RuntimeVersion, Model: "fixture-model", Decision: "candidate", Summary: "Fixture assessment"}, nil
		})
	}
	s.investigateApproval(context.Background(), target)
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", result.ExecutionStatus)
	require.Equal(t, "expired", result.Freshness)
	require.Contains(t, result.ReasonCodesJSON, "validation_failed")
	require.NotEmpty(t, result.AssessmentJSON)
}

func TestApprovalWorkerRecoversInvestigatorPanic(t *testing.T) {
	s, store, target := approvalWorkerFixture(t)
	s.approvalExecution.investigator = func(db.ApprovalTarget) approval.Investigator {
		return approvalInvestigatorFunc(func(context.Context, approval.Snapshot, approval.Repository, approval.Budget) (approval.Assessment, error) {
			panic("fixture")
		})
	}
	s.investigateApproval(context.Background(), target)
	result, err := store.GetApprovalTarget(target.UserID, target.ScanID, target.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", result.ExecutionStatus)
	require.Contains(t, result.ReasonCodesJSON, "runtime_failed")
}

type approvalCountingDatabase struct {
	db.Database
	db.ApprovalStore
	cancellations atomic.Int32
}

func (d *approvalCountingDatabase) CancelAllApprovalScans(now time.Time) error {
	d.cancellations.Add(1)
	return d.ApprovalStore.CancelAllApprovalScans(now)
}
func TestApprovalDisabledWorkerDoesNotContinuouslyWrite(t *testing.T) {
	s, store, _ := approvalWorkerFixture(t)
	counting := &approvalCountingDatabase{Database: store, ApprovalStore: store}
	s.db = counting
	t.Setenv("APPROVAL_CANDIDATES_ENABLED", "false")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runApprovalWorkers(ctx)
	time.Sleep(2200 * time.Millisecond)
	require.EqualValues(t, 1, counting.cancellations.Load())
}
