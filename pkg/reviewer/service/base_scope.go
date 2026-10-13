package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
)

// computeReviewScope finds the merge-base of HEAD with base and the changed
// paths whose content at HEAD matches a version base held after that
// merge-base. Those paths came from the base branch (a sync commit with no
// merge ancestry, or work that already landed there), not from the PR.
func computeReviewScope(ctx context.Context, dir, base string) (*payload.ReviewScope, error) {
	mergeBase, err := gitOutput(ctx, dir, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	tip, err := gitOutput(ctx, dir, "rev-parse", base)
	if err != nil {
		return nil, err
	}
	scope := &payload.ReviewScope{MergeBase: strings.TrimSpace(mergeBase), BaseTip: strings.TrimSpace(tip)}
	if scope.MergeBase == scope.BaseTip {
		return scope, nil
	}
	changed, err := gitOutput(ctx, dir, "diff", "--raw", "--no-abbrev", "--no-renames", scope.MergeBase, "HEAD")
	if err != nil {
		return nil, err
	}
	head := map[string]string{}
	for _, c := range rawChanges(changed) {
		head[c.path] = c.blob
	}
	if len(head) == 0 {
		return scope, nil
	}
	history, err := gitOutput(ctx, dir, "log", "--raw", "--no-abbrev", "--no-renames", "--diff-merges=first-parent", "--format=", scope.MergeBase+".."+scope.BaseTip)
	if err != nil {
		return nil, err
	}
	for _, c := range rawChanges(history) {
		if blob, ok := head[c.path]; ok && blob == c.blob {
			scope.InheritedPaths = append(scope.InheritedPaths, c.path)
			delete(head, c.path)
		}
	}
	sort.Strings(scope.InheritedPaths)
	return scope, nil
}

type rawChange struct{ path, blob string }

// rawChanges parses `git --raw --no-abbrev` lines into each path's
// post-image blob; a deletion reports the all-zero blob.
func rawChanges(out string) []rawChange {
	var changes []rawChange
	for _, line := range strings.Split(out, "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(meta, ":") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 5 {
			continue
		}
		changes = append(changes, rawChange{path: path, blob: fields[3]})
	}
	return changes
}

// reviewScopeForWorktree computes the worktree's review scope against
// origin/<baseBranch>; nil (an unscoped review) when git cannot.
func reviewScopeForWorktree(ctx context.Context, dir, cacheKey, baseBranch, logPrefix string) *payload.ReviewScope {
	base := "origin/HEAD"
	if baseBranch != "" {
		base = "origin/" + baseBranch
	}
	scope, err := computeReviewScope(ctx, dir, base)
	if err != nil {
		log.Printf("%s review scope unavailable (%v); reviewing the full diff", logPrefix, err)
		return nil
	}
	scope.BaseFetchFailed = baseBranch != "" && baseFetchFailed(cacheKey, baseBranch)
	if n := len(scope.InheritedPaths); n > 0 {
		log.Printf("%s %d changed path(s) inherited from %s left out of the review (merge-base %.7s, base tip %.7s)", logPrefix, n, base, scope.MergeBase, scope.BaseTip)
	}
	return scope
}

// withoutInherited drops the scope's inherited paths from a parsed diff.
func withoutInherited(files []diffFile, scope *payload.ReviewScope) []diffFile {
	if scope == nil || len(scope.InheritedPaths) == 0 || files == nil {
		return files
	}
	inherited := make(map[string]bool, len(scope.InheritedPaths))
	for _, p := range scope.InheritedPaths {
		inherited[p] = true
	}
	kept := make([]diffFile, 0, len(files))
	for _, f := range files {
		if !inherited[f.Path] {
			kept = append(kept, f)
		}
	}
	return kept
}

// scopePathspec returns the pathspec arguments that keep inherited paths out
// of a git diff, or nil when nothing is inherited.
func scopePathspec(scope *payload.ReviewScope) []string {
	if scope == nil || len(scope.InheritedPaths) == 0 {
		return nil
	}
	args := []string{"--", "."}
	for _, p := range scope.InheritedPaths {
		args = append(args, ":(exclude,literal)"+p)
	}
	return args
}

// scopeNote tells the reviewer which changed paths it is not shown and why.
func scopeNote(scope *payload.ReviewScope, baseBranch string) string {
	if scope == nil || len(scope.InheritedPaths) == 0 {
		return ""
	}
	const listed = 20
	shown := scope.InheritedPaths
	if len(shown) > listed {
		shown = shown[:listed]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Changes inherited from %s\n\n", baseBranchLabel(baseBranch))
	fmt.Fprintf(&b, "%d changed path(s) are left out of the diff because their content at HEAD matches a version already on the base branch: the head picked up base-branch changes (for example a sync commit without merge ancestry), and the PR did not write them. Do not review these paths and do not cite them as regressions or behavior changes of this PR:\n", len(scope.InheritedPaths))
	for _, p := range shown {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	if len(scope.InheritedPaths) > listed {
		fmt.Fprintf(&b, "- and %d more\n", len(scope.InheritedPaths)-listed)
	}
	return b.String()
}

func baseBranchLabel(baseBranch string) string {
	if baseBranch == "" {
		return "the base branch"
	}
	return "origin/" + baseBranch
}
