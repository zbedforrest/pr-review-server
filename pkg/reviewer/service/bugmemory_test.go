package service

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func mkEntry(id, class, pattern string, paths, kws []string, prs ...int) BugMemoryEntry {
	return BugMemoryEntry{
		ID: id, SourceCases: []string{id + "-case"}, SourceRepo: "acme/example",
		SourcePRs: prs, Class: class, Pattern: pattern,
		TriggerPaths: paths, TriggerKeywords: kws,
	}
}

func TestLoadBugMemory_ValidationAndSanitization(t *testing.T) {
	data := []byte(`{"version":"v0","entries":[
		{"id":"ok","source_repo":"acme/example","source_prs":[1],"class":"c","pattern":"a` + "```" + `b\n\n  c","trigger_paths":["src/**"]},
		{"id":"","pattern":"x","trigger_paths":["a"]},
		{"id":"no-trigger","pattern":"x"},
		{"id":"no-pattern","trigger_paths":["a"]}
	]}`)
	lib, dropped, err := LoadBugMemory(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(lib.Entries) != 1 || lib.Entries[0].ID != "ok" {
		t.Fatalf("want 1 valid entry, got %+v", lib.Entries)
	}
	if len(dropped) != 3 {
		t.Errorf("want 3 dropped, got %v", dropped)
	}
	if got := lib.Entries[0].Pattern; got != "a'''b c" {
		t.Errorf("sanitization: got %q", got)
	}
	if _, _, err := LoadBugMemory([]byte("not json")); err == nil {
		t.Error("want parse error on invalid JSON")
	}
}

func TestMatchBugMemory_TriggersAndLeakage(t *testing.T) {
	lib := &BugMemoryLibrary{Version: "v0", Entries: []BugMemoryEntry{
		mkEntry("path-hit", "a", "p1", []string{"src/menus/**"}, nil, 11),
		mkEntry("kw-added", "b", "p2", nil, []string{"preventDefault"}, 12),
		mkEntry("kw-removed", "c", "p3", nil, []string{"stopPropagation"}, 13),
		mkEntry("no-hit", "d", "p4", []string{"other/**"}, []string{"zzz"}, 14),
		mkEntry("self", "e", "p5", []string{"src/**"}, nil, 99),
	}}
	files := []diffFile{{
		Path:    "src/menus/Item.tsx",
		Added:   []string{"  el.PREVENTDEFAULT()"}, // case-insensitive
		Removed: []string{"  e.stopPropagation()"}, // omission bugs live in removals
	}}
	matched, tele := MatchBugMemory(lib, files, "acme", "example", 99)
	ids := map[string]bool{}
	for _, e := range matched {
		ids[e.ID] = true
	}
	for _, want := range []string{"path-hit", "kw-added", "kw-removed"} {
		if !ids[want] {
			t.Errorf("want %s matched; got %v", want, tele.Matched)
		}
	}
	if ids["no-hit"] || ids["self"] {
		t.Errorf("unexpected match: %v", tele.Matched)
	}
	if len(tele.Excluded) != 1 || tele.Excluded[0] != "self" {
		t.Errorf("structural LOO: want [self] excluded, got %v", tele.Excluded)
	}
	// Same entry, different PR -> not excluded.
	_, tele2 := MatchBugMemory(lib, files, "acme", "example", 100)
	if len(tele2.Excluded) != 0 {
		t.Errorf("different PR should not exclude: %v", tele2.Excluded)
	}
	// Nil library -> nothing, no panic.
	if m, _ := MatchBugMemory(nil, files, "acme", "example", 1); m != nil {
		t.Errorf("nil lib should match nothing")
	}
}

func TestMatchBugMemory_CapsAndTiebreak(t *testing.T) {
	var entries []BugMemoryEntry
	// 5 same-class entries all matching -> per-class cap 2 applies.
	for i := 0; i < 5; i++ {
		entries = append(entries, mkEntry(fmt.Sprintf("same-%d", i), "one-class", "p", []string{"**/*.go"}, nil, i))
	}
	// 6 distinct-class entries -> total cap 6 applies across everything.
	for i := 0; i < 6; i++ {
		entries = append(entries, mkEntry(fmt.Sprintf("uniq-%d", i), fmt.Sprintf("class-%d", i), "p", []string{"**/*.go"}, nil, 100+i))
	}
	// One double-trigger entry must outrank the single-trigger ones.
	entries = append(entries, mkEntry("double", "dbl", "p", []string{"**/*.go"}, []string{"needle"}, 200))
	lib := &BugMemoryLibrary{Version: "v0", Entries: entries}
	files := []diffFile{{Path: "a/b.go", Added: []string{"has needle here"}}}
	matched, tele := MatchBugMemory(lib, files, "acme", "example", 1)
	if len(matched) != bugMemoryMaxEntries {
		t.Fatalf("want cap %d, got %d (%v)", bugMemoryMaxEntries, len(matched), tele.Matched)
	}
	if matched[0].ID != "double" {
		t.Errorf("double-trigger entry should rank first, got %v", tele.Matched)
	}
	perClass := map[string]int{}
	for _, e := range matched {
		perClass[e.Class]++
	}
	if perClass["one-class"] > bugMemoryMaxPerClass {
		t.Errorf("per-class cap violated: %v", tele.Matched)
	}
}

func TestBugMemorySection_EmptyAndBudget(t *testing.T) {
	if got := bugMemorySection(nil); got != "" {
		t.Errorf("no matches must render nothing, got %q", got)
	}
	long := strings.Repeat("x", bugMemoryPatternMax)
	var entries []BugMemoryEntry
	for i := 0; i < 30; i++ {
		entries = append(entries, mkEntry(fmt.Sprintf("e%d", i), "c", long, []string{"**"}, nil, i))
	}
	if got := bugMemorySection(entries); len(got) > bugMemorySectionBudget+200 {
		t.Errorf("section exceeds budget: %d chars", len(got))
	}
}

func TestBuildAgentPromptContent_MemorySection(t *testing.T) {
	// No gates, no memory -> byte-identical to a memoryless build.
	base, err := buildAgentPromptContent("", nil, "", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(base, "BUG HISTORY") {
		t.Error("empty memory must not add the section")
	}
	withMem, err := buildAgentPromptContent("", nil, "", nil, nil, []BugMemoryEntry{
		mkEntry("e1", "c", "watch for dropped ids in context menus", []string{"**"}, nil, 1),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withMem, "THIS REPO'S BUG HISTORY") ||
		!strings.Contains(withMem, "dropped ids in context menus") ||
		!strings.Contains(withMem, "priors to check, not findings to report") {
		t.Errorf("memory section missing pieces:\n%s", withMem)
	}
}

func TestLoadBugMemory_InvalidGlobDropped(t *testing.T) {
	data := []byte(`{"version":"v0","entries":[
		{"id":"bad-glob","source_repo":"acme/example","source_prs":[1],"class":"c","pattern":"p","trigger_paths":["src/[oops"]},
		{"id":"good","source_repo":"acme/example","source_prs":[2],"class":"c","pattern":"p","trigger_paths":["src/**"]}
	]}`)
	lib, dropped, err := LoadBugMemory(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Entries) != 1 || lib.Entries[0].ID != "good" {
		t.Fatalf("want only 'good' kept, got %+v", lib.Entries)
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "invalid trigger path glob") {
		t.Errorf("want invalid-glob drop reason, got %v", dropped)
	}
}

func TestSanitizeText_RuneBoundary(t *testing.T) {
	p := strings.Repeat("x", bugMemoryPatternMax-1) + "héllo"
	got := sanitizeText(p, bugMemoryPatternMax)
	if len(got) > bugMemoryPatternMax {
		t.Fatalf("not truncated: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got[len(got)-4:])
	}
}
