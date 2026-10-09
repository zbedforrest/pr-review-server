package github

import "testing"

const patchA = `diff --git a/pkg/a.go b/pkg/a.go
index 1111111..2222222 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -10,3 +10,4 @@ func A() {
 	x := 1
+	y := 2
 	return x
 }
`

func TestPatchID_IgnoresLineNumbersIndexAndWhitespace(t *testing.T) {
	moved := `diff --git a/pkg/a.go b/pkg/a.go
index 3333333..4444444 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -42,3 +42,4 @@ func A() {
 	x := 1
+	y := 2
 	return x
 }
`
	if PatchID(patchA) != PatchID(moved) {
		t.Fatal("a merge from the base branch moves hunk headers and index lines only; the id must not move")
	}
	if PatchID(patchA) != PatchID(patchA+"\n   \n") {
		t.Fatal("trailing whitespace must not move the id")
	}
}

func TestPatchID_ChangesWithContent(t *testing.T) {
	edited := `diff --git a/pkg/a.go b/pkg/a.go
index 1111111..2222222 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -10,3 +10,4 @@ func A() {
 	x := 1
+	y := 3
 	return x
 }
`
	if PatchID(patchA) == PatchID(edited) {
		t.Fatal("a changed added line must move the id")
	}
	if PatchID(patchA) == PatchID(patchA+"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-a\n+b\n") {
		t.Fatal("an extra file must move the id")
	}
}

func TestPatchID_EmptyDiffHasNoID(t *testing.T) {
	if PatchID("") != "" || PatchID("  \n") != "" {
		t.Fatal("an empty diff has no id, so nothing can match it")
	}
	if len(PatchID(patchA)) != 40 {
		t.Fatalf("id length = %d", len(PatchID(patchA)))
	}
}
