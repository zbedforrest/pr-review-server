package poller

import (
	"reflect"
	"testing"

	"pr-review-server/db"
)

func TestPrewarmReposPrefersConfigThenTheMostReviewedRepos(t *testing.T) {
	database := NewMockDatabase()
	p := newTestPoller(NewMockGitHubClient(), database)
	add := func(id, owner, repo string) {
		database.ReviewRuns[id] = &db.ReviewRun{RunID: id, RepoOwner: owner, RepoName: repo}
	}
	for i, r := range []string{"a", "a", "a", "b", "b", "c", "d", "e", "f", "f"} {
		add("run-"+string(rune('0'+i)), "acme", r)
	}
	if got, want := p.prewarmRepos(), []string{"acme/a", "acme/b", "acme/f", "acme/c", "acme/d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prewarmRepos() = %v, want %v", got, want)
	}
	p.cfg.AgentPrewarmRepos = []string{"acme/monorepo"}
	if got := p.prewarmRepos(); !reflect.DeepEqual(got, []string{"acme/monorepo"}) {
		t.Fatalf("configured repos must win, got %v", got)
	}
}
