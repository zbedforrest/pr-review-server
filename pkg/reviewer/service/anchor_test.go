package service

import (
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

const anchorDiff = `diff --git a/cmd.py b/cmd.py
--- a/cmd.py
+++ b/cmd.py
@@ -150,4 +150,5 @@ def handle(self):
             changed += 1
-        if flipped:
+        if old_is_churned != new_is_churned:
+            flipped.append(user_id)
             PsychographicProfile.objects.filter(user_id__in=flipped)
`

func TestAnchorWholeFileFindingsUsesQuotedCodeInTheDiff(t *testing.T) {
	comments := []types.LineComment{
		{FilePath: "cmd.py", CommentBody: "Only bumps `PsychographicProfile` when `old_is_churned != new_is_churned`, so other corrections are never exported."},
		{FilePath: "cmd.py", CommentBody: "No completion marker is recorded for the run."},
		{FilePath: "SUMMARY"},
	}
	if n := AnchorWholeFileFindings(comments, anchorDiff); n != 1 {
		t.Fatalf("anchored %d, want 1", n)
	}
	if comments[0].LineNumber != 151 {
		t.Fatalf("anchored to line %d, want the added line matching the most quoted spans (151)", comments[0].LineNumber)
	}
	if comments[1].LineNumber != 0 || comments[2].LineNumber != 0 {
		t.Fatalf("findings without a quoted match stay whole-file: %+v", comments)
	}
}

func TestAnchorWholeFileFindingsLeavesAnchoredAndOtherFilesAlone(t *testing.T) {
	comments := []types.LineComment{
		{FilePath: "cmd.py", LineNumber: 7, CommentBody: "`flipped` is reused"},
		{FilePath: "other.py", CommentBody: "`flipped` is reused"},
		{FilePath: "cmd.py", CommentBody: "`cmd.py` needs a docstring"},
	}
	if n := AnchorWholeFileFindings(comments, anchorDiff); n != 0 {
		t.Fatalf("anchored %d findings that should be left alone: %+v", n, comments)
	}
}
