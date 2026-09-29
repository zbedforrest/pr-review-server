package poller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
)

func TestAutoReviewAuthorsFallsBackToThePublishListWhenUnset(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice")
	for author, want := range map[string]bool{"Alice": true, "bob": false} {
		got, err := f.p.autoReviewEligible(author)
		require.NoError(t, err)
		assert.Equal(t, want, got, author)
	}
	require.NoError(t, f.db.SetSetting(SettingAutoReviewAuthors, "*"))
	got, err := f.p.autoReviewEligible("bob")
	require.NoError(t, err)
	assert.True(t, got, "auto_review_authors widens automatic reviews beyond the publish list")
}

func TestAutoReviewAuthorsNeverMatchBots(t *testing.T) {
	f := newAutoReviewFixture(t, true, "alice")
	require.NoError(t, f.db.SetSetting(SettingAutoReviewAuthors, "*"))
	require.NoError(t, f.db.SetSetting(db.SettingCIStatusExcludeAuthors, "house-renovate"))
	for _, bot := range []string{"renovate[bot]", "dependabot[bot]", "house-renovate"} {
		got, err := f.p.autoReviewEligible(bot)
		require.NoError(t, err)
		assert.False(t, got, "%s must not match *", bot)
	}
}

func TestWebhookSkipsAuthorsGitHubReportsAsBots(t *testing.T) {
	f := newAutoReviewFixture(t, true, "*")
	d := readyDelivery("synchronize", autoReviewNewHead, false)
	d.AuthorType = "Bot"
	require.NoError(t, f.p.HandleWebhookDelivery(context.Background(), d))
	assert.Empty(t, f.intents(t))
	assert.Empty(t, f.runs())
}

func TestPublishAllowsAutomaticReviewsForAutoReviewAuthorsOnly(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	require.NoError(t, database.SetSetting(settingPublishEnabledAuthors, "alice"))
	require.NoError(t, database.SetSetting(SettingAutoReviewAuthors, "*"))
	bob := github.PullRequest{Owner: "acme", Repo: "example", Number: 1, CommitSHA: "abc", Author: "bob"}

	ts, _ := gitHubStub(t, openPRJSON, false)
	p := &Poller{cfg: &config.Config{}, db: database, ghClientConcrete: github.NewTestClient(ts.URL, "bot")}

	_, manual := p.publishGitHubReview(context.Background(), bob, []byte(scoredSidecar), false)
	assert.Equal(t, publicationNotAllowed, manual, "a manual review of a non-publish author stays unpublished")
	_, auto := p.publishGitHubReview(context.Background(), bob, []byte(scoredSidecar), true)
	assert.Equal(t, publicationPosted, auto, "an automatic review of an auto_review_authors author is published")
}
