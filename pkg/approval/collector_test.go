package approval

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v57/github"
	githubclient "pr-review-server/github"
)

type fixtureEvidenceClient struct {
	data *githubclient.ApprovalEvidence
}

func (f fixtureEvidenceClient) CollectApprovalEvidence(context.Context, string, string, int, githubclient.ApprovalReadLimits) (*githubclient.ApprovalEvidence, error) {
	return f.data, nil
}
func fixtureCollector() (*Collector, *githubclient.ApprovalEvidence) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	remote := &githubclient.ApprovalEvidence{PR: &gh.PullRequest{State: gh.String("open"), User: &gh.User{ID: gh.Int64(1), Login: gh.String("author"), Type: gh.String("User")}, Head: &gh.PullRequestBranch{SHA: &head}, Base: &gh.PullRequestBranch{SHA: &base, Repo: &gh.Repository{ID: gh.Int64(10)}}}, MergeBase: base, Endpoints: []githubclient.ApprovalEndpoint{{Name: "all", Complete: true}}, Checks: []*gh.CheckRun{{ID: gh.Int64(1), Name: gh.String("test"), HeadSHA: &head, Status: gh.String("completed"), Conclusion: gh.String("success")}}}
	collector := &Collector{GitHub: fixtureEvidenceClient{remote}, Providers: []ProviderIdentity{{Provider: "greptile", ActorID: 20}, {Provider: "copilot", ActorID: 30}}, PRism: func(context.Context, Target) ([]Source, []Evidence, []Concern, []Endpoint, error) {
		return nil, nil, nil, []Endpoint{{Name: "prism", Complete: true}}, nil
	}}
	return collector, remote
}
func collectFixture(t *testing.T, c *Collector) Snapshot {
	t.Helper()
	s, err := c.Collect(context.Background(), Target{Owner: "acme", Repo: "example", Number: 1}, Viewer{ID: 2, Login: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func fixtureReview(id int64, actor int64, kind, state, sha string) *gh.PullRequestReview {
	return &gh.PullRequestReview{ID: &id, User: &gh.User{ID: &actor, Login: gh.String("reviewer"), Type: &kind}, State: &state, CommitID: &sha, SubmittedAt: &gh.Timestamp{Time: time.Unix(id, 0)}}
}

func TestCollectorStandingHumanOpinionSurvivesComment(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 2, "User", "CHANGES_REQUESTED", head), fixtureReview(2, 2, "User", "COMMENTED", head)}
	if !collectFixture(t, c).HumanChangesRequested {
		t.Fatal("comment erased objection")
	}
	remote.Reviews = append(remote.Reviews, fixtureReview(3, 2, "User", "APPROVED", head))
	s := collectFixture(t, c)
	if s.HumanChangesRequested || s.Eligible {
		t.Fatalf("standing approval not respected: %+v", s)
	}
	remote.Reviews[2].CommitID = gh.String(strings.Repeat("c", 40))
	if !collectFixture(t, c).Eligible {
		t.Fatal("old approval excluded target")
	}
	remote.Reviews[2].State = gh.String("DISMISSED")
	if !collectFixture(t, c).Eligible {
		t.Fatal("dismissed approval excluded target")
	}
}

func TestCollectorDraftStaysEligible(t *testing.T) {
	c, remote := fixtureCollector()
	ready := collectFixture(t, c)
	remote.PR.Draft = gh.Bool(true)
	s := collectFixture(t, c)
	if !s.Eligible || !s.Draft || len(s.ExclusionReasons) != 0 {
		t.Fatalf("draft excluded: %+v", s)
	}
	if s.Digest == ready.Digest {
		t.Fatal("draft state did not change the digest")
	}
}

func TestCollectorAuthenticCopilotCommentReviewAndSpoof(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 30, "Bot", "COMMENTED", head), fixtureReview(2, 20, "User", "APPROVED", head), fixtureReview(3, 999, "Bot", "APPROVED", head)}
	remote.Reviews[0].Body = gh.String("## Pull request overview\nCopilot reviewed 2 out of 2 changed files in this pull request.")
	s := collectFixture(t, c)
	trusted := 0
	for _, source := range s.Sources {
		if source.Verified {
			trusted++
			if source.Provider != "copilot" || source.Completion != "completed" || source.ReviewedSHA != head {
				t.Fatalf("bad source: %+v", source)
			}
		}
	}
	if trusted != 1 {
		t.Fatalf("trusted %d sources", trusted)
	}
}

func TestCollectorEmptyBotCommentReviewDoesNotProveCompletion(t *testing.T) {
	for _, body := range []string{"", "Thanks, fixed.", "This looks good"} {
		c, remote := fixtureCollector()
		remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 30, "Bot", "COMMENTED", remote.PR.GetHead().GetSHA())}
		remote.Reviews[0].Body = gh.String(body)
		for _, source := range collectFixture(t, c).Sources {
			if source.ID == "review:1" && source.Completion == "completed" {
				t.Fatalf("reply proved completion: %q", body)
			}
		}
	}
}

func TestCollectorCopilotPartialCoverageIsNotComplete(t *testing.T) {
	for _, body := range []string{"Copilot reviewed 2 out of 5 changed files in this pull request.", "Copilot wasn't able to review some files.", "Copilot was not able to review the remaining file."} {
		c, remote := fixtureCollector()
		remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 30, "Bot", "COMMENTED", remote.PR.GetHead().GetSHA())}
		remote.Reviews[0].Body = gh.String(body)
		for _, source := range collectFixture(t, c).Sources {
			if source.ID == "review:1" && (!source.Incomplete || source.FileCoverage != "reported_partial") {
				t.Fatalf("partial review accepted: %+v", source)
			}
		}
	}
}

func TestCollectorRetainsFailingChecksAcrossSuites(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	remote.Checks = []*gh.CheckRun{
		{ID: gh.Int64(1), App: &gh.App{ID: gh.Int64(10)}, CheckSuite: &gh.CheckSuite{ID: gh.Int64(100)}, Name: gh.String("test"), HeadSHA: &head, Status: gh.String("completed"), Conclusion: gh.String("failure")},
		{ID: gh.Int64(2), App: &gh.App{ID: gh.Int64(10)}, CheckSuite: &gh.CheckSuite{ID: gh.Int64(200)}, Name: gh.String("test"), HeadSHA: &head, Status: gh.String("completed"), Conclusion: gh.String("success")},
	}
	snapshot := collectFixture(t, c)
	if len(snapshot.Checks) != 2 {
		t.Fatalf("lost independent workflow check: %+v", snapshot.Checks)
	}
	if Evaluate(snapshot, Assessment{}).Decision != "needs_attention" {
		t.Fatal("failed independent workflow accepted")
	}
	remote.Checks[1].CheckSuite.ID = gh.Int64(100)
	snapshot = collectFixture(t, c)
	if len(snapshot.Checks) != 1 || snapshot.Checks[0].State != "success" {
		t.Fatal("latest rerun in same suite not retained")
	}
}

func TestCollectorPendingAndActionRequiredSuitesBlock(t *testing.T) {
	for _, state := range []string{"queued", "action_required"} {
		c, remote := fixtureCollector()
		suite := &gh.CheckSuite{ID: gh.Int64(7), HeadSHA: remote.PR.Head.SHA, Status: gh.String("completed"), Conclusion: &state}
		if state == "queued" {
			suite.Status = &state
			remote.Checks = append(remote.Checks, &gh.CheckRun{ID: gh.Int64(2), CheckSuite: &gh.CheckSuite{ID: gh.Int64(7)}, Name: gh.String("lint"), HeadSHA: remote.PR.Head.SHA, Status: gh.String("queued")})
		}
		remote.Suites = []*gh.CheckSuite{suite}
		snapshot := collectFixture(t, c)
		want := 2
		if state == "queued" {
			want = 3
		}
		if len(snapshot.Checks) != want {
			t.Fatalf("got %d checks, want %d: suite omitted", len(snapshot.Checks), want)
		}
		decision := Evaluate(snapshot, Assessment{})
		if decision.Decision == "candidate" {
			t.Fatal("unfinished suite accepted")
		}
	}
}

func TestCollectorIgnoresUnfinishedSuitesWithNoCheckRuns(t *testing.T) {
	c, remote := fixtureCollector()
	queued := "queued"
	remote.Suites = []*gh.CheckSuite{{ID: gh.Int64(8), HeadSHA: remote.PR.Head.SHA, Status: &queued}}
	snapshot := collectFixture(t, c)
	if len(snapshot.Checks) != 1 {
		t.Fatalf("got %d checks, want only the fixture's check run: an empty suite is not CI", len(snapshot.Checks))
	}
	for _, reason := range Evaluate(snapshot, Assessment{}).ReasonCodes {
		if reason == "ci_pending" {
			t.Fatal("empty queued suite reported pending CI")
		}
	}
}

func TestCollectorStandingBotRequestChangesBlocksCurrentHead(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 30, "Bot", "CHANGES_REQUESTED", head), fixtureReview(2, 30, "Bot", "COMMENTED", head)}
	snapshot := collectFixture(t, c)
	if !snapshot.ProviderChangesRequested || Evaluate(snapshot, Assessment{}).Decision != "needs_attention" {
		t.Fatal("bot objection discarded")
	}
	remote.Reviews = append(remote.Reviews, fixtureReview(3, 30, "Bot", "APPROVED", head))
	if collectFixture(t, c).ProviderChangesRequested {
		t.Fatal("later standing approval ignored")
	}
	remote.Reviews = remote.Reviews[:1]
	remote.Reviews[0].CommitID = gh.String(strings.Repeat("c", 40))
	if collectFixture(t, c).ProviderChangesRequested {
		t.Fatal("old revision verdict became unconditional veto")
	}
}

func TestCollectorViewerIdentitySurvivesRename(t *testing.T) {
	c, remote := fixtureCollector()
	remote.PR.User = &gh.User{ID: gh.Int64(77), Login: gh.String("renamed"), Type: gh.String("User")}
	snapshot, err := c.Collect(context.Background(), Target{Owner: "acme", Repo: "example", Number: 1}, Viewer{ID: 2, GitHubID: 77, Login: "old-name"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Eligible {
		t.Fatal("own PR accepted after rename")
	}
	remote.PR.User.ID = gh.Int64(78)
	remote.PR.User.Login = gh.String("old-name")
	snapshot, err = c.Collect(context.Background(), Target{Owner: "acme", Repo: "example", Number: 1}, Viewer{ID: 2, GitHubID: 77, Login: "old-name"})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Eligible {
		t.Fatal("recycled login treated as viewer")
	}
}

func TestCollectorInlineAnchorsRetainOriginalRange(t *testing.T) {
	c, remote := fixtureCollector()
	remote.InlineComments = []*gh.PullRequestComment{{ID: gh.Int64(1), Path: gh.String("handler.go"), OriginalStartLine: gh.Int(20), OriginalLine: gh.Int(23), Line: gh.Int(40), OriginalCommitID: gh.String(strings.Repeat("c", 40))}}
	remote.Threads = []githubclient.ApprovalThread{{ID: "thread", Comments: []int64{1}}}
	for _, e := range collectFixture(t, c).Evidence {
		if e.ID == "inline:1" {
			if e.Path != "handler.go" || e.StartLine != 20 || e.EndLine != 23 {
				t.Fatalf("lost original anchor: %+v", e)
			}
			return
		}
	}
	t.Fatal("inline evidence missing")
}

func TestCollectorLatestCompletedPRismVerdictWinsAtCurrentHead(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	latestVerdict := "approve"
	c.PRism = func(context.Context, Target) ([]Source, []Evidence, []Concern, []Endpoint, error) {
		return []Source{{ID: "old", Provider: "prism", Completion: "completed", ReviewedSHA: head, Verdict: "request_changes"}, {ID: "new", Provider: "prism", Completion: "completed", ReviewedSHA: head, Verdict: latestVerdict}, {ID: "failed", Provider: "prism", Completion: "failed", ReviewedSHA: head, Verdict: "request_changes"}}, []Evidence{{ID: "old", SourceID: "old", Kind: "prism_run", CreatedAt: time.Unix(1, 0)}, {ID: "new", SourceID: "new", Kind: "prism_run", CreatedAt: time.Unix(2, 0)}, {ID: "failed", SourceID: "failed", Kind: "prism_run", CreatedAt: time.Unix(3, 0)}}, nil, []Endpoint{{Name: "prism", Complete: true}}, nil
	}
	if collectFixture(t, c).ProviderChangesRequested {
		t.Fatal("historical or failed verdict overwrote newer completed approval")
	}
	latestVerdict = "request_changes"
	if !collectFixture(t, c).ProviderChangesRequested {
		t.Fatal("current completed veto lost")
	}
}

func TestCollectorEditedSummaryInvalidatesDigest(t *testing.T) {
	c, remote := fixtureCollector()
	remote.Comments = []*gh.IssueComment{{ID: gh.Int64(42), User: &gh.User{ID: gh.Int64(20), Type: gh.String("Bot")}, Body: gh.String("Overview: ready")}}
	first := collectFixture(t, c)
	remote.Comments[0].Body = gh.String("Overview: permission check missing")
	second := collectFixture(t, c)
	if first.Digest == second.Digest {
		t.Fatal("edited body did not invalidate snapshot")
	}
	if len(second.Evidence) != 2 || !strings.Contains(second.Evidence[0].Body, "permission check missing") {
		t.Fatal("summary omitted")
	}
	for _, source := range second.Sources {
		if source.ID == "comment:42" && source.Completion == "completed" {
			t.Fatal("summary comment supplied completion")
		}
	}
}

func TestCollectorIncompleteThreadsAndRequestsFailClosed(t *testing.T) {
	c, remote := fixtureCollector()
	remote.InlineComments = []*gh.PullRequestComment{{ID: gh.Int64(4), Body: gh.String("bug"), User: &gh.User{ID: gh.Int64(20), Type: gh.String("Bot")}}}
	remote.RequestedUsers = []*gh.User{{ID: gh.Int64(30), Type: gh.String("Bot")}}
	s := collectFixture(t, c)
	if s.Manifest.Complete || !s.ReviewInProgress {
		t.Fatal("incomplete collection or outstanding review lost")
	}
	c.PRism = nil
	if collectFixture(t, c).Manifest.Complete {
		t.Fatal("missing PRism collector accepted")
	}
}

func TestCollectorCombinedArtifactCeiling(t *testing.T) {
	c, remote := fixtureCollector()
	for index := 0; index < 2000; index++ {
		remote.Comments = append(remote.Comments, &gh.IssueComment{ID: gh.Int64(int64(index + 1)), Body: gh.String("context")})
	}
	s := collectFixture(t, c)
	if s.Manifest.Complete {
		t.Fatal("combined artifact ceiling ignored")
	}
}

func TestCollectorOldRunningReviewDoesNotBlockCurrentHead(t *testing.T) {
	c, _ := fixtureCollector()
	c.PRism = func(context.Context, Target) ([]Source, []Evidence, []Concern, []Endpoint, error) {
		return []Source{{ID: "older", Completion: "running", ReviewedSHA: strings.Repeat("f", 40)}}, nil, nil, []Endpoint{{Name: "prism", Complete: true}}, nil
	}
	if collectFixture(t, c).ReviewInProgress {
		t.Fatal("old revision review blocked current head")
	}
}

func TestCollectorSummaryCoverageCountsRemainEvidence(t *testing.T) {
	c, remote := fixtureCollector()
	remote.Comments = []*gh.IssueComment{{ID: gh.Int64(42), User: &gh.User{ID: gh.Int64(20), Type: gh.String("Bot")}, Body: gh.String("Reviewed 3 of 8 files. Remaining inputs were unavailable.")}}
	s := collectFixture(t, c)
	for _, source := range s.Sources {
		if source.ID == "comment:42" {
			if !source.Incomplete || source.FileCoverage != "reported_partial" || source.Completion == "completed" {
				t.Fatalf("invalid summary coverage: %+v", source)
			}
			return
		}
	}
	t.Fatal("summary source missing")
}
