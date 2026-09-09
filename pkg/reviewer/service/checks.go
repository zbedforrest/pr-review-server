package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"pr-review-server/pkg/reviewer/types"
)

// Required checks: convert the review's deterministic signals (fired
// mechanical gates + matched bug-memory entries) into forced-choice questions
// the agent must answer inside its findings JSON. Delivering a signal into
// the prompt is necessary but measured to be insufficient — the agent reaches
// the right code and still declines to claim the defect — so each check
// demands a machine-parsed verdict backed by file:line evidence, and silence
// or evidence-free endorsement has a deterministic cost computed here in Go
// (EnforceRequiredChecks), never a hoped-for model behavior.
//
// Feature-gated by REQUIRED_CHECKS (default off). With the flag unset the
// prompt and the review output are byte-identical to a checkless build.

const (
	// requiredChecksMax caps checks per review; gates fill the budget first
	// (higher measured precision), then memory entries. The cap drops on very
	// large diffs so the added agent turns cannot push heavy PRs over the
	// wall-clock budget (a timeout loses the entire review).
	requiredChecksMax          = 4
	requiredChecksMaxLargeDiff = 2
	requiredChecksLargeDiff    = 5000 // changed lines beyond which the reduced cap applies

	// checkFilePath is the reserved file_path for check answers, mirroring
	// the SUMMARY convention so parseAgentJSON needs no changes.
	checkFilePath = "CHECK"
)

// RequiredCheck is one forced-choice verification the agent must answer.
type RequiredCheck struct {
	ID         string // stable kebab-case id, e.g. "CHK-portal-layer-1"
	Source     string // "gate" or "memory"
	Question   string // imperative question including the VIOLATED/SAFE contract
	TargetFile string // file the underlying signal anchors to ("" when unresolvable)

	gateIndex int               // Source=="gate": index into the gate-alert slice passed to BuildRequiredChecks
	memAlert  types.LineComment // Source=="memory": alert re-admitted when the check goes unanswered
}

// CheckAnswer is one parsed check response from the agent's output.
type CheckAnswer struct {
	ID       string
	Verdict  string // "VIOLATED", "SAFE", or "NOT-APPLICABLE"
	Evidence string // raw EVIDENCE segment ("" when absent)
	Body     string // full comment body (quoted verbatim in synthesized findings)
}

// RequiredCheckTelemetry is the conversion-funnel counters for one review.
// Issued/Answered measure grammar compliance; Violated/EvidenceOK measure
// what the answers actually claimed and whether the evidence resolved.
type RequiredCheckTelemetry struct {
	ChecksIssued     int `json:"checks_issued"`
	ChecksAnswered   int `json:"checks_answered"`
	ChecksViolated   int `json:"checks_violated"`
	ChecksEvidenceOK int `json:"checks_evidence_ok"`
	// Records is the per-check ledger the report renders in its review
	// details; it replaces the table that used to be appended to the SUMMARY.
	Records []RequiredCheckRecord `json:"records,omitempty"`
}

// RequiredCheckRecord is what one issued check asked and what came back.
// EvidenceResolved means the cited path exists in the diff or worktree, not
// that the evidence proves the answer. Unresolved marks checks that stay
// open risk: unanswered, or cleared without resolvable evidence.
type RequiredCheckRecord struct {
	ID               string `json:"id"`
	Source           string `json:"source"`
	Question         string `json:"question"`
	TargetFile       string `json:"target_file,omitempty"`
	Verdict          string `json:"verdict"`
	Answer           string `json:"answer,omitempty"`
	EvidencePath     string `json:"evidence_path,omitempty"`
	EvidenceResolved bool   `json:"evidence_resolved"`
	Unresolved       bool   `json:"unresolved"`
}

// gateCheckKinds maps a stable phrase from each gate's alert body to that
// gate type's check-id slug. Keyed on the alert header text because gate
// findings are plain LineComments with no structured kind field; the phrases
// are owned by gates.go in this package, so the coupling is local.
var gateCheckKinds = []struct{ marker, slug string }{
	{"portal overlay without an explicit layer", "portal-layer"},
	{"undefined settings reference", "settings-ref"},
	{"shared module edited", "shared-file"},
	{"new model property", "model-property"},
	{"backend topic without frontend counterpart", "registry-split"},
	{"new symbol with no references", "wiring"},
	{"symbol de-wired", "wiring"},
	{"resource acquired without unmount cleanup", "effect-cleanup"},
	{"task payload contract changed", "task-skew"},
	{"migration residue", "migration-residue"},
}

// backtickToken pulls the first `code` span out of an alert body (the gates
// put the offending identifier there).
var backtickToken = regexp.MustCompile("`([^`]+)`")

// nonKebabRun collapses to a dash in kebabSlug.
var nonKebabRun = regexp.MustCompile(`[^a-z0-9]+`)

// kebabSlug lowercases s and collapses every non-alphanumeric run to a single
// dash, producing the id-safe slug the answer regex accepts.
func kebabSlug(s string) string {
	return strings.Trim(nonKebabRun.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// BuildRequiredChecks converts fired gate alerts and matched bug-memory
// entries into the per-PR check list. Gates come first, in firing order; then
// memory entries (already ranked by trigger-hit count) that carry a non-empty
// authored Check procedure. The total is capped at requiredChecksMax, reduced
// to requiredChecksMaxLargeDiff when the diff exceeds requiredChecksLargeDiff
// changed lines. files is the same parsed diff the gates and the memory
// matcher ran against; it anchors memory checks to a concrete changed file
// and supplies the changed-line count.
func BuildRequiredChecks(gateAlerts []types.LineComment, memEntries []BugMemoryEntry, files []diffFile) []RequiredCheck {
	limit := requiredChecksMax
	changed := 0
	for _, f := range files {
		changed += len(f.Added) + len(f.Removed)
	}
	if changed > requiredChecksLargeDiff {
		limit = requiredChecksMaxLargeDiff
	}

	var out []RequiredCheck
	ids := map[string]bool{}
	perKind := map[string]int{}
	for i, g := range gateAlerts {
		if len(out) >= limit {
			return out
		}
		slug := "mechanical"
		for _, k := range gateCheckKinds {
			if strings.Contains(g.CommentBody, k.marker) {
				slug = k.slug
				break
			}
		}
		perKind[slug]++
		id := fmt.Sprintf("CHK-%s-%d", slug, perKind[slug])
		ids[id] = true
		out = append(out, RequiredCheck{
			ID:         id,
			Source:     "gate",
			Question:   gateCheckQuestion(slug, g),
			TargetFile: g.FilePath,
			gateIndex:  i,
		})
	}

	for _, e := range memEntries {
		if len(out) >= limit {
			break
		}
		if strings.TrimSpace(e.Check) == "" {
			continue
		}
		slug := kebabSlug(e.Class)
		if slug == "" {
			slug = kebabSlug(e.ID)
		}
		if slug == "" {
			slug = "entry"
		}
		id := "CHK-mem-" + slug
		for n := 2; ids[id]; n++ {
			id = fmt.Sprintf("CHK-mem-%s-%d", slug, n)
		}
		ids[id] = true
		target := memoryCheckTarget(e, files)
		out = append(out, RequiredCheck{
			ID:         id,
			Source:     "memory",
			Question:   e.Check,
			TargetFile: target,
			gateIndex:  -1,
			memAlert: types.LineComment{
				FilePath: target, LineNumber: 0, Importance: "MEDIUM",
				CommentBody: fmt.Sprintf("**Bug-memory alert — past failure pattern (%s).** %s", e.ID, e.Pattern),
			},
		})
	}
	return out
}

// gateCheckQuestion renders the question template for one gate firing. The
// templates are generic on purpose: anything repo-specific (paths, settings
// keys, class names) arrives through the alert the gate itself produced.
// Every template demands file:line evidence and defines VIOLATED vs SAFE.
func gateCheckQuestion(slug string, alert types.LineComment) string {
	switch slug {
	case "portal-layer":
		return fmt.Sprintf("%s renders portal-based UI without an explicit stacking layer. Trace where the portal mounts and which stacking layer it lands on, including under browser fullscreen (only descendants of the fullscreen element are visible there). VIOLATED = it renders on the default layer or into a target that breaks under fullscreen/modal stacking — cite the mount site file:line and the user-visible failure. SAFE = cite the file:line where an explicit layer (or an equivalent stacking guarantee) is set.", alert.FilePath)
	case "settings-ref":
		ref := "the flagged settings reference"
		if m := gateSettingsRef.FindStringSubmatch(alert.CommentBody); m != nil {
			ref = "`settings." + m[1] + "`"
		}
		return fmt.Sprintf("%s references %s but the name is assigned nowhere in the tree. Find the first runtime path that evaluates the reference. VIOLATED = a reachable path evaluates it (AttributeError at runtime) — cite the caller chain file:line. SAFE = every reference is provably dead or the name is defined dynamically — cite the file:line or the command run + result that proves it.", alert.FilePath, ref)
	case "shared-file":
		return fmt.Sprintf("This PR edits shared/base module(s) (%s and any others listed in the shared-module alert). Enumerate the consumers of each edited symbol (e.g. via git grep) and determine, per consumer, whether its behavior changes. VIOLATED = a consumer breaks or silently changes behavior — cite the consumer file:line. SAFE = cite the command run + result showing every consumer is unaffected.", alert.FilePath)
	case "model-property":
		return fmt.Sprintf("%s adds a property to a model class. For each subclass and consumer, determine what the new property returns or raises, and check existing hasattr/getattr call sites. VIOLATED = a subclass or call site now misbehaves (wrong value or new exception) — cite the file:line. SAFE = cite the file:line of the subclasses/call sites you checked (or the command run + result) showing the contract holds.", alert.FilePath)
	case "registry-split":
		cls := "the newly registered class"
		if m := backtickToken.FindStringSubmatch(alert.CommentBody); m != nil {
			cls = "`" + m[1] + "`"
		}
		return fmt.Sprintf("%s registers %s on the backend and no same-named frontend counterpart was found. VIOLATED = the counterpart is genuinely missing (clients cannot handle the unknown topic) — cite the backend registration file:line. SAFE = cite the frontend counterpart class's file:line.", alert.FilePath, cls)
	case "wiring":
		sym := "the flagged symbol"
		if m := backtickToken.FindStringSubmatch(alert.CommentBody); m != nil {
			sym = "`" + m[1] + "`"
		}
		return fmt.Sprintf("%s changes the wiring of %s: either it is newly added with nothing referencing it, or this diff removed its last call/render site. Trace the runtime path from the relevant entrypoint(s) — including each platform variant the code serves — to the symbol. VIOLATED = no reachable path invokes/mounts it where it is needed (dead or de-wired) — cite the entrypoint file:line where the wiring is missing and the user-visible effect. SAFE = cite the caller/mount chain file:line that reaches it (or the command run + result proving reachability).", alert.FilePath, sym)
	case "effect-cleanup":
		res := "the acquired resource"
		if m := backtickToken.FindStringSubmatch(alert.CommentBody); m != nil {
			res = "`" + m[1] + "`"
		}
		return fmt.Sprintf("%s acquires a leak-prone resource (%s) without an unmount cleanup in the added code. Identify where the resource is released when the component unmounts (not merely on a mirroring UI event — pointer/UI handlers do not fire on unmount or navigation). VIOLATED = no unmount path releases it, so navigating away leaks it — cite the acquisition file:line and state what keeps running. SAFE = cite the cleanup file:line (effect cleanup return, unmount hook, or an equivalent guarantee).", alert.FilePath, res)
	case "task-skew":
		tok := "the flagged payload element"
		if m := backtickToken.FindStringSubmatch(alert.CommentBody); m != nil {
			tok = "`" + m[1] + "`"
		}
		return fmt.Sprintf("%s changes the payload contract of a deferred/queued task (%s). During a deploy, old-code producers and workers coexist with new code and messages are already in flight: determine what happens when an old-format payload meets new code, and when old code meets a new-format payload. VIOLATED = either direction fails or renders wrong during the window — cite the incompatible read/write file:line. SAFE = cite the compat handling file:line (both formats accepted) or the evidence that no old-format payload can cross the deploy.", alert.FilePath, tok)
	case "migration-residue":
		val := "the migrated value"
		if m := backtickToken.FindStringSubmatch(alert.CommentBody); m != nil {
			val = "`" + m[1] + "`"
		}
		return fmt.Sprintf("%s migrates %s but other files still carry the old value (survivors listed in the alert). For each cited survivor, determine whether it still produces or consumes the old value on a reachable runtime path. VIOLATED = a surviving producer/consumer now breaks (fails validation, silently stops matching) — cite the survivor file:line and the failure. SAFE = cite file:line evidence that every survivor is dead code, test-only, or intentionally accepts both values.", alert.FilePath, val)
	default:
		return fmt.Sprintf("Resolve this mechanical alert with evidence from this PR's code: %s VIOLATED = the alerted risk is real — cite the defect file:line. SAFE = cite the file:line or the command run + result that clears it.", alert.CommentBody)
	}
}

// memoryCheckTarget anchors a memory-derived check to a concrete changed
// file, mirroring MatchBugMemory's trigger logic: first path-glob match, else
// the first file whose diff lines contain a trigger keyword.
func memoryCheckTarget(e BugMemoryEntry, files []diffFile) string {
	for _, g := range e.TriggerPaths {
		for _, f := range files {
			if ok, err := doublestar.Match(g, f.Path); err == nil && ok {
				return f.Path
			}
		}
	}
	for _, kw := range e.TriggerKeywords {
		k := strings.ToLower(kw)
		for _, f := range files {
			for _, l := range f.Added {
				if strings.Contains(strings.ToLower(l), k) {
					return f.Path
				}
			}
			for _, l := range f.Removed {
				if strings.Contains(strings.ToLower(l), k) {
					return f.Path
				}
			}
		}
	}
	return ""
}

// requiredChecksSection renders the prompt block for the issued checks.
// Empty checks render nothing, keeping the prompt byte-identical to a
// checkless build.
func requiredChecksSection(checks []RequiredCheck) string {
	if len(checks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(promptRequiredChecksContract)
	for _, c := range checks {
		b.WriteString("- " + c.ID + ": " + c.Question + "\n")
	}
	return b.String()
}

// checkAnswerRe parses '<CHK-id> | VERDICT | ...'. Tolerant of surrounding
// whitespace; anything that doesn't match (lowercase verdict, prose, missing
// pipes) is an unanswered check — the safe default, since enforcement
// escalates unanswered checks rather than clearing them.
var checkAnswerRe = regexp.MustCompile(`^(CHK-[a-z0-9-]+)\s*\|\s*(VIOLATED|SAFE|NOT-APPLICABLE)\b`)

// ParseCheckAnswers strips reserved file_path=="CHECK" entries out of the
// parsed findings and returns the remaining findings plus the structured
// answers. Unparseable CHECK entries are dropped without producing an answer:
// an unparseable answer is an unanswered check.
func ParseCheckAnswers(comments []types.LineComment) ([]types.LineComment, []CheckAnswer) {
	var kept []types.LineComment
	var answers []CheckAnswer
	for _, c := range comments {
		if c.FilePath != checkFilePath {
			kept = append(kept, c)
			continue
		}
		body := strings.TrimSpace(c.CommentBody)
		m := checkAnswerRe.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		answers = append(answers, CheckAnswer{
			ID:       m[1],
			Verdict:  m[2],
			Evidence: evidenceSegment(body),
			Body:     body,
		})
	}
	return kept, answers
}

// evidenceSegment extracts the text after "EVIDENCE:" up to the trailing
// pipe-separated reason (or the end of the body). Returns "" when absent.
// The reason boundary is the first spaced pipe (" | ", how the grammar
// renders its separators): a bare '|' inside the evidence itself — a regex
// alternation or shell pipeline in the cited command — must not truncate
// the citation, and a reason containing '|' must not leak fragments back
// into the evidence.
func evidenceSegment(body string) string {
	idx := strings.Index(strings.ToUpper(body), "EVIDENCE:")
	if idx < 0 {
		return ""
	}
	rest := body[idx+len("EVIDENCE:"):]
	if cut := strings.Index(rest, " | "); cut >= 0 {
		rest = rest[:cut]
	}
	return strings.TrimSpace(rest)
}

// pathToken tokenizes evidence text into path-like candidates.
var pathToken = regexp.MustCompile(`[\w./-]+`)

// evidenceCitedPath returns the first path-like token in evidence that names
// a file changed by the PR or present in the worktree, or "" when the
// evidence is empty or cites nothing real. Deliberately a simple
// deterministic heuristic: the answer contract asks for a 'file:line read or
// command run + result', so genuine evidence virtually always contains at
// least one resolvable path.
func evidenceCitedPath(evidence string, diffPaths []string, worktreeDir string) string {
	for _, tok := range pathToken.FindAllString(evidence, -1) {
		tok = strings.Trim(tok, "./")
		if len(tok) < 3 || !strings.ContainsAny(tok, "./") {
			continue // not path-like
		}
		for _, p := range diffPaths {
			if p == tok || strings.HasSuffix(p, "/"+tok) || strings.HasSuffix(tok, "/"+p) {
				return tok
			}
		}
		if worktreeDir != "" && !filepath.IsAbs(tok) && !strings.Contains(tok, "..") {
			if _, err := os.Stat(filepath.Join(worktreeDir, tok)); err == nil {
				return tok
			}
		}
	}
	return ""
}

// EnforceRequiredChecks applies the answer-or-escalate rules after
// parseAgentJSON and before the poller merge:
//
//	(a) VIOLATED with no accompanying finding on the check's target file →
//	    synthesize one there from the answer body, capped MEDIUM
//	    (machine-assembled text must not create blockers).
//	(b) SAFE / NOT-APPLICABLE whose evidence is empty or cites no path in the
//	    diff or worktree → treated as unanswered.
//	(c) Unanswered → the underlying signal is re-admitted: gate alerts (which
//	    already merge with provenance "mechanical") get the unresolved-risk
//	    note appended; memory-derived checks, which have no merge presence
//	    otherwise, emit their alert into the escalated set.
//	(d) A check ledger (id | verdict | evidence) is appended to the SUMMARY
//	    entry, created if the agent didn't emit one.
//
// comments must already have CHECK entries stripped (ParseCheckAnswers).
// Returns the comments with the ledger applied, a copy of gates with notes
// appended, the escalated findings (merged by the caller as a lower-priority
// FindingSet so the existing provenance/severity-cap handling is reused), and
// the funnel telemetry.
func EnforceRequiredChecks(
	checks []RequiredCheck,
	answers []CheckAnswer,
	comments, gates []types.LineComment,
	diffPaths []string,
	worktreeDir string,
) (outComments, outGates, escalated []types.LineComment, tel RequiredCheckTelemetry) {
	outComments = comments
	outGates = append([]types.LineComment(nil), gates...)
	tel.ChecksIssued = len(checks)
	if len(checks) == 0 {
		return outComments, outGates, nil, tel
	}

	byID := map[string]*CheckAnswer{}
	for i := range answers {
		if _, ok := byID[answers[i].ID]; !ok {
			byID[answers[i].ID] = &answers[i] // first answer per id wins
		}
	}

	for _, c := range checks {
		ans := byID[c.ID]
		evPath := ""
		if ans != nil {
			tel.ChecksAnswered++
			evPath = evidenceCitedPath(ans.Evidence, diffPaths, worktreeDir)
			if evPath != "" {
				tel.ChecksEvidenceOK++
			}
		}

		verdict := "UNANSWERED"
		unresolved := ans == nil
		switch {
		case ans == nil:
		case ans.Verdict == "VIOLATED":
			tel.ChecksViolated++
			verdict = ans.Verdict
			anchor := c.TargetFile
			if anchor == "" {
				anchor = evPath
			}
			if !hasFindingOnFile(outComments, anchor) {
				escalated = append(escalated, types.LineComment{
					FilePath:   anchor,
					LineNumber: 0,
					Importance: "MEDIUM",
					CommentBody: fmt.Sprintf(
						"**Required check %s answered VIOLATED without an accompanying finding — escalated automatically.** %s",
						c.ID, ans.Body),
				})
			}
		default: // SAFE / NOT-APPLICABLE
			verdict = ans.Verdict
			if evPath == "" {
				unresolved = true // rule (b): evidence-free clearance is no clearance
			}
		}

		if unresolved {
			note := fmt.Sprintf(
				"\n\n_Required check %s was not answered with evidence — treat as unresolved risk, not a cleared concern._",
				c.ID)
			if c.Source == "gate" && c.gateIndex >= 0 && c.gateIndex < len(outGates) {
				outGates[c.gateIndex].CommentBody += note
			} else {
				alert := c.memAlert
				alert.CommentBody += note
				escalated = append(escalated, alert)
			}
		}

		record := RequiredCheckRecord{
			ID: c.ID, Source: c.Source, Question: c.Question, TargetFile: c.TargetFile,
			Verdict: verdict, EvidencePath: evPath, EvidenceResolved: evPath != "", Unresolved: unresolved,
		}
		if ans != nil {
			record.Answer = strings.TrimSpace(ans.Body)
		}
		tel.Records = append(tel.Records, record)
	}
	return outComments, outGates, escalated, tel
}

// hasFindingOnFile reports whether any non-SUMMARY finding targets file.
// Matching is strict (exact or whole-path suffix): a bare-basename match
// would let an unrelated finding on another directory's models.py or
// index.ts — exactly the names the gates target — suppress a synthesis.
func hasFindingOnFile(comments []types.LineComment, file string) bool {
	for _, f := range comments {
		if f.FilePath != "SUMMARY" && sameFileStrict(f.FilePath, file) {
			return true
		}
	}
	return false
}
