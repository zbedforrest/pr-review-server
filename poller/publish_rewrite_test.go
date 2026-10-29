package poller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/rewrite"
)

type rewriteFixture struct {
	p      *Poller
	exec   *reviewExecution
	review *service.AgentReview
	seen   []rewrite.Item
	calls  atomic.Int32
}

func newRewriteFixture(t *testing.T, mode string, runErr error) *rewriteFixture {
	t.Helper()
	cfg := testConfig()
	cfg.PublishRewrite, cfg.PublishRewriteModel, cfg.PublishRewriteWallClockSec = mode, "writer", 60
	db := NewMockDatabase()
	_ = db.SetSetting(SettingPublishEnabledAuthors, "alice")
	f := &rewriteFixture{
		p: &Poller{cfg: cfg, db: db, storage: &MockReviewStorage{}},
		exec: &reviewExecution{Job: ReviewJob{RunID: "run1", PR: github.PullRequest{
			Owner: "o", Repo: "r", Number: 7, CommitSHA: "abcdef1234567890", Author: "alice",
		}}},
		review: &service.AgentReview{Comments: []types.LineComment{
			{FilePath: "a.go", LineNumber: 3, Importance: "MEDIUM", CommentBody: "Real bug", FindingContract: &types.FindingContract{Headline: "A long headline", Uncertainty: "Unsure"}},
			{FilePath: "b.go", LineNumber: 9, Importance: "LOW", CommentBody: "A note"},
			{FilePath: "c.go", LineNumber: 1, Importance: "CRITICAL", CommentBody: "Dropped by the gate", Verification: &types.Verification{Status: types.VerifySkipped, Gate: "BAD", GateApplied: true}},
			{FilePath: "SUMMARY", Summary: &types.SummaryBlock{Verdict: "approve"}},
		}},
	}
	orig := rewriteRunFn
	t.Cleanup(func() { rewriteRunFn = orig })
	rewriteRunFn = func(_ context.Context, _ service.AgentConfig, _ service.Spawner, in service.RewriteInput) (*service.RewriteResult, error) {
		f.calls.Add(1)
		f.seen = in.Items
		if runErr != nil {
			return &service.RewriteResult{CostUSD: 0.01}, runErr
		}
		res := &service.RewriteResult{CostUSD: 0.02}
		for _, it := range in.Items {
			res.Rewrites = append(res.Rewrites, rewrite.Result{ID: it.ID, Title: "Short title", Body: "Trigger. Consequence. Fix it."})
		}
		return res, nil
	}
	return f
}

func (f *rewriteFixture) run() *RewriteReport {
	return f.p.rewriteReview(context.Background(), f.exec, service.AgentConfig{CloneRootDir: "/tmp/clones"}, f.review)
}

func TestRewriteOnSetsTheDisplayOfInlineBoundFindingsOnly(t *testing.T) {
	f := newRewriteFixture(t, gateModeOn, nil)
	rep := f.run()
	if rep.Status != "applied" || rep.Items != 1 || rep.Rewritten != 1 || rep.CostUSD != 0.02 {
		t.Fatalf("report = %+v", rep)
	}
	if len(f.seen) != 1 || f.seen[0].Headline != "A long headline" || f.seen[0].Caveat != "Unsure" || f.seen[0].Explanation != "Real bug" {
		t.Fatalf("items = %+v", f.seen)
	}
	c := f.review.Comments
	if c[0].Display == nil || c[0].Display.Title != "Short title" || c[0].CommentBody != "Real bug" {
		t.Errorf("inline finding: %+v", c[0])
	}
	if c[1].Display != nil || c[2].Display != nil {
		t.Error("a LOW note or a gated finding was rewritten")
	}
}

func TestRewriteFailsOpen(t *testing.T) {
	f := newRewriteFixture(t, gateModeOn, errors.New("timeout"))
	if rep := f.run(); rep.Status != "error" || f.review.Comments[0].Display != nil {
		t.Fatalf("report = %+v display = %+v", rep, f.review.Comments[0].Display)
	}
}

func TestRewriteShadowChangesNothing(t *testing.T) {
	f := newRewriteFixture(t, gateModeShadow, nil)
	if rep := f.run(); rep.Status != "pending" {
		t.Fatalf("report = %+v", rep)
	}
	deadline := time.Now().Add(2 * time.Second)
	for (f.calls.Load() == 0 || len(rewriteBackground) > 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.calls.Load() != 1 || f.review.Comments[0].Display != nil {
		t.Fatalf("calls %d, display %+v", f.calls.Load(), f.review.Comments[0].Display)
	}
}

func TestRewriteSkipsWhenOffOrNothingPostsInline(t *testing.T) {
	if rep := newRewriteFixture(t, "off", nil).run(); rep != nil {
		t.Fatalf("off returned %+v", rep)
	}
	f := newRewriteFixture(t, gateModeOn, nil)
	f.review.Comments = f.review.Comments[1:]
	if rep := f.run(); rep.Skip != "no_inline_findings" || f.calls.Load() != 0 {
		t.Fatalf("report = %+v calls %d", rep, f.calls.Load())
	}
}

func TestRewriteSkipsOverBudgetAndLegacyTitles(t *testing.T) {
	f := newRewriteFixture(t, gateModeOn, nil)
	f.p.cfg.PublishRewriteBudgetUSD = 0.01
	day := time.Now().UTC().Format("2006-01-02")
	rewriteSpent(day, 0.02)
	t.Cleanup(func() { rewriteSpent(day, -0.02) })
	if rep := f.run(); rep.Skip != "over_budget" || f.calls.Load() != 0 {
		t.Fatalf("report = %+v calls %d", rep, f.calls.Load())
	}
	f = newRewriteFixture(t, gateModeOn, nil)
	f.p.cfg.PublishLegacyTitles = true
	if rep := f.run(); rep.Skip != "legacy_titles" || f.calls.Load() != 0 {
		t.Fatalf("report = %+v calls %d", rep, f.calls.Load())
	}
}
