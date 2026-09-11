package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/runconfig"
	"pr-review-server/poller"

	gh "pr-review-server/github"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type publishRecordingPoller struct {
	reviewAPITestPoller
	immediatePublish []bool
}

func (p *publishRecordingPoller) ProcessReviewImmediate(_ context.Context, _, _ string, _ int, _, _, _ string, _ *time.Time, _ bool, _ bool, publish bool) {
	p.immediatePublish = append(p.immediatePublish, publish)
}

func TestGenerateReview_PublishFlagDefaultsTrueAndCanBeDisabled(t *testing.T) {
	headSHA := "abcdef1234567890abcdef1234567890abcdef12"
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":5,"title":"t","state":"open","merged":false,"draft":false,"user":{"login":"alice"},"head":{"sha":"%s"}}`, headSHA)
	}))
	defer mockGH.Close()
	server, database := newTestServerWithGH(t, "testuser", gh.NewTestClient(mockGH.URL, "testuser"))
	defer database.Close()
	rec := &publishRecordingPoller{}
	server.poller = rec

	for _, body := range []string{
		`{"owner":"o","repo":"r","number":5}`,
		`{"owner":"o","repo":"r","number":5,"publish":false}`,
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/prs/generate-review", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.handleGenerateReview(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	assert.Equal(t, []bool{true, false}, rec.immediatePublish)
}

func TestCreateReviewRun_PublishFalseMarksJobSkipPublish(t *testing.T) {
	headSHA := "abcdef1234567890abcdef1234567890abcdef12"
	s, database, apiPoller, userID := newReviewAPIServer(t, githubPRResponse(headSHA))
	defer database.Close()

	body := `{"target":{"owner":"acme","repo":"widgets","pull_request":42},"publish":false}`
	request := addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(body)), *userID)
	recorder := httptest.NewRecorder()
	s.handleReviewRuns(recorder, request)

	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
	require.Len(t, apiPoller.jobs, 1)
	assert.True(t, apiPoller.jobs[0].SkipPublish)

	body = `{"target":{"owner":"acme","repo":"widgets","pull_request":43}}`
	request = addReviewAPIUser(httptest.NewRequest(http.MethodPost, reviewRunsPath, strings.NewReader(body)), *userID)
	recorder = httptest.NewRecorder()
	s.handleReviewRuns(recorder, request)
	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
	require.Len(t, apiPoller.jobs, 2)
	assert.False(t, apiPoller.jobs[1].SkipPublish, "publish defaults to true")
}

var _ = github.PullRequest{}
var _ = runconfig.Overrides{}
var _ = poller.ReviewJob{}
