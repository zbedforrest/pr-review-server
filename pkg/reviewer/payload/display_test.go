package payload

import (
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestBuild_DisplaySurvivesToLineCommentsAndKeepsTheFingerprint(t *testing.T) {
	plain := []types.LineComment{{FilePath: "a.go", LineNumber: 1, Importance: "MEDIUM", CommentBody: "Real bug"}}
	rewritten := []types.LineComment{{FilePath: "a.go", LineNumber: 1, Importance: "MEDIUM", CommentBody: "Real bug", Display: &types.Display{Title: "Short", Body: "Body."}}}
	a, b := Build("o", "r", 1, "sha", plain, "", nil), Build("o", "r", 1, "sha", rewritten, "", nil)
	if a.Findings[0].ID != b.Findings[0].ID {
		t.Fatal("the rewrite changed the finding's fingerprint")
	}
	round := b.ToLineComments()
	if len(round) != 1 || round[0].Display == nil || round[0].Display.Title != "Short" {
		t.Fatalf("ToLineComments display = %+v", round)
	}
}
