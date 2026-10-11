package poller

import (
	"context"
	"errors"
	"testing"
	"time"

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

func seedSummary(t *testing.T, database *db.GormDB, commentID int64, muted bool) {
	t.Helper()
	require.NoError(t, database.UpsertPublishedFinding(&db.PublishedFinding{
		RepoOwner: "acme", RepoName: "example", PRNumber: 7, Kind: db.PublishedKindSummary, Fingerprint: "summary",
		ReviewedSHA: "abc", LastSeenSHA: "abc", State: db.PublishedStateOpen, CommentID: commentID, Rounds: 1, Muted: muted,
	}))
}

func TestPRMuted_AuthorThumbsDownOnTheSummaryMutesRecordsAndAcknowledges(t *testing.T) {
	p, database, gh := muteFixture(t)
	seedSummary(t, database, 900, false)
	gh.reactions = []github.CommentReaction{{UserID: 2, Content: "-1"}, {UserID: 1, Content: "+1"}}
	muted, err := p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted, "someone else's thumbs-down is not the author's")
	assert.Empty(t, gh.reacted)

	gh.reactions = append(gh.reactions, github.CommentReaction{UserID: 1, Content: "-1"})
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.True(t, muted)
	assert.Equal(t, []string{muteAcknowledgement}, gh.reacted)
	row, _, err := database.GetPublishedSummaryForPR("acme", "example", 7)
	require.NoError(t, err)
	assert.True(t, row.Muted, "the mute is recorded on the summary row")

	gh.listErr = errors.New("must not be called")
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.True(t, muted, "the recorded flag answers every later check without a GitHub read")
	assert.Len(t, gh.reacted, 1, "the acknowledgement is left once")
}

func TestPRMuted_LedgerFlagWinsWithoutAGitHubRead(t *testing.T) {
	p, database, gh := muteFixture(t)
	seedSummary(t, database, 900, true)
	gh.listErr = errors.New("must not be called")
	muted, err := p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.True(t, muted)
}

func TestPRMuted_UnreadableReactionsNoSummaryOrTheFlagOffLeaveThePRUnmuted(t *testing.T) {
	p, database, gh := muteFixture(t)
	gh.reactions = []github.CommentReaction{{UserID: 1, Content: "-1"}}
	muted, err := p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted, "no summary row, nothing to read")

	seedSummary(t, database, 0, false)
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted, "a summary without a comment has no reactions")

	seedSummary(t, database, 900, false)
	gh.listErr = errors.New("HTTP 502")
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted)
	gh.listErr = nil
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 0)
	require.NoError(t, err)
	assert.False(t, muted, "without the author's id no reaction can be theirs")

	seedSummary(t, database, 900, true)
	p.cfg.PublishNoPRMute = true
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted, "PUBLISH_PR_MUTE=false ignores even a recorded mute")
	assert.Nil(t, p.mentionMuter())
	assert.Empty(t, gh.reacted)
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

func TestPRMuted_ALiftedMuteIgnoresTheThumbsDownThatCausedIt(t *testing.T) {
	p, database, gh := muteFixture(t)
	seedSummary(t, database, 900, false)
	earlier := time.Now().UTC().Add(-time.Hour)
	gh.reactions = []github.CommentReaction{{UserID: 1, Content: "-1", CreatedAt: earlier}}
	muted, err := p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	require.True(t, muted)

	lifted, err := database.SetPublishedMute("acme", "example", 7, false)
	require.NoError(t, err)
	require.True(t, lifted)
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.False(t, muted, "the author's @review lifted the mute; the old thumbs-down was already honoured")

	gh.reactions = append(gh.reactions, github.CommentReaction{UserID: 1, Content: "-1", CreatedAt: time.Now().UTC().Add(time.Hour)})
	muted, err = p.prMuted(context.Background(), "acme", "example", 7, 1)
	require.NoError(t, err)
	assert.True(t, muted, "a thumbs-down newer than the lift mutes again")
}
