package poller

import (
	"strings"
	"testing"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

func TestMentionCommandRecognisesReviewRequestsAndNothingElse(t *testing.T) {
	handle := "prism-pr-review-server"
	yes := []string{
		"@prism-pr-review-server review",
		"@Prism-PR-Review-Server please review this",
		"can you take another look? @prism-pr-review-server re-review",
		"@prism-pr-review-server rereview",
	}
	for _, body := range yes {
		if !mentionCommand(body, handle) {
			t.Errorf("%q should trigger", body)
		}
	}
	no := []string{
		"@prism-pr-review-server thanks",
		"@prism-pr-review-server what does this finding mean?",
		"please review @someone-else",
		"the review from @prism-pr-review-serverbot was useful",
		"email me at prism-pr-review-server@example.com to review",
		"> @prism-pr-review-server review\n\nquoting what Zed said above",
		"the review from @prism-pr-review-server was useful",
		"@prism-pr-review-server don't review this yet",
		"@prism-pr-review-server why did the review flag X?",
	}
	for _, body := range no {
		if mentionCommand(body, handle) {
			t.Errorf("%q should not trigger", body)
		}
	}
}

func TestMentionCandidatesUseTheCachedRowsThatMoved(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	t1, t2 := now.Add(-2*time.Minute), now.Add(-time.Minute)
	prs := []*db.PR{
		{RepoOwner: "acme", RepoName: "example", PRNumber: 1, PRState: "open", GitHubUpdatedAt: &t2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 2, PRState: "open", GitHubUpdatedAt: &t1},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 3, PRState: "closed", GitHubUpdatedAt: &t2},
		{RepoOwner: "acme", RepoName: "example", PRNumber: 4, PRState: "open"},
	}
	last := map[string]time.Time{"acme/example#1": t1, "acme/example#2": t1}
	got := mentionCandidates(prs, last, false)
	if len(got) != 2 || got[0].PRNumber != 1 || got[1].PRNumber != 4 {
		t.Fatalf("incremental = %+v, want the moved open PR and the one without a cached time", got)
	}
	if got := mentionCandidates(prs, last, true); len(got) != 3 {
		t.Fatalf("full scan = %d open PRs, want 3", len(got))
	}
}

func TestMentionPublishNoteExplainsWhyAReviewStaysOffGitHub(t *testing.T) {
	if note := mentionPublishNote(true, false, "abc1234567"); note != "" {
		t.Fatalf("an eligible PR needs no note, got %q", note)
	}
	note := mentionPublishNote(false, true, "abc1234567")
	for _, want := range []string{"abc1234", "draft", "dashboard"} {
		if !containsFold(note, want) {
			t.Errorf("note %q should mention %q", note, want)
		}
	}
	note = mentionPublishNote(false, false, "abc1234567")
	if !containsFold(note, "comment pilot") {
		t.Errorf("note %q should explain the allowlist", note)
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func TestMentionTelemetryEvent(t *testing.T) {
	ev := mentionTelemetryEvent(github.PullRequest{Owner: "acme", Repo: "example", Number: 7}, "alice", true, 3)
	if ev.Action != "mention_trigger" || ev.Label != "by=alice publish=true" || ev.PRNumber != 7 || ev.UserID != 3 {
		t.Fatalf("event = %+v", ev)
	}
}
