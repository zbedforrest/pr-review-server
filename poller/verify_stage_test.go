package poller

import (
	"context"
	"encoding/json"
	"errors"
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
	cfg.VerifyPacketCall = true
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

const unavailableF1 = `{"decisions":[{"key":"F1","decision":"evidence_unavailable","cites":[],"reason":"The handler is in another file.","refuting_quote":null,"revised":null}]}`

func stubVerifyAgent(t *testing.T, d *verify.AgentDecision, err error) *[]service.VerifyAgentInput {
	t.Helper()
	var calls []service.VerifyAgentInput
	orig := escalateVerifyFn
	escalateVerifyFn = func(_ context.Context, _ service.AgentConfig, _ service.Spawner, in service.VerifyAgentInput, comments []types.LineComment) (service.EscalationResult, error) {
		calls = append(calls, in)
		res := service.EscalationResult{Escalated: 1}
		if err != nil {
			return res, err
		}
		res.CostUSD, res.Decisions = 0.4, map[string]int{}
		for i, c := range comments {
			if c.Verification.Status == types.VerifyUnavailable {
				applied, _ := verify.ApplyAgent(in.Findings[i].Comment, *d)
				applied.Verification.Phase = 2
				comments[i] = applied
				res.Decisions[applied.Verification.Status]++
			}
		}
		return res, nil
	}
	t.Cleanup(func() { escalateVerifyFn = orig })
	return &calls
}

func TestVerifyEscalatesUnavailableFindingsToTheAgent(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", unavailableF1)
	f.p.cfg.VerifyEscalate = true
	calls := stubVerifyAgent(t, &verify.AgentDecision{
		Key: "F1", Decision: "drop", Reason: "The handler returns early.", Cites: []verify.AgentCite{{File: "b.go", Start: 4, End: 4}}, Quote: "b.go:4\nreturn",
	}, nil)
	rep := f.run()
	v := f.review.Comments[0].Verification
	if len(*calls) != 1 || (*calls)[0].BaseSHA != "base123" || (*calls)[0].Owner != "o" {
		t.Fatalf("calls = %+v", *calls)
	}
	if rep.Status != "applied" || rep.Escalated != 1 || rep.AgentCostUSD != 0.4 || v.Status != types.VerifyDrop || v.Phase != 2 || !v.Applied || v.Evidence != "b.go:4\nreturn" {
		t.Fatalf("report=%+v verification=%+v", rep, v)
	}
	if rep.Decisions[types.VerifyDrop] != 1 || rep.Decisions[types.VerifyUnavailable] != 0 {
		t.Fatalf("decisions = %v", rep.Decisions)
	}
}

func TestVerifyKeepsFindingsUnavailableWhenTheAgentFails(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", unavailableF1)
	f.p.cfg.VerifyEscalate = true
	stubVerifyAgent(t, nil, errors.New("wall-clock timeout"))
	rep := f.run()
	v := f.review.Comments[0].Verification
	if rep.Status != "applied" || rep.AgentError != "wall-clock timeout" || v.Status != types.VerifyUnavailable || v.Suppresses() {
		t.Fatalf("report=%+v verification=%+v", rep, v)
	}
}

func TestVerifyDoesNotEscalateWhenTurnedOff(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", unavailableF1)
	calls := stubVerifyAgent(t, nil, errors.New("unexpected"))
	if rep := f.run(); rep.Escalated != 0 || len(*calls) != 0 {
		t.Fatalf("report=%+v calls=%d", rep, len(*calls))
	}
}

func TestVerifyWithoutThePacketCallSendsEveryFindingToTheAgent(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", "unused")
	f.p.cfg.VerifyPacketCall = false
	f.p.cfg.VerifyEscalate = true
	f.p.cfg.OpenRouterAPIKey = ""
	calls := stubVerifyAgent(t, &verify.AgentDecision{Key: "F1", Decision: "drop", Reason: "r", Cites: []verify.AgentCite{{File: "a.go", Start: 1, End: 1}}, Quote: "a.go:1\nx"}, nil)
	rep := f.run()
	if f.completer.calls != 0 || len(*calls) != 1 || rep.Status != "applied" || f.review.Comments[0].Verification.Status != types.VerifyDrop {
		t.Fatalf("calls=%d agent=%d report=%+v v=%+v", f.completer.calls, len(*calls), rep, f.review.Comments[0].Verification)
	}
}

func TestVerifyOnCountsAgentSpendInTheReviewCost(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", unavailableF1)
	f.p.cfg.VerifyEscalate = true
	stubVerifyAgent(t, &verify.AgentDecision{Key: "F1", Decision: "keep", Reason: "ok"}, nil)
	rep := f.run()
	if rep.AgentCostUSD != 0.4 || rep.Call == nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyWithoutAnOpenRouterKeyFallsBackToTheAgent(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", "unused")
	f.p.cfg.OpenRouterAPIKey = ""
	f.p.cfg.VerifyEscalate = true
	calls := stubVerifyAgent(t, &verify.AgentDecision{Key: "F1", Decision: "keep", Reason: "ok"}, nil)
	if rep := f.run(); rep.Skip != "" || len(*calls) != 1 || f.completer.calls != 0 {
		t.Fatalf("report=%+v agent=%d packet=%d", rep, len(*calls), f.completer.calls)
	}
}

func TestVerifyWallClockCoversEveryWaveOfBatches(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", "unused")
	f.p.cfg.VerifyEscalate, f.p.cfg.VerifyAgentWallClockSec = true, 100
	waves := (verify.MaxAgentBatches + service.AgentBatchParallel - 1) / service.AgentBatchParallel
	if got, want := f.p.verifyWallClock(), time.Duration(30+waves*100)*time.Second; got != want {
		t.Fatalf("wall = %v, want %v", got, want)
	}
	f.p.cfg.VerifyEscalate, f.p.cfg.VerifyPacketCall = false, false
	if got := f.p.verifyWallClock(); got != 30*time.Second {
		t.Fatalf("agent-off wall = %v", got)
	}
}

func TestVerifyWithTheAgentOffAndNoPacketCallSkips(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", "unused")
	f.p.cfg.VerifyPacketCall, f.p.cfg.VerifyEscalate = false, false
	calls := stubVerifyAgent(t, &verify.AgentDecision{Key: "F1", Decision: "keep", Reason: "ok"}, nil)
	if rep := f.run(); rep.Skip != "agent_off" || len(*calls) != 0 {
		t.Fatalf("report=%+v calls=%d", rep, len(*calls))
	}
}

func TestShadowVerificationStopsBeforeTheWholeBudget(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", dropF1)
	f.p.cfg.VerifyDailyBudgetUSD = 1
	f.p.verifySpend.add(time.Now(), 0.8)
	rep := &VerifyReport{Mode: verifyModeShadow}
	f.p.runVerify(context.Background(), verifyTarget{CloneRoot: "/tmp/x"}, verify.Input{Findings: []verify.Finding{{Key: "F1", Comment: f.review.Comments[0]}}}, rep)
	if rep.Skip != "over_budget" {
		t.Fatalf("shadow report = %+v", rep)
	}
	sync := &VerifyReport{Mode: verifyModeOn, Sync: true}
	f.p.runVerify(context.Background(), verifyTarget{CloneRoot: "/tmp/x"}, verify.Input{Findings: []verify.Finding{{Key: "F1", Comment: f.review.Comments[0]}}}, sync)
	if sync.Skip == "over_budget" {
		t.Fatalf("publish-bound run was starved: %+v", sync)
	}
}

func TestShadowEscalationStopsBeforeTheWholeBudget(t *testing.T) {
	f := newVerifyFixture(t, verifyModeOn, "alice", "unused")
	f.p.cfg.VerifyDailyBudgetUSD = 1
	f.p.verifySpend.add(time.Now(), 0.8)
	calls := stubVerifyAgent(t, &verify.AgentDecision{Key: "F1", Decision: "keep", Reason: "ok"}, nil)
	rep := &VerifyReport{Mode: verifyModeShadow}
	c := f.review.Comments[0]
	c.Verification = &types.Verification{Status: types.VerifyUnavailable}
	f.p.escalateVerify(context.Background(), verifyTarget{}, verify.Input{Findings: []verify.Finding{{Key: "F1", Comment: f.review.Comments[0]}}}, nil, []types.LineComment{c}, rep)
	if rep.AgentError != "over_budget" || len(*calls) != 0 {
		t.Fatalf("report=%+v calls=%d", rep, len(*calls))
	}
}
