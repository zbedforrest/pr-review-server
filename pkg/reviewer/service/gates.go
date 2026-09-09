package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"pr-review-server/pkg/reviewer/types"
)

// Mechanical gates: deterministic, zero-LLM signals computed from the PR's
// diff and the checked-out worktree. Offline evaluation against real
// release-blocker PRs showed each fires precisely (settings-ref: exactly the
// undefined-settings production bug; registry-split: exactly the missing
// frontend topic counterpart; shared-file: the incidental shared-module edits
// reviews anchor away from) with near-zero noise on known-good PRs.
//
// Gate findings enter the review through MergeFindings with provenance
// "mechanical", so an agent that waves them off cannot delete them.

var (
	gateSharedPath  = regexp.MustCompile(`(^|/)(common|shared|base|lib|utils?)/|(^|/)Base[A-Z]\w*\.(tsx?|jsx?|py)$`)
	gateSettingsRef = regexp.MustCompile(`settings\.([A-Z][A-Z0-9_]{2,})`)
	gateTopicClass  = regexp.MustCompile(`(?m)^\+class ([A-Z]\w*Topic)\b`)

	// The registry-split gate is repo-convention-specific, so its paths come
	// from the environment rather than being hardcoded: backend topic classes
	// live under GATE_TOPIC_BE_DIR and each must have a same-named frontend
	// class under GATE_TOPIC_FE_DIR. Both unset => the gate is disabled.
	gateTopicBEDir = os.Getenv("GATE_TOPIC_BE_DIR")
	gateTopicFEDir = os.Getenv("GATE_TOPIC_FE_DIR")

	// Overlay components that portal to a stacking layer; using them without
	// an explicit layer inherits the top-of-everything default (measured: the
	// one layerless overlay addition across 38 real PRs was the production
	// stacking bug; every clean addition passed layer= explicitly).
	// Matches the class signature — any JSX component named *Portal and direct
	// createPortal( calls — not one component's spelling: an overlay built on
	// raw createPortal shipped the same invisible/mis-stacked bug class the
	// narrow pattern missed.
	gateOverlayOpen  = regexp.MustCompile(`(?m)^\+.*(<(RichTooltip|[A-Za-z]\w*Portal|Portal)\b|\bcreatePortal\s*\()`)
	gateOverlayLayer = regexp.MustCompile(`(?m)^\+.*\blayer\s*[=:]`)

	// New properties on Django models change behavior/exception contracts for
	// every consumer and subclass (measured: 2 of 2 additions across 38 real
	// PRs were culprit PRs; zero on known-good PRs).
	gateModelProperty = regexp.MustCompile(`(?m)^\+\s*@(cached_)?property\b`)

	// ----- Gates round 3 (v4 phase 2). Every gate below is env-gated and
	// silent by default: unset envs => byte-identical behavior to a round-2
	// build. Each firing also becomes a required check (checks.go).

	// wiring/reachability (2.1): dead-symbol (new exported symbol nothing
	// references) + de-wired (the diff removes the last call/render site of a
	// symbol that stays defined). Enabled by GATE_WIRING=true.
	gateWiringEnabled = os.Getenv("GATE_WIRING") == "true"
	gateExportPyDef   = regexp.MustCompile(`^def ([a-z_][a-z0-9_]{3,})\(`)
	gateExportTS      = regexp.MustCompile(`^export (?:default )?(?:async )?(?:function|const|class) ([A-Za-z_]\w{3,})`)
	gateJSXUse        = regexp.MustCompile(`<([A-Z]\w{2,})[\s/>]`)
	gateCallUse       = regexp.MustCompile(`\b([A-Za-z_]\w{3,})\(`)
	gateDefLine       = regexp.MustCompile(`\b(?:function|class|def)\s+([A-Za-z_]\w{2,})|\b(?:const|let|var)\s+([A-Za-z_]\w{2,})\s*[=:]|\b(?:private|protected|public)\s+(?:async\s+)?([A-Za-z_]\w{2,})\s*\(`)

	// effect-cleanup (2.2): a React change that acquires a leak-prone
	// resource (listener/interval/stream/subscription) with no cleanup in
	// the same change. Enabled by GATE_EFFECT_CLEANUP=true.
	gateEffectEnabled = os.Getenv("GATE_EFFECT_CLEANUP") == "true"
	gateEffectHook    = regexp.MustCompile(`(?m)^\+.*\buse(?:Layout)?Effect\s*\(`)
	gateEffectAcquire = regexp.MustCompile(`(?m)^\+.*(\baddEventListener\s*\(|\bsetInterval\s*\(|\brequestAnimationFrame\s*\(|\bnew\s+(?:WebSocket|EventSource)\b|\.subscribe\s*\(|\.play\s*\(|\.observe\s*\()`)
	gateEffectCleanup = regexp.MustCompile(`(?m)^\+.*(\breturn\s+\(\s*\)\s*=>|\breturn\s+function\b|\breturn\s+[A-Za-z_$][\w$]*\s*$|\bremoveEventListener\b|\bclearInterval\b|\bclearTimeout\b|\bcancelAnimationFrame\b|\bunsubscribe\b|\.disconnect\s*\(|\.close\s*\(|\babort|\bdispose\b|\bcomponentWillUnmount\b)`)
	gateHandlerProp   = regexp.MustCompile(`(?m)^\+.*\bon[A-Z]\w*=\{`)
	gateHandlerStart  = regexp.MustCompile(`\bstart[A-Z]\w*\s*\(`)
	gateHandlerOther  = regexp.MustCompile(`(?m)^\+.*(\.play\s*\(|\bnew\s+(?:WebSocket|EventSource)\b|\bsetInterval\s*\(|\brequestAnimationFrame\s*\(|\.subscribe\s*\()`)

	// async-task payload version-skew (2.3): payload shape produced for a
	// deferred/queued task changes with no compat for in-flight old-format
	// messages (deploy window: old workers + new producers coexist). Like
	// GATE_TOPIC_*, the repo-specific layout arrives via env: files matching
	// GATE_TASK_PRODUCER_GLOB (doublestar) hold task producers; the optional
	// GATE_TASK_CONSUMER_GLOB narrows where compat handling is searched for.
	// Producer glob unset => the gate is disabled.
	gateTaskProducerGlob = os.Getenv("GATE_TASK_PRODUCER_GLOB")
	gateTaskConsumerGlob = os.Getenv("GATE_TASK_CONSUMER_GLOB")
	gateDictKey          = regexp.MustCompile(`["']([A-Za-z_][A-Za-z0-9_-]{2,})["']\s*:`)
	gatePyDefLine        = regexp.MustCompile(`^\s*(?:async\s+)?def\s+(\w+)\(([^)]*)`)

	// enum/key migration-residue (2.4): a quoted literal migrated old->new
	// (paired lines identical except the literal), or an enum-ish constant
	// newly excluded from accepted values, while other files still carry the
	// old one. Enabled by GATE_MIGRATION_RESIDUE=true; alerts are suppressed
	// above GATE_RESIDUE_MAX_SURVIVORS surviving lines (default 20 — that
	// volume means a shared constant, not a half-finished migration).
	gateResidueEnabled      = os.Getenv("GATE_MIGRATION_RESIDUE") == "true"
	gateResidueMaxSurvivors = residueMaxSurvivors()
	gateQuotedLit           = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	gateConstExclusion      = regexp.MustCompile(`\bfor\b.*\bif\b.*!==?\s*(?:[A-Za-z_][\w.]*\.)?([A-Z][A-Z0-9_]{2,})\b|\.remove\(\s*(?:[A-Za-z_][\w.]*\.)?([A-Z][A-Z0-9_]{2,})\s*\)`)
	gateIdentTok            = regexp.MustCompile(`[A-Za-z_]\w{2,}`)
)

// residueMaxSurvivors reads the residue-gate suppression threshold, keeping
// the documented default on any unset/garbage value.
func residueMaxSurvivors() int {
	if v := os.Getenv("GATE_RESIDUE_MAX_SURVIVORS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 20
}

// gateNoiseTest matches "test"/"tests" as a whole word within the path — a
// path segment (tests/), or delimited inside one (test_foo.py, foo_test.go,
// Comp.test.tsx, __tests__) — never as a bare substring: latest.tsx,
// attestation.py, or fastestRoute.ts are live code, and misreading them as
// noise flips the wiring gate's err-silent contract (any surviving reference
// suppresses the alert) into a false FIRE when a symbol's only references
// live in such files.
var gateNoiseTest = regexp.MustCompile(`(^|[^a-z])tests?($|[^a-z])`)

// gateNoisePath reports paths whose matches never count as live references:
// tests (run by the runner, not by wiring), stories/storybook, fixtures,
// snapshots, migrations, hidden/tooling dirs (.devcontainer, .pystubs, …),
// and vendored/minified assets.
func gateNoisePath(p string) bool {
	l := strings.ToLower(p)
	return gateNoiseTest.MatchString(l) || strings.Contains(l, ".stories.") ||
		strings.Contains(l, "storybook") || strings.Contains(l, "fixture") ||
		strings.Contains(l, "__snapshots__") || strings.Contains(l, "migration") ||
		strings.HasPrefix(l, ".") || strings.Contains(l, "/.") ||
		strings.Contains(l, "vendor") || strings.Contains(l, "node_modules") ||
		strings.Contains(l, ".min.")
}

// diffFile is one changed file: its path and the diff's added/removed lines.
type diffFile struct {
	Path    string
	Added   []string // lines added by the PR (no leading '+')
	Removed []string // lines removed by the PR (no leading '-') — omission
	// bugs live in removals, so bug-memory keyword matching needs them.
	Status string // "modified", "added", "removed"
	// AddedHunks is Added grouped by contiguous diff hunk (same lines, same
	// order). Gates whose contract is "in the same added hunk" (effect-
	// cleanup) scope to these groups; nil (producers that don't track hunks,
	// e.g. hand-built test fixtures) means fall back to whole-file Added.
	AddedHunks [][]string
}

// RunMechanicalGates evaluates all gates and returns provenance-ready findings
// (Importance MEDIUM at most — gates flag risk, they do not gate merges).
// grep runs against the worktree at dir via git grep.
func RunMechanicalGates(ctx context.Context, dir string, files []diffFile) []types.LineComment {
	var out []types.LineComment

	// shared-file: one aggregated advisory per PR.
	var shared []string
	for _, f := range files {
		if f.Status != "added" && gateSharedPath.MatchString(f.Path) {
			shared = append(shared, f.Path)
		}
	}
	if len(shared) > 0 {
		out = append(out, types.LineComment{
			FilePath: shared[0], LineNumber: 0, Importance: "MEDIUM",
			CommentBody: fmt.Sprintf("**Mechanical alert — shared module edited.** This PR modifies %d shared/base module(s): %s. Incidental edits to shared code are a leading cause of regressions outside the PR's headline feature. Check each module's consumers; report any regression as a finding on the affected code and answer the module's required check, not a summary paragraph.",
				len(shared), strings.Join(shared, ", ")),
		})
	}

	for _, f := range files {
		lower := strings.ToLower(f.Path)

		// settings-ref: python, non-test — referenced settings.X must be
		// assigned somewhere in the tree.
		if strings.HasSuffix(f.Path, ".py") && !strings.Contains(lower, "test") {
			seen := map[string]bool{}
			for _, l := range f.Added {
				for _, m := range gateSettingsRef.FindAllStringSubmatch(l, -1) {
					key := m[1]
					if seen[key] {
						continue
					}
					seen[key] = true
					pattern := "^[[:space:]]*" + key + "[[:space:]]*="
					if n, err := gitGrepCount(ctx, dir, pattern, false); err == nil && n == 0 {
						out = append(out, types.LineComment{
							FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
							CommentBody: fmt.Sprintf("**Mechanical alert — undefined settings reference.** `settings.%s` is referenced by this PR but `%s` is not assigned anywhere in the tree. At runtime this raises AttributeError on first use.", key, key),
						})
					}
				}
			}
		}

		// portal-layer: overlay component added without an explicit layer.
		if strings.HasSuffix(f.Path, ".tsx") || strings.HasSuffix(f.Path, ".jsx") || strings.HasSuffix(f.Path, ".ts") {
			joined := "+" + strings.Join(f.Added, "\n+")
			if gateOverlayOpen.MatchString(joined) && !gateOverlayLayer.MatchString(joined) {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: "**Mechanical alert — portal overlay without an explicit layer.** This change renders portal-based UI (a *Portal component or createPortal call) without a `layer` prop. Portaled content escapes its parent's stacking and visibility context: on the default layer it renders above modals, and under browser fullscreen only descendants of the fullscreen element are visible at all. State the intended stacking layer explicitly, and verify the portal target is correct for fullscreen contexts.",
				})
			}
		}

		// model-property: new property on a Django model.
		if strings.HasSuffix(f.Path, "models.py") && !strings.Contains(lower, "test") {
			joined := "+" + strings.Join(f.Added, "\n+")
			if gateModelProperty.MatchString(joined) {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: "**Mechanical alert — new model property.** This change adds a property to a Django model. Properties on shared models define behavior and exception contracts for every consumer and subclass: verify what it returns or raises for each subclass, and whether existing hasattr/getattr call sites remain correct.",
				})
			}
		}

		// registry-split: new backend push topic must have an FE counterpart.
		if gateTopicBEDir != "" && gateTopicFEDir != "" &&
			strings.HasPrefix(f.Path, gateTopicBEDir) && !strings.Contains(lower, "test") {
			joined := "+" + strings.Join(f.Added, "\n+")
			for _, m := range gateTopicClass.FindAllStringSubmatch(joined, -1) {
				cls := m[1]
				if n, err := gitGrepCount(ctx, dir, `\b`+regexp.QuoteMeta(cls)+`\b`, true, gateTopicFEDir); err == nil && n == 0 {
					out = append(out, types.LineComment{
						FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
						CommentBody: fmt.Sprintf("**Mechanical alert — backend topic without frontend counterpart.** `%s` is registered on the backend, but no class of that name exists under `%s`. Clients cannot handle unknown topics; historically a missing counterpart broke the entire grouped subscription.", cls, gateTopicFEDir),
					})
				}
			}
		}
	}

	// Gates round 3 — each env-gated, silent when unconfigured.
	out = append(out, runWiringGate(ctx, dir, files)...)
	out = append(out, runEffectCleanupGate(ctx, dir, files)...)
	out = append(out, runTaskSkewGate(ctx, dir, files)...)
	out = append(out, runMigrationResidueGate(ctx, dir, files)...)
	return out
}

// gitGrepCount counts matching lines via `git grep -c -E` in dir, restricted to
// pathspec when given. POSIX ERE only (git grep -E has no \s/\b — callers use
// [[:space:]]; word matching uses -w via the word flag).
func gitGrepCount(ctx context.Context, dir, pattern string, word bool, pathspec ...string) (int, error) {
	args := []string{"grep", "-I", "-c", "-E"}
	if word {
		args = append(args, "-w")
		pattern = strings.TrimPrefix(strings.TrimSuffix(pattern, `\b`), `\b`)
	}
	args = append(args, pattern, "--")
	if len(pathspec) > 0 {
		args = append(args, pathspec...)
	} else {
		args = append(args, ".")
	}
	out, err := runGit(ctx, dir, args...)
	if err != nil {
		// git grep exits 1 on "no matches" — treat as zero, not error.
		if strings.TrimSpace(out) == "" {
			return 0, nil
		}
		return 0, err
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line[i+1:], "%d", &n); err == nil {
			total += n
		}
	}
	return total, nil
}

// grepFileCount is one file's match-line count from gitGrepFileCounts.
type grepFileCount struct {
	Path  string
	Count int
}

// gitGrepFileCounts is gitGrepCount's per-file variant: `git grep -I -c -E`
// returning counts in git's (path-sorted, deterministic) output order. Same
// POSIX-ERE constraints as gitGrepCount.
func gitGrepFileCounts(ctx context.Context, dir, pattern string, word bool, pathspec ...string) ([]grepFileCount, error) {
	args := []string{"grep", "-I", "-c", "-E"}
	if word {
		args = append(args, "-w")
	}
	args = append(args, pattern, "--")
	if len(pathspec) > 0 {
		args = append(args, pathspec...)
	} else {
		args = append(args, ".")
	}
	out, err := runGit(ctx, dir, args...)
	if err != nil {
		// git grep exits 1 on "no matches" — zero rows, not an error.
		if strings.TrimSpace(out) == "" {
			return nil, nil
		}
		return nil, err
	}
	var counts []grepFileCount
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line[i+1:], "%d", &n); err == nil {
			counts = append(counts, grepFileCount{Path: line[:i], Count: n})
		}
	}
	return counts, nil
}

// runWiringGate implements gate 2.1 (GATE_WIRING).
//
// dead-symbol: a newly added module-level def / export with zero word-matches
// outside its definition file (tests and other noise paths excluded) — code
// that nothing can reach yet. de-wired: a call/render site removed by the
// diff whose symbol stays defined in the tree but is now invoked nowhere —
// the diff disconnected it. Both directions err silent: any surviving
// reference (even a registry mention for dead-symbol) suppresses the alert.
func runWiringGate(ctx context.Context, dir string, files []diffFile) []types.LineComment {
	if !gateWiringEnabled {
		return nil
	}
	var out []types.LineComment

	// Diff-wide sets: symbols the diff (re-)wires or (un)defines. A symbol
	// re-invoked in any added line was re-wired, not de-wired; a symbol whose
	// definition the diff itself removes is a deletion, not a de-wiring.
	addedUses := map[string]bool{}
	removedDefs := map[string]bool{}
	addedDefs := map[string]bool{}
	collectDefs := func(lines []string, into map[string]bool) {
		for _, l := range lines {
			for _, m := range gateDefLine.FindAllStringSubmatch(l, -1) {
				for _, g := range m[1:] {
					if g != "" {
						into[g] = true
					}
				}
			}
		}
	}
	for _, f := range files {
		collectDefs(f.Removed, removedDefs)
		collectDefs(f.Added, addedDefs)
		for _, l := range f.Added {
			for _, m := range gateJSXUse.FindAllStringSubmatch(l, -1) {
				addedUses[m[1]] = true
			}
			for _, m := range gateCallUse.FindAllStringSubmatch(l, -1) {
				addedUses[m[1]] = true
			}
		}
	}

	// dead-symbol direction (port of the offline prototype).
	greps := 0
	const grepBudget = 40 // per-PR bound on tree greps for this gate
	seenDead := map[string]bool{}
	for _, f := range files {
		if gateNoisePath(f.Path) {
			continue
		}
		isPy := strings.HasSuffix(f.Path, ".py")
		isTS := hasAnySuffix(f.Path, ".ts", ".tsx", ".js", ".jsx")
		if !isPy && !isTS {
			continue
		}
		for _, l := range f.Added {
			var m []string
			if isPy {
				m = gateExportPyDef.FindStringSubmatch(l)
			} else {
				m = gateExportTS.FindStringSubmatch(l)
			}
			if m == nil || seenDead[m[1]] || greps >= grepBudget {
				continue
			}
			sym := m[1]
			seenDead[sym] = true
			greps++
			counts, err := gitGrepFileCounts(ctx, dir, regexp.QuoteMeta(sym), true)
			if err != nil {
				continue
			}
			uses := 0
			for _, fc := range counts {
				if gateNoisePath(fc.Path) {
					continue
				}
				n := fc.Count
				if fc.Path == f.Path {
					n-- // discount the definition line; further in-file uses are alive
				}
				if n > 0 {
					uses += n
				}
			}
			if uses == 0 {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: fmt.Sprintf("**Mechanical alert — new symbol with no references.** `%s` is added by this PR but has no references outside its definition file (tests excluded). Nothing can reach it yet: verify the wiring that is supposed to invoke it exists on every entrypoint that needs it, or state that a follow-up wires it.", sym),
				})
			}
		}
	}

	// de-wired direction: candidates are call/JSX usages in removed lines.
	type removedUse struct {
		file string
		jsx  bool
	}
	candidates := map[string]removedUse{}
	var order []string
	for _, f := range files {
		// Candidates come from source code only — templates/config removals
		// produce junk call-forms.
		if gateNoisePath(f.Path) || !hasAnySuffix(f.Path, ".py", ".ts", ".tsx", ".js", ".jsx") {
			continue
		}
		for _, l := range f.Removed {
			for _, m := range gateJSXUse.FindAllStringSubmatch(l, -1) {
				if _, ok := candidates[m[1]]; !ok {
					candidates[m[1]] = removedUse{f.Path, true}
					order = append(order, m[1])
				}
			}
			for _, m := range gateCallUse.FindAllStringSubmatch(l, -1) {
				if _, ok := candidates[m[1]]; !ok && !gateCallKeywords[m[1]] {
					candidates[m[1]] = removedUse{f.Path, false}
					order = append(order, m[1])
				}
			}
		}
	}
	for _, sym := range order {
		if greps >= grepBudget {
			break
		}
		if addedUses[sym] || removedDefs[sym] || addedDefs[sym] {
			continue
		}
		use := candidates[sym]
		q := regexp.QuoteMeta(sym)
		var survivors int
		var defined bool
		if use.jsx {
			greps += 2
			survivors = grepSumNoNoise(ctx, dir, "<"+q+"[^A-Za-z0-9_]", false)
			defined = grepSumNoNoise(ctx, dir, `(function|class)[[:space:]]+`+q+`[^A-Za-z0-9_]|(const|let|var)[[:space:]]+`+q+`[[:space:]]*[=:(]`, false) > 0
		} else {
			greps += 3
			calls := grepSumNoNoise(ctx, dir, q+`\(`, true)
			fnDefs := grepSumNoNoise(ctx, dir, `(function|def)[[:space:]]+`+q+`\(|(private|protected|public)[[:space:]]+(async[[:space:]]+)?`+q+`\(`, false)
			otherDefs := grepSumNoNoise(ctx, dir, `(const|let|var)[[:space:]]+`+q+`[[:space:]]*=|class[[:space:]]+`+q+`[^A-Za-z0-9_]`, false)
			// Every fnDef line also matches the call pattern, so exact zero is
			// the only trustworthy "nothing calls it" reading; a negative
			// value means the patterns diverged — err silent.
			survivors = calls - fnDefs
			defined = fnDefs+otherDefs > 0
		}
		if defined && survivors == 0 {
			out = append(out, types.LineComment{
				FilePath: use.file, LineNumber: 0, Importance: "MEDIUM",
				CommentBody: fmt.Sprintf("**Mechanical alert — symbol de-wired.** This PR removes the last remaining call/render site of `%s` (tests excluded): the symbol is still defined but nothing invokes or mounts it anymore. If a replacement wiring was added, verify it is actually reachable on every entrypoint/platform the removed site served.", sym),
			})
		}
	}
	return out
}

// gateCallKeywords excludes language keywords/builtins that look like calls
// in removed lines; anything else is filtered by the defined-in-tree +
// zero-survivors requirement, which errs silent.
var gateCallKeywords = map[string]bool{
	"return": true, "while": true, "switch": true, "catch": true,
	"function": true, "assert": true, "await": true, "yield": true,
	"super": true, "constructor": true, "typeof": true, "delete": true,
	"throw": true, "case": true, "print": true, "import": true,
	"require": true, "expect": true, "describe": true, "isinstance": true,
}

// grepSumNoNoise sums per-file grep counts, dropping noise paths. Errors and
// no-matches both read as zero — the callers' silent default.
func grepSumNoNoise(ctx context.Context, dir, pattern string, word bool, pathspec ...string) int {
	counts, err := gitGrepFileCounts(ctx, dir, pattern, word, pathspec...)
	if err != nil {
		return 0
	}
	total := 0
	for _, fc := range counts {
		if !gateNoisePath(fc.Path) {
			total += fc.Count
		}
	}
	return total
}

// grepSumNoNoiseGlob is grepSumNoNoise restricted to files matching a
// doublestar glob ("" = whole tree). The glob is evaluated here rather than
// handed to git as a pathspec: GATE_TASK_*_GLOB is documented as doublestar
// syntax (and the producer side is matched with doublestar.Match), while
// git's pathspec engine both diverges on `*` (it crosses '/') and silently
// matches zero files on doublestar-only syntax like braces — which would
// read every candidate as "zero survivors" and false-fire the gate.
func grepSumNoNoiseGlob(ctx context.Context, dir, pattern string, word bool, glob string) int {
	counts, err := gitGrepFileCounts(ctx, dir, pattern, word)
	if err != nil {
		return 0
	}
	total := 0
	for _, fc := range counts {
		if gateNoisePath(fc.Path) {
			continue
		}
		if glob != "" {
			if ok, merr := doublestar.Match(glob, fc.Path); merr != nil || !ok {
				continue
			}
		}
		total += fc.Count
	}
	return total
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// runEffectCleanupGate implements gate 2.2 (GATE_EFFECT_CLEANUP).
//
// Two arms, both requiring a known leak-prone acquisition in the ADDED lines
// and no cleanup in the same added hunk (the tight-FP contract):
// (a) effect arm — an added useEffect/useLayoutEffect whose own hunk also
// contains the acquisition and no cleanup return / release call;
// (b) handler arm (.tsx/.jsx only) — the acquisition is wired to JSX event
// handlers with no effect at all in the change or the file: pointer/UI
// handlers never fire on unmount, so navigation leaks the resource.
func runEffectCleanupGate(ctx context.Context, dir string, files []diffFile) []types.LineComment {
	if !gateEffectEnabled {
		return nil
	}
	var out []types.LineComment
	for _, f := range files {
		if gateNoisePath(f.Path) || !hasAnySuffix(f.Path, ".ts", ".tsx", ".jsx") {
			continue
		}
		if len(f.Added) == 0 {
			continue
		}
		joined := "+" + strings.Join(f.Added, "\n+")
		// Effect arm, hunk-scoped: the acquisition and the missing cleanup
		// must sit in the SAME added hunk as the effect — whole-file
		// co-occurrence both false-fires (an inert added effect plus an
		// unrelated `.play(` handler elsewhere in the file) and false-
		// suppresses (a `.close(` added in unrelated code masking a genuinely
		// leaked acquisition). Producers without hunk info (test fixtures)
		// fall back to treating all added lines as one hunk.
		hunks := f.AddedHunks
		if hunks == nil {
			hunks = [][]string{f.Added}
		}
		hasHook := false
		fired := false
		for _, h := range hunks {
			hj := "+" + strings.Join(h, "\n+")
			if !gateEffectHook.MatchString(hj) {
				continue
			}
			hasHook = true
			if fired || gateEffectCleanup.MatchString(hj) {
				continue // this hunk carries its own cleanup — clean by contract
			}
			if m := gateEffectAcquire.FindStringSubmatch(hj); m != nil {
				fired = true
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: fmt.Sprintf("**Mechanical alert — resource acquired without unmount cleanup.** An added effect in this file acquires a leak-prone resource (`%s`) and the added code contains no cleanup return or release call. On unmount/re-render the resource keeps running. Cite the cleanup path, or add one.", strings.TrimSpace(m[1])),
				})
			}
		}
		if hasHook {
			continue // an effect owns this change; the handler arm is moot
		}
		if gateEffectCleanup.MatchString(joined) {
			continue // the change carries its own cleanup — clean by contract
		}
		// handler arm: component files only, and only when neither the change
		// nor the file has any effect hook to run an unmount cleanup.
		if !hasAnySuffix(f.Path, ".tsx", ".jsx") || !gateHandlerProp.MatchString(joined) {
			continue
		}
		acq := ""
		if m := gateHandlerOther.FindStringSubmatch(joined); m != nil {
			acq = strings.TrimSpace(m[1])
		} else {
			for _, m := range gateHandlerStart.FindAllString(joined, -1) {
				name := strings.TrimRight(strings.TrimSpace(m), "( \t")
				if name != "startTransition" {
					acq = name + "("
					break
				}
			}
		}
		if acq == "" {
			continue
		}
		if n, err := gitGrepCount(ctx, dir, `use(Layout)?Effect[[:space:]]*\(|componentWillUnmount`, false, f.Path); err == nil && n > 0 {
			continue // the component has an effect that may own the cleanup
		}
		out = append(out, types.LineComment{
			FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
			CommentBody: fmt.Sprintf("**Mechanical alert — resource acquired without unmount cleanup.** This change starts a leak-prone resource (`%s`) from JSX event handlers, and neither the change nor the component has an effect cleanup. UI handlers do not fire on unmount: navigating away leaves the resource running. Verify the stop path runs on unmount, not only on the mirroring UI event.", acq),
		})
	}
	return out
}

// runTaskSkewGate implements gate 2.3 (GATE_TASK_PRODUCER_GLOB).
//
// In task-producer files (env-configured glob, checked for queue/task
// markers), two payload-contract changes fire: (a) a serialized dict key
// present in removed lines that neither the added lines nor anywhere else in
// the tree still carries — the old payload shape is retired in the same
// deploy that stops producing it, so in-flight/old-code workers emit or read
// the wrong shape during the deploy window; (b) a queued function's
// signature changed — old-format messages already in the queue hit the new
// signature.
func runTaskSkewGate(ctx context.Context, dir string, files []diffFile) []types.LineComment {
	if gateTaskProducerGlob == "" {
		return nil
	}
	var out []types.LineComment
	for _, f := range files {
		if gateNoisePath(f.Path) {
			continue
		}
		if ok, err := doublestar.Match(gateTaskProducerGlob, f.Path); err != nil || !ok {
			continue
		}
		// Only files that actually hold deferred/queued task machinery.
		if n, err := gitGrepCount(ctx, dir, `@[[:alnum:]_]*(task|celery)|apply_async|\.delay\(`, false, f.Path); err != nil || n == 0 {
			continue
		}

		// (a) dropped serialized payload keys.
		removedKeys := dictKeys(f.Removed)
		addedKeys := dictKeys(f.Added)
		dropped := 0
		for _, key := range removedKeys.order {
			if addedKeys.contains(key) || dropped >= 5 {
				continue
			}
			dropped++
			pat := `["']` + regexp.QuoteMeta(key) + `["']`
			if grepSumNoNoiseGlob(ctx, dir, pat, false, gateTaskConsumerGlob) == 0 {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: fmt.Sprintf("**Mechanical alert — task payload contract changed.** The serialized payload key `%s` is removed by this PR and survives nowhere in the tree: the old payload shape is retired in the same deploy that stops producing it. During the deploy window, workers/readers still on old code emit or expect `%s` while new code does not — verify both directions of the window tolerate the other format.", key, key),
				})
			}
		}

		// (b) queued-function signature drift.
		removedSigs := pyDefSignatures(f.Removed)
		addedSigs := pyDefSignatures(f.Added)
		drifts := 0
		for name, oldParams := range removedSigs {
			newParams, ok := addedSigs[name]
			if !ok || oldParams == newParams || drifts >= 3 {
				continue
			}
			if n, err := gitGrepCount(ctx, dir, regexp.QuoteMeta(name)+`\.(delay|apply_async)\(`, false); err != nil || n == 0 {
				continue
			}
			drifts++
			out = append(out, types.LineComment{
				FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
				CommentBody: fmt.Sprintf("**Mechanical alert — task payload contract changed.** The queued task `%s` changed its signature (`%s` -> `%s`). Messages enqueued by old code are still in flight during the deploy window and will be delivered to the new signature — verify the new code accepts the old argument shape (defaults/compat), or that the queue drains first.", name, oldParams, newParams),
			})
		}
	}
	return out
}

// dictKeys extracts serialized-dict key literals ("key": / 'key':) from diff
// lines, in first-seen order, as an order-preserving membership map.
func dictKeys(lines []string) mapOrder {
	m := mapOrder{set: map[string]bool{}}
	for _, l := range lines {
		for _, g := range gateDictKey.FindAllStringSubmatch(l, -1) {
			if !m.set[g[1]] {
				m.set[g[1]] = true
				m.order = append(m.order, g[1])
			}
		}
	}
	return m
}

type mapOrder struct {
	set   map[string]bool
	order []string
}

func (m mapOrder) contains(k string) bool { return m.set[k] }

// pyDefSignatures maps def name -> first-line parameter text.
func pyDefSignatures(lines []string) map[string]string {
	sigs := map[string]string{}
	for _, l := range lines {
		if m := gatePyDefLine.FindStringSubmatch(l); m != nil {
			if _, dup := sigs[m[1]]; !dup {
				sigs[m[1]] = strings.Join(strings.Fields(m[2]), " ")
			}
		}
	}
	return sigs
}

// runMigrationResidueGate implements gate 2.4 (GATE_MIGRATION_RESIDUE).
//
// (a) literal-pair: a removed/added line pair identical except one quoted
// literal (the signature of a key/enum/value migration); the old literal is
// then searched for alongside its context identifier. (b) const-exclusion:
// an added comprehension-filter or .remove() that drops an enum-ish constant
// from accepted values. Either way, surviving references in OTHER files mean
// the migration missed consumers: cite at most 3; above the survivor
// threshold the alert is suppressed entirely (a shared constant, not a
// migration).
func runMigrationResidueGate(ctx context.Context, dir string, files []diffFile) []types.LineComment {
	if !gateResidueEnabled {
		return nil
	}
	changed := map[string]bool{}
	for _, f := range files {
		changed[f.Path] = true
	}
	// residueSurvivors counts matching lines outside the PR's own files and
	// noise paths, returning up to 3 citation files.
	residueSurvivors := func(pattern string, word bool) (int, []string) {
		counts, err := gitGrepFileCounts(ctx, dir, pattern, word)
		if err != nil {
			return 0, nil
		}
		total := 0
		var cite []string
		for _, fc := range counts {
			if changed[fc.Path] || gateNoisePath(fc.Path) {
				continue
			}
			total += fc.Count
			if len(cite) < 3 {
				cite = append(cite, fc.Path)
			}
		}
		return total, cite
	}

	var out []types.LineComment
	pairs := 0
	consts := 0
	seenLit := map[string]bool{}
	seenConst := map[string]bool{}
	for _, f := range files {
		if gateNoisePath(f.Path) || f.Status == "removed" {
			continue
		}

		// (a) literal-pair rename.
		masked := map[string][]string{}
		for _, l := range f.Added {
			if gateQuotedLit.MatchString(l) {
				k := gateQuotedLit.ReplaceAllString(l, `"§"`)
				masked[k] = append(masked[k], l)
			}
		}
		for _, l := range f.Removed {
			if pairs >= 8 {
				break
			}
			if !gateQuotedLit.MatchString(l) {
				continue
			}
			old, new_, ok := literalPairDiff(l, masked[gateQuotedLit.ReplaceAllString(l, `"§"`)])
			if !ok || len(old) < 2 || seenLit[old] {
				continue
			}
			seenLit[old] = true
			ctxIdent := contextIdentifier(l, old)
			if ctxIdent == "" {
				continue
			}
			pairs++
			qc, qo := regexp.QuoteMeta(ctxIdent), regexp.QuoteMeta(old)
			pat := qc + `.*["']` + qo + `["']|["']` + qo + `["'].*` + qc
			n, cite := residueSurvivors(pat, false)
			if n >= 1 && n <= gateResidueMaxSurvivors {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: fmt.Sprintf("**Mechanical alert — migration residue.** This PR migrates the value `%s` -> `%s` (context `%s`) in %s, but %d line(s) in other files still carry the old value, e.g.: %s. Consumers still on the old value silently stop matching — verify each survivor was intentionally left.", old, new_, ctxIdent, f.Path, n, strings.Join(cite, ", ")),
				})
			}
		}

		// (b) enum-constant exclusion.
		for _, l := range f.Added {
			if consts >= 5 {
				break
			}
			m := gateConstExclusion.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if name == "" || seenConst[name] {
				continue
			}
			seenConst[name] = true
			consts++
			n, cite := residueSurvivors(regexp.QuoteMeta(name), true)
			if n >= 1 && n <= gateResidueMaxSurvivors {
				out = append(out, types.LineComment{
					FilePath: f.Path, LineNumber: 0, Importance: "MEDIUM",
					CommentBody: fmt.Sprintf("**Mechanical alert — migration residue.** This PR stops accepting the enum/key constant `%s` in %s, but %d line(s) in other files still reference it, e.g.: %s. A producer or consumer still emitting the old value now fails validation or is silently dropped — verify each survivor.", name, f.Path, n, strings.Join(cite, ", ")),
				})
			}
		}
	}
	return out
}

// literalPairDiff finds an added line whose only difference from the removed
// line is exactly one quoted literal, returning (old, new) unquoted.
func literalPairDiff(removed string, addedCandidates []string) (string, string, bool) {
	oldLits := gateQuotedLit.FindAllString(removed, -1)
	for _, a := range addedCandidates {
		newLits := gateQuotedLit.FindAllString(a, -1)
		if len(newLits) != len(oldLits) {
			continue
		}
		diffAt := -1
		for i := range oldLits {
			if oldLits[i] != newLits[i] {
				if diffAt >= 0 {
					diffAt = -2
					break
				}
				diffAt = i
			}
		}
		if diffAt >= 0 {
			old := strings.Trim(oldLits[diffAt], `"'`)
			new_ := strings.Trim(newLits[diffAt], `"'`)
			if old != "" && old != new_ {
				return old, new_, true
			}
		}
	}
	return "", "", false
}

// contextIdentifier picks the identifier nearest before the old literal on
// the removed line (e.g. the key or attribute name being assigned), the
// co-occurrence anchor that makes short literals greppable.
func contextIdentifier(line, oldLit string) string {
	idx := strings.Index(line, oldLit)
	if idx < 0 {
		idx = len(line)
	}
	toks := gateIdentTok.FindAllString(line[:idx], -1)
	if len(toks) > 0 {
		return toks[len(toks)-1]
	}
	if toks := gateIdentTok.FindAllString(line[idx:], -1); len(toks) > 0 {
		return toks[0]
	}
	return ""
}

// parseNameStatusDiff builds diffFiles from `git diff` output: the name-status
// listing plus the unified diff, both against base...HEAD in the worktree.
func parseNameStatusDiff(ctx context.Context, dir, base string) ([]diffFile, error) {
	ns, err := runGit(ctx, dir, "diff", "--name-status", base+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("name-status: %w (%s)", err, ns)
	}
	status := map[string]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(ns), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		st := "modified"
		switch parts[0][0] {
		case 'A':
			st = "added"
		case 'D':
			st = "removed"
		}
		p := parts[len(parts)-1]
		status[p] = st
		order = append(order, p)
	}

	full, err := runGit(ctx, dir, "diff", "--unified=0", base+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("unified diff: %w", err)
	}
	added := map[string][]string{}
	removed := map[string][]string{}
	addedHunks := map[string][][]string{}
	cur := ""
	// With --unified=0 a hunk's added lines are one contiguous '+' run, so
	// hunk grouping is "flush the pending run on any non-'+' line".
	var pendingHunk []string
	flushHunk := func(file string) {
		if len(pendingHunk) > 0 && file != "" {
			addedHunks[file] = append(addedHunks[file], pendingHunk)
		}
		pendingHunk = nil
	}
	for _, line := range strings.Split(full, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flushHunk(cur)
			cur = "" // reset so one file's hunks don't attach to the previous file
			continue
		}
		// A deleted file has `--- a/<path>` / `+++ /dev/null`, so the old-side
		// path is the only name it carries. Whole-file deletion is the
		// strongest omission signal there is — those removed lines must feed
		// the matchers. For modified files the `+++ b/` line overwrites this.
		if strings.HasPrefix(line, "--- a/") {
			flushHunk(cur)
			cur = strings.TrimPrefix(line, "--- a/")
			continue
		}
		if strings.HasPrefix(line, "+++ b/") {
			flushHunk(cur)
			cur = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if cur == "" {
			continue
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added[cur] = append(added[cur], line[1:])
			pendingHunk = append(pendingHunk, line[1:])
		} else {
			flushHunk(cur)
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed[cur] = append(removed[cur], line[1:])
			}
		}
	}
	flushHunk(cur)

	var files []diffFile
	for _, p := range order {
		files = append(files, diffFile{Path: p, Added: added[p], Removed: removed[p], Status: status[p], AddedHunks: addedHunks[p]})
	}
	return files, nil
}

// diffFilesForWorktree parses the PR's diff against origin/<defaultBranch>.
// Best-effort — on any error (or a pathological >20k-added-lines diff) it
// returns nil, which downstream consumers (gates, bug memory) treat as
// "no signal", never as a review failure.
func diffFilesForWorktree(ctx context.Context, dir, defaultBranch, token, repoLockKey string, prNumber int) []diffFile {
	if dir == "" {
		return nil
	}
	// The poller does not always know the base branch; origin/HEAD points at
	// the clone's default branch and works for the three-dot diff.
	base := "origin/HEAD"
	if defaultBranch != "" {
		base = "origin/" + defaultBranch
	}
	files, err := parseNameStatusDiff(ctx, dir, base)
	if err != nil {
		// Two known causes. (1) The cache clone is single-branch (--depth
		// implies --single-branch), so origin/<base> simply does not exist
		// for a PR based on any branch other than the one the cache was
		// initialized with — no amount of deepening creates it. (2) A shallow
		// clone (prod uses --depth 200) has no merge-base for the three-dot
		// diff — fresh PRs fit the window; old PRs (replays, benchmarks) do
		// not. Fetch the base ref and deepen once, then retry, rather than
		// silently reporting "no signal".
		log.Printf("[GATES] diff vs %s failed (%v) — fetching the base ref, deepening history, and retrying", base, err)
		// Deepening writes the shared repo's shallow file; concurrent cache
		// fetches for the same repo fail on shallow.lock instead of waiting,
		// so hold the same per-repo mutex cloneForAgent uses for cache work.
		deepen := func() error {
			if repoLockKey != "" {
				m := cacheLock(repoLockKey)
				m.Lock()
				defer m.Unlock()
			}
			// Cause (1): create/refresh origin/<base> explicitly — a
			// single-branch clone's fetch refspec never will. Best-effort:
			// when the ref already existed, the real failure is a severed
			// merge-base and the unshallow below is the actual fix.
			if defaultBranch != "" {
				bargs := authHeaderArgs(token)
				bargs = append(bargs, "fetch", "--quiet", "--depth", "200", "origin",
					fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", defaultBranch, defaultBranch))
				if bout, berr := runGit(ctx, dir, bargs...); berr != nil {
					log.Printf("[GATES] base-ref fetch for %s failed: %v (%s)", base, berr, redactToken(bout, token))
				}
			}
			// Cause (2): a bounded deepen cannot reconnect an old PR: the
			// PR-head ref is itself fetched shallow, so its ancestry is
			// severed from the base branch regardless of how deep the base
			// goes. Fetch the PR ref with --unshallow, which completes both
			// sides of the graph until they connect. One-time cost per
			// cache repo.
			args := authHeaderArgs(token)
			args = append(args, "fetch", "--quiet", "--unshallow", "origin")
			if prNumber > 0 {
				args = append(args, fmt.Sprintf("+pull/%d/head:refs/agent-pr/%d", prNumber, prNumber))
			}
			out, derr := runGit(ctx, dir, args...)
			if derr != nil && strings.Contains(out, "does not make sense") {
				// Already unshallow: refetch just the PR ref to connect it.
				// With no PR ref to fetch, the missing piece can only have
				// been the base ref handled above — retry the diff.
				if prNumber > 0 {
					args = authHeaderArgs(token)
					args = append(args, "fetch", "--quiet", "origin",
						fmt.Sprintf("+pull/%d/head:refs/agent-pr/%d", prNumber, prNumber))
					out, derr = runGit(ctx, dir, args...)
				} else {
					derr = nil
				}
			}
			if derr != nil {
				return fmt.Errorf("%v (%s)", derr, redactToken(out, token))
			}
			return nil
		}
		if derr := deepen(); derr != nil {
			log.Printf("[GATES] deepen failed: %v — no deterministic signals for this review", derr)
			return nil
		}
		files, err = parseNameStatusDiff(ctx, dir, base)
		if err != nil {
			log.Printf("[GATES] diff still failing after deepen: %v — no deterministic signals for this review", err)
			return nil
		}
	}
	// Guard pathological diffs: gates and memory are per-line regex scans.
	total := 0
	for _, f := range files {
		total += len(f.Added) + len(f.Removed)
	}
	if total > 20000 {
		return nil
	}
	return files
}

// OfflineWorktreeReport is the deterministic-signal report for one worktree:
// what the gates and the bug-memory matcher would contribute to a review of
// this diff. Used by cmd/gatecheck for offline validation sweeps.
type OfflineWorktreeReport struct {
	DiffParsed bool                `json:"diff_parsed"` // false = parse error or pathological diff (distinct from "quiet")
	Files      int                 `json:"files"`
	Gates      []types.LineComment `json:"gates"`
	BugMemory  BugMemoryMatch      `json:"bug_memory"`
}

// OfflineCheckWorktree runs gates + bug-memory matching against a checked-out
// worktree exactly as production would (same diff parse, same matchers).
func OfflineCheckWorktree(ctx context.Context, dir, defaultBranch string, lib *BugMemoryLibrary, owner, repo string, prNumber int) OfflineWorktreeReport {
	rep := OfflineWorktreeReport{}
	files := diffFilesForWorktree(ctx, dir, defaultBranch, "", "", prNumber)
	if files == nil {
		return rep
	}
	rep.DiffParsed = true
	rep.Files = len(files)
	rep.Gates = RunMechanicalGates(ctx, dir, files)
	_, rep.BugMemory = MatchBugMemory(lib, files, owner, repo, prNumber)
	return rep
}

// GatesForWorktree diffs the worktree and runs all gates. Best-effort — on
// any error it returns nil findings (gates are advisory; they must never
// fail a review).
func GatesForWorktree(ctx context.Context, dir, defaultBranch string) []types.LineComment {
	files := diffFilesForWorktree(ctx, dir, defaultBranch, "", "", 0)
	if files == nil {
		return nil
	}
	return RunMechanicalGates(ctx, dir, files)
}
