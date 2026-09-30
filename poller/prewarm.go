package poller

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/reviewer/service"
)

// Without AGENT_PREWARM_REPOS, startup warms the prewarmTopRepos repos with
// the most runs among the latest prewarmRecentRuns review runs.
const (
	prewarmTopRepos   = 5
	prewarmRecentRuns = 1000
)

// prewarmCloneCaches builds clone caches in the background at startup. A
// cache that cannot be built is left to the first review, as before.
func (p *Poller) prewarmCloneCaches(ctx context.Context) {
	if !p.cfg.AgenticReviews || p.ghClientConcrete == nil {
		return
	}
	for _, full := range p.prewarmRepos() {
		owner, repo, ok := strings.Cut(full, "/")
		if !ok || owner == "" || repo == "" {
			log.Printf("[PREWARM] skipping %q: want owner/repo", full)
			continue
		}
		token, err := p.ghClientConcrete.CurrentToken(ctx)
		if err != nil {
			log.Printf("[PREWARM] no GitHub token, stopping: %v", err)
			return
		}
		start := time.Now()
		if err := service.WarmAgentCache(ctx, p.cfg.AgentCloneRootDir, owner, repo, token); err != nil {
			log.Printf("[PREWARM] %s failed after %s: %v", full, time.Since(start), err)
			continue
		}
		log.Printf("[PREWARM] %s ready in %s", full, time.Since(start))
	}
}

func (p *Poller) prewarmRepos() []string {
	if len(p.cfg.AgentPrewarmRepos) > 0 {
		return p.cfg.AgentPrewarmRepos
	}
	runs, err := p.db.ListReviewRuns(db.ReviewRunFilter{Limit: prewarmRecentRuns})
	if err != nil {
		log.Printf("[PREWARM] cannot list recent runs: %v", err)
		return nil
	}
	counts := map[string]int{}
	for _, r := range runs {
		counts[r.RepoOwner+"/"+r.RepoName]++
	}
	repos := make([]string, 0, len(counts))
	for repo := range counts {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(a, b int) bool {
		if counts[repos[a]] != counts[repos[b]] {
			return counts[repos[a]] > counts[repos[b]]
		}
		return repos[a] < repos[b]
	})
	if len(repos) > prewarmTopRepos {
		repos = repos[:prewarmTopRepos]
	}
	return repos
}
