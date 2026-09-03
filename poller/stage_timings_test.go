package poller

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
)

type stageTimingGithubStub struct{}

func (stageTimingGithubStub) GetReviewPR(token, owner, repoName string, prNumber int) (github.PR, error) {
	pr := github.PR{Number: prNumber, Body: "test PR"}
	pr.Head.SHA = "abcdef0123456789abcdef0123456789abcdef01"
	pr.Base.Ref = "main"
	return pr, nil
}

func (stageTimingGithubStub) GetPRDiff(token, owner, repoName string, prNumber int) (string, error) {
	return "diff --git a/main.go b/main.go\n", nil
}

func (stageTimingGithubStub) GetPRFiles(token, owner, repoName string, prNumber int) ([]github.PRFile, error) {
	return nil, nil
}

func (stageTimingGithubStub) GetFileContent(token, owner, repoName, filePath, branch string) (string, error) {
	return "", nil
}

func (stageTimingGithubStub) PostLineComment(token, owner, repoName string, prNumber int, commitID, comment, path string, line int) error {
	return nil
}

func (stageTimingGithubStub) PostPRComment(token, owner, repoName string, prNumber int, comment string) error {
	return nil
}

func (stageTimingGithubStub) GetPRComments(token, owner, repoName string, prNumber int) ([]github.PRComment, error) {
	return nil, nil
}

func (stageTimingGithubStub) GetCurrentUser(token string) (github.User, error) {
	return github.User{Login: "bot"}, nil
}

type stageTimingLLMStub struct {
	response string
	delay    time.Duration
}

func (s stageTimingLLMStub) GetReview(prompt string) (string, int32, int32, int32, error) {
	time.Sleep(s.delay)
	return s.response, 10, 5, 15, nil
}

func (s stageTimingLLMStub) GetReviewStream(prompt string, w io.Writer) (string, int32, int32, int32, error) {
	return s.GetReview(prompt)
}

func (s stageTimingLLMStub) ValidateAPIKey() error { return nil }

func timingsByStage(timings []payload.StageTiming, stage string) []payload.StageTiming {
	var matched []payload.StageTiming
	for _, timing := range timings {
		if timing.Stage == stage {
			matched = append(matched, timing)
		}
	}
	return matched
}

func TestPipelineRunProducesStageTimings(t *testing.T) {
	exec := &reviewExecution{Job: customReviewJob(t, "run-91000000000000000000000000000001")}
	svc := service.NewService(
		stageTimingGithubStub{},
		stageTimingLLMStub{response: `[{"file_path":"main.go","line_number":3,"comment_body":"possible nil deref"}]`, delay: 30 * time.Millisecond},
		stageTimingLLMStub{response: `{"RC-1":"CRITICAL","RC-2":"MEDIUM","RC-3":"LOW"}`, delay: 15 * time.Millisecond},
	)

	_, err := svc.PerformReviewWithContext(context.Background(), service.PerformReviewConfig{
		Owner: "acme", RepoName: "example", PRNumber: 7, NRequests: 3,
		AttemptObserver: func(event service.ProviderAttemptEvent) error {
			exec.recordProviderAttempt(event)
			return nil
		},
	})
	require.NoError(t, err)

	artifactSaveStartedAt := time.Now().UTC()
	completedAt := artifactSaveStartedAt.Add(20 * time.Millisecond)
	timings := exec.stageTimings(artifactSaveStartedAt, completedAt)

	firstPass := timingsByStage(timings, "first_pass")
	require.Len(t, firstPass, 1)
	samples := timingsByStage(timings, "first_pass_sample")
	require.Len(t, samples, 3)
	invocations := make([]int, len(samples))
	for i, sample := range samples {
		invocations[i] = sample.Invocation
		assert.GreaterOrEqual(t, sample.DurationMS, int64(25), "sample %d duration should cover the provider call", sample.Invocation)
		assert.False(t, sample.StartedAt.Before(firstPass[0].StartedAt))
		assert.LessOrEqual(t, sample.DurationMS, firstPass[0].DurationMS)
	}
	assert.ElementsMatch(t, []int{1, 2, 3}, invocations)

	classification := timingsByStage(timings, "classification")
	require.Len(t, classification, 1)
	assert.GreaterOrEqual(t, classification[0].DurationMS, int64(10))
	firstPassEnd := firstPass[0].StartedAt.Add(time.Duration(firstPass[0].DurationMS) * time.Millisecond)
	assert.False(t, classification[0].StartedAt.Before(firstPassEnd.Add(-time.Millisecond)),
		"classification must start after the first pass completes")

	artifactSave := timingsByStage(timings, "artifact_save")
	require.Len(t, artifactSave, 1)
	assert.Equal(t, int64(20), artifactSave[0].DurationMS)
	assert.Equal(t, artifactSaveStartedAt, artifactSave[0].StartedAt)

	for i := 1; i < len(timings); i++ {
		assert.False(t, timings[i].StartedAt.Before(timings[i-1].StartedAt),
			"stage_timings must be ordered by start time: %q before %q", timings[i-1].Stage, timings[i].Stage)
	}
}

func TestStageTimingsAggregatesRetriesAndOrdersRecordedStages(t *testing.T) {
	exec := &reviewExecution{}
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	record := func(stage string, invocation, attempt int, start time.Time, duration time.Duration, status string) {
		end := start.Add(duration)
		exec.recordProviderAttempt(service.ProviderAttemptEvent{
			Stage: stage, InvocationNumber: invocation, AttemptNumber: attempt,
			StartedAt: &start, CompletedAt: &end, DurationMS: duration.Milliseconds(), Status: status,
		})
	}
	record("first_pass", 1, 1, base, 2*time.Second, "failed")
	record("first_pass", 1, 2, base.Add(2*time.Second), 3*time.Second, "completed")
	record("first_pass", 2, 1, base.Add(250*time.Millisecond), 4*time.Second, "completed")
	record("classification", 1, 1, base.Add(6*time.Second), time.Second, "completed")
	record("agent", 1, 1, base.Add(8*time.Second), 30*time.Second, "completed")
	exec.recordStageTiming(payload.StageTiming{Stage: "gates", StartedAt: base.Add(7500 * time.Millisecond), DurationMS: 40})

	artifactSaveStartedAt := base.Add(39 * time.Second)
	timings := exec.stageTimings(artifactSaveStartedAt, artifactSaveStartedAt.Add(700*time.Millisecond))

	stages := make([]string, len(timings))
	for i, timing := range timings {
		stages[i] = timing.Stage
	}
	assert.Equal(t, []string{
		"first_pass", "first_pass_sample", "first_pass_sample",
		"classification", "gates", "agent", "artifact_save",
	}, stages)

	assert.Equal(t, base, timings[0].StartedAt)
	assert.Equal(t, int64(5000), timings[0].DurationMS, "aggregate spans first start to last completion")

	sampleOne := timings[1]
	assert.Equal(t, 1, sampleOne.Invocation)
	assert.Equal(t, base, sampleOne.StartedAt)
	assert.Equal(t, int64(5000), sampleOne.DurationMS, "sample span covers the retry")
	sampleTwo := timings[2]
	assert.Equal(t, 2, sampleTwo.Invocation)
	assert.Equal(t, int64(4000), sampleTwo.DurationMS)

	assert.Equal(t, int64(40), timings[4].DurationMS)
	assert.Equal(t, int64(30000), timings[5].DurationMS)
	assert.Equal(t, int64(700), timings[6].DurationMS)
}

func TestStageTimingsOmittedForStagesThatNeverRan(t *testing.T) {
	exec := &reviewExecution{}
	timings := exec.stageTimings(time.Time{}, time.Time{})
	assert.Empty(t, timings)
}
