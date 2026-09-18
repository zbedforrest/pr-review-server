package poller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v57/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

func greptileReview(id int64, commit string) *gh.PullRequestReview {
	return &gh.PullRequestReview{ID: gh.Int64(id), CommitID: gh.String(commit), User: &gh.User{Login: gh.String("greptile-apps[bot]")}}
}

func greptileComment(id, reviewID int64, badge string) github.ReviewCommentInfo {
	return github.ReviewCommentInfo{
		ID: id, ReviewID: reviewID, Author: "greptile-apps[bot]", Path: "a.go", Line: 3,
		Body: fmt.Sprintf(`<img alt="%s" src="x"> **Something** body text`, badge),
	}
}

func TestComputeGreptileStatus(t *testing.T) {
	human := &gh.PullRequestReview{ID: gh.Int64(1), CommitID: gh.String("abc"), User: &gh.User{Login: gh.String("alice")}}
	assert.Equal(t, GreptileStatusAbsent, computeGreptileStatus(nil, nil, "abc"))
	assert.Equal(t, GreptileStatusAbsent, computeGreptileStatus([]*gh.PullRequestReview{human}, nil, "abc"), "human reviews do not count")
	assert.Equal(t, GreptileStatusAbsent, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "old")}, nil, "abc"), "review of an older head")

	assert.Equal(t, GreptileStatusGreen, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc")}, nil, "abc"), "clean review")
	assert.Equal(t, GreptileStatusGreen, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc")},
		[]github.ReviewCommentInfo{greptileComment(10, 2, "P2")}, "abc"), "P2 stays green")
	assert.Equal(t, GreptileStatusRed, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc")},
		[]github.ReviewCommentInfo{greptileComment(10, 2, "P1")}, "abc"))
	assert.Equal(t, GreptileStatusRed, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc")},
		[]github.ReviewCommentInfo{greptileComment(10, 2, "P0")}, "abc"))
	assert.Equal(t, GreptileStatusGreen, computeGreptileStatus(
		[]*gh.PullRequestReview{greptileReview(2, "old"), greptileReview(3, "abc")},
		[]github.ReviewCommentInfo{greptileComment(10, 2, "P0")}, "abc"), "P0 from the previous head does not count")
	assert.Equal(t, GreptileStatusGreen, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc")},
		[]github.ReviewCommentInfo{{ID: 11, ReviewID: 2, Author: "greptile-apps[bot]", InReplyToID: 10, Body: `<img alt="P0"> **reply**`}}, "abc"), "thread replies carry no verdict")
	assert.Equal(t, GreptileStatusGreen, computeGreptileStatus([]*gh.PullRequestReview{greptileReview(2, "abc1234567")}, nil, "abc1234"), "short and long SHAs match")
}

func TestRefreshGreptileStatus_StoresVerdictForHead(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/example/pulls/1/reviews":
			fmt.Fprint(w, `[{"id": 5, "commit_id": "abc", "user": {"login": "greptile-apps[bot]"}}]`)
		case "/repos/acme/example/pulls/1/comments":
			fmt.Fprint(w, `[{"id": 9, "pull_request_review_id": 5, "path": "a.go", "line": 3, "user": {"login": "greptile-apps[bot]"}, "body": "<img alt=\"P1\" src=\"x\"> **Bug** text"}]`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 1, LastCommitSHA: "abc"}))
	p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}

	p.refreshGreptileStatus(context.Background(), "acme", "example", 1, "abc")
	pr, err := database.GetPR("acme", "example", 1)
	require.NoError(t, err)
	assert.Equal(t, GreptileStatusRed, pr.GreptileStatus)
	assert.Equal(t, "abc", pr.GreptileStatusSHA)

	p.refreshGreptileStatus(context.Background(), "acme", "example", 1, "def")
	pr, err = database.GetPR("acme", "example", 1)
	require.NoError(t, err)
	assert.Equal(t, GreptileStatusAbsent, pr.GreptileStatus, "no Greptile review of the new head")
	assert.Equal(t, "def", pr.GreptileStatusSHA)
}

func TestRefreshGreptileStatus_NoConcreteClientIsNoop(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.UpsertPR(&db.PR{RepoOwner: "acme", RepoName: "example", PRNumber: 1, LastCommitSHA: "abc"}))
	p := &Poller{cfg: &config.Config{}, db: database}
	p.refreshGreptileStatus(context.Background(), "acme", "example", 1, "abc")
	pr, _ := database.GetPR("acme", "example", 1)
	assert.Empty(t, pr.GreptileStatus)
}

func TestHeadReviewedByGreptile(t *testing.T) {
	assert.False(t, headReviewedByGreptile(&github.PRReviewData{HeadReviewers: []string{"alice"}}))
	assert.True(t, headReviewedByGreptile(&github.PRReviewData{HeadReviewers: []string{"alice", "greptile-apps[bot]"}}))
}
