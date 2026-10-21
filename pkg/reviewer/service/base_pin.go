package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BasePin says where a PR's own base stood, from the PR API: the merge
// commit of a merged PR, else the base tip reported for an open one, and the
// PR's commit count for a fast-forward merge whose merge commit is the head.
type BasePin struct {
	MergeCommitSHA string
	BaseSHA        string
	Commits        int
}

func (p BasePin) anchor() string {
	if p.MergeCommitSHA != "" {
		return p.MergeCommitSHA
	}
	return p.BaseSHA
}

const basePinDepth = "--depth=200"

// cloneURLFor names the remote a pinned clone fetches from; tests point it at
// a local repository.
var cloneURLFor = func(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// basePinDeepenSteps widen a pinned clone until the pinned base and the PR
// head share history; the last step completes it.
var basePinDeepenSteps = []string{"--deepen=1000", "--deepen=5000", "--unshallow"}

// pinnedCache is the per-PR cache a pinned review checks out from.
type pinnedCache struct {
	key, dir, logPrefix string
}

func pinnedCacheFor(cloneRoot, owner, repo string, prNumber int) (pinnedCache, error) {
	absCloneRoot, err := filepath.Abs(cloneRoot)
	if err != nil {
		return pinnedCache{}, fmt.Errorf("abs cloneRoot: %w", err)
	}
	name := fmt.Sprintf("%s__%s__pr%d__pinned", owner, repo, prNumber)
	return pinnedCache{
		// The same key evictCloneCaches derives from the directory name.
		key:       owner + "/" + strings.TrimPrefix(name, owner+"__"),
		dir:       filepath.Join(absCloneRoot, ".cache", name),
		logPrefix: fmt.Sprintf("[AGENT %s/%s#%d pinned]", owner, repo, prNumber),
	}, nil
}

// cloneForAgentPinned is cloneForAgent for a review that must not see
// anything newer than the PR's merge: a per-PR clone whose only fetched tips
// are the reviewed head commit and the pin anchor (the merge commit, or the
// reported base of an open PR), both by SHA rather than by ref, which could
// carry commits pushed later. Deepening follows the parents of those tips,
// and the remote's refspec names only the head commit, so no fetch brings a
// live branch in, and origin/<base> is pinned at the PR's own base
// (basePinSHA). Benchmarks over historical PRs need this: in the shared
// cache origin/<base> is today's tip, which already contains the fix.
// With a cache cap set (AGENT_CLONE_CACHE_MAX_GB) the per-PR cache stays for
// the next run of the same PR until evicted; without one it is removed with
// the worktree, so pinned reviews cannot fill an uncapped clone root.
func cloneForAgentPinned(ctx context.Context, cloneRoot, dir, owner, repo, defaultBranch string, prNumber int, commitSHA, token string, pin BasePin) (cleanup func() error, err error) {
	noopCleanup := func() error { return nil }
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return noopCleanup, fmt.Errorf("abs worktree dir: %w", err)
	}
	cache, err := pinnedCacheFor(cloneRoot, owner, repo, prNumber)
	if err != nil {
		return noopCleanup, err
	}
	release := acquireCacheUse(cache.key)
	fail := func(err error) (func() error, error) {
		release()
		dropUncappedPinnedCache(cache)
		return noopCleanup, err
	}
	if err := ensurePinnedCache(ctx, cache, owner, repo, defaultBranch, prNumber, commitSHA, token, pin); err != nil {
		return fail(err)
	}
	t := time.Now()
	if out, err := runGit(ctx, cache.dir, "worktree", "add", "--detach", absDir, commitSHA); err != nil {
		return fail(fmt.Errorf("git worktree add: %w (%s)", err, out))
	}
	log.Printf("%s git worktree add DONE in %s", cache.logPrefix, time.Since(t))
	removeTree := removeWorktree(cloneRoot, cache.dir, absDir, cache.logPrefix, release)
	return func() error {
		err := removeTree()
		dropUncappedPinnedCache(cache)
		return err
	}, nil
}

// warmPinnedCache populates a PR's pinned cache ahead of the checkout, so the
// fetch and any deepening run under the caller's budget rather than the
// per-review prep budget, as ensureAgentCache does for the shared cache.
func warmPinnedCache(ctx context.Context, cloneRoot, owner, repo, defaultBranch string, prNumber int, commitSHA, token string, pin BasePin) error {
	cache, err := pinnedCacheFor(cloneRoot, owner, repo, prNumber)
	if err != nil {
		return err
	}
	release := acquireCacheUse(cache.key)
	err = ensurePinnedCache(ctx, cache, owner, repo, defaultBranch, prNumber, commitSHA, token, pin)
	release()
	if err != nil {
		dropUncappedPinnedCache(cache)
	}
	return err
}

// ensurePinnedCache leaves the cache holding the head commit and the pin
// anchor with origin/<base> pinned (idempotent: a populated cache costs only
// a merge-base check). Holds the cache mutex.
func ensurePinnedCache(ctx context.Context, cache pinnedCache, owner, repo, defaultBranch string, prNumber int, commitSHA, token string, pin BasePin) error {
	if pin.anchor() == "" {
		return errors.New("base pin: the PR has neither a merge commit nor a base SHA")
	}
	branch := defaultBranch
	if branch == "" {
		branch = "base"
	}
	headSpec := fmt.Sprintf("+%s:refs/agent-pr/%d", commitSHA, prNumber)
	anchorRef := fmt.Sprintf("refs/agent-base/%d", prNumber)
	anchorSpec := "+" + pin.anchor() + ":" + anchorRef

	mu := cacheLock(cache.key)
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Stat(filepath.Join(cache.dir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(cache.dir, 0o755); err != nil {
			return fmt.Errorf("create pinned cache: %w", err)
		}
		for _, args := range [][]string{
			{"init", "--quiet"},
			{"remote", "add", "origin", cloneURLFor(owner, repo)},
		} {
			if out, err := runGit(ctx, cache.dir, args...); err != nil {
				_ = os.RemoveAll(cache.dir)
				return fmt.Errorf("git %s (pinned cache init): %w (%s)", args[0], err, out)
			}
		}
	} else if err != nil {
		return fmt.Errorf("stat pinned cache .git: %w", err)
	}
	// Re-set on every run: a kept cache may be reused at a newer head.
	if out, err := runGit(ctx, cache.dir, "config", "remote.origin.fetch", headSpec); err != nil {
		return fmt.Errorf("git config remote.origin.fetch (pinned): %w (%s)", err, out)
	}

	fetch := func(depth string) error {
		args := authHeaderArgs(token)
		args = append(args, "fetch", "--quiet", depth, "origin", headSpec, anchorSpec)
		out, err := runGit(ctx, cache.dir, args...)
		if err != nil && depth == "--unshallow" && strings.Contains(out, "does not make sense") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("git fetch %s (pinned): %w (%s)", depth, err, redactToken(out, token))
		}
		return nil
	}
	t0 := time.Now()
	if !hasCommit(ctx, cache.dir, commitSHA) || !hasCommit(ctx, cache.dir, pin.anchor()) {
		log.Printf("%s fetch head and base anchor %s START", cache.logPrefix, pin.anchor())
		if err := fetch(basePinDepth); err != nil {
			return err
		}
		if !hasCommit(ctx, cache.dir, commitSHA) {
			return fmt.Errorf("base pin: the remote did not serve head commit %s", commitSHA)
		}
	}
	pinSHA, err := basePinSHA(ctx, cache.dir, pin, anchorRef, commitSHA, fetch)
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"update-ref", "refs/remotes/origin/" + branch, pinSHA},
		{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/" + branch},
		{"config", "prism.basepin", pinSHA},
	} {
		if out, err := runGit(ctx, cache.dir, args...); err != nil {
			return fmt.Errorf("git %s (base pin): %w (%s)", args[0], err, out)
		}
	}
	log.Printf("%s origin/%s pinned at %s (anchor %s) in %s", cache.logPrefix, branch, pinSHA, pin.anchor(), time.Since(t0))
	return nil
}

// dropUncappedPinnedCache removes a pinned cache nobody uses when no cache
// cap bounds the clone root.
func dropUncappedPinnedCache(cache pinnedCache) {
	mu := cacheLock(cache.key)
	mu.Lock()
	defer mu.Unlock()
	cloneCache.Lock()
	keep := cloneCache.maxBytes > 0 || cloneCache.inUse[cache.key] > 0
	cloneCache.Unlock()
	if keep {
		return
	}
	if err := os.RemoveAll(cache.dir); err != nil {
		log.Printf("%s removing the pinned cache failed: %v", cache.logPrefix, err)
	}
}

// basePinSHA picks the commit origin/<base> is pinned at. For a PR merged
// with a merge commit it is the merge's first parent: the base as it stood at
// merge time. A squash or rebase merge leaves a single-parent commit, so the
// pin is the fork point, the merge-base of that commit and the PR head. A
// fast-forward merge reports the head itself as the merge commit, so the pin
// is the commit below the PR's own commits, taken to lie on the head's
// first-parent chain (a PR that merged side branches of its own would pin
// too deep, which still keeps the fix out). An open PR pins at the base tip
// the API reported. History is widened until the pin and the head connect,
// which the three-dot diff needs.
func basePinSHA(ctx context.Context, cacheDir string, pin BasePin, anchorRef, commitSHA string, fetch func(depth string) error) (string, error) {
	connect := func(a, b string) error {
		for i := 0; ; i++ {
			if _, err := runGit(ctx, cacheDir, "merge-base", a, b); err == nil {
				return nil
			}
			if i == len(basePinDeepenSteps) {
				return fmt.Errorf("base pin: %s and %s share no history after %s", a, b, strings.Join(basePinDeepenSteps, ", "))
			}
			if err := fetch(basePinDeepenSteps[i]); err != nil {
				return err
			}
		}
	}
	pinSHA := ""
	switch {
	case pin.MergeCommitSHA == "":
		if err := connect(anchorRef, commitSHA); err != nil {
			return "", err
		}
		pinSHA = pin.BaseSHA
	case pin.MergeCommitSHA == commitSHA:
		if pin.Commits <= 0 {
			return "", errors.New("base pin: a fast-forward merge needs the PR's commit count")
		}
		below := fmt.Sprintf("%s~%d", commitSHA, pin.Commits)
		if err := connect(below, commitSHA); err != nil {
			return "", err
		}
		out, err := runGit(ctx, cacheDir, "rev-parse", "--verify", below+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("base pin: %s: %w (%s)", below, err, out)
		}
		pinSHA = strings.TrimSpace(out)
	default:
		out, err := runGit(ctx, cacheDir, "rev-list", "--parents", "-n", "1", anchorRef)
		if err != nil {
			return "", fmt.Errorf("base pin: read the merge commit's parents: %w (%s)", err, out)
		}
		if parents := strings.Fields(out)[1:]; len(parents) >= 2 {
			if err := connect(parents[0], commitSHA); err != nil {
				return "", err
			}
			pinSHA = parents[0]
		} else {
			if err := connect(anchorRef, commitSHA); err != nil {
				return "", err
			}
			out, err := runGit(ctx, cacheDir, "merge-base", anchorRef, commitSHA)
			if err != nil {
				return "", fmt.Errorf("base pin: fork point: %w (%s)", err, out)
			}
			pinSHA = strings.TrimSpace(out)
		}
	}
	if pinSHA == commitSHA {
		return "", fmt.Errorf("base pin: the pinned base %s is the PR head itself", pinSHA)
	}
	return pinSHA, nil
}
