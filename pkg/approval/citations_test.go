package approval

import (
	"context"
	"testing"
)

func TestFixedCitationRequiresChangeAtCitedCode(t *testing.T) {
	repo, base, head := fixtureRepository(t)
	snapshot := Snapshot{Revision: Revision{Head: head, Base: base, MergeBase: base}, AllowedRevisions: []string{base, head}}
	for _, tc := range []struct {
		name             string
		oldLine, newLine int
		oldText, newText string
		valid            bool
	}{
		{"changed line", 2, 2, "two", "changed", true},
		{"different unchanged lines", 1, 3, "one", "three", false},
		{"unchanged context", 1, 1, "one", "one", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assessment := Assessment{Concerns: []Concern{{ID: "concern", Disposition: "fixed", OriginalRevision: base, Citations: []Citation{{Path: "file.txt", Revision: base, StartLine: tc.oldLine, EndLine: tc.oldLine, Excerpt: tc.oldText}, {Path: "file.txt", Revision: head, StartLine: tc.newLine, EndLine: tc.newLine, Excerpt: tc.newText}}}}}
			err := validateCitations(context.Background(), snapshot, repo, &assessment)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
}
