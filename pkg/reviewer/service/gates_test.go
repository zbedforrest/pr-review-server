package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateFixtureRepo builds a tiny git worktree with the structures the gates
// grep against: a settings file, a wired FE topic, and shared modules.
func gateFixtureRepo(t *testing.T) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.c",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.c")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main", ".")
	write("settings_base.py", "DEFINED_BUCKET = \"x\"\n  INDENTED_SETTING = 1\n")
	write("fe_registry/topics/room.ts", "export class WiredTopic extends RoomTopic {}\n")
	write("common/util.py", "def helper():\n    pass\n")
	run("add", ".")
	run("commit", "-q", "-m", "fixture")
	return dir
}

func TestGates_SettingsRef(t *testing.T) {
	dir := gateFixtureRepo(t)
	files := []diffFile{{
		Path:   "photo/tasks.py",
		Status: "modified",
		Added: []string{
			"    b = settings.DEFINED_BUCKET",     // defined -> no alert
			"    i = settings.INDENTED_SETTING",   // defined (indented) -> no alert
			"    m = settings.MISSING_BUCKET_KEY", // undefined -> alert
		},
	}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 alert (MISSING_BUCKET_KEY), got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].CommentBody, "MISSING_BUCKET_KEY") ||
		got[0].Importance != "MEDIUM" {
		t.Errorf("unexpected alert: %+v", got[0])
	}
	// Test files are exempt.
	files[0].Path = "photo/tests/test_tasks.py"
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("test files should not alert: %+v", got)
	}
}

func TestGates_RegistrySplit(t *testing.T) {
	dir := gateFixtureRepo(t)
	// The registry gate is disabled unless configured; point it at the
	// fixture's generic layout for the duration of the test.
	oldBE, oldFE := gateTopicBEDir, gateTopicFEDir
	gateTopicBEDir, gateTopicFEDir = "be_topics/", "fe_registry"
	defer func() { gateTopicBEDir, gateTopicFEDir = oldBE, oldFE }()
	files := []diffFile{{
		Path:   "be_topics/room.py",
		Status: "modified",
		Added: []string{
			"class WiredTopic(RoomTopic):",  // FE counterpart exists -> no alert
			"class OrphanTopic(RoomTopic):", // no FE counterpart -> alert
		},
	}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 alert (OrphanTopic), got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].CommentBody, "OrphanTopic") {
		t.Errorf("unexpected alert: %+v", got[0])
	}

	// Unconfigured (default) => gate disabled entirely.
	gateTopicBEDir, gateTopicFEDir = "", ""
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("registry gate should be disabled when unconfigured: %+v", got)
	}
}

func TestGates_SharedFileAggregates(t *testing.T) {
	dir := gateFixtureRepo(t)
	files := []diffFile{
		{Path: "common/util.py", Status: "modified"},
		{Path: "app/components/common/molecules/BaseWidget/BaseWidget.tsx", Status: "modified"},
		{Path: "feature/page.tsx", Status: "modified"},
		{Path: "common/newfile.py", Status: "added"}, // added files exempt
	}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 {
		t.Fatalf("want 1 aggregated shared-file alert, got %d: %+v", len(got), got)
	}
	b := got[0].CommentBody
	if !strings.Contains(b, "2 shared/base module(s)") ||
		!strings.Contains(b, "common/util.py") || !strings.Contains(b, "BaseWidget.tsx") {
		t.Errorf("aggregation wrong: %q", b)
	}
}

func TestGates_BestEffortOnBadWorktree(t *testing.T) {
	// GatesForWorktree must never error a review: bad dir -> nil findings.
	if got := GatesForWorktree(context.Background(), "/nonexistent/dir", "main"); got != nil {
		t.Errorf("want nil on bad worktree, got %+v", got)
	}
	if got := GatesForWorktree(context.Background(), "", ""); got != nil {
		t.Errorf("want nil on empty args, got %+v", got)
	}
}

func TestGates_PortalLayer(t *testing.T) {
	dir := gateFixtureRepo(t)
	// Layerless overlay -> alert.
	files := []diffFile{{Path: "app/Tooltip.tsx", Status: "modified",
		Added: []string{"        <RichTooltip", "            isOpen={true}"}}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "overlay without an explicit layer") {
		t.Fatalf("want 1 portal-layer alert, got %+v", got)
	}
	// Explicit layer -> silent.
	files[0].Added = []string{"    return <Portal layer={portalLayer}>{content}</Portal>"}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("explicit layer should not alert: %+v", got)
	}
	// Non-jsx file -> silent.
	files[0].Path = "app/notes.md"
	files[0].Added = []string{"<RichTooltip"}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("non-jsx should not alert: %+v", got)
	}
}

func TestGates_ModelProperty(t *testing.T) {
	dir := gateFixtureRepo(t)
	files := []diffFile{{Path: "tipping/models.py", Status: "modified",
		Added: []string{"    @cached_property", "    def wanted(self):"}}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "new model property") {
		t.Fatalf("want 1 model-property alert, got %+v", got)
	}
	// Plain @property also fires; non-models.py and tests do not.
	files[0].Added = []string{"    @property", "    def x(self):"}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 1 {
		t.Errorf("@property should alert: %+v", got)
	}
	files[0].Path = "tipping/views.py"
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("non-models.py should not alert: %+v", got)
	}
	files[0].Path = "tipping/tests/models.py"
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("test files should not alert: %+v", got)
	}
}

func TestGates_PortalLayer_ClassWide(t *testing.T) {
	dir := gateFixtureRepo(t)
	// createPortal without layer -> alert (including plain .ts).
	files := []diffFile{{Path: "app/preview.ts", Status: "modified",
		Added: []string{"    return createPortal(content, document.body)"}}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "portal overlay") {
		t.Fatalf("want createPortal alert, got %+v", got)
	}
	// Any *Portal component -> alert.
	files[0] = diffFile{Path: "app/Media.tsx", Status: "modified",
		Added: []string{"        <FullscreenMediaPortal media={m} />"}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 1 {
		t.Fatalf("want *Portal component alert, got %+v", got)
	}
	// createPortal alongside an explicit layer -> silent.
	files[0] = diffFile{Path: "app/preview.ts", Status: "modified",
		Added: []string{"    return createPortal(content, target)", "    layer={overlayLayer}"}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("explicit layer should suppress: %+v", got)
	}
	// Identifier merely containing the substring -> silent.
	files[0] = diffFile{Path: "app/util.ts", Status: "modified",
		Added: []string{"    const recreatePortalCount = 3"}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("substring identifier should not alert: %+v", got)
	}
}

// gateRepoWith builds a committed fixture repo from a path->content map, for
// the round-3 gates that grep the (tracked) worktree.
func gateRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.c",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.c")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "fixture")
	return dir
}

// withGate flips one of the round-3 enable vars for the test's duration.
func withGate(t *testing.T, enabled *bool) {
	t.Helper()
	old := *enabled
	*enabled = true
	t.Cleanup(func() { *enabled = old })
}

func TestGates_WiringDeadSymbol(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		"app/new.py":      "def orphan_fn(arg):\n    return arg\n\ndef wired_fn():\n    return 1\n",
		"app/consumer.py": "from app.new import wired_fn\nwired_fn()\n",
		"app/exports.ts":  "export const OrphanExport = () => 1\nexport function WiredExport() {}\n",
		"app/use.ts":      "import { WiredExport } from \"./exports\"\nWiredExport()\n",
	})
	files := []diffFile{
		{Path: "app/new.py", Status: "modified", Added: []string{"def orphan_fn(arg):", "def wired_fn():"}},
		{Path: "app/exports.ts", Status: "modified", Added: []string{"export const OrphanExport = () => 1", "export function WiredExport() {}"}},
	}

	// Disabled by default — byte-identical to a round-2 build.
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Fatalf("wiring gate must be off when unconfigured: %+v", got)
	}

	withGate(t, &gateWiringEnabled)
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 2 {
		t.Fatalf("want 2 dead-symbol alerts (orphan_fn, OrphanExport), got %d: %+v", len(got), got)
	}
	joined := got[0].CommentBody + got[1].CommentBody
	if !strings.Contains(joined, "`orphan_fn`") || !strings.Contains(joined, "`OrphanExport`") {
		t.Errorf("wrong symbols flagged: %q", joined)
	}
	for _, g := range got {
		if g.Importance != "MEDIUM" || !strings.Contains(g.CommentBody, "no references") {
			t.Errorf("unexpected alert: %+v", g)
		}
	}

	// Indented (non-module-level) defs and test files never alert.
	files = []diffFile{
		{Path: "app/new.py", Status: "modified", Added: []string{"    def inner_helper(self):"}},
		{Path: "app/tests/test_new.py", Status: "modified", Added: []string{"def totally_unused_fn():"}},
	}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("indented/test defs should not alert: %+v", got)
	}
}

// gateNoisePath must match "test" at word/segment boundaries only: a live
// reference in latestNews.tsx (or attestation.py, fastestRoute.ts, …) is
// real wiring, and discarding it flips the err-silent contract into a false
// dead-symbol fire.
func TestGates_NoisePathWordBoundary(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		"app/exports.ts":     "export function formatDate() {}\n",
		"app/latestNews.tsx": "import { formatDate } from \"./exports\"\nformatDate()\n",
	})
	withGate(t, &gateWiringEnabled)
	files := []diffFile{{Path: "app/exports.ts", Status: "modified",
		Added: []string{"export function formatDate() {}"}}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("a reference in latestNews.tsx is live, not test noise: %+v", got)
	}

	noise := []string{"app/tests/util.ts", "app/foo_test.go", "app/Comp.test.tsx",
		"src/__tests__/x.ts", "test/e2e.py", "app/test_tasks.py"}
	live := []string{"app/latestNews.tsx", "src/attestation.py", "lib/fastestRoute.ts",
		"app/greatest_hits.py", "app/shortest.go", "app/contest.tsx"}
	for _, p := range noise {
		if !gateNoisePath(p) {
			t.Errorf("gateNoisePath(%q) = false, want noise", p)
		}
	}
	for _, p := range live {
		if gateNoisePath(p) {
			t.Errorf("gateNoisePath(%q) = true, want live", p)
		}
	}
}

func TestGates_WiringDeWired(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		// OrphanWidget stays defined; its only remaining JSX usage is a test.
		"app/Widget.tsx":      "export function OrphanWidget(props: P) { return <div /> }\n",
		"app/Widget.test.tsx": "render(<OrphanWidget a={1} />)\n",
		"app/page.tsx":        "export function Page() { return <div /> }\n",
		// orphanCall stays defined with no remaining callers.
		"app/lib.ts": "export function orphanCall(): void {}\n",
	})
	withGate(t, &gateWiringEnabled)

	files := []diffFile{{Path: "app/page.tsx", Status: "modified",
		Removed: []string{"            <OrphanWidget a={1} />"}}}
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "`OrphanWidget`") ||
		!strings.Contains(got[0].CommentBody, "de-wired") {
		t.Fatalf("want OrphanWidget de-wired alert, got %+v", got)
	}

	// Removed function call with no survivors -> alert.
	files = []diffFile{{Path: "app/page.tsx", Status: "modified",
		Removed: []string{"    orphanCall()"}}}
	got = RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "`orphanCall`") {
		t.Fatalf("want orphanCall de-wired alert, got %+v", got)
	}

	// Re-wired in the same diff -> silent.
	files = []diffFile{
		{Path: "app/page.tsx", Status: "modified", Removed: []string{"<OrphanWidget a={1} />"}},
		{Path: "app/other.tsx", Status: "modified", Added: []string{"<OrphanWidget a={2} />"}},
	}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("re-wired symbol should not alert: %+v", got)
	}

	// Clean deletion (definition removed together with the call) -> silent.
	files = []diffFile{{Path: "app/page.tsx", Status: "modified",
		Removed: []string{"this.goneHelper()", "private goneHelper(): void {"}}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("clean deletion should not alert: %+v", got)
	}
}

func TestGates_EffectCleanup(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		"app/Comp.tsx":  "export function Comp() { return null }\n",
		"app/Thumb.tsx": "export function Thumb() { return <img /> }\n",
	})
	leaky := []string{
		"    useEffect(() => {",
		"        window.addEventListener(\"resize\", onResize)",
		"    }, [])",
	}

	// Disabled by default.
	files := []diffFile{{Path: "app/Comp.tsx", Status: "modified", Added: leaky}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Fatalf("effect gate must be off when unconfigured: %+v", got)
	}

	withGate(t, &gateEffectEnabled)
	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "without unmount cleanup") ||
		!strings.Contains(got[0].CommentBody, "addEventListener") {
		t.Fatalf("want effect-cleanup alert, got %+v", got)
	}

	// Cleanup in the added hunk -> silent.
	files[0].Added = append(leaky[:2:2],
		"        return () => window.removeEventListener(\"resize\", onResize)",
		"    }, [])")
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("cleanup present should not alert: %+v", got)
	}

	// Handler arm: acquisition wired to JSX handlers, no effect anywhere.
	files = []diffFile{{Path: "app/Thumb.tsx", Status: "modified", Added: []string{
		"                onPointerEnter={(evt) => {",
		"                    media.startPolling(item, img)",
		"                }}",
	}}}
	got = RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "startPolling(") {
		t.Fatalf("want handler-arm alert, got %+v", got)
	}

	// startTransition is not an acquisition.
	files[0].Added = []string{"    onClick={() => startTransition(() => setX(1))}"}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("startTransition should not alert: %+v", got)
	}

	// Plain .ts files never hit the handler arm.
	files = []diffFile{{Path: "app/util.ts", Status: "modified", Added: []string{
		"    onEnter={() => startPolling(id)}",
	}}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("handler arm must be .tsx/.jsx only: %+v", got)
	}

	// Hunk-scoped (the tight-FP contract): an inert added effect in one hunk
	// plus an unrelated acquisition in another hunk of the same file is not
	// a leak.
	files = []diffFile{{Path: "app/Comp.tsx", Status: "modified",
		Added: []string{
			"    useEffect(() => {",
			"        setReady(true)",
			"    }, [])",
			"    onPlay={() => videoRef.current?.play()}",
		},
		AddedHunks: [][]string{
			{"    useEffect(() => {", "        setReady(true)", "    }, [])"},
			{"    onPlay={() => videoRef.current?.play()}"},
		},
	}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("acquisition outside the effect's hunk should not alert: %+v", got)
	}

	// …and a cleanup token in an unrelated hunk must not mask a genuine
	// leak inside the effect's own hunk.
	files = []diffFile{{Path: "app/Comp.tsx", Status: "modified",
		Added: []string{
			"    useEffect(() => {",
			"        window.addEventListener(\"resize\", onResize)",
			"    }, [])",
			"    socketRef.current?.close()",
		},
		AddedHunks: [][]string{
			{"    useEffect(() => {", "        window.addEventListener(\"resize\", onResize)", "    }, [])"},
			{"    socketRef.current?.close()"},
		},
	}}
	got = RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "addEventListener") {
		t.Fatalf("cleanup in an unrelated hunk must not mask the leak, got %+v", got)
	}
}

func TestGates_TaskSkew(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		"queue/tasks.py":   "@shared_task\ndef work(crit):\n    row = {\"new_key\": 1, \"kept_key\": 2}\n    return row\n",
		"queue/enqueue.py": "from queue.tasks import work\nwork.delay(crit)\n",
	})
	files := []diffFile{{Path: "queue/tasks.py", Status: "modified",
		Removed: []string{"    row = {\"old_key\": 1, \"kept_key\": 2}"},
		Added:   []string{"    row = {\"new_key\": 1, \"kept_key\": 2}"},
	}}

	// Disabled unless the producer glob is configured.
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Fatalf("task gate must be off when unconfigured: %+v", got)
	}
	oldGlob := gateTaskProducerGlob
	gateTaskProducerGlob = "**/tasks.py"
	t.Cleanup(func() { gateTaskProducerGlob = oldGlob })

	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "`old_key`") ||
		!strings.Contains(got[0].CommentBody, "task payload contract changed") {
		t.Fatalf("want dropped-key alert for old_key only, got %+v", got)
	}

	// A key that still survives somewhere (compat/consumer) -> silent.
	files[0].Removed = []string{"    row = {\"kept_key\": 2}"}
	files[0].Added = nil
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("surviving key should not alert: %+v", got)
	}

	// Signature drift of a queued function -> alert.
	files = []diffFile{{Path: "queue/tasks.py", Status: "modified",
		Removed: []string{"def work(crit):"},
		Added:   []string{"def work(crit, extra=None):"},
	}}
	got = RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "`work`") ||
		!strings.Contains(got[0].CommentBody, "signature") {
		t.Fatalf("want signature-drift alert, got %+v", got)
	}

	// Files outside the producer glob are ignored.
	files[0].Path = "queue/views.py"
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("non-producer files should not alert: %+v", got)
	}

	// The consumer glob is doublestar (brace sets, `*` not crossing '/'),
	// NOT a git pathspec: a doublestar-only pattern must still see the
	// surviving key in matching files rather than reading "zero survivors"
	// and false-firing on every dropped-key candidate.
	oldConsumer := gateTaskConsumerGlob
	gateTaskConsumerGlob = "{queue,consumers}/**"
	t.Cleanup(func() { gateTaskConsumerGlob = oldConsumer })
	files = []diffFile{{Path: "queue/tasks.py", Status: "modified",
		Removed: []string{"    row = {\"kept_key\": 2}"},
	}}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("doublestar consumer glob must see the surviving key: %+v", got)
	}
}

func TestGates_MigrationResidue(t *testing.T) {
	dir := gateRepoWith(t, map[string]string{
		"app/config.py":   "kind = \"modern_kind\"\n",
		"app/producer.py": "send(kind=\"legacy_kind\")\n",
		"app/other.py":    "if kind == \"legacy_kind\":\n    pass\n",
		"app/consts.py":   "OLD_KEY = \"s\"\nALL = [OLD_KEY]\n",
		"app/reader.py":   "if key == OLD_KEY:\n    pass\n",
	})
	files := []diffFile{{Path: "app/config.py", Status: "modified",
		Removed: []string{"kind = \"legacy_kind\""},
		Added:   []string{"kind = \"modern_kind\""},
	}}

	// Disabled by default.
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Fatalf("residue gate must be off when unconfigured: %+v", got)
	}
	withGate(t, &gateResidueEnabled)

	got := RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 {
		t.Fatalf("want 1 residue alert, got %d: %+v", len(got), got)
	}
	b := got[0].CommentBody
	if !strings.Contains(b, "`legacy_kind`") || !strings.Contains(b, "`modern_kind`") ||
		!strings.Contains(b, "migration residue") ||
		!strings.Contains(b, "app/producer.py") || !strings.Contains(b, "app/other.py") {
		t.Errorf("residue alert missing specifics/citations: %q", b)
	}

	// Zero survivors (complete migration) -> silent.
	files[0].Removed = []string{"kind = \"gone_kind\""}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("complete migration should not alert: %+v", got)
	}

	// Above the survivor threshold -> suppressed (shared constant, not a
	// migration).
	oldMax := gateResidueMaxSurvivors
	gateResidueMaxSurvivors = 1
	files[0].Removed = []string{"kind = \"legacy_kind\""}
	if got := RunMechanicalGates(context.Background(), dir, files); len(got) != 0 {
		t.Errorf("survivors above threshold should suppress: %+v", got)
	}
	gateResidueMaxSurvivors = oldMax

	// Const-exclusion arm: newly filtering out an enum constant while other
	// files still reference it.
	files = []diffFile{{Path: "app/forms.py", Status: "modified", Added: []string{
		"        choices = [c for c in choices if c != LEGACY.OLD_KEY]",
	}}}
	got = RunMechanicalGates(context.Background(), dir, files)
	if len(got) != 1 || !strings.Contains(got[0].CommentBody, "`OLD_KEY`") {
		t.Fatalf("want const-exclusion residue alert, got %+v", got)
	}
}

func TestParseNameStatusDiff_DeletedFileRemovals(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := runGit(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(dir+"/doomed.ts", []byte("const menuId = getMenuId()\nexport { menuId }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("branch", "base")
	run("rm", "-q", "doomed.ts")
	run("commit", "-q", "-m", "delete file")

	files, err := parseNameStatusDiff(context.Background(), dir, "base")
	if err != nil {
		t.Fatal(err)
	}
	var doomed *diffFile
	for i := range files {
		if files[i].Path == "doomed.ts" {
			doomed = &files[i]
		}
	}
	if doomed == nil || doomed.Status != "removed" {
		t.Fatalf("deleted file missing from diff: %+v", files)
	}
	// The whole point: a deleted file's removed lines must feed the matchers.
	joined := strings.Join(doomed.Removed, "\n")
	if !strings.Contains(joined, "getMenuId") {
		t.Errorf("removed lines of a deleted file were dropped: %+v", doomed.Removed)
	}
}

// Added lines must arrive grouped per hunk (AddedHunks) alongside the flat
// Added list — the effect-cleanup gate's "same added hunk" contract depends
// on the grouping.
func TestParseNameStatusDiff_AddedHunkGrouping(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := runGit(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(dir+"/f.ts", []byte("l1\nl2\nl3\nl4\nl5\nl6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("branch", "base")
	if err := os.WriteFile(dir+"/f.ts", []byte("l1\nADD_ONE\nl2\nl3\nl4\nl5\nADD_TWO\nADD_THREE\nl6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-q", "-am", "two separated insertions")

	files, err := parseNameStatusDiff(context.Background(), dir, "base")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "f.ts" {
		t.Fatalf("unexpected files: %+v", files)
	}
	f := files[0]
	if len(f.Added) != 3 {
		t.Fatalf("flat Added wrong: %+v", f.Added)
	}
	if len(f.AddedHunks) != 2 ||
		strings.Join(f.AddedHunks[0], ",") != "ADD_ONE" ||
		strings.Join(f.AddedHunks[1], ",") != "ADD_TWO,ADD_THREE" {
		t.Errorf("hunk grouping wrong: %+v", f.AddedHunks)
	}
}

// TestDiffFilesForWorktree_StackedBase proves the deterministic layer diffs
// against the PR's true base when it is provided, and documents the
// origin/HEAD fallback's inflation for PRs stacked on non-default branches:
// with base="" the parent branch's changes are misattributed to the PR.
func TestDiffFilesForWorktree_StackedBase(t *testing.T) {
	skipIfNoGit(t)
	upstream := t.TempDir()
	runIn := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.c",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.c")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
		}
		return string(out)
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(upstream, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Upstream: main -> feature-parent (adds parent.ts) -> feature-child (adds child.ts).
	runIn(upstream, "init", "-q", "-b", "main")
	write("app/base.ts", "export const base = 1\n")
	runIn(upstream, "add", ".")
	runIn(upstream, "commit", "-q", "-m", "base")
	runIn(upstream, "checkout", "-q", "-b", "feature-parent")
	write("app/parent.ts", "export const parent = 2\n")
	runIn(upstream, "add", ".")
	runIn(upstream, "commit", "-q", "-m", "parent work")
	runIn(upstream, "checkout", "-q", "-b", "feature-child")
	write("app/child.ts", "export const child = 3\n")
	runIn(upstream, "add", ".")
	runIn(upstream, "commit", "-q", "-m", "child pr")
	runIn(upstream, "checkout", "-q", "main") // so origin/HEAD resolves to main in clones

	clone := filepath.Join(t.TempDir(), "clone")
	runIn(".", "clone", "-q", upstream, clone)
	runIn(clone, "checkout", "-q", "origin/feature-child")

	names := func(files []diffFile) map[string]bool {
		m := map[string]bool{}
		for _, f := range files {
			m[f.Path] = true
		}
		return m
	}

	// True base: only the child PR's own change.
	got := names(diffFilesForWorktree(context.Background(), clone, "feature-parent", "", "", 0, ""))
	if !got["app/child.ts"] || got["app/parent.ts"] {
		t.Fatalf("base=feature-parent: want exactly the child change, got %v", got)
	}

	// Fallback ("" -> origin/HEAD = main): the parent branch's changes leak in.
	got = names(diffFilesForWorktree(context.Background(), clone, "", "", "", 0, ""))
	if !got["app/child.ts"] || !got["app/parent.ts"] {
		t.Fatalf("base=\"\": expected documented inflation (parent+child), got %v", got)
	}

	// Production-shaped cache: --depth implies --single-branch, so a cache
	// initialized on main has no origin/feature-parent ref at all (and
	// deepening alone can never create it). The recovery path must fetch the
	// missing base ref, or every stacked PR silently loses the entire
	// deterministic layer. file:// forces the wire protocol so --depth is
	// honored for a local upstream.
	sb := filepath.Join(t.TempDir(), "sb-clone")
	runIn(".", "clone", "-q", "--depth", "1", "--branch", "main", "file://"+upstream, sb)
	runIn(sb, "fetch", "-q", "origin", "+refs/heads/feature-child:refs/agent-pr/7")
	runIn(sb, "checkout", "-q", "--detach", "refs/agent-pr/7")
	got = names(diffFilesForWorktree(context.Background(), sb, "feature-parent", "", "", 0, ""))
	if !got["app/child.ts"] || got["app/parent.ts"] {
		t.Fatalf("single-branch clone: want the missing base ref fetched and only the child change diffed, got %v", got)
	}
}
