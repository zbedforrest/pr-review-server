package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestFingerprint_BucketsLineAndHashesNormalizedPrefix(t *testing.T) {
	sum := sha256.Sum256([]byte("nil deref when user is missing"))
	want := "app/views.py:4:" + hex.EncodeToString(sum[:])[:12]

	got := Fingerprint("app/views.py", 47, "  Nil   DEREF when\nuser is MISSING ")

	if got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

func TestFingerprint_StableWithinBucketDistinctAcrossFindings(t *testing.T) {
	base := Fingerprint("a/b.go", 42, "Nil map write: sessions[id] assigned before init")

	if Fingerprint("a/b.go", 45, "Nil map write: sessions[id] assigned before init") != base {
		t.Error("line drift within the same 10-line bucket must keep the fingerprint")
	}
	if Fingerprint("a/b.go", 52, "Nil map write: sessions[id] assigned before init") == base {
		t.Error("a different line bucket must change the fingerprint")
	}
	if Fingerprint("a/other.go", 42, "Nil map write: sessions[id] assigned before init") == base {
		t.Error("a different file must change the fingerprint")
	}
	if Fingerprint("a/b.go", 42, "Unrelated comment about a different defect") == base {
		t.Error("a different comment must change the fingerprint")
	}
}

func TestFingerprint_WholeFileFindingUsesBucketZero(t *testing.T) {
	if got := Fingerprint("a.go", 0, "x"); got[:len("a.go:0:")] != "a.go:0:" {
		t.Fatalf("whole-file fingerprint = %q, want bucket 0", got)
	}
}

func TestFingerprint_HashesOnlyFirst120Runes(t *testing.T) {
	prefix := make([]rune, 120)
	for i := range prefix {
		prefix[i] = 'a'
	}
	base := string(prefix)

	if Fingerprint("f", 1, base+" tail one") != Fingerprint("f", 1, base+" tail two") {
		t.Fatal("comment tails beyond 120 runes must not change the fingerprint")
	}
}

func TestBuild_AssignsFingerprintIDToEveryFinding(t *testing.T) {
	comments := []types.LineComment{
		{FilePath: "foo.go", LineNumber: 12, CommentBody: "off by one", Importance: "MEDIUM"},
		{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Verdict: approve", Importance: "LOW"},
	}

	p := Build("o", "r", 1, "sha", comments, "", nil)

	for _, f := range p.Findings {
		if f.ID != Fingerprint(f.File, f.Line, f.Comment) {
			t.Errorf("finding %s:%d id = %q, want %q", f.File, f.Line, f.ID, Fingerprint(f.File, f.Line, f.Comment))
		}
	}
}
