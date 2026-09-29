package approval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
			assessment := Assessment{Concerns: []Concern{{ID: "concern", Path: "file.txt", StartLine: tc.oldLine, EndLine: tc.oldLine, Disposition: "fixed", OriginalRevision: base, Citations: []Citation{{Path: "file.txt", Revision: base, StartLine: tc.oldLine, EndLine: tc.oldLine, Excerpt: tc.oldText}, {Path: "file.txt", Revision: head, StartLine: tc.newLine, EndLine: tc.newLine, Excerpt: tc.newText}}}}}
			err := validateCitations(context.Background(), snapshot, repo, &assessment)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
}

type citationRepositoryFixture struct {
	head, old string
	diffReads []string
}

func (r *citationRepositoryFixture) Read(_ context.Context, name string, q ReadRequest) (ReadResult, error) {
	const handler = "func permit(user User) bool {\n    return user.Enabled && user.Trusted\n}"
	switch name {
	case "read_file":
		text := handler
		if q.Path == "README.md" {
			text = "Original deployment instructions"
			if q.Revision == r.head {
				text = "Updated deployment instructions"
			}
		}
		lines := strings.Split(text, "\n")
		return ReadResult{Text: strings.Join(lines[q.StartLine-1:q.EndLine], "\n")}, nil
	case "read_diff":
		r.diffReads = append(r.diffReads, q.Path)
		if q.Path == "handler.go" {
			return ReadResult{Text: ""}, nil
		}
		if q.Path == "README.md" {
			return ReadResult{Text: "@@ -1 +1 @@\n-Original deployment instructions\n+Updated deployment instructions\n"}, nil
		}
	}
	return ReadResult{}, fmt.Errorf("unexpected read")
}

func TestFixedCitationReadsLaterDiffPages(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "objects.git")
	fixtureGit(t, dir, "", "init", "--bare", "--template=", dir)
	lines := make([]string, 1000)
	for i := range lines {
		lines[i] = fmt.Sprintf("unchanged source line %04d", i)
	}
	lines[499] = "return user.Enabled && user.Trusted"
	oldBlob := fixtureGit(t, dir, strings.Join(lines, "\n"), "hash-object", "-w", "--stdin")
	oldTree := fixtureGit(t, dir, "100644 blob "+oldBlob+"\thandler.go\n", "mktree")
	old := fixtureGit(t, dir, "before\n", "commit-tree", oldTree)
	lines[499] = "return user.Enabled && user.Trusted && user.Authorized"
	headText := strings.Repeat("additional source statement\n", 400) + strings.Join(lines, "\n")
	headBlob := fixtureGit(t, dir, headText, "hash-object", "-w", "--stdin")
	headTree := fixtureGit(t, dir, "100644 blob "+headBlob+"\thandler.go\n", "mktree")
	head := fixtureGit(t, dir, "after\n", "commit-tree", headTree, "-p", old)
	repo := &GitRepository{directory: dir, revisions: map[string]bool{head: true, old: true}}
	s := Snapshot{Revision: Revision{Head: head, Base: old, MergeBase: old}, AllowedRevisions: []string{head, old}}
	a := Assessment{Concerns: []Concern{{ID: "concern", Path: "handler.go", StartLine: 500, EndLine: 500, OriginalRevision: old, Disposition: "fixed", Citations: []Citation{{Revision: old, Path: "handler.go", StartLine: 500, EndLine: 500, Excerpt: "return user.Enabled && user.Trusted"}, {Revision: head, Path: "handler.go", StartLine: 900, EndLine: 900, Excerpt: "return user.Enabled && user.Trusted && user.Authorized"}}}}}
	first, err := repo.Read(context.Background(), "read_diff", ReadRequest{Revision: head, OtherRevision: old, Path: "handler.go"})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("fixture failed to produce multiple diff pages")
	}
	second, err := repo.Read(context.Background(), "read_diff", ReadRequest{Revision: head, OtherRevision: old, Path: "handler.go", Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Text, "+return user.Enabled && user.Trusted && user.Authorized") {
		t.Fatal("fix was not on second page")
	}
	err = validateCitations(context.Background(), s, repo, &a)
	if err != nil {
		t.Fatalf("valid fix on later diff page rejected: %v", err)
	}
}

func TestFixedCitationRejectsUnrelatedChangedFile(t *testing.T) {
	head, old := strings.Repeat("a", 40), strings.Repeat("b", 40)
	canonical := Concern{ID: "concern", EvidenceIDs: []string{"finding"}, Claim: "Missing authorization check allows forbidden requests", OriginalSeverity: "high", OriginalRevision: old, Path: "handler.go", StartLine: 2, EndLine: 2, Impact: "unknown"}
	s := Snapshot{Target: Target{ExpectedHeadSHA: head}, Revision: Revision{Head: head, Base: old, MergeBase: old}, Eligible: true, Sources: []Source{{ID: "old_review", Provider: "prism", Verified: true, Completion: "completed", ReviewedSHA: old, FileCoverage: "not_reported"}, {ID: "current-review", Provider: "prism", Verified: true, Completion: "completed", ReviewedSHA: head, FileCoverage: "not_reported"}}, Evidence: []Evidence{{ID: "finding", SourceID: "old_review", Body: canonical.Claim, ReviewedSHA: old, Path: "handler.go", StartLine: 2, EndLine: 2, ConcernIDs: []string{"concern"}}, {ID: "current-summary", SourceID: "current-review", Body: "No additional findings in current review", ReviewedSHA: head}}, Concerns: []Concern{canonical}, Manifest: Manifest{Complete: true, Endpoints: []Endpoint{{Name: "fixture", Complete: true}}}, Checks: []Check{{Name: "ci", State: "success", SHA: head}}, AllowedRevisions: []string{head, old}}
	CanonicalizeSnapshot(&s)
	assessed := canonical
	assessed.Disposition = "fixed"
	assessed.Rationale = "The cited change addresses the reported authorization concern."
	assessed.Citations = []Citation{
		{Revision: old, Path: "handler.go", StartLine: 2, EndLine: 2, Excerpt: "return user.Enabled && user.Trusted"},
		{Revision: head, Path: "handler.go", StartLine: 1, EndLine: 3, Excerpt: "func permit(user User) bool {\n    return user.Enabled && user.Trusted\n}"},
		{Revision: old, Path: "README.md", StartLine: 1, EndLine: 1, Excerpt: "Original deployment instructions"},
		{Revision: head, Path: "README.md", StartLine: 1, EndLine: 1, Excerpt: "Updated deployment instructions"},
	}
	a := Assessment{SnapshotID: s.ID, SnapshotDigest: s.Digest, Summary: "Concern addressed by the cited change", Concerns: []Concern{assessed}, Artifacts: []ArtifactDisposition{{EvidenceID: "finding", Classification: "concerns", Rationale: "A supported concern", ConcernIDs: []string{"concern"}}, {EvidenceID: "current-summary", Classification: "non_actionable", Rationale: "No additional findings"}}}
	repository := &citationRepositoryFixture{head: head, old: old}
	if err := validateCitations(context.Background(), s, repository, &a); err == nil {
		t.Fatal("unrelated changed file accepted as proof of a fix")
	}
	if strings.Contains(strings.Join(repository.diffReads, ","), "README.md") {
		t.Fatal("unrelated file was read as fix evidence")
	}
}
