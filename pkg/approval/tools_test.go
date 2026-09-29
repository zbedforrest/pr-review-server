package approval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestChangedLinesSupportCountsContentBeginningWithDiffMarkers(t *testing.T) {
	diff := "--- a/auth.go\n+++ b/auth.go\n@@ -0,0 +1,2 @@\n+++counter\n+authorize(request)\n"
	if !changedLinesSupport(diff, 2, 2) {
		t.Fatal("added operator line shifted the authorization anchor")
	}
	if changedLinesSupport(diff, 3, 3) {
		t.Fatal("accepted a line beyond the changed range")
	}
	if !changedLinesSupport("@@ -1,1 +1,0 @@\n---unsafeCounter\n", 1, 1) {
		t.Fatal("deleted operator line was mistaken for a header")
	}
}

func TestReadEvidenceBatchIsBoundedAndAtomic(t *testing.T) {
	s, _ := validFixture()
	s.Evidence = append(s.Evidence, Evidence{ID: "second", Body: "second complete body"})
	text, err := dispatch(context.Background(), s, nil, "read_evidence", json.RawMessage(`{"evidence_ids":["review","second"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var artifacts []Evidence
	if err := json.Unmarshal([]byte(text), &artifacts); err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 || artifacts[0].Body == "" || artifacts[1].Body == "" {
		t.Fatal("batch omitted bodies")
	}
	for _, args := range []string{`{"evidence_ids":["review","missing"]}`, `{"evidence_id":"review","evidence_ids":["second"]}`, `{"evidence_ids":["review","review"]}`, `{"evidence_ids":[]}`} {
		if _, err := dispatch(context.Background(), s, nil, "read_evidence", json.RawMessage(args)); err == nil {
			t.Fatal("accepted invalid batch", args)
		}
	}
	s.Evidence[1].Body = strings.Repeat("x", 65536)
	if _, err := dispatch(context.Background(), s, nil, "read_evidence", json.RawMessage(`{"evidence_ids":["review","second"]}`)); err == nil {
		t.Fatal("oversized batch accepted")
	}
}

func TestEvidenceInventoryOnlyIncludesPageSources(t *testing.T) {
	s, _ := validFixture()
	s.Sources = append(s.Sources, Source{ID: "not-on-page", Provider: "generic"})
	text, err := dispatch(context.Background(), s, nil, "list_evidence", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "not-on-page") {
		t.Fatal("unpaged source inventory leaked into a page")
	}
}

func TestChangedLinesRequireTheConcernHunk(t *testing.T) {
	diff := "@@ -1,3 +1,3 @@\n unchanged source\n-old value\n+new value\n unchanged source\n@@ -500,3 +500,3 @@\n distant source\n-old unrelated value\n+new unrelated value\n distant source\n"
	concern := Concern{StartLine: 2, EndLine: 2}
	if !changedLinesSupportAtAnchor(diff, 2, 2, &concern) {
		t.Fatal("relevant changed hunk rejected")
	}
	if changedLinesSupportAtAnchor(diff, 501, 501, &concern) {
		t.Fatal("unrelated hunk in the same file accepted")
	}
}
