package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

// gateAlertFixture returns a representative alert for each gate kind, with
// the body phrases BuildRequiredChecks keys on (owned by gates.go).
func gateAlertFixture(kind, file string) types.LineComment {
	bodies := map[string]string{
		"portal-layer":   "**Mechanical alert — portal overlay without an explicit layer.** This change renders portal-based UI without a `layer` prop.",
		"settings-ref":   "**Mechanical alert — undefined settings reference.** `settings.MISSING_KEY` is referenced by this PR but `MISSING_KEY` is not assigned anywhere in the tree. At runtime this raises AttributeError on first use.",
		"shared-file":    "**Mechanical alert — shared module edited.** This PR modifies 2 shared/base module(s): common/util.py, common/base.ts.",
		"model-property": "**Mechanical alert — new model property.** This change adds a property to a Django model.",
		"registry-split": "**Mechanical alert — backend topic without frontend counterpart.** `OrphanTopic` is registered on the backend, but no class of that name exists under `fe/`.",
		// Round-3 gates (owned by gates.go).
		"wiring-dead":       "**Mechanical alert — new symbol with no references.** `orphan_fn` is added by this PR but has no references outside its definition file (tests excluded).",
		"wiring-dewired":    "**Mechanical alert — symbol de-wired.** This PR removes the last remaining call/render site of `OrphanWidget` (tests excluded).",
		"effect-cleanup":    "**Mechanical alert — resource acquired without unmount cleanup.** This change starts a leak-prone resource (`startPolling(`) from JSX event handlers.",
		"task-skew":         "**Mechanical alert — task payload contract changed.** The serialized payload key `old_key` is removed by this PR and survives nowhere in the tree.",
		"migration-residue": "**Mechanical alert — migration residue.** This PR migrates the value `legacy_kind` -> `modern_kind` (context `kind`) in app/config.py, but 2 line(s) in other files still carry the old value.",
	}
	return types.LineComment{FilePath: file, LineNumber: 0, Importance: "MEDIUM", CommentBody: bodies[kind]}
}

func mkCheckEntry(id, class, check string, paths, kws []string) BugMemoryEntry {
	e := mkEntry(id, class, "pattern for "+id, paths, kws, 1)
	e.Check = check
	return e
}

func TestBuildRequiredChecks_TemplatesAndOrdering(t *testing.T) {
	gates := []types.LineComment{
		gateAlertFixture("shared-file", "common/util.py"),
		gateAlertFixture("settings-ref", "photo/tasks.py"),
	}
	mem := []BugMemoryEntry{
		mkCheckEntry("ctx-menu", "menu-id", "For each handler, identify the id it resolves against at invocation time.", []string{"src/menus/**"}, nil),
		mkEntry("no-check", "other", "plain prior", []string{"**"}, nil, 2), // no Check field -> no check
	}
	files := []diffFile{{Path: "src/menus/Item.tsx", Status: "modified", Added: []string{"x"}}}

	got := BuildRequiredChecks(gates, mem, files)
	if len(got) != 3 {
		t.Fatalf("want 3 checks (2 gates + 1 memory), got %d: %+v", len(got), got)
	}
	// Gates first, in firing order; ids are stable kebab-case per kind.
	if got[0].ID != "CHK-shared-file-1" || got[1].ID != "CHK-settings-ref-1" || got[2].ID != "CHK-mem-menu-id" {
		t.Errorf("ids/order wrong: %q %q %q", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Source != "gate" || got[2].Source != "memory" {
		t.Errorf("sources wrong: %+v", got)
	}
	// Every question must define the verdicts and demand evidence.
	for _, c := range got {
		if c.Source == "gate" && (!strings.Contains(c.Question, "VIOLATED") || !strings.Contains(c.Question, "SAFE") || !strings.Contains(c.Question, "file:line")) {
			t.Errorf("%s: question missing verdict contract: %q", c.ID, c.Question)
		}
	}
	// Template selection: repo specifics flow in from the alert, not the template.
	if !strings.Contains(got[1].Question, "settings.MISSING_KEY") || !strings.Contains(got[1].Question, "photo/tasks.py") {
		t.Errorf("settings-ref question missing alert specifics: %q", got[1].Question)
	}
	if !strings.Contains(got[0].Question, "common/util.py") {
		t.Errorf("shared-file question missing target: %q", got[0].Question)
	}
	// Memory checks carry the authored procedure verbatim and anchor to the
	// glob-matched changed file.
	if got[2].Question != mem[0].Check {
		t.Errorf("memory question should be the authored Check: %q", got[2].Question)
	}
	if got[2].TargetFile != "src/menus/Item.tsx" {
		t.Errorf("memory target: got %q", got[2].TargetFile)
	}
	if got[0].TargetFile != "common/util.py" {
		t.Errorf("gate target: got %q", got[0].TargetFile)
	}
}

func TestBuildRequiredChecks_RegistrySplitAndModelTemplates(t *testing.T) {
	gates := []types.LineComment{
		gateAlertFixture("registry-split", "be_topics/room.py"),
		gateAlertFixture("model-property", "tipping/models.py"),
		gateAlertFixture("portal-layer", "app/Tooltip.tsx"),
	}
	got := BuildRequiredChecks(gates, nil, []diffFile{{Path: "be_topics/room.py"}})
	if len(got) != 3 {
		t.Fatalf("want 3, got %+v", got)
	}
	if got[0].ID != "CHK-registry-split-1" || !strings.Contains(got[0].Question, "`OrphanTopic`") {
		t.Errorf("registry-split: %q / %q", got[0].ID, got[0].Question)
	}
	if got[1].ID != "CHK-model-property-1" || !strings.Contains(got[1].Question, "hasattr/getattr") {
		t.Errorf("model-property: %q / %q", got[1].ID, got[1].Question)
	}
	if got[2].ID != "CHK-portal-layer-1" || !strings.Contains(got[2].Question, "fullscreen") {
		t.Errorf("portal-layer: %q / %q", got[2].ID, got[2].Question)
	}
}

func TestBuildRequiredChecks_Round3Templates(t *testing.T) {
	gates := []types.LineComment{
		gateAlertFixture("wiring-dead", "app/new.py"),
		gateAlertFixture("effect-cleanup", "app/Thumb.tsx"),
		gateAlertFixture("task-skew", "queue/tasks.py"),
		gateAlertFixture("migration-residue", "app/config.py"),
	}
	got := BuildRequiredChecks(gates, nil, []diffFile{{Path: "app/new.py"}})
	if len(got) != 4 {
		t.Fatalf("want 4, got %+v", got)
	}
	if got[0].ID != "CHK-wiring-1" || !strings.Contains(got[0].Question, "`orphan_fn`") ||
		!strings.Contains(got[0].Question, "entrypoint") {
		t.Errorf("wiring: %q / %q", got[0].ID, got[0].Question)
	}
	if got[1].ID != "CHK-effect-cleanup-1" || !strings.Contains(got[1].Question, "`startPolling(`") ||
		!strings.Contains(got[1].Question, "unmount") {
		t.Errorf("effect-cleanup: %q / %q", got[1].ID, got[1].Question)
	}
	if got[2].ID != "CHK-task-skew-1" || !strings.Contains(got[2].Question, "`old_key`") ||
		!strings.Contains(got[2].Question, "deploy") {
		t.Errorf("task-skew: %q / %q", got[2].ID, got[2].Question)
	}
	if got[3].ID != "CHK-migration-residue-1" || !strings.Contains(got[3].Question, "`legacy_kind`") ||
		!strings.Contains(got[3].Question, "survivor") {
		t.Errorf("migration-residue: %q / %q", got[3].ID, got[3].Question)
	}
	// Both wiring directions share the wiring template/slug.
	got = BuildRequiredChecks([]types.LineComment{gateAlertFixture("wiring-dewired", "app/page.tsx")}, nil, nil)
	if len(got) != 1 || got[0].ID != "CHK-wiring-1" || !strings.Contains(got[0].Question, "`OrphanWidget`") {
		t.Errorf("de-wired direction: %+v", got)
	}
	// Every round-3 template carries the verdict/evidence contract.
	for _, c := range BuildRequiredChecks(gates, nil, nil) {
		if !strings.Contains(c.Question, "VIOLATED") || !strings.Contains(c.Question, "SAFE") ||
			!strings.Contains(c.Question, "file:line") {
			t.Errorf("%s: missing verdict contract: %q", c.ID, c.Question)
		}
	}
}

func TestBuildRequiredChecks_CapsAndLargeDiff(t *testing.T) {
	var gates []types.LineComment
	for i := 0; i < 5; i++ {
		gates = append(gates, gateAlertFixture("portal-layer", fmt.Sprintf("app/P%d.tsx", i)))
	}
	mem := []BugMemoryEntry{mkCheckEntry("m1", "c1", "check it", []string{"**"}, nil)}
	small := []diffFile{{Path: "a.go", Added: []string{"one line"}}}

	got := BuildRequiredChecks(gates, mem, small)
	if len(got) != requiredChecksMax {
		t.Fatalf("want cap %d, got %d", requiredChecksMax, len(got))
	}
	// Gates fill the budget first — memory never displaces one.
	for i, c := range got {
		if c.Source != "gate" {
			t.Errorf("check %d should be gate-derived, got %+v", i, c)
		}
	}
	if got[3].ID != "CHK-portal-layer-4" {
		t.Errorf("per-kind numbering: got %q", got[3].ID)
	}

	// >5000 changed lines -> reduced cap (wall-clock protection).
	large := []diffFile{{Path: "big.go", Added: make([]string, requiredChecksLargeDiff+1)}}
	got = BuildRequiredChecks(gates, mem, large)
	if len(got) != requiredChecksMaxLargeDiff {
		t.Fatalf("large diff: want cap %d, got %d", requiredChecksMaxLargeDiff, len(got))
	}
}

func TestBuildRequiredChecks_MemoryTargetsAndIDCollisions(t *testing.T) {
	files := []diffFile{
		{Path: "src/pages/form.py", Added: []string{"choices = FIELD_CHOICES"}},
		{Path: "src/other.py", Removed: []string{"old_keyword_here"}},
	}
	mem := []BugMemoryEntry{
		mkCheckEntry("kw-entry", "same class!", "check one", nil, []string{"old_keyword"}),
		mkCheckEntry("glob-entry", "Same Class?", "check two", []string{"src/pages/**"}, nil),
		mkCheckEntry("no-match", "same class!", "check three", []string{"zzz/**"}, []string{"zzz"}),
	}
	got := BuildRequiredChecks(nil, mem, files)
	if len(got) != 3 {
		t.Fatalf("want 3 memory checks, got %+v", got)
	}
	// Class slugs are kebab-cased; collisions get a numeric suffix.
	if got[0].ID != "CHK-mem-same-class" || got[1].ID != "CHK-mem-same-class-2" || got[2].ID != "CHK-mem-same-class-3" {
		t.Errorf("ids: %q %q %q", got[0].ID, got[1].ID, got[2].ID)
	}
	// Keyword match anchors to the file containing the keyword (removals count).
	if got[0].TargetFile != "src/other.py" {
		t.Errorf("keyword target: %q", got[0].TargetFile)
	}
	if got[1].TargetFile != "src/pages/form.py" {
		t.Errorf("glob target: %q", got[1].TargetFile)
	}
	// An entry whose triggers no longer resolve keeps an empty target.
	if got[2].TargetFile != "" {
		t.Errorf("unresolvable target should be empty: %q", got[2].TargetFile)
	}
}

func TestParseCheckAnswers(t *testing.T) {
	ck := func(body string) types.LineComment {
		return types.LineComment{FilePath: "CHECK", LineNumber: 0, CommentBody: body}
	}
	cases := []struct {
		name         string
		in           []types.LineComment
		wantKept     int
		wantAnswers  int
		wantVerdict  string
		wantEvidence string
	}{
		{"valid violated", []types.LineComment{ck("CHK-portal-layer-1 | VIOLATED | EVIDENCE: app/Tooltip.tsx:12 read | mounts on default layer")},
			0, 1, "VIOLATED", "app/Tooltip.tsx:12 read"},
		{"extra whitespace", []types.LineComment{ck("  CHK-a-1   |   SAFE   |  EVIDENCE:  src/a.py:3  |  layer is set  ")},
			0, 1, "SAFE", "src/a.py:3"},
		{"not applicable", []types.LineComment{ck("CHK-mem-x | NOT-APPLICABLE | EVIDENCE: src/a.py:3 | out of scope")},
			0, 1, "NOT-APPLICABLE", "src/a.py:3"},
		{"lowercase verdict is unanswered", []types.LineComment{ck("CHK-a-1 | safe | EVIDENCE: src/a.py:3 | fine")},
			0, 0, "", ""},
		{"missing evidence segment", []types.LineComment{ck("CHK-a-1 | SAFE | no portals here")},
			0, 1, "SAFE", ""},
		{"bare pipe inside evidence, no reason", []types.LineComment{ck("CHK-a-1 | SAFE | EVIDENCE: git grep -E 'FOO|BAR' photo/tasks.py returned 0 hits")},
			0, 1, "SAFE", "git grep -E 'FOO|BAR' photo/tasks.py returned 0 hits"},
		{"pipe inside reason stays out of evidence", []types.LineComment{ck("CHK-a-1 | SAFE | EVIDENCE: src/a.py:3 | fine | either way")},
			0, 1, "SAFE", "src/a.py:3"},
		{"prose body is unanswered", []types.LineComment{ck("I checked everything and it looks fine.")},
			0, 0, "", ""},
		{"non-check comments pass through", []types.LineComment{
			{FilePath: "a.go", LineNumber: 3, CommentBody: "bug"},
			ck("CHK-a-1 | SAFE | EVIDENCE: a.go:3 | ok"),
			{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "verdict"},
		}, 2, 1, "SAFE", "a.go:3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, answers := ParseCheckAnswers(c.in)
			if len(kept) != c.wantKept {
				t.Errorf("kept: got %d want %d (%+v)", len(kept), c.wantKept, kept)
			}
			if len(answers) != c.wantAnswers {
				t.Fatalf("answers: got %d want %d (%+v)", len(answers), c.wantAnswers, answers)
			}
			if c.wantAnswers == 0 {
				return
			}
			if answers[0].Verdict != c.wantVerdict {
				t.Errorf("verdict: got %q want %q", answers[0].Verdict, c.wantVerdict)
			}
			if answers[0].Evidence != c.wantEvidence {
				t.Errorf("evidence: got %q want %q", answers[0].Evidence, c.wantEvidence)
			}
			// CHECK entries never leak into findings.
			for _, k := range kept {
				if k.FilePath == "CHECK" {
					t.Errorf("CHECK entry leaked into findings: %+v", k)
				}
			}
		})
	}
}

// Rule (a): VIOLATED without an accompanying finding on the target file
// synthesizes one there, capped MEDIUM.
func TestEnforceRequiredChecks_ViolatedSynthesizesFinding(t *testing.T) {
	checks := BuildRequiredChecks(
		[]types.LineComment{gateAlertFixture("portal-layer", "app/Tooltip.tsx")}, nil,
		[]diffFile{{Path: "app/Tooltip.tsx", Added: []string{"<Portal>"}}})
	answers := []CheckAnswer{{
		ID: "CHK-portal-layer-1", Verdict: "VIOLATED",
		Evidence: "app/Tooltip.tsx:12 read",
		Body:     "CHK-portal-layer-1 | VIOLATED | EVIDENCE: app/Tooltip.tsx:12 read | default layer",
	}}
	gates := []types.LineComment{gateAlertFixture("portal-layer", "app/Tooltip.tsx")}

	_, outGates, escalated, tel := EnforceRequiredChecks(checks, answers, nil, gates,
		[]string{"app/Tooltip.tsx"}, "")
	if len(escalated) != 1 {
		t.Fatalf("want 1 synthesized finding, got %+v", escalated)
	}
	s := escalated[0]
	if s.FilePath != "app/Tooltip.tsx" || s.Importance != "MEDIUM" {
		t.Errorf("synthesized finding wrong anchor/severity: %+v", s)
	}
	if !strings.Contains(s.CommentBody, "CHK-portal-layer-1") || !strings.Contains(s.CommentBody, "default layer") {
		t.Errorf("synthesized body missing answer content: %q", s.CommentBody)
	}
	// Answered with evidence -> no unresolved-risk note on the gate.
	if strings.Contains(outGates[0].CommentBody, "not answered") {
		t.Errorf("gate should not carry the unanswered note: %q", outGates[0].CommentBody)
	}
	if tel.ChecksIssued != 1 || tel.ChecksAnswered != 1 || tel.ChecksViolated != 1 || tel.ChecksEvidenceOK != 1 {
		t.Errorf("telemetry: %+v", tel)
	}

	// With an accompanying finding on the file, nothing is synthesized.
	comments := []types.LineComment{{FilePath: "app/Tooltip.tsx", LineNumber: 30, Importance: "CRITICAL", CommentBody: "portal is invisible under fullscreen"}}
	_, _, escalated, _ = EnforceRequiredChecks(checks, answers, comments, gates,
		[]string{"app/Tooltip.tsx"}, "")
	if len(escalated) != 0 {
		t.Errorf("accompanying finding present — want no synthesis, got %+v", escalated)
	}

	// A finding on a different file that merely shares the basename must not
	// suppress the synthesis.
	unrelated := []types.LineComment{{FilePath: "widgets/Tooltip.tsx", LineNumber: 8, Importance: "LOW", CommentBody: "unrelated nit"}}
	_, _, escalated, _ = EnforceRequiredChecks(checks, answers, unrelated, gates,
		[]string{"app/Tooltip.tsx"}, "")
	if len(escalated) != 1 {
		t.Errorf("basename-only match must not suppress synthesis, got %+v", escalated)
	}
}

// Rule (b): SAFE/NOT-APPLICABLE with evidence that cites no real path is
// treated as unanswered.
func TestEnforceRequiredChecks_SafeRequiresRealEvidence(t *testing.T) {
	gates := []types.LineComment{gateAlertFixture("settings-ref", "photo/tasks.py")}
	checks := BuildRequiredChecks(gates, nil, []diffFile{{Path: "photo/tasks.py", Added: []string{"x"}}})

	// Evidence cites nothing that exists -> unanswered -> note on the gate.
	bad := []CheckAnswer{{ID: "CHK-settings-ref-1", Verdict: "SAFE", Evidence: "seems implausible, trust me", Body: "..."}}
	_, outGates, escalated, tel := EnforceRequiredChecks(checks, bad, nil, gates, []string{"photo/tasks.py"}, "")
	if !strings.Contains(outGates[0].CommentBody, "Required check CHK-settings-ref-1 was not answered with evidence — treat as unresolved risk, not a cleared concern.") {
		t.Errorf("gate missing unresolved-risk note: %q", outGates[0].CommentBody)
	}
	if len(escalated) != 0 {
		t.Errorf("gate-derived unanswered must not add a second finding: %+v", escalated)
	}
	if tel.ChecksAnswered != 1 || tel.ChecksEvidenceOK != 0 {
		t.Errorf("telemetry: %+v", tel)
	}
	// The input gates slice stays untouched (enforcement works on a copy).
	if strings.Contains(gates[0].CommentBody, "not answered") {
		t.Errorf("input gates mutated: %q", gates[0].CommentBody)
	}

	// Evidence citing a diff path -> answered, no note.
	good := []CheckAnswer{{ID: "CHK-settings-ref-1", Verdict: "SAFE", Evidence: "photo/tasks.py:44 — reference is behind a dead flag", Body: "..."}}
	_, outGates, _, tel = EnforceRequiredChecks(checks, good, nil, gates, []string{"photo/tasks.py"}, "")
	if strings.Contains(outGates[0].CommentBody, "not answered") {
		t.Errorf("valid SAFE should clear the note: %q", outGates[0].CommentBody)
	}
	if tel.ChecksEvidenceOK != 1 {
		t.Errorf("telemetry: %+v", tel)
	}

	// Evidence citing a worktree file (not in the diff) also validates.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "util.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := []CheckAnswer{{ID: "CHK-settings-ref-1", Verdict: "SAFE", Evidence: "lib/util.py:1 defines it dynamically", Body: "..."}}
	_, outGates, _, tel = EnforceRequiredChecks(checks, wt, nil, gates, nil, dir)
	if strings.Contains(outGates[0].CommentBody, "not answered") {
		t.Errorf("worktree-backed SAFE should clear the note: %q", outGates[0].CommentBody)
	}
	if tel.ChecksEvidenceOK != 1 {
		t.Errorf("telemetry: %+v", tel)
	}
}

// Rule (c): an unanswered check re-admits the underlying alert. Gate alerts
// (already merged with provenance "mechanical") carry the note; memory
// checks emit a new alert since they have no merge presence otherwise.
func TestEnforceRequiredChecks_UnansweredReadmitsUnderlyingAlert(t *testing.T) {
	gates := []types.LineComment{gateAlertFixture("portal-layer", "app/Tooltip.tsx")}
	files := []diffFile{{Path: "src/menus/Item.tsx", Added: []string{"handler"}}}
	mem := []BugMemoryEntry{mkCheckEntry("ctx-menu", "menu-id", "verify handler ids", []string{"src/menus/**"}, nil)}
	checks := BuildRequiredChecks(gates, mem, files)
	if len(checks) != 2 {
		t.Fatalf("fixture: want 2 checks, got %+v", checks)
	}

	_, outGates, escalated, tel := EnforceRequiredChecks(checks, nil, nil, gates, []string{"src/menus/Item.tsx"}, "")
	if !strings.Contains(outGates[0].CommentBody, "Required check CHK-portal-layer-1 was not answered with evidence") {
		t.Errorf("gate note missing: %q", outGates[0].CommentBody)
	}
	if len(escalated) != 1 {
		t.Fatalf("want 1 memory re-admission, got %+v", escalated)
	}
	m := escalated[0]
	if m.FilePath != "src/menus/Item.tsx" || m.Importance != "MEDIUM" {
		t.Errorf("memory re-admission anchor/severity: %+v", m)
	}
	if !strings.Contains(m.CommentBody, "pattern for ctx-menu") ||
		!strings.Contains(m.CommentBody, "Required check CHK-mem-menu-id was not answered with evidence — treat as unresolved risk, not a cleared concern.") {
		t.Errorf("memory re-admission body: %q", m.CommentBody)
	}
	if tel.ChecksIssued != 2 || tel.ChecksAnswered != 0 || tel.ChecksViolated != 0 || tel.ChecksEvidenceOK != 0 {
		t.Errorf("telemetry: %+v", tel)
	}
}

// Rule (d): the ledger is structured telemetry, one record per check; the
// SUMMARY prose is never touched and never created for it.
func TestEnforceRequiredChecks_LedgerIsStructured(t *testing.T) {
	gates := []types.LineComment{gateAlertFixture("portal-layer", "app/Tooltip.tsx")}
	checks := BuildRequiredChecks(gates, nil, []diffFile{{Path: "app/Tooltip.tsx"}})
	answers := []CheckAnswer{{ID: "CHK-portal-layer-1", Verdict: "SAFE", Evidence: "app/Tooltip.tsx:9 sets layer", Body: "SAFE because the layer is explicit"}}

	comments := []types.LineComment{
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Verdict: approve"},
		{FilePath: "a.go", LineNumber: 1, CommentBody: "nit"},
	}
	out, _, _, tel := EnforceRequiredChecks(checks, answers, comments, gates, []string{"app/Tooltip.tsx"}, "")
	if out[0].CommentBody != "Verdict: approve" {
		t.Errorf("SUMMARY must stay as written: %q", out[0].CommentBody)
	}
	if len(tel.Records) != 1 {
		t.Fatalf("want one record per check, got %+v", tel.Records)
	}
	r := tel.Records[0]
	if r.ID != "CHK-portal-layer-1" || r.Source != "gate" || r.Verdict != "SAFE" || !r.EvidenceResolved || r.Unresolved ||
		r.TargetFile != "app/Tooltip.tsx" || r.Answer != "SAFE because the layer is explicit" || r.Question == "" {
		t.Errorf("record = %+v", r)
	}

	out, _, _, tel = EnforceRequiredChecks(checks, nil, nil, gates, nil, "")
	if len(out) != 0 {
		t.Fatalf("no SUMMARY may be created for the ledger, got %+v", out)
	}
	if len(tel.Records) != 1 || tel.Records[0].Verdict != "UNANSWERED" || !tel.Records[0].Unresolved || tel.Records[0].EvidenceResolved {
		t.Errorf("unanswered record = %+v", tel.Records)
	}
}

func TestEnforceRequiredChecks_TelemetryMixed(t *testing.T) {
	gates := []types.LineComment{
		gateAlertFixture("portal-layer", "a.tsx"),
		gateAlertFixture("settings-ref", "b.py"),
		gateAlertFixture("shared-file", "common/c.py"),
	}
	checks := BuildRequiredChecks(gates, nil, []diffFile{{Path: "a.tsx"}})
	answers := []CheckAnswer{
		{ID: "CHK-portal-layer-1", Verdict: "VIOLATED", Evidence: "a.tsx:1", Body: "CHK-portal-layer-1 | VIOLATED | EVIDENCE: a.tsx:1 | broken"},
		{ID: "CHK-settings-ref-1", Verdict: "SAFE", Evidence: "no evidence really", Body: "..."},
		// CHK-shared-file-1 unanswered.
	}
	_, _, _, tel := EnforceRequiredChecks(checks, answers, nil, gates, []string{"a.tsx"}, "")
	counts := [4]int{tel.ChecksIssued, tel.ChecksAnswered, tel.ChecksViolated, tel.ChecksEvidenceOK}
	if counts != [4]int{3, 2, 1, 1} || len(tel.Records) != 3 {
		t.Errorf("telemetry: got %+v", tel)
	}
}

// With the feature off (no checks issued) the prompt must be byte-identical
// to the pre-required-checks build — gates, memory and first-pass claims only.
func TestBuildAgentPromptContent_ChecksOffByteIdentical(t *testing.T) {
	gemini := firstPassClaims([]types.LineComment{lc("a.go", 3, "LOW", "x")})
	gates := []types.LineComment{gateAlertFixture("settings-ref", "photo/tasks.py")}
	mem := []BugMemoryEntry{mkEntry("e1", "c", "a prior", []string{"**"}, nil, 1)}

	got, err := buildAgentPromptContent("", nil, "", gemini, gates, mem, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Reconstruct today's (checkless) prompt byte-for-byte.
	commentsJSON, err := json.MarshalIndent(gemini, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(promptAgentReview)
	b.WriteString("\n--- MECHANICAL ALERTS (deterministic checks; explicitly address each in your review) ---\n")
	for _, g := range gates {
		b.WriteString("- [" + g.FilePath + "] " + g.CommentBody + "\n")
	}
	b.WriteString(bugMemorySection(mem))
	b.WriteString("\n--- FIRST-PASS CLAIMS (JSON; account for every source_id) ---\n")
	b.Write(commentsJSON)

	if got != b.String() {
		t.Errorf("prompt not byte-identical with checks off:\ngot:  %q\nwant: %q", got, b.String())
	}
	if strings.Contains(got, "REQUIRED CHECKS") {
		t.Error("checks-off prompt must not mention REQUIRED CHECKS")
	}
}

func TestBuildAgentPromptContent_ChecksSection(t *testing.T) {
	gates := []types.LineComment{gateAlertFixture("portal-layer", "app/Tooltip.tsx")}
	checks := BuildRequiredChecks(gates, nil, []diffFile{{Path: "app/Tooltip.tsx"}})
	got, err := buildAgentPromptContent("", nil, "", nil, gates, nil, checks)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--- REQUIRED CHECKS (answer each; an unanswered check is escalated automatically) ---",
		`"file_path": "CHECK"`,
		"'<CHK-id> | VIOLATED|SAFE|NOT-APPLICABLE | EVIDENCE: <file:line read or command run + result> | <one-sentence reason>'",
		"- CHK-portal-layer-1: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The block sits with the other context sections, before the comments JSON.
	if strings.Index(got, "REQUIRED CHECKS") > strings.Index(got, "FIRST-PASS CLAIMS") {
		t.Error("REQUIRED CHECKS block must precede the first-pass claims")
	}
}

// The optional check field must round-trip and stay absent when unset
// (backward compatibility with existing libraries).
func TestBugMemoryEntry_CheckJSONRoundTrip(t *testing.T) {
	with := mkCheckEntry("e1", "c", "verify the id at invocation time", []string{"src/**"}, nil)
	data, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"check":"verify the id at invocation time"`) {
		t.Errorf("check field not marshaled: %s", data)
	}
	var back BugMemoryEntry
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Check != with.Check {
		t.Errorf("round trip: got %q want %q", back.Check, with.Check)
	}

	without := mkEntry("e2", "c", "p", []string{"src/**"}, nil, 1)
	data, err = json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"check"`) {
		t.Errorf("unset check must be omitted: %s", data)
	}

	// Library load keeps the field and applies structural sanitization.
	lib, dropped, err := LoadBugMemory([]byte(`{"version":"v1","entries":[
		{"id":"a","source_repo":"acme/example","source_prs":[1],"class":"c","pattern":"p","trigger_paths":["src/**"],"check":"line one` + "\\n\\n" + `line two"},
		{"id":"b","source_repo":"acme/example","source_prs":[2],"class":"c","pattern":"p","trigger_paths":["src/**"]}
	]}`))
	if err != nil || len(dropped) != 0 {
		t.Fatalf("load: %v dropped=%v", err, dropped)
	}
	if lib.Entries[0].Check != "line one line two" {
		t.Errorf("check not sanitized: %q", lib.Entries[0].Check)
	}
	if lib.Entries[1].Check != "" {
		t.Errorf("entry without check should stay empty: %q", lib.Entries[1].Check)
	}
}
