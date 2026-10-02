package replaykit

import (
	"testing"

	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/payload"
)

func TestSameDefect(t *testing.T) {
	a := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately."
	b := "fetchUser hammers the endpoint after a 429 response because no delay is inserted between attempts."
	if SameDefect("api/users.go", 140, a, nil, "api/users.go", 143, b, nil) {
		t.Fatal("low-overlap rewording without subjects must not match")
	}
	if !SameDefect("api/users.go", 140, a, []string{"fetchuser"}, "api/users.go", 143, b, []string{"fetchuser"}) {
		t.Fatal("shared subject must match")
	}
	if !SameDefect("api/users.go", 140, a, nil, "api/users.go", 145, a, nil) {
		t.Fatal("same text, same file and nearby line must match")
	}
	if SameDefect("api/users.go", 140, a, nil, "src/api/users.go", 145, a, nil) {
		t.Fatal("a nested path that merely ends in the other must not match")
	}
	if SameDefect("api/users.go", 140, a, []string{"fetchuser"}, "api/users.go", 200, a, []string{"fetchuser"}) {
		t.Fatal("lines more than ten apart must not match")
	}
	if SameDefect("api/users.go", 140, a, nil, "api/other.go", 140, a, nil) {
		t.Fatal("different files must not match")
	}
	if SameDefect("a/index.ts", 10, a, nil, "b/index.ts", 10, a, nil) {
		t.Fatal("a shared basename under different directories must not match")
	}
}

func TestFileOfFingerprint(t *testing.T) {
	cases := map[string]string{
		"svc/retry.go:7:f1f1f1f1f1f1": "svc/retry.go",
		"a:b/c.go:0:abcdef012345":     "a:b/c.go",
		"SUMMARY:0:abcdef012345":      "SUMMARY",
		"noseparators":                "noseparators",
	}
	for fp, want := range cases {
		if got := FileOfFingerprint(fp); got != want {
			t.Errorf("FileOfFingerprint(%q) = %q, want %q", fp, got, want)
		}
	}
}

func TestPatchesFromHunks(t *testing.T) {
	pl := payload.Payload{Findings: []payload.Finding{
		{File: "a.go", Line: 72, DiffHunk: "@@ -70,3 +70,3 @@\n x\n+y\n z"},
		{File: "a.go", Line: 90, DiffHunk: "@@ -88,3 +88,3 @@\n x\n y\n z"},
		{File: "b.go", Line: 5},
		{File: "SUMMARY", Line: 0},
	}}
	patches := PatchesFromHunks(pl)
	commentable := publisher.CommentableLines(patches["a.go"])
	for _, l := range []int{70, 71, 72, 88, 89, 90} {
		if !commentable[l] {
			t.Errorf("a.go line %d should be commentable", l)
		}
	}
	if commentable[80] {
		t.Error("a.go line 80 lies between hunks")
	}
	if !publisher.CommentableLines(patches["b.go"])[5] {
		t.Error("a finding without a hunk keeps its own line")
	}
	if _, ok := patches["SUMMARY"]; ok {
		t.Error("narrative findings have no patch")
	}
}
