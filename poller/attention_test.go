package poller

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"pr-review-server/db"
	"pr-review-server/github"
)

const attentionHead = "abcdef1234567890abcdef1234567890abcdef12"

type attentionFixture struct {
	mockGH     *MockGitHubClient
	mockDB     *MockDatabase
	poller     *Poller
	userEvents []int
}

// newAttentionFixture wires one open PR (acme/example#7, DB ID 10) authored by
// "author" (ID 3) with dashboard users alice (ID 1) and bob (ID 2) in org mode.
func newAttentionFixture(reviewData *github.PRReviewData) *attentionFixture {
	mockGH := NewMockGitHubClient()
	mockDB := NewMockDatabase()
	mockDB.AutoReviewEnabled = false
	mockDB.Users = []db.User{
		{ID: 1, GitHubUsername: "alice"},
		{ID: 2, GitHubUsername: "bob"},
		{ID: 3, GitHubUsername: "author"},
	}
	mockDB.PRs["acme/example/7"] = &db.PR{
		ID: 10, RepoOwner: "acme", RepoName: "example", PRNumber: 7,
		LastCommitSHA: attentionHead, Status: "completed", Author: "author",
	}
	reviewData.Owner, reviewData.Repo, reviewData.Number = "acme", "example", 7
	reviewData.HeadOID = attentionHead
	mockGH.BatchGetPRReviewDataResults["acme/example/7"] = reviewData

	f := &attentionFixture{mockGH: mockGH, mockDB: mockDB}
	f.poller = newTestPollerFull(mockGH, mockDB, NewMockReviewStorage(), NewMockReviewGenerator())
	f.poller.cfg.GitHubOrgName = "acme"
	f.poller.EventFunc = func(eventType string, payload interface{}) {}
	f.poller.UserEventFunc = func(userID int, eventType string, payload interface{}) {
		if eventType == "pr_updated" {
			f.userEvents = append(f.userEvents, userID)
		}
	}
	return f
}

func (f *attentionFixture) seedView(userID int, needsAttention bool, status string) {
	f.mockDB.UserPRViews[viewMockKey(userID, 10)] = &db.UserPRView{
		UserID: userID, PRID: 10, ViaTeams: "[]", ReviewStatus: status, NeedsAttention: needsAttention,
	}
}

func (f *attentionFixture) pollCapturingLog() string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f.poller.poll(context.Background())
	return buf.String()
}

func (f *attentionFixture) view(userID int) *db.UserPRView {
	return f.mockDB.UserPRViews[viewMockKey(userID, 10)]
}

func changesRequestedBy(logins ...string) *github.PRReviewData {
	data := &github.PRReviewData{UserReviews: map[string]string{}, AttentionByUser: map[string]bool{}}
	for _, login := range logins {
		data.UserReviews[login] = "CHANGES_REQUESTED"
		data.AttentionByUser[login] = true
	}
	return data
}

func TestPoll_Attention_FlagsReviewerFromReducer(t *testing.T) {
	f := newAttentionFixture(changesRequestedBy("Alice"))

	f.pollCapturingLog()

	view := f.view(1)
	if view == nil || !view.NeedsAttention {
		t.Fatalf("expected alice's view flagged, got %+v", view)
	}
	if view.ReviewStatus != "CHANGES_REQUESTED" {
		t.Errorf("review status should still sync alongside attention, got %q", view.ReviewStatus)
	}
	if bobView := f.view(2); bobView != nil {
		t.Errorf("bob never reviewed and must not get a row, got %+v", bobView)
	}
}

func TestPoll_Attention_AuthorNeverFlagged(t *testing.T) {
	f := newAttentionFixture(changesRequestedBy("author"))
	f.seedView(3, true, "CHANGES_REQUESTED")

	f.pollCapturingLog()

	view := f.view(3)
	if view == nil || view.NeedsAttention {
		t.Fatalf("author's own row must be forced to false, got %+v", view)
	}
}

func TestPoll_Attention_DraftAndClosedForceFalse(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *attentionFixture)
	}{
		{"draft", func(f *attentionFixture) { f.mockGH.BatchGetPRReviewDataResults["acme/example/7"].IsDraft = true }},
		{"closed", func(f *attentionFixture) { f.mockDB.PRs["acme/example/7"].PRState = "closed" }},
		{"merged", func(f *attentionFixture) { f.mockDB.PRs["acme/example/7"].PRState = "merged" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttentionFixture(changesRequestedBy("alice"))
			tc.setup(f)
			f.seedView(1, true, "CHANGES_REQUESTED")

			f.pollCapturingLog()

			if view := f.view(1); view == nil || view.NeedsAttention {
				t.Fatalf("expected flag cleared for %s PR, got %+v", tc.name, view)
			}
		})
	}
}

func TestPoll_Attention_DraftComesFromFetchedReviewDataNotStaleRow(t *testing.T) {
	t.Run("converted to draft", func(t *testing.T) {
		f := newAttentionFixture(changesRequestedBy("alice"))
		f.mockDB.PRs["acme/example/7"].Draft = false
		f.mockGH.BatchGetPRReviewDataResults["acme/example/7"].IsDraft = true
		f.seedView(1, true, "CHANGES_REQUESTED")

		f.pollCapturingLog()

		if view := f.view(1); view == nil || view.NeedsAttention {
			t.Fatalf("draft flip fetched this cycle must clear the flag, got %+v", view)
		}
	})
	t.Run("marked ready", func(t *testing.T) {
		f := newAttentionFixture(changesRequestedBy("alice"))
		f.mockDB.PRs["acme/example/7"].Draft = true
		f.mockGH.BatchGetPRReviewDataResults["acme/example/7"].IsDraft = false
		f.seedView(1, false, "CHANGES_REQUESTED")

		f.pollCapturingLog()

		if view := f.view(1); view == nil || !view.NeedsAttention {
			t.Fatalf("ready flip fetched this cycle must honour the reducer, got %+v", view)
		}
	})
}

func TestPoll_Attention_AbsentFromReducerLeavesExistingValue(t *testing.T) {
	f := newAttentionFixture(&github.PRReviewData{
		UserReviews: map[string]string{"alice": "APPROVED"},
	})
	f.seedView(1, true, "CHANGES_REQUESTED")

	f.pollCapturingLog()

	view := f.view(1)
	if view == nil || !view.NeedsAttention {
		t.Fatalf("reducer had no verdict, stored flag must survive, got %+v", view)
	}
	if view.ReviewStatus != "APPROVED" {
		t.Errorf("review status must still update, got %q", view.ReviewStatus)
	}
}

func TestPoll_Attention_TransitionLogsAndBroadcastsOnce(t *testing.T) {
	f := newAttentionFixture(changesRequestedBy("alice"))

	logs := f.pollCapturingLog()

	want := "[ATTENTION] user=alice pr=acme/example#7 flagged head=abcdef1"
	if !strings.Contains(logs, want) {
		t.Errorf("expected log line %q in:\n%s", want, logs)
	}
	if len(f.userEvents) != 1 || f.userEvents[0] != 1 {
		t.Fatalf("expected exactly one pr_updated for alice, got %v", f.userEvents)
	}
}

func TestPoll_Attention_ClearTransitionLogsCleared(t *testing.T) {
	f := newAttentionFixture(&github.PRReviewData{
		UserReviews:     map[string]string{"alice": "CHANGES_REQUESTED"},
		AttentionByUser: map[string]bool{"alice": false},
	})
	f.seedView(1, true, "CHANGES_REQUESTED")

	logs := f.pollCapturingLog()

	want := "[ATTENTION] user=alice pr=acme/example#7 cleared head=abcdef1"
	if !strings.Contains(logs, want) {
		t.Errorf("expected log line %q in:\n%s", want, logs)
	}
	if len(f.userEvents) != 1 || f.userEvents[0] != 1 {
		t.Fatalf("expected exactly one pr_updated for alice, got %v", f.userEvents)
	}
}

func TestPoll_Attention_NoChangeIsSilent(t *testing.T) {
	f := newAttentionFixture(changesRequestedBy("alice"))
	f.seedView(1, true, "CHANGES_REQUESTED")

	logs := f.pollCapturingLog()

	if strings.Contains(logs, "[ATTENTION]") {
		t.Errorf("unchanged flag must not log a transition:\n%s", logs)
	}
	if len(f.userEvents) != 0 {
		t.Errorf("unchanged flag must not broadcast, got %v", f.userEvents)
	}
	if view := f.view(1); view == nil || !view.NeedsAttention {
		t.Fatalf("flag must stay set, got %+v", view)
	}
}

func TestPoll_Attention_DismissedReviewerRowIsCleared(t *testing.T) {
	f := newAttentionFixture(&github.PRReviewData{
		UserReviews:     map[string]string{},
		AttentionByUser: map[string]bool{"alice": false},
	})
	f.seedView(1, true, "CHANGES_REQUESTED")

	f.pollCapturingLog()

	if view := f.view(1); view == nil || view.NeedsAttention {
		t.Fatalf("a reviewer whose decision was dismissed must be cleared, got %+v", view)
	}
	if bobView := f.view(2); bobView != nil {
		t.Errorf("a user without a row must not gain one from a false verdict, got %+v", bobView)
	}
}

func TestPoll_Attention_SnapshotReadFailureWritesButSuppressesTransitions(t *testing.T) {
	f := newAttentionFixture(changesRequestedBy("alice"))
	f.seedView(1, false, "CHANGES_REQUESTED")
	f.mockDB.GetUserPRViewsForPRsError = errors.New("connection reset")

	logs := f.pollCapturingLog()

	if view := f.view(1); view == nil || !view.NeedsAttention {
		t.Fatalf("verdict must still be written when the snapshot read fails, got %+v", view)
	}
	if strings.Contains(logs, "[ATTENTION]") {
		t.Errorf("no transition may be reported without a trustworthy snapshot:\n%s", logs)
	}
	if len(f.userEvents) != 0 {
		t.Errorf("no per-user broadcast without a trustworthy snapshot, got %v", f.userEvents)
	}
	if strings.Count(logs, "WARNING: Failed to read current attention flags") != 1 {
		t.Errorf("expected exactly one snapshot warning in:\n%s", logs)
	}
}

func TestPoll_Attention_SnapshotReadFailureDoesNotCreateRowsFromVerdicts(t *testing.T) {
	f := newAttentionFixture(&github.PRReviewData{
		UserReviews:     map[string]string{},
		AttentionByUser: map[string]bool{"alice": false, "bob": false},
	})
	f.seedView(1, true, "CHANGES_REQUESTED")
	f.mockDB.GetUserPRViewsForPRsError = errors.New("connection reset")

	f.pollCapturingLog()

	if view := f.view(1); view == nil || !view.NeedsAttention {
		t.Fatalf("a row whose existence is unknown this cycle must be left alone, got %+v", view)
	}
	if bobView := f.view(2); bobView != nil {
		t.Errorf("a verdict alone must not create a row, got %+v", bobView)
	}
}

func TestViewBatch_SetNeedsAttention(t *testing.T) {
	b := newViewBatch()
	b.EnsureView(1, 100, false)
	b.SetNeedsAttention(1, 100, true)
	b.SetReviewStatus(2, 200, "APPROVED")

	byUser := map[int]db.UserPRViewBatchItem{}
	for _, item := range b.Items() {
		byUser[item.UserID] = item
	}
	if byUser[1].NeedsAttention == nil || !*byUser[1].NeedsAttention {
		t.Errorf("expected needs_attention pointer to true, got %v", byUser[1].NeedsAttention)
	}
	if byUser[2].NeedsAttention != nil {
		t.Errorf("untouched entry must keep nil pointer, got %v", *byUser[2].NeedsAttention)
	}
}
