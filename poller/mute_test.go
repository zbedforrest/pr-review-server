package poller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

type fakeSummaryGH struct {
	reactions []github.CommentReaction
	listErr   error
	reacted   []string
}

func (f *fakeSummaryGH) ListIssueCommentReactions(context.Context, string, string, int64) ([]github.CommentReaction, error) {
	return f.reactions, f.listErr
}

func (f *fakeSummaryGH) CreateIssueCommentReaction(_ context.Context, _, _ string, _ int64, content string) error {
	f.reacted = append(f.reacted, content)
	return nil
}

func muteFixture(t *testing.T) (*Poller, *db.GormDB, *fakeSummaryGH) {
	t.Helper()
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	gh := &fakeSummaryGH{}
	return &Poller{cfg: &config.Config{}, db: database, summaryGH: gh}, database, gh
}

func summaryRows(commentID int64, muted bool) []db.PublishedFinding {
	return []db.PublishedFinding{
		{Kind: db.PublishedKindFinding, Fingerprint: "a.go:1:abc", CommentID: 5},
		{Kind: db.PublishedKindSummary, Fingerprint: "summary", CommentID: commentID, LastSeenSHA: "abc", Muted: muted},
	}
}

var mutePR = github.PullRequest{Owner: "acme", Repo: "example", Number: 7, Author: "alice"}

func TestPRMuted_AuthorThumbsDownOnTheSummaryMutesRecordsAndAcknowledges(t *testing.T) {
	p, database, gh := muteFixture(t)
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindSummary, Fingerprint: "summary",
		ReviewedSHA: "abc", LastSeenSHA: "abc", State: db.PublishedStateOpen, CommentID: 900, Rounds: 1,
	}))
	gh.reactions = []github.CommentReaction{{UserID: 2, Content: "-1"}, {UserID: 1, Content: "+1"}}
	assert.False(t, p.prMuted(context.Background(), mutePR, summaryRows(900, false), 1), "someone else's thumbs-down is not the author's")
	assert.Empty(t, gh.reacted)

	gh.reactions = append(gh.reactions, github.CommentReaction{UserID: 1, Content: "-1"})
	assert.True(t, p.prMuted(context.Background(), mutePR, summaryRows(900, false), 1))
	assert.Equal(t, []string{muteAcknowledgement}, gh.reacted)
	row, _, err := database.GetPublishedSummaryForPR("acme", "example", 7)
	require.NoError(t, err)
	assert.True(t, row.Muted, "the mute is recorded on the summary row")
	muted, err := p.prMutedInLedger("acme", "example", 7)
	require.NoError(t, err)
	assert.True(t, muted, "the reply scan sees the same flag")
}

func TestPRMuted_LedgerFlagWinsWithoutAGitHubRead(t *testing.T) {
	p, _, gh := muteFixture(t)
	gh.listErr = errors.New("must not be called")
	assert.True(t, p.prMuted(context.Background(), mutePR, summaryRows(900, true), 1))
}

func TestPRMuted_UnreadableReactionsNoSummaryOrTheFlagOffLeaveThePRUnmuted(t *testing.T) {
	p, _, gh := muteFixture(t)
	gh.listErr = errors.New("HTTP 502")
	assert.False(t, p.prMuted(context.Background(), mutePR, summaryRows(900, false), 1))
	gh.listErr = nil
	gh.reactions = []github.CommentReaction{{UserID: 1, Content: "-1"}}
	assert.False(t, p.prMuted(context.Background(), mutePR, nil, 1), "no summary row, nothing to read")
	assert.False(t, p.prMuted(context.Background(), mutePR, summaryRows(0, false), 1), "a summary without a comment has no reactions")
	p.cfg.PublishNoPRMute = true
	assert.False(t, p.prMuted(context.Background(), mutePR, summaryRows(900, true), 1), "PUBLISH_PR_MUTE=false ignores even a recorded mute")
	assert.Nil(t, p.mentionMuter())
	muted, err := p.prMutedInLedger("acme", "example", 7)
	require.NoError(t, err)
	assert.False(t, muted)
}

func TestSettlementTreatsAMutedPublicationAsDone(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), readyDelivery("ready_for_review", autoReviewOldHead, false)))
	waitForDetachedReviews(t, f.p)
	intent := f.intents(t)[0]
	waitForReviewRunStatus(t, f.db, intent.RunID, db.ReviewRunStatusCompleted)
	require.NoError(t, f.db.SetAutoReviewIntentPublicationByRun(intent.RunID, publicationMuted))
	f.p.settleAutoReviewIntents()
	assert.Equal(t, db.AutoReviewIntentDone, f.intents(t)[0].Status)
}
