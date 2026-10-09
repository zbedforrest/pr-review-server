package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPublished(overrides func(*PublishedFinding)) *PublishedFinding {
	p := &PublishedFinding{
		RepoOwner:    "owner",
		RepoName:     "repo",
		PRNumber:     7,
		Kind:         PublishedKindFinding,
		Fingerprint:  "pkg/api/handler.go:4:deadbeef0123",
		SourceTag:    "prism-only",
		Severity:     "medium",
		ReviewedSHA:  "abc1234",
		LastSeenSHA:  "abc1234",
		CommentID:    9001,
		ThreadNodeID: "PRRT_kwDOabc",
		ReviewID:     4242,
		State:        PublishedStateOpen,
		PublishedAt:  time.Now().UTC().Truncate(time.Second),
	}
	if overrides != nil {
		overrides(p)
	}
	return p
}

func TestGormDB_PublishedFinding_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	want := testPublished(nil)
	require.NoError(t, db.UpsertPublishedFinding(want))

	got, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, want.Kind, got[0].Kind)
	assert.Equal(t, want.Fingerprint, got[0].Fingerprint)
	assert.Equal(t, want.SourceTag, got[0].SourceTag)
	assert.Equal(t, want.Severity, got[0].Severity)
	assert.Equal(t, want.ReviewedSHA, got[0].ReviewedSHA)
	assert.Equal(t, want.LastSeenSHA, got[0].LastSeenSHA)
	assert.Equal(t, want.CommentID, got[0].CommentID)
	assert.Equal(t, want.ThreadNodeID, got[0].ThreadNodeID)
	assert.Equal(t, want.ReviewID, got[0].ReviewID)
	assert.Equal(t, want.State, got[0].State)
	assert.WithinDuration(t, want.PublishedAt, got[0].PublishedAt, time.Second)
	assert.NotZero(t, got[0].ID)
}

func TestGormDB_PublishedFinding_UpsertOverwritesSameFingerprintAcrossPushes(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.LastSeenSHA = "def5678"
		p.State = PublishedStateResolved
	})))

	got, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, got, 1, "a finding is published once per PR, not once per push")
	assert.Equal(t, "abc1234", got[0].ReviewedSHA, "first-published sha is preserved")
	assert.Equal(t, "def5678", got[0].LastSeenSHA)
	assert.Equal(t, PublishedStateResolved, got[0].State)
	assert.Equal(t, int64(9001), got[0].CommentID, "comment id survives the overwrite")
}

func TestGormDB_PublishedFinding_SummaryAndFindingsCoexist(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Kind = PublishedKindSummary
		p.Fingerprint = PublishedKindSummary
		p.CommentID = 5555
	})))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))

	got, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestGormDB_PublishedFinding_ScopedToPR(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.PRNumber = 8 })))

	got, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestGormDB_PublishedFinding_RoundsUpdatesOnlyWhenProvided(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	summary := func(rounds int) *PublishedFinding {
		return testPublished(func(p *PublishedFinding) {
			p.Kind = PublishedKindSummary
			p.Fingerprint = PublishedKindSummary
			p.Rounds = rounds
		})
	}
	require.NoError(t, db.UpsertPublishedFinding(summary(2)))
	got, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, 2, got[0].Rounds)

	require.NoError(t, db.UpsertPublishedFinding(summary(3)))
	got, _ = db.GetPublishedFindingsForPR("owner", "repo", 7)
	assert.Equal(t, 3, got[0].Rounds)

	require.NoError(t, db.UpsertPublishedFinding(summary(0)))
	got, _ = db.GetPublishedFindingsForPR("owner", "repo", 7)
	assert.Equal(t, 3, got[0].Rounds, "a zero Rounds on upsert must not clobber the counter")
}

func TestGormDB_GetPublishedSummaryForPR_ReturnsOnlyTheSummaryRow(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	row, ok, err := db.GetPublishedSummaryForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.False(t, ok, "a finding row is not a summary")
	assert.Zero(t, row.Rounds)

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Kind = PublishedKindSummary
		p.Fingerprint = "SUMMARY"
		p.Rounds = 3
	})))
	row, ok, err = db.GetPublishedSummaryForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 3, row.Rounds)
}

func TestGormDB_UpsertPublishedFinding_KeepsADismissedRowDismissed(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.SetPublishedFindingState("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123", PublishedStateDismissed))

	stale := testPublished(func(p *PublishedFinding) { p.LastSeenSHA = "def5678"; p.State = PublishedStateOpen })
	require.NoError(t, db.UpsertPublishedFinding(stale))

	rows, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, PublishedStateDismissed, rows[0].State, "a concession must survive a publication that loaded the row before it")
	assert.Equal(t, "def5678", rows[0].LastSeenSHA, "other columns still update")
}

func TestGormDB_UpsertPublishedFinding_KeepsAContestedRowContested(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	changed, err := db.ContestPublishedFinding("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123")
	require.NoError(t, err)
	assert.True(t, changed)

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) { p.LastSeenSHA = "def5678"; p.State = PublishedStateOpen })))
	rows, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, PublishedStateContested, rows[0].State)
}

func TestGormDB_ContestPublishedFinding_MovesOpenAndResolvedRowsOnly(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.UpsertPublishedFinding(testPublished(nil)))
	require.NoError(t, db.SetPublishedFindingState("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123", PublishedStateDismissed))
	changed, err := db.ContestPublishedFinding("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123")
	require.NoError(t, err)
	assert.False(t, changed)
	rows, err := db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, PublishedStateDismissed, rows[0].State)

	changed, err = db.ContestPublishedFinding("owner", "repo", 7, "nope")
	require.NoError(t, err)
	assert.False(t, changed)

	require.NoError(t, db.SetPublishedFindingState("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123", PublishedStateResolved))
	changed, err = db.ContestPublishedFinding("owner", "repo", 7, "pkg/api/handler.go:4:deadbeef0123")
	require.NoError(t, err)
	assert.True(t, changed)
	rows, err = db.GetPublishedFindingsForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.Equal(t, PublishedStateContested, rows[0].State)
}

func TestGormDB_SetPublishedMute_FlagsTheSummaryRowAndSurvivesRounds(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	changed, err := db.SetPublishedMute("owner", "repo", 7, true)
	require.NoError(t, err)
	assert.True(t, changed, "a PR without a summary row gets a muted placeholder")
	row, found, err := db.GetPublishedSummaryForPR("owner", "repo", 7)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, row.Muted)
	assert.Zero(t, row.CommentID)
	assert.Empty(t, row.LastSeenSHA, "the placeholder never reads as a published head")

	changed, err = db.SetPublishedMute("owner", "repo", 7, true)
	require.NoError(t, err)
	assert.False(t, changed, "muting twice changes nothing")

	require.NoError(t, db.UpsertPublishedFinding(testPublished(func(p *PublishedFinding) {
		p.Kind, p.Fingerprint, p.CommentID, p.Rounds = PublishedKindSummary, PublishedKindSummary, 777, 1
	})))
	row, _, err = db.GetPublishedSummaryForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.True(t, row.Muted, "a round rewriting the summary row keeps the mute")
	assert.Equal(t, int64(777), row.CommentID)

	changed, err = db.SetPublishedMute("owner", "repo", 7, false)
	require.NoError(t, err)
	assert.True(t, changed)
	row, _, err = db.GetPublishedSummaryForPR("owner", "repo", 7)
	require.NoError(t, err)
	assert.False(t, row.Muted)
	changed, err = db.SetPublishedMute("owner", "repo", 8, false)
	require.NoError(t, err)
	assert.False(t, changed, "clearing a mute on a PR without a row creates nothing")
	_, found, err = db.GetPublishedSummaryForPR("owner", "repo", 8)
	require.NoError(t, err)
	assert.False(t, found)
}
