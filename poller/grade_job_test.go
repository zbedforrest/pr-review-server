package poller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/db"
	"pr-review-server/pkg/grader"
	"pr-review-server/pkg/reviewer/service"
)

func seedGradeFindings(t *testing.T, database *db.GormDB, posted time.Time, prs ...int) {
	t.Helper()
	for _, n := range prs {
		require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
			RepoOwner: "acme", RepoName: "example", PRNumber: n, Kind: db.PublishedKindFinding, Fingerprint: "a.go:3:x",
			Severity: "medium", ReviewedSHA: "head", LastSeenSHA: "head", State: db.PublishedStateOpen,
			PublishedAt: posted, CommentID: int64(100 + n), CommentText: "finding text", Line: 3,
		}))
	}
}

func stubGradeAgent(t *testing.T, cost float64, calls *int) {
	t.Helper()
	prev := gradeAgentFn
	t.Cleanup(func() { gradeAgentFn = prev })
	gradeAgentFn = func(_ context.Context, _ service.AgentConfig, _ service.Spawner, in service.GradeAgentInput) (*service.GradeAgentResult, error) {
		*calls++
		res := &service.GradeAgentResult{Model: "judge", CostUSD: cost}
		for _, it := range in.Items {
			res.Grades = append(res.Grades, grader.Grade{ID: it.ID, Grade: grader.GradeBad, Category: "wrong-claim", Reason: "x"})
		}
		return res, nil
	}
}

func newGradePoller(t *testing.T, budget float64) (*Poller, *db.GormDB) {
	t.Helper()
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	p := newTestPollerFull(NewMockGitHubClient(), database, NewMockReviewStorage(), NewMockReviewGenerator())
	p.cfg.GraderEnabled = true
	p.cfg.GraderModel = "judge"
	p.cfg.GraderMaxItems = 60
	p.cfg.GraderDailyBudgetUSD = budget
	p.cfg.GraderWallClockSec, p.cfg.GraderMaxTurns = 60, 10
	return p, database
}

func TestRunDailyGradeGradesEachPRAndStopsAtTheBudget(t *testing.T) {
	p, database := newGradePoller(t, 1.5)
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	seedGradeFindings(t, database, now.Add(-time.Hour), 1, 2, 3)
	calls := 0
	stubGradeAgent(t, 1.0, &calls)

	started := false
	rep, err := p.RunDailyGrade(context.Background(), now, func() { started = true })
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, 2, calls, "the third PR is skipped once $2 of the $1.50 budget is spent")
	assert.Equal(t, GradeReport{Candidates: 3, Selected: 3, Graded: 2, Skipped: 1, Batches: 2, CostUSD: 2, Stopped: "budget"}, rep)

	cands, err := database.GradeCandidates(now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.Len(t, cands, 1, "graded comments are not offered again")
}

func TestRunDailyGradeRefusesWhenDisabledOrAlreadyRunning(t *testing.T) {
	p, _ := newGradePoller(t, 20)
	p.cfg.GraderEnabled = false
	_, err := p.RunDailyGrade(context.Background(), time.Now(), nil)
	assert.ErrorIs(t, err, ErrGraderDisabled)

	p.cfg.GraderEnabled = true
	gradeRunning.Store(true)
	defer gradeRunning.Store(false)
	_, err = p.RunDailyGrade(context.Background(), time.Now(), nil)
	assert.ErrorIs(t, err, ErrGraderBusy)
}

func TestCommentURLPointsAtTheRightGitHubAnchor(t *testing.T) {
	c := db.GradeCandidate{RepoOwner: "o", RepoName: "r", PRNumber: 4, Kind: grader.KindInline, CommentID: 7}
	assert.Equal(t, "https://github.com/o/r/pull/4#discussion_r7", commentURL(c))
	c.Kind, c.SummaryCommentID = grader.KindSummaryNote, 9
	assert.Equal(t, "https://github.com/o/r/pull/4#issuecomment-9", commentURL(c))
}
