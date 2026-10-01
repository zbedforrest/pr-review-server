package poller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/health"
	"pr-review-server/pkg/publisher"
)

func TestHygieneTelemetryEvents_OneEventPerNoteUnderTheSharedActionNames(t *testing.T) {
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7}
	h := publisher.Hygiene{
		RepeatedPosts:          []publisher.HygieneNote{{Fingerprint: "a.go:1:x", Detail: "prior_state=resolved prior_comment=5"}, {Fingerprint: "b.go:2:y", Detail: "prior_state=resolved prior_comment=6"}},
		RepeatAfterDismiss:     []publisher.HygieneNote{{Fingerprint: "a.go:1:x", Detail: "prior_comment=5"}},
		FixedWithoutFileChange: []publisher.HygieneNote{{Fingerprint: "c.go:3:z", Detail: "file=c.go from=s1 to=s2"}},
		SameCommitResolves:     []publisher.HygieneNote{{Fingerprint: "d.go:4:w", Detail: "head=s2"}},
		SeverityEscalations:    []publisher.HygieneNote{{Fingerprint: "b.go:2:y", Detail: "from=medium to=critical prior_state=resolved"}},
	}

	events := hygieneTelemetryEvents(pr, h, 9)

	byAction := map[string]int{}
	for _, ev := range events {
		byAction[ev.Action]++
		assert.Equal(t, 9, ev.UserID)
		assert.Equal(t, "acme", ev.PROwner)
		assert.Equal(t, "example", ev.PRRepo)
		assert.Equal(t, 7, ev.PRNumber)
		assert.Contains(t, ev.Label, "fp=")
	}
	assert.Equal(t, map[string]int{
		health.ActionRepeatedPost: 2, health.ActionRepeatAfterDismiss: 1, health.ActionFixedWithoutFileChange: 1,
		health.ActionSameCommitResolve: 1, health.ActionSeverityEscalation: 1,
	}, byAction)
	assert.Empty(t, hygieneTelemetryEvents(pr, publisher.Hygiene{}, 9))
}

func TestPublishGitHubReview_RecordsSameCommitResolvesAsTelemetry(t *testing.T) {
	ts, _ := gitHubStub(t, openPRJSON, false)
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting("publish_enabled_authors", "alice"))
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 1,
		Kind: db.PublishedKindFinding, Fingerprint: "g.go:0:feedfacecafe", Severity: "critical",
		ReviewedSHA: "abc", LastSeenSHA: "abc", CommentID: 55, State: db.PublishedStateOpen,
	}))
	p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}

	report, outcome := p.publishGitHubReview(context.Background(), github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "alice"}, []byte(scoredSidecar))

	require.NotNil(t, report)
	assert.Equal(t, publicationPosted, outcome)
	require.Len(t, report.Hygiene.SameCommitResolves, 1)
	stats, err := database.GetTelemetryStats(1)
	require.NoError(t, err)
	var recorded []db.ActionCount
	for _, a := range stats.ByAction {
		if a.Action == health.ActionSameCommitResolve {
			recorded = append(recorded, a)
		}
	}
	require.Len(t, recorded, 1, "actions recorded: %+v", stats.ByAction)
	assert.Equal(t, 1, recorded[0].Count)
	assert.Equal(t, 1, stats.ActiveUsers, "events belong to the reserved system user")
}

func TestRecordHygieneEvent_WritesOneEventWithPRCoordinates(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	p := &Poller{cfg: &config.Config{}, db: database}

	p.recordHygieneEvent(github.PullRequest{Owner: "acme", Repo: "example", Number: 3}, health.ActionOptedOut, "author=carol")

	stats, err := database.GetTelemetryStats(1)
	require.NoError(t, err)
	require.Equal(t, 1, stats.TotalEvents)
	assert.Equal(t, health.ActionOptedOut, stats.ByAction[0].Action)
	require.Len(t, stats.TopPRs, 1)
	assert.Equal(t, 3, stats.TopPRs[0].Number)
}
