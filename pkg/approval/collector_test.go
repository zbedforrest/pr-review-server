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

func TestCollectorAuthenticCopilotCommentReviewAndSpoof(t *testing.T) {
	c, remote := fixtureCollector()
	head := remote.PR.GetHead().GetSHA()
	remote.Reviews = []*gh.PullRequestReview{fixtureReview(1, 30, "Bot", "COMMENTED", head), fixtureReview(2, 20, "User", "APPROVED", head), fixtureReview(3, 999, "Bot", "APPROVED", head)}
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
