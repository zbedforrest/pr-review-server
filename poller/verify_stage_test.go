package poller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
	"pr-review-server/pkg/reviewer/verify"
)

type stubGit struct{}

func (stubGit) Show(context.Context, string, string) (string, error) {
	return "package a\n\nfunc Handle() {}\n", nil
}
func (stubGit) Diff(context.Context, string, string, string) (string, error) { return "", nil }
func (stubGit) Grep(context.Context, string, string, int) ([]verify.Hit, error) {
	return nil, nil
}

type stubCompleter struct {
	reply string
	calls int
}

func (s *stubCompleter) CompleteStructured(context.Context, llm.StructuredRequest) (string, llm.Call, error) {
	s.calls++
	return s.reply, llm.Call{ServedModel: "anthropic/claude-fable-5.1", CostUSD: 0.25}, nil
}

type verifyFixture struct {
	p         *Poller
	storage   *MockReviewStorage
	completer *stubCompleter
	exec      *reviewExecution
	review    *service.AgentReview
}

func newVerifyFixture(t *testing.T, mode, author, reply string) *verifyFixture {
	t.Helper()
	cfg := testConfig()
	cfg.EnsembleVerify = mode
	cfg.OpenRouterAPIKey = "key"
	cfg.VerifyModel = "anthropic/claude-fable-5.1"
	cfg.VerifyWallClockSec = 30
	cfg.VerifyMaxInputTokens = 100000
	cfg.VerifyShadowSample = 1
	db := NewMockDatabase()
	_ = db.SetSetting(SettingPublishEnabledAuthors, "alice")
	storage := &MockReviewStorage{}
	f := &verifyFixture{
		p:         &Poller{cfg: cfg, db: db, storage: storage},
		storage:   storage,
		completer: &stubCompleter{reply: reply},
		exec: &reviewExecution{Job: ReviewJob{RunID: "run1", PR: github.PullRequest{
			Owner: "o", Repo: "r", Number: 7, CommitSHA: "abcdef1234567890", Author: author,
		}}},
		review: &service.AgentReview{
			Scope: &payload.ReviewScope{MergeBase: "base123"},
			Comments: []types.LineComment{
				{FilePath: "a.go", LineNumber: 3, Importance: "MEDIUM", CommentBody: "Handle drops the error"},
				{FilePath: "SUMMARY", Summary: &types.SummaryBlock{Verdict: "approve"}},
			},
		},
	}
	origGit, origCompleter := newVerifyGit, newVerifyCompleter
	newVerifyGit = func(string) verify.Git { return stubGit{} }
	newVerifyCompleter = func(string, string, string) verify.Completer { return f.completer }
	t.Cleanup(func() { newVerifyGit, newVerifyCompleter = origGit, origCompleter })
	return f
}

func (f *verifyFixture) run() *VerifyReport {
	return f.p.verifyEnsembleReview(context.Background(), f.exec, service.AgentConfig{CloneRootDir: "/tmp/clones"}, &service.ReviewResult{PRTitle: "t"}, f.review)
}

const dropF1 = `{"decisions":[{"key":"F1","decision":"drop","cites":["P2"],"reason":"Handle returns nothing.","refuting_quote":null,"revised":null}]}`

func TestVerifyOnAppliesDecisionsToAPublishBoundReview(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", dropF1)
	rep := f.run()
	v := f.review.Comments[0].Verification
	if rep.Status != "applied" || !rep.Sync || v == nil || !v.Applied || v.Status != types.VerifyDrop || !strings.HasPrefix(v.Evidence, "a.go:1") {
		t.Fatalf("report=%+v verification=%+v", rep, v)
	}
	if f.review.Comments[1].Verification != nil {
		t.Fatal("the summary was verified")
	}
	if rep.Call == nil || rep.Call.CostUSD != 0.25 {
		t.Fatalf("call = %+v", rep.Call)
	}
}

func TestVerifyOnLeavesTheReviewUnchangedWhenADecisionIsMissing(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", `{"decisions":[]}`)
	rep := f.run()
	if rep.Status != "incomplete" || strings.Join(rep.Missing, ",") != "F1" || f.review.Comments[0].Verification != nil {
		t.Fatalf("report=%+v comments=%+v", rep, f.review.Comments)
	}
}

func waitForArtifact(t *testing.T, s *MockReviewStorage) verifyArtifact {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		calls := append(s.SaveSidecarCalls[:0:0], s.SaveSidecarCalls...)
		s.mu.Unlock()
		for _, c := range calls {
			if strings.HasSuffix(c.Filename, ".verify.json") {
				var art verifyArtifact
				if err := json.Unmarshal(c.Content, &art); err != nil {
					t.Fatal(err)
				}
				return art
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no verify artifact was saved")
	return verifyArtifact{}
}

func TestVerifyShadowRecordsInTheBackgroundAndChangesNothing(t *testing.T) {
	f := newVerifyFixture(t, verifyModeShadow, "alice", dropF1)
	rep := f.run()
	if rep.Status != "pending" || rep.Sync || !strings.HasSuffix(rep.Artifact, ".verify.json") || f.review.Comments[0].Verification != nil {
		t.Fatalf("report=%+v comments=%+v", rep, f.review.Comments)
	}
	art := waitForArtifact(t, f.storage)
	if art.Report.Status != "recorded" || art.Base != "base123" || len(art.Findings) != 1 || art.Findings[0].Verified == nil {
		t.Fatalf("artifact = %+v", art)
	}
	if v := art.Findings[0].Verified.Verification; v.Status != types.VerifyDrop || v.Applied {
		t.Fatalf("recorded verification = %+v", v)
	}
}

func TestVerifyOnRecordsADashboardOnlyReviewInTheBackground(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "bob", dropF1)
	rep := f.run()
	if rep.Sync || rep.Status != "pending" || f.review.Comments[0].Verification != nil {
		t.Fatalf("report=%+v", rep)
	}
	waitForArtifact(t, f.storage)
}

func TestVerifySkipsADashboardOnlyReviewOutsideTheSample(t *testing.T) {
	f := newVerifyFixture(t, verifyModeShadow, "bob", dropF1)
	f.p.cfg.VerifyShadowSample = 0
	if rep := f.run(); rep.Status != "skipped" || rep.Skip != "not_sampled" || f.completer.calls != 0 {
		t.Fatalf("report=%+v calls=%d", rep, f.completer.calls)
	}
}

func TestVerifyStopsForTheDayOnceTheBudgetIsSpent(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", dropF1)
	f.p.cfg.VerifyDailyBudgetUSD = 0.2
	if rep := f.run(); rep.Status != "applied" {
		t.Fatalf("first report = %+v", rep)
	}
	f.review.Comments[0].Verification = nil
	if rep := f.run(); rep.Skip != "over_budget" || f.completer.calls != 1 {
		t.Fatalf("second report=%+v calls=%d", rep, f.completer.calls)
	}
}

func TestVerifyOffReturnsNoReport(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOff, "alice", dropF1)
	if rep := f.run(); rep != nil || f.completer.calls != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifySpendRollsOverAtUTCMidnight(t *testing.T) {
	var s verifySpend
	day1 := time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC)
	s.add(day1, 5)
	if s.allow(day1, 5) {
		t.Fatal("allowed at the cap")
	}
	if !s.allow(day1.Add(2*time.Hour), 5) {
		t.Fatal("did not roll over")
	}
}

func TestReviewTimeoutGrowsOnlyForEnsembleReviewsInOnMode(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", dropF1)
	single := f.p.reviewTimeout(ensembleEffective(1))
	ensemble := f.p.reviewTimeout(ensembleEffective(5))
	if ensemble-reviewTimeoutWithMargin(ensembleEffective(5), ReviewPipelineMargin) != 30*time.Second || single != reviewTimeoutWithMargin(ensembleEffective(1), ReviewPipelineMargin) {
		t.Fatalf("single=%v ensemble=%v", single, ensemble)
	}
	f.p.cfg.EnsembleVerify = verifyModeShadow
	if f.p.reviewTimeout(ensembleEffective(5)) != reviewTimeoutWithMargin(ensembleEffective(5), ReviewPipelineMargin) {
		t.Fatal("shadow mode changed the timeout")
	}
}

func ensembleEffective(runs int) runconfig.Effective {
	return runconfig.Effective{Agent: runconfig.Agent{WallClockSeconds: 360}, Ensemble: &runconfig.Ensemble{Runs: runs}}
}
