package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

// computeReviewScope finds the merge-base of HEAD with base and the changed
// paths whose content at HEAD equals the base tip's. Merging those paths
// changes nothing, so they came from the base branch (a sync commit with no
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
	changed, err := gitOutput(ctx, dir, "-c", "core.quotePath=false", "diff", "--raw", "--no-abbrev", "--no-renames", scope.MergeBase, "HEAD")
	if err != nil {
		return nil, err
	}
	baseMoved, err := gitOutput(ctx, dir, "-c", "core.quotePath=false", "diff", "--raw", "--no-abbrev", "--no-renames", scope.MergeBase, scope.BaseTip)
	if err != nil {
		return nil, err
	}
	tipState := map[string]string{}
	for _, c := range rawChanges(baseMoved) {
		tipState[c.path] = c.state()
	}
	for _, c := range rawChanges(changed) {
		if tip, moved := tipState[c.path]; moved && tip == c.state() {
			scope.InheritedPaths = append(scope.InheritedPaths, c.path)
		}
	}
	sort.Strings(scope.InheritedPaths)
	return scope, nil
}

// maxScopedPaths bounds the exclude pathspec handed to git diff.
const maxScopedPaths = 2000

type rawChange struct{ path, mode, blob string }

// state is the post-image mode and blob, so a mode-only change still differs.
func (c rawChange) state() string { return c.mode + " " + c.blob }

// rawChanges parses `git --raw --no-abbrev` lines into each path's
// post-image mode and blob; a deletion reports the all-zero blob.
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
		changes = append(changes, rawChange{path: path, mode: fields[1], blob: fields[3]})
	}
	return changes
}

// reviewScopeForWorktree computes the worktree's review scope against
// origin/<baseBranch>, fetching the base ref once if it is missing. It returns
// nil (an unscoped review) when git cannot, or a scope with only
// BaseFetchFailed set when this review could not fetch the base at all, which
// consumers read as an unknown base.
func reviewScopeForWorktree(ctx context.Context, dir, cacheKey, token, baseBranch, logPrefix string) *payload.ReviewScope {
	base := "origin/HEAD"
	if baseBranch != "" {
		base = "origin/" + baseBranch
	}
	scope, err := computeReviewScope(ctx, dir, base)
	if err != nil && baseBranch != "" {
		fetched := fetchBaseRef(ctx, dir, baseBranch, token, cacheKey, logPrefix)
		if scope, err = computeReviewScope(ctx, dir, base); err != nil && !fetched {
			log.Printf("%s review scope unavailable: %s could not be fetched (%v)", logPrefix, base, err)
			return &payload.ReviewScope{BaseFetchFailed: true}
		}
	}
	if err != nil {
		log.Printf("%s review scope unavailable (%v); reviewing the full diff", logPrefix, err)
		return nil
	}
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

// CommentsInScope drops comments on the scope's inherited paths.
func CommentsInScope(comments []types.LineComment, scope *payload.ReviewScope) []types.LineComment {
	if scope == nil || len(scope.InheritedPaths) == 0 {
		return comments
	}
	inherited := make(map[string]bool, len(scope.InheritedPaths))
	for _, p := range scope.InheritedPaths {
		inherited[p] = true
	}
	kept := make([]types.LineComment, 0, len(comments))
	for _, c := range comments {
		if !inherited[normalizeScopePath(c.FilePath)] {
			kept = append(kept, c)
		}
	}
	return kept
}

func normalizeScopePath(p string) string {
	return strings.TrimLeft(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
}

// scopePathspec returns the pathspec arguments that keep inherited paths out
// of a git diff, or nil when nothing is inherited or the list is too long for
// a command line (the prompt note and the output filter still apply).
func scopePathspec(scope *payload.ReviewScope) []string {
	if scope == nil || len(scope.InheritedPaths) == 0 || len(scope.InheritedPaths) > maxScopedPaths {
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
	fmt.Fprintf(&b, "%d changed path(s) are out of scope because their content at HEAD is identical to the base branch's current content, so merging the PR changes nothing in them (typically a sync commit without merge ancestry picked them up). `git diff %s...HEAD` still lists them; the change under review is that diff minus these paths. Do not review these paths and do not cite them as regressions or behavior changes of this PR:\n", len(scope.InheritedPaths), baseBranchLabel(baseBranch))
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
		return "origin/HEAD"
	}
	return "origin/" + baseBranch
}
