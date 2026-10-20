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
// commit of a merged PR, else the base tip reported for an open one.
type BasePin struct {
	MergeCommitSHA string
	BaseSHA        string
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

// cloneForAgentPinned is cloneForAgent for a review that must not see
// anything newer than the PR: a per-PR clone holding only the reviewed head
// commit and the PR's own base, both fetched by SHA (not by ref, which could
// carry commits pushed to the branch later), with origin/<base> pinned at
// that base (basePinSHA). The remote's refspec names only the head commit, so
// no later fetch brings a live branch in, and the object store never holds a
// commit made after the PR. Benchmarks over historical PRs need this: in the
// shared cache origin/<base> is today's tip, which already contains the fix.
// Each PR keeps its own cache under the clone root until the cache cap
// (AGENT_CLONE_CACHE_MAX_GB) evicts it, so a benchmark deployment sets one.
func cloneForAgentPinned(ctx context.Context, cloneRoot, dir, owner, repo, defaultBranch string, prNumber int, commitSHA, token string, pin BasePin) (cleanup func() error, err error) {
	noopCleanup := func() error { return nil }
	if pin.anchor() == "" {
		return noopCleanup, errors.New("base pin: the PR has neither a merge commit nor a base SHA")
	}
	absCloneRoot, err := filepath.Abs(cloneRoot)
	if err != nil {
		return noopCleanup, fmt.Errorf("abs cloneRoot: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return noopCleanup, fmt.Errorf("abs worktree dir: %w", err)
	}
	branch := defaultBranch
	if branch == "" {
		branch = "base"
	}
	name := fmt.Sprintf("%s__%s__pr%d__pinned", owner, repo, prNumber)
	// The same key evictCloneCaches derives from the directory name.
	cacheKey := owner + "/" + strings.TrimPrefix(name, owner+"__")
	cacheDir := filepath.Join(absCloneRoot, ".cache", name)
	cloneURL := cloneURLFor(owner, repo)
	logPrefix := fmt.Sprintf("[AGENT %s/%s#%d pinned]", owner, repo, prNumber)
	headSpec := fmt.Sprintf("+%s:refs/agent-pr/%d", commitSHA, prNumber)
	anchorRef := fmt.Sprintf("refs/agent-base/%d", prNumber)
	anchorSpec := "+" + pin.anchor() + ":" + anchorRef

	release := acquireCacheUse(cacheKey)
	fail := func(err error) (func() error, error) {
		release()
		return noopCleanup, err
	}
	mu := cacheLock(cacheKey)
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return fail(fmt.Errorf("create pinned cache: %w", err))
		}
		for _, args := range [][]string{
			{"init", "--quiet"},
			{"remote", "add", "origin", cloneURL},
			{"config", "remote.origin.fetch", headSpec},
		} {
			if out, err := runGit(ctx, cacheDir, args...); err != nil {
				_ = os.RemoveAll(cacheDir)
				return fail(fmt.Errorf("git %s (pinned cache init): %w (%s)", args[0], err, out))
			}
		}
	} else if err != nil {
		return fail(fmt.Errorf("stat pinned cache .git: %w", err))
	}

	fetch := func(depth string) error {
		args := authHeaderArgs(token)
		args = append(args, "fetch", "--quiet", depth, "origin", headSpec, anchorSpec)
		out, err := runGit(ctx, cacheDir, args...)
		if err != nil && strings.Contains(out, "does not make sense") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("git fetch %s (pinned): %w (%s)", depth, err, redactToken(out, token))
		}
		return nil
	}
	t0 := time.Now()
	if !hasCommit(ctx, cacheDir, commitSHA) || !hasCommit(ctx, cacheDir, pin.anchor()) {
		log.Printf("%s fetch head and base anchor %s START", logPrefix, pin.anchor())
		if err := fetch(basePinDepth); err != nil {
			return fail(err)
		}
		if !hasCommit(ctx, cacheDir, commitSHA) {
			return fail(fmt.Errorf("base pin: the remote did not serve head commit %s", commitSHA))
		}
	}
	pinSHA, err := basePinSHA(ctx, cacheDir, pin, anchorRef, commitSHA, fetch)
	if err != nil {
		return fail(err)
	}
	for _, args := range [][]string{
		{"update-ref", "refs/remotes/origin/" + branch, pinSHA},
		{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/" + branch},
		{"config", "prism.basepin", pinSHA},
	} {
		if out, err := runGit(ctx, cacheDir, args...); err != nil {
			return fail(fmt.Errorf("git %s (base pin): %w (%s)", args[0], err, out))
		}
	}
	log.Printf("%s origin/%s pinned at %s (anchor %s) in %s", logPrefix, branch, pinSHA, pin.anchor(), time.Since(t0))

	t1 := time.Now()
	if out, err := runGit(ctx, cacheDir, "worktree", "add", "--detach", absDir, commitSHA); err != nil {
		return fail(fmt.Errorf("git worktree add: %w (%s)", err, out))
	}
	log.Printf("%s git worktree add DONE in %s", logPrefix, time.Since(t1))
	return removeWorktree(cloneRoot, cacheDir, absDir, logPrefix, release), nil
}

// basePinSHA picks the commit origin/<base> is pinned at. For a PR merged
// with a merge commit it is the merge's first parent: the base as it stood at
// merge time. A squash or rebase merge leaves a single-parent commit, so the
// pin is the fork point, the merge-base of that commit and the PR head. An
// open PR pins at the base tip the API reported. History is widened until the
// pin and the head connect, which the three-dot diff needs.
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
