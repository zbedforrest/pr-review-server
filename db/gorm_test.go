package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDB creates an in-memory SQLite database for testing
func newTestDB(t *testing.T) *GormDB {
	db, err := NewGormSQLite(":memory:")
	require.NoError(t, err)
	return db
}

// =============================================================================
// PR Operations Tests
// =============================================================================

func TestGormDB_GetPR_NotFound(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	pr, err := db.GetPR("owner", "repo", 1)
	assert.NoError(t, err)
	assert.Nil(t, pr)
}

func TestGormDB_UpsertPR_Insert(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
		Title:         "Test PR",
		Author:        "testuser",
		CreatedAt:     &now,
		Draft:         false,
	}

	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Verify we can fetch it back
	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	require.NotNil(t, fetched)

	assert.Equal(t, "owner", fetched.RepoOwner)
	assert.Equal(t, "repo", fetched.RepoName)
	assert.Equal(t, 1, fetched.PRNumber)
	assert.Equal(t, "abc123", fetched.LastCommitSHA)
	assert.Equal(t, "pending", fetched.Status)
	assert.Equal(t, "Test PR", fetched.Title)
	assert.Equal(t, "testuser", fetched.Author)
}

func TestGormDB_UpsertPR_Update(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert initial PR
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
		Title:         "Initial Title",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Update it
	pr.LastCommitSHA = "def456"
	pr.Title = "Updated Title"
	err = db.UpsertPR(pr)
	require.NoError(t, err)

	// Verify the update
	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "def456", fetched.LastCommitSHA)
	assert.Equal(t, "Updated Title", fetched.Title)
}

// Regression: UpsertPR's OnConflict whitelist (upsertMetadataColumns) must
// include the columns the poller's reviewPRBatch / ciPRBatch flushes write
// to — approval_count, my_review_status, ci_state, ci_failed_checks. If any
// of these get dropped from the whitelist, those updates silently no-op on
// existing rows (the mock DB used in poller_test does a full struct
// overwrite and won't catch this).
func TestGormDB_UpsertPR_UpdatesPollerWrittenColumns(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert initial PR with zero values for the columns the poller writes.
	err := db.UpsertPR(&PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123", Status: "pending",
	})
	require.NoError(t, err)

	// Simulate the poller flushing review/CI updates onto the existing row.
	err = db.UpsertPR(&PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123",
		ApprovalCount:  2,
		MyReviewStatus: "APPROVED",
		CIState:        "failure",
		CIFailedChecks: `["build","test"]`,
	})
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, 2, fetched.ApprovalCount, "approval_count must be updated on conflict")
	assert.Equal(t, "APPROVED", fetched.MyReviewStatus, "my_review_status must be updated on conflict")
	assert.Equal(t, "failure", fetched.CIState, "ci_state must be updated on conflict")
	assert.Equal(t, `["build","test"]`, fetched.CIFailedChecks, "ci_failed_checks must be updated on conflict")
}

// Regression: when a PR is in-flight (status=generating or agent_reviewing),
// a stale poll-cycle UpsertPR with the previous SHA must NOT overwrite the
// freshly-set last_commit_sha. Without the CASE expression in upsertOnConflict
// this drift would cause checkForOutdatedReviews to see a phantom mismatch
// and kill the legitimate in-flight review.
func TestGormDB_UpsertPR_PreservesSHAWhileInFlight(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	cases := []string{"generating", "agent_reviewing"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			// Manual trigger has just bumped the row to commit B.
			err := db.UpsertPR(&PR{
				RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
				LastCommitSHA: "newSHA_B", Status: "pending",
			})
			require.NoError(t, err)
			err = db.SetPRGenerating("owner", "repo", 1, "newSHA_B", "T", "A", nil, false)
			require.NoError(t, err)
			if status == "agent_reviewing" {
				require.NoError(t, db.SetPRAgentReviewing("owner", "repo", 1))
			}

			// Stale poll cycle's BatchUpsertPRs writes the old SHA.
			err = db.BatchUpsertPRs([]*PR{{
				RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
				LastCommitSHA: "staleSHA_A",
				Title:         "fresh title from poll",
			}})
			require.NoError(t, err)

			fetched, err := db.GetPR("owner", "repo", 1)
			require.NoError(t, err)
			assert.Equal(t, "newSHA_B", fetched.LastCommitSHA,
				"in-flight last_commit_sha must not be clobbered by stale poll")
			assert.Equal(t, "fresh title from poll", fetched.Title,
				"non-SHA metadata fields should still update")
			assert.Equal(t, status, fetched.Status, "status must be unchanged")

			require.NoError(t, db.DeletePR("owner", "repo", 1))
		})
	}
}

// Companion: when the row is NOT in-flight, the upsert SHOULD update
// last_commit_sha so the poll cycle continues to track new commits on
// completed/pending rows.
func TestGormDB_UpsertPR_UpdatesSHAWhenNotInFlight(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	for _, status := range []string{"completed", "pending", "error"} {
		t.Run(status, func(t *testing.T) {
			require.NoError(t, db.UpsertPR(&PR{
				RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
				LastCommitSHA: "oldSHA", Status: status,
			}))
			// Force the status (UpsertPR doesn't touch status on conflict).
			require.NoError(t, db.UpdatePRStatus("owner", "repo", 1, status))

			require.NoError(t, db.UpsertPR(&PR{
				RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
				LastCommitSHA: "freshSHA",
			}))

			fetched, err := db.GetPR("owner", "repo", 1)
			require.NoError(t, err)
			assert.Equal(t, "freshSHA", fetched.LastCommitSHA,
				"last_commit_sha should track the latest poll for non-in-flight rows")

			require.NoError(t, db.DeletePR("owner", "repo", 1))
		})
	}
}

// Regression: UpsertPR must NOT touch status / review_path / importance counts
// on conflict — those are owned by the dedicated setters and a stale read in
// the poll cycle could otherwise clobber a fresh review's terminal state.
func TestGormDB_UpsertPR_DoesNotClobberReviewState(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Initial pending row.
	err := db.UpsertPR(&PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123", Status: "pending",
	})
	require.NoError(t, err)

	// Mark it completed via the dedicated setter.
	err = db.MarkPRCompleted("owner", "repo", 1, "abc123", "review.html", 1, 2, 3)
	require.NoError(t, err)

	// The poller does a metadata refresh with a stale read still showing pending.
	err = db.UpsertPR(&PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123", Status: "pending", // stale
		Title: "Updated Title",
	})
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "completed", fetched.Status, "status must not be clobbered by a stale poller upsert")
	assert.Equal(t, "review.html", fetched.ReviewHTMLPath, "review_path must not be cleared")
	assert.Equal(t, 1, fetched.CriticalCount, "critical_count must not be reset")
	assert.Equal(t, "Updated Title", fetched.Title, "title (whitelisted) should still update")
}

func TestGormDB_UpdatePRStatus(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a PR
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Update status to generating
	err = db.UpdatePRStatus("owner", "repo", 1, "generating")
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "generating", fetched.Status)
	assert.NotNil(t, fetched.GeneratingSince)

	// Update status to completed
	err = db.UpdatePRStatus("owner", "repo", 1, "completed")
	require.NoError(t, err)

	fetched, err = db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "completed", fetched.Status)

	// Update status to error
	err = db.UpdatePRStatus("owner", "repo", 1, "error")
	require.NoError(t, err)

	fetched, err = db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "error", fetched.Status)
	assert.NotNil(t, fetched.LastReviewedAt)
}

func TestGormDB_ResetPRToOutdated(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a completed PR
	now := time.Now().UTC()
	pr := &PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "abc123",
		Status:         "completed",
		ReviewHTMLPath: "/path/to/review.html",
		LastReviewedAt: &now,
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Reset to outdated
	err = db.ResetPRToOutdated("owner", "repo", 1, "newsha456")
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "pending", fetched.Status)
	assert.Equal(t, "newsha456", fetched.LastCommitSHA)
	assert.Empty(t, fetched.ReviewHTMLPath)
	assert.Nil(t, fetched.LastReviewedAt)
	assert.Nil(t, fetched.GeneratingSince)
}

func TestGormDB_SetPRGenerating(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)

	err := db.SetPRGenerating("owner", "repo", 1, "abc123", "Test PR", "author", &now, true)
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "generating", fetched.Status)
	assert.Equal(t, "abc123", fetched.LastCommitSHA)
	assert.Equal(t, "Test PR", fetched.Title)
	assert.Equal(t, "author", fetched.Author)
	assert.True(t, fetched.Draft)
	assert.NotNil(t, fetched.GeneratingSince)
}

func TestGormDB_GetAllPRs(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert multiple PRs
	for i := 1; i <= 3; i++ {
		pr := &PR{
			RepoOwner:     "owner",
			RepoName:      "repo",
			PRNumber:      i,
			LastCommitSHA: "sha",
			Status:        "pending",
		}
		err := db.UpsertPR(pr)
		require.NoError(t, err)
	}

	prs, err := db.GetAllPRs()
	require.NoError(t, err)
	assert.Len(t, prs, 3)
}

func TestGormDB_DeletePR(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a PR
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Delete it
	err = db.DeletePR("owner", "repo", 1)
	require.NoError(t, err)

	// Verify it's gone
	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Nil(t, fetched)
}

func TestGormDB_ResetStaleGeneratingPRs(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a PR that's been generating for a while
	oldTime := time.Now().UTC().Add(-10 * time.Minute)
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "generating",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Manually set generating_since to old time
	db.db.Model(&PRModel{}).Where("pr_number = ?", 1).Update("generating_since", oldTime)

	// Reset stale PRs (timeout of 5 minutes)
	count, err := db.ResetStaleGeneratingPRs(5)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "pending", fetched.Status)
}

func TestGormDB_ResetErrorPRs(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert an error PR
	oldTime := time.Now().UTC().Add(-10 * time.Minute)
	pr := &PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "abc123",
		Status:         "error",
		LastReviewedAt: &oldTime,
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Reset error PRs older than 5 minutes; allow up to 1 auto-retry.
	count, err := db.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "pending", fetched.Status)
}

// Regression: once a PR has hit the auto-retry cap, ResetErrorPRs must
// stop resetting it back to pending so deterministic failures don't burn
// quota in a 5-minute loop. SetPRGenerating (manual trigger) re-arms the
// counter.
func TestGormDB_ResetErrorPRs_RespectsRetryCap(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert an error PR old enough to be eligible for retry.
	oldTime := time.Now().UTC().Add(-10 * time.Minute)
	require.NoError(t, db.UpsertPR(&PR{
		RepoOwner: "o", RepoName: "r", PRNumber: 1,
		LastCommitSHA: "sha", Status: "error", LastReviewedAt: &oldTime,
	}))

	// First auto-retry succeeds and increments the counter.
	count, err := db.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "first auto-retry should fire")

	// Put it back in error state with an old timestamp (simulating the
	// retried run failing again). Raw SQL avoids depending on a setter
	// that might also touch error_retry_count.
	require.NoError(t, db.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ?", "o", "r").
		Updates(map[string]interface{}{"status": "error", "last_reviewed_at": oldTime}).Error)

	// Second auto-retry should be blocked by the cap.
	count, err = db.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "second auto-retry should be blocked by cap")

	fetched, err := db.GetPR("o", "r", 1)
	require.NoError(t, err)
	assert.Equal(t, "error", fetched.Status)

	// Manual trigger (SetPRGenerating) re-arms the counter and the next
	// auto-retry can fire.
	require.NoError(t, db.SetPRGenerating("o", "r", 1, "newsha", "T", "A", nil, false))
	require.NoError(t, db.db.Model(&PRModel{}).
		Where("repo_owner = ? AND repo_name = ?", "o", "r").
		Updates(map[string]interface{}{"status": "error", "last_reviewed_at": oldTime}).Error)
	count, err = db.ResetErrorPRs(5, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "manual trigger should re-arm the retry counter")
}

func TestGormDB_GetPRsWithMissingMetadata(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert PRs with and without metadata
	prWithMeta := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "sha1",
		Status:        "pending",
		Title:         "Has Title",
		Author:        "author",
	}
	err := db.UpsertPR(prWithMeta)
	require.NoError(t, err)

	prWithoutMeta := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "sha2",
		Status:        "pending",
		Title:         "",
		Author:        "",
	}
	err = db.UpsertPR(prWithoutMeta)
	require.NoError(t, err)

	missing, err := db.GetPRsWithMissingMetadata()
	require.NoError(t, err)
	assert.Len(t, missing, 1)
	assert.Equal(t, 2, missing[0].PRNumber)
}

func TestGormDB_UpdatePRMetadata(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a PR without metadata
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Update metadata
	err = db.UpdatePRMetadata("owner", "repo", 1, "New Title", "new_author")
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, "New Title", fetched.Title)
	assert.Equal(t, "new_author", fetched.Author)
}

func TestGormDB_GetPRsWithMissingCreatedAt(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert PR without created_at using raw SQL to ensure NULL
	err := db.db.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, status, created_at)
		VALUES (?, ?, ?, ?, ?, NULL)
	`, "owner", "repo", 1, "abc123", "pending").Error
	require.NoError(t, err)

	// Insert PR with created_at
	now := time.Now().UTC()
	prWithCreatedAt := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "def456",
		Status:        "pending",
		CreatedAt:     &now,
	}
	err = db.UpsertPR(prWithCreatedAt)
	require.NoError(t, err)

	missing, err := db.GetPRsWithMissingCreatedAt()
	require.NoError(t, err)
	assert.Len(t, missing, 1)
	if len(missing) > 0 {
		assert.Equal(t, 1, missing[0].PRNumber)
	}
}

func TestGormDB_UpdatePRCreatedAt(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a PR without created_at
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Update created_at
	createdAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	err = db.UpdatePRCreatedAt("owner", "repo", 1, createdAt)
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.NotNil(t, fetched.CreatedAt)
	assert.Equal(t, createdAt.Unix(), fetched.CreatedAt.Unix())
}

// =============================================================================
// Settings Operations Tests
// =============================================================================

func TestGormDB_Settings_DefaultValues(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Verify defaults are set
	autoReview, err := db.GetAutoReviewRequestedPRs()
	require.NoError(t, err)
	assert.False(t, autoReview)

	reviewN, err := db.GetReviewNRequests()
	require.NoError(t, err)
	assert.Equal(t, 3, reviewN)

	generateHTML, err := db.GetGenerateHTML()
	require.NoError(t, err)
	assert.True(t, generateHTML)
}

func TestGormDB_GetSetSetting(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Set a new setting
	err := db.SetSetting("test_key", "test_value")
	require.NoError(t, err)

	// Get it back
	value, err := db.GetSetting("test_key")
	require.NoError(t, err)
	assert.Equal(t, "test_value", value)

	// Update it
	err = db.SetSetting("test_key", "updated_value")
	require.NoError(t, err)

	value, err = db.GetSetting("test_key")
	require.NoError(t, err)
	assert.Equal(t, "updated_value", value)
}

func TestGormDB_GetSetting_NotFound(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	value, err := db.GetSetting("nonexistent_key")
	require.NoError(t, err)
	assert.Empty(t, value)
}

func TestGormDB_AutoReviewRequestedPRs(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Disable
	err := db.SetAutoReviewRequestedPRs(false)
	require.NoError(t, err)

	enabled, err := db.GetAutoReviewRequestedPRs()
	require.NoError(t, err)
	assert.False(t, enabled)

	// Re-enable
	err = db.SetAutoReviewRequestedPRs(true)
	require.NoError(t, err)

	enabled, err = db.GetAutoReviewRequestedPRs()
	require.NoError(t, err)
	assert.True(t, enabled)
}

func TestGormDB_ReviewNRequests(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Update
	err := db.SetReviewNRequests(5)
	require.NoError(t, err)

	n, err := db.GetReviewNRequests()
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

func TestGormDB_GenerateHTML(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Disable
	err := db.SetGenerateHTML(false)
	require.NoError(t, err)

	enabled, err := db.GetGenerateHTML()
	require.NoError(t, err)
	assert.False(t, enabled)
}

// =============================================================================
// User Operations Tests
// =============================================================================

func TestGormDB_CreateUser(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user := &User{
		GitHubID:        12345,
		GitHubUsername:  "testuser",
		GitHubAvatarURL: "https://example.com/avatar.png",
	}

	err := db.CreateUser(user)
	require.NoError(t, err)
	assert.NotZero(t, user.ID)
	assert.False(t, user.CreatedAt.IsZero())
}

func TestGormDB_GetUserByGitHubID(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create a user
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	// Fetch by GitHub ID
	fetched, err := db.GetUserByGitHubID(12345)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, "testuser", fetched.GitHubUsername)

	// Non-existent user
	notFound, err := db.GetUserByGitHubID(99999)
	require.NoError(t, err)
	assert.Nil(t, notFound)
}

func TestGormDB_GetUserByID(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create a user
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	// Fetch by internal ID
	fetched, err := db.GetUserByID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, int64(12345), fetched.GitHubID)
}

func TestGormDB_GetAllUsers(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Empty DB returns empty slice
	users, err := db.GetAllUsers()
	require.NoError(t, err)
	assert.Empty(t, users)

	// Insert 3 users
	for i, name := range []string{"alice", "bob", "charlie"} {
		err := db.CreateUser(&User{
			GitHubID:       int64(1000 + i),
			GitHubUsername: name,
		})
		require.NoError(t, err)
	}

	// All 3 returned
	users, err = db.GetAllUsers()
	require.NoError(t, err)
	assert.Len(t, users, 3)

	// Verify usernames present
	names := make(map[string]bool)
	for _, u := range users {
		names[u.GitHubUsername] = true
	}
	assert.True(t, names["alice"])
	assert.True(t, names["bob"])
	assert.True(t, names["charlie"])
}

func TestGormDB_UpdateUserLastLogin(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create a user
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)
	assert.Nil(t, user.LastLoginAt)

	// Update last login
	err = db.UpdateUserLastLogin(user.ID)
	require.NoError(t, err)

	// Verify
	fetched, err := db.GetUserByID(user.ID)
	require.NoError(t, err)
	assert.NotNil(t, fetched.LastLoginAt)
}

// =============================================================================
// Session Operations Tests
// =============================================================================

func TestGormDB_CreateSession(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create a user first
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	// Create session
	session := &Session{
		ID:        "test-session-id-123",
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}

	err = db.CreateSession(session)
	require.NoError(t, err)
	assert.False(t, session.CreatedAt.IsZero())
}

func TestGormDB_GetSession(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and session
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	session := &Session{
		ID:        "test-session-id",
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	err = db.CreateSession(session)
	require.NoError(t, err)

	// Fetch session
	fetched, err := db.GetSession("test-session-id")
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, user.ID, fetched.UserID)

	// Non-existent session
	notFound, err := db.GetSession("nonexistent")
	require.NoError(t, err)
	assert.Nil(t, notFound)
}

func TestGormDB_DeleteSession(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and session
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	session := &Session{
		ID:        "test-session-id",
		UserID:    user.ID,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	err = db.CreateSession(session)
	require.NoError(t, err)

	// Delete it
	err = db.DeleteSession("test-session-id")
	require.NoError(t, err)

	// Verify it's gone
	fetched, err := db.GetSession("test-session-id")
	require.NoError(t, err)
	assert.Nil(t, fetched)
}

// =============================================================================
// User-PR View Operations Tests
// =============================================================================

func TestGormDB_GetUserPRAssignment_NotFound(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	assignment, err := db.GetUserPRAssignment(1, 1)
	require.NoError(t, err)
	assert.Nil(t, assignment)
}

func TestGormDB_UpsertUserPRAssignment(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and PR
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)

	// Get the PR ID
	fetchedPR, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)

	// Create assignment
	assignment := &UserPRAssignment{
		UserID:         user.ID,
		PRID:           fetchedPR.ID,
		IsAuthor:       true,
		IsReviewer:     false,
		ReviewerGroups: `["team1"]`,
		MyReviewStatus: "APPROVED",
		Notes:          "test notes",
	}

	err = db.UpsertUserPRAssignment(assignment)
	require.NoError(t, err)

	// Fetch it back
	fetched, err := db.GetUserPRAssignment(user.ID, fetchedPR.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.True(t, fetched.IsAuthor)
	assert.Equal(t, "APPROVED", fetched.MyReviewStatus)
	assert.Equal(t, "test notes", fetched.Notes)
}

// Regression test: UpsertUserPRAssignment must NOT overwrite existing via_teams on conflict.
// via_teams is only written by UpdateUserViaTeams (guarded by shouldUpdateViaTeams).
func TestGormDB_UpsertUserPRAssignment_PreservesViaTeams(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user := &User{GitHubID: 12345, GitHubUsername: "testuser"}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123", Status: "pending",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)
	fetchedPR, _ := db.GetPR("owner", "repo", 1)

	// Step 1: Create the initial user_pr_view record
	err = db.UpsertUserPRAssignment(&UserPRAssignment{
		UserID: user.ID, PRID: fetchedPR.ID, IsReviewer: true,
	})
	require.NoError(t, err)

	// Step 2: Set via_teams through the dedicated function (the only sanctioned writer)
	err = db.UpdateUserViaTeams(user.ID, fetchedPR.ID, []string{"frontend-team:approved", "backend-team:pending"})
	require.NoError(t, err)

	// Step 3: Upsert again with empty ReviewerGroups (simulates poll updating review_status)
	err = db.UpsertUserPRAssignment(&UserPRAssignment{
		UserID: user.ID, PRID: fetchedPR.ID, IsReviewer: true,
		MyReviewStatus: "APPROVED",
		ReviewerGroups: "", // empty — must NOT wipe via_teams
	})
	require.NoError(t, err)

	// Verify via_teams was preserved
	fetched, err := db.GetUserPRAssignment(user.ID, fetchedPR.ID)
	require.NoError(t, err)
	assert.Equal(t, "APPROVED", fetched.MyReviewStatus, "review_status should be updated")
	assert.Contains(t, fetched.ReviewerGroups, "frontend-team:approved",
		"via_teams must be preserved when UpsertUserPRAssignment is called with empty ReviewerGroups")
	assert.Contains(t, fetched.ReviewerGroups, "backend-team:pending",
		"via_teams must be preserved when UpsertUserPRAssignment is called with empty ReviewerGroups")
}

func TestGormDB_GetPRsForUser(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	// Create PRs
	pr1 := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "sha1",
		Status:        "pending",
		Title:         "PR 1",
	}
	err = db.UpsertPR(pr1)
	require.NoError(t, err)

	pr2 := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      2,
		LastCommitSHA: "sha2",
		Status:        "completed",
		Title:         "PR 2",
	}
	err = db.UpsertPR(pr2)
	require.NoError(t, err)

	// Get PR IDs
	fetchedPR1, _ := db.GetPR("owner", "repo", 1)
	fetchedPR2, _ := db.GetPR("owner", "repo", 2)

	// Create assignments
	assignment1 := &UserPRAssignment{
		UserID:     user.ID,
		PRID:       fetchedPR1.ID,
		IsAuthor:   false,
		IsReviewer: true,
		Notes:      "review this",
	}
	err = db.UpsertUserPRAssignment(assignment1)
	require.NoError(t, err)

	assignment2 := &UserPRAssignment{
		UserID:   user.ID,
		PRID:     fetchedPR2.ID,
		IsAuthor: true,
	}
	err = db.UpsertUserPRAssignment(assignment2)
	require.NoError(t, err)

	// Get PRs for user
	prs, err := db.GetPRsForUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, prs, 2)

	// Verify PRs are returned (IsMine and Notes are now in UserPRView, not PR)
	prNumbers := make(map[int]bool)
	for _, pr := range prs {
		prNumbers[pr.PRNumber] = true
	}
	assert.True(t, prNumbers[1])
	assert.True(t, prNumbers[2])
}

func TestGormDB_UpdateUserPRNotes(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and PR
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)

	fetchedPR, _ := db.GetPR("owner", "repo", 1)

	// Create assignment
	assignment := &UserPRAssignment{
		UserID:     user.ID,
		PRID:       fetchedPR.ID,
		IsReviewer: true,
		Notes:      "initial",
	}
	err = db.UpsertUserPRAssignment(assignment)
	require.NoError(t, err)

	// Update notes
	err = db.UpdateUserPRNotes(user.ID, fetchedPR.ID, "updated notes")
	require.NoError(t, err)

	// Verify
	fetched, err := db.GetUserPRAssignment(user.ID, fetchedPR.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated notes", fetched.Notes)
}

func TestGormDB_UpdateUserPRNotes_Truncation(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and PR
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)

	fetchedPR, _ := db.GetPR("owner", "repo", 1)

	assignment := &UserPRAssignment{
		UserID:     user.ID,
		PRID:       fetchedPR.ID,
		IsReviewer: true,
	}
	err = db.UpsertUserPRAssignment(assignment)
	require.NoError(t, err)

	// Try to set notes longer than 15 chars
	err = db.UpdateUserPRNotes(user.ID, fetchedPR.ID, "this is way too long for notes")
	require.NoError(t, err)

	// Verify truncation
	fetched, err := db.GetUserPRAssignment(user.ID, fetchedPR.ID)
	require.NoError(t, err)
	assert.Len(t, fetched.Notes, 15)
	assert.Equal(t, "this is way too", fetched.Notes)
}

func TestGormDB_EnsureUserPRView_UnhidesHiddenPR(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create user and PR
	user := &User{
		GitHubID:       12345,
		GitHubUsername: "testuser",
	}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "completed",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)

	fetchedPR, _ := db.GetPR("owner", "repo", 1)

	// Create initial view
	err = db.EnsureUserPRView(user.ID, fetchedPR.ID, true)
	require.NoError(t, err)

	// Verify PR is visible
	prs, err := db.GetPRsForUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, prs, 1)

	// Hide the PR
	err = db.HidePRForUser(user.ID, fetchedPR.ID)
	require.NoError(t, err)

	// Verify PR is now hidden
	prs, err = db.GetPRsForUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, prs, 0)

	// Call EnsureUserPRView again (simulating poller finding the PR again)
	err = db.EnsureUserPRView(user.ID, fetchedPR.ID, true)
	require.NoError(t, err)

	// Verify PR is visible again (un-hidden)
	prs, err = db.GetPRsForUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, prs, 1, "PR should be un-hidden after EnsureUserPRView")
}

// Regression test: EnsureUserPRView must NOT touch via_teams on conflict.
// via_teams is only written by UpdateUserViaTeams (guarded by shouldUpdateViaTeams).
func TestGormDB_EnsureUserPRView_PreservesViaTeams(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user := &User{GitHubID: 12345, GitHubUsername: "testuser"}
	err := db.CreateUser(user)
	require.NoError(t, err)

	pr := &PR{
		RepoOwner: "owner", RepoName: "repo", PRNumber: 1,
		LastCommitSHA: "abc123", Status: "completed",
	}
	err = db.UpsertPR(pr)
	require.NoError(t, err)
	fetchedPR, _ := db.GetPR("owner", "repo", 1)

	// Step 1: Create user_pr_view via EnsureUserPRView
	err = db.EnsureUserPRView(user.ID, fetchedPR.ID, false)
	require.NoError(t, err)

	// Step 2: Set via_teams through the dedicated function
	err = db.UpdateUserViaTeams(user.ID, fetchedPR.ID, []string{"team-alpha:approved", "team-beta:pending"})
	require.NoError(t, err)

	// Step 3: Call EnsureUserPRView again (simulates next poll cycle)
	err = db.EnsureUserPRView(user.ID, fetchedPR.ID, false)
	require.NoError(t, err)

	// Verify via_teams was NOT wiped
	prsWithViews, err := db.GetPRsForUserWithNotes(user.ID)
	require.NoError(t, err)
	require.Len(t, prsWithViews, 1)
	assert.Contains(t, prsWithViews[0].ViaTeams, "team-alpha:approved",
		"via_teams must survive EnsureUserPRView calls")
	assert.Contains(t, prsWithViews[0].ViaTeams, "team-beta:pending",
		"via_teams must survive EnsureUserPRView calls")
}

// =============================================================================
// Edge Cases and Integration Tests
// =============================================================================

// Regression: BatchUpsertUserPRViews must drop items whose pr_id has been
// deleted between the poller's dbPRMap snapshot and the views flush. Without
// the pre-filter, the FK violation aborts the entire batch, losing all
// in-flight writes. With the pre-filter, only the orphaned items are
// dropped; the rest succeed and the warning lands in the log.
func TestGormDB_BatchUpsertUserPRViews_DropsOrphans(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user := &User{GitHubID: 12345, GitHubUsername: "u"}
	require.NoError(t, db.CreateUser(user))

	// Two PRs exist; we'll delete one to simulate the race.
	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "o", RepoName: "r", PRNumber: 1, Status: "completed"}))
	require.NoError(t, db.UpsertPR(&PR{RepoOwner: "o", RepoName: "r", PRNumber: 2, Status: "completed"}))
	pr1, _ := db.GetPR("o", "r", 1)
	pr2, _ := db.GetPR("o", "r", 2)
	deletedID := pr2.ID + 9999 // an id that will never exist

	require.NoError(t, db.DeletePR("o", "r", 2))

	// Mix three items: one valid, one for a never-existed PR, one for a
	// just-deleted PR. Only the valid one should be written.
	err := db.BatchUpsertUserPRViews([]UserPRViewBatchItem{
		{UserID: user.ID, PRID: pr1.ID, IsAuthor: true},
		{UserID: user.ID, PRID: deletedID, IsAuthor: false},
		{UserID: user.ID, PRID: pr2.ID, IsAuthor: false},
	})
	require.NoError(t, err, "batch must not fail despite orphan items")

	// Verify the valid item landed.
	view, err := db.GetUserPRAssignment(user.ID, pr1.ID)
	require.NoError(t, err)
	require.NotNil(t, view)
	assert.True(t, view.IsAuthor)
}

func TestGormDB_CIFailedChecks_JSONHandling(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	pr := &PR{
		RepoOwner:      "owner",
		RepoName:       "repo",
		PRNumber:       1,
		LastCommitSHA:  "abc123",
		Status:         "pending",
		CIState:        "failure",
		CIFailedChecks: `["lint", "test", "build"]`,
	}

	err := db.UpsertPR(pr)
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Equal(t, `["lint","test","build"]`, fetched.CIFailedChecks)
}

func TestGormDB_NullTimestamps(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Use raw SQL to insert PR with NULL timestamps
	// (GORM's Create may auto-populate certain fields)
	err := db.db.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, status, created_at, last_reviewed_at, generating_since)
		VALUES (?, ?, ?, ?, ?, NULL, NULL, NULL)
	`, "owner", "repo", 1, "abc123", "pending").Error
	require.NoError(t, err)

	fetched, err := db.GetPR("owner", "repo", 1)
	require.NoError(t, err)
	assert.Nil(t, fetched.CreatedAt)
	assert.Nil(t, fetched.LastReviewedAt)
	assert.Nil(t, fetched.GeneratingSince)
}

func TestGormDB_DatabaseLifecycle(t *testing.T) {
	db := newTestDB(t)

	// Verify tables exist by inserting data
	pr := &PR{
		RepoOwner:     "owner",
		RepoName:      "repo",
		PRNumber:      1,
		LastCommitSHA: "abc123",
		Status:        "pending",
	}
	err := db.UpsertPR(pr)
	require.NoError(t, err)

	// Close and verify
	err = db.Close()
	require.NoError(t, err)
}

// =============================================================================
// Telemetry Operations Tests
// =============================================================================

func TestGormDB_CreateTelemetryEvents_AndGetTelemetryStats(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user1 := &User{GitHubID: 111, GitHubUsername: "user-one"}
	user2 := &User{GitHubID: 222, GitHubUsername: "user-two"}
	require.NoError(t, db.CreateUser(user1))
	require.NoError(t, db.CreateUser(user2))

	events := []TelemetryEvent{
		{UserID: user1.ID, Action: "search", Label: "bugfix"},
		{UserID: user1.ID, Action: "view_review", PROwner: "owner", PRRepo: "repo", PRNumber: 11},
		{UserID: user2.ID, Action: "view_review", PROwner: "owner", PRRepo: "repo", PRNumber: 11},
		{UserID: user2.ID, Action: "filter_repo", Label: "owner/repo"},
	}

	require.NoError(t, db.CreateTelemetryEvents(events))

	stats, err := db.GetTelemetryStats(7)
	require.NoError(t, err)

	assert.Equal(t, 4, stats.TotalEvents)
	assert.Equal(t, 2, stats.ActiveUsers)
	require.NotEmpty(t, stats.ByAction)
	assert.Equal(t, "view_review", stats.ByAction[0].Action)
	assert.Equal(t, 2, stats.ByAction[0].Count)

	require.Len(t, stats.TopSearches, 1)
	assert.Equal(t, "bugfix", stats.TopSearches[0].Label)
	assert.Equal(t, 1, stats.TopSearches[0].Count)

	require.Len(t, stats.TopPRs, 1)
	assert.Equal(t, "owner", stats.TopPRs[0].Owner)
	assert.Equal(t, "repo", stats.TopPRs[0].Repo)
	assert.Equal(t, 11, stats.TopPRs[0].Number)
	assert.Equal(t, 2, stats.TopPRs[0].Count)
}

func TestGormDB_CreateTelemetryEvents_StoresLongLabels(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	user := &User{GitHubID: 333, GitHubUsername: "telemetry-user"}
	require.NoError(t, db.CreateUser(user))

	longLabel := strings.Repeat("x", 255)
	require.NoError(t, db.CreateTelemetryEvents([]TelemetryEvent{
		{UserID: user.ID, Action: "search", Label: longLabel},
	}))

	stats, err := db.GetTelemetryStats(7)
	require.NoError(t, err)
	require.Len(t, stats.TopSearches, 1)
	assert.Equal(t, longLabel, stats.TopSearches[0].Label)
}

func TestGormDB_GetUserByUsername_PrefersMostRecentlyActiveDuplicate(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	older := &User{
		GitHubID:       111,
		GitHubUsername: "duplicate-user",
	}
	require.NoError(t, db.CreateUser(older))

	newer := &User{
		GitHubID:       222,
		GitHubUsername: "duplicate-user",
	}
	require.NoError(t, db.CreateUser(newer))
	require.NoError(t, db.UpdateUserLastLogin(newer.ID))

	user, err := db.GetUserByUsername("duplicate-user")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, newer.ID, user.ID)
	assert.Equal(t, int64(222), user.GitHubID)
}
