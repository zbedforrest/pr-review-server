package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"pr-review-server/github"
	"pr-review-server/pkg/grader"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

type gateFixture struct {
	p      *Poller
	exec   *reviewExecution
	review *service.AgentReview
	seen   []grader.Item
}

func newGateFixture(t *testing.T, mode, author string, grades map[string]string, runErr error) *gateFixture {
	t.Helper()
	cfg := testConfig()
	cfg.QualityGate = mode
	cfg.GraderModel, cfg.GraderWallClockSec, cfg.GraderMaxTurns = "judge", 60, 10
	db := NewMockDatabase()
	_ = db.SetSetting(SettingPublishEnabledAuthors, "alice")
	f := &gateFixture{
		p: &Poller{cfg: cfg, db: db, storage: &MockReviewStorage{}},
		exec: &reviewExecution{Job: ReviewJob{RunID: "run1", PR: github.PullRequest{
			Owner: "o", Repo: "r", Number: 7, CommitSHA: "abcdef1234567890", Author: author,
		}}},
		review: &service.AgentReview{
			Scope: &payload.ReviewScope{MergeBase: "base123"},
			Comments: []types.LineComment{
				{FilePath: "a.go", LineNumber: 3, Importance: "MEDIUM", CommentBody: "Restates the intended change"},
				{FilePath: "b.go", LineNumber: 9, Importance: "CRITICAL", CommentBody: "Fixture secret flagged"},
				{FilePath: "c.go", LineNumber: 1, Importance: "MEDIUM", CommentBody: "Real bug"},
				{FilePath: "d.go", LineNumber: 2, Importance: "LOW", CommentBody: "Held already", Verification: &types.Verification{Status: types.VerifyHold, Applied: true}},
				{FilePath: "SUMMARY", Summary: &types.SummaryBlock{Verdict: "approve"}},
			},
		},
	}
	orig := gateRunFn
	t.Cleanup(func() { gateRunFn = orig })
	gateRunFn = func(_ context.Context, _ service.AgentConfig, _ service.Spawner, in service.GradeAgentInput) (*service.GradeAgentResult, error) {
		f.seen = in.Items
		if runErr != nil {
			return &service.GradeAgentResult{CostUSD: 0.1}, runErr
		}
		res := &service.GradeAgentResult{CostUSD: 0.5}
		for _, it := range in.Items {
			if g, ok := grades[it.File]; ok {
				res.Grades = append(res.Grades, grader.Grade{ID: it.ID, Grade: g, Category: "deliberate-choice", Reason: "r"})
			}
		}
		return res, nil
	}
	return f
}

func (f *gateFixture) run() *GateReport {
	return f.p.qualityGateReview(context.Background(), f.exec, service.AgentConfig{CloneRootDir: "/tmp/clones"}, &service.ReviewResult{PRTitle: "t", PRBody: "b"}, f.review)
}

func TestGateOnDropsBadFindingsAndPostsABadCriticalAsALowNote(t *testing.T) {
	f := newGateFixture(t, gateModeOn, "alice", map[string]string{"a.go": "BAD", "b.go": "BAD", "c.go": "GOOD"}, nil)
	rep := f.run()
	if rep.Status != "applied" || rep.Dropped != 1 || rep.Demoted != 1 || rep.CostUSD != 0.5 {
		t.Fatalf("report = %+v", rep)
	}
	if len(f.seen) != 3 {
		t.Fatalf("graded %d items, want 3 (the held finding and the summary are skipped)", len(f.seen))
	}
	c := f.review.Comments
	if !c[0].Verification.Suppresses() {
		t.Error("a BAD MEDIUM still posts")
	}
	if c[1].Verification.Suppresses() || c[1].Importance != "LOW" || !c[1].Verification.GateDemoted {
		t.Errorf("BAD CRITICAL: importance %q verification %+v", c[1].Importance, c[1].Verification)
	}
	if c[2].Verification.Suppresses() || c[2].Verification.Gate != "GOOD" {
		t.Errorf("a GOOD finding was changed: %+v", c[2].Verification)
	}
}

func TestGateFailsOpenWhenTheGraderErrors(t *testing.T) {
	f := newGateFixture(t, gateModeOn, "alice", nil, errors.New("timeout"))
	rep := f.run()
	if rep.Status != "error" {
		t.Fatalf("report = %+v", rep)
	}
	for _, c := range f.review.Comments[:3] {
		if c.Verification.Suppresses() || c.Importance == "LOW" {
			t.Fatalf("a finding changed after a gate error: %+v", c)
		}
	}
}

func TestGateShadowChangesNothing(t *testing.T) {
	f := newGateFixture(t, gateModeShadow, "alice", map[string]string{"a.go": "BAD"}, nil)
	rep := f.run()
	if rep.Status != "pending" {
		t.Fatalf("report = %+v", rep)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(gateBackground) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.review.Comments[0].Verification != nil {
		t.Fatalf("shadow mode changed a finding: %+v", f.review.Comments[0].Verification)
	}
}

func TestGateSkipsOffDashboardOnlyAndOverBudgetReviews(t *testing.T) {
	if rep := newGateFixture(t, "off", "alice", nil, nil).run(); rep != nil {
		t.Fatalf("off returned %+v", rep)
	}
	if rep := newGateFixture(t, gateModeOn, "bob", nil, nil).run(); rep.Skip != "not_publish_bound" {
		t.Fatalf("dashboard-only review: %+v", rep)
	}
	f := newGateFixture(t, gateModeOn, "alice", nil, nil)
	f.p.cfg.QualityGateBudgetUSD = 1
	gateSpent(time.Now().UTC().Format("2006-01-02"), 5)
	t.Cleanup(func() { gateSpendDay = "" })
	if rep := f.run(); rep.Skip != "over_budget" {
		t.Fatalf("over budget: %+v", rep)
	}
}
