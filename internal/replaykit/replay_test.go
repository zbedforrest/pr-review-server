package replaykit

import (
	"testing"
	"time"

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

func TestExternalDuplicate(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	text := "The retry loop in fetchUser never backs off, so a 429 from upstream is retried immediately and the rate limiter keeps rejecting every call."
	roots := []ExternalRoot{
		{CommentID: 1, File: "api/users.go", Line: 143, Text: "fetchUser hammers the endpoint after a 429 response because no delay is inserted between attempts.", At: at.Add(-time.Minute)},
		{CommentID: 2, File: "api/users.go", Line: 141, Text: "Retry never backs off: the loop retries a 429 at once and the rate limiter rejects every call.", At: at.Add(-time.Minute)},
		{CommentID: 3, File: "api/users.go", Line: 140, Text: text, At: at.Add(time.Minute)},
	}
	if r, ok := ExternalDuplicate("api/users.go", 140, text, nil, roots, at); !ok || r.CommentID != 2 {
		t.Fatalf("containment finds the earlier root on the point: %+v %v", r, ok)
	}
	if r, ok := ExternalDuplicate("api/users.go", 140, text, []string{"fetchuser"}, roots[:1], at); !ok || r.CommentID != 1 {
		t.Fatalf("a named subject carries a low-overlap root: %+v %v", r, ok)
	}
	if _, ok := ExternalDuplicate("api/users.go", 140, text, nil, roots[:1], at); ok {
		t.Fatal("two shared words are not the same point")
	}
	if _, ok := ExternalDuplicate("api/users.go", 140, text, nil, roots[2:], at); ok {
		t.Fatal("a root posted after PRism's is not an earlier duplicate")
	}
	if _, ok := ExternalDuplicate("api/users.go", 170, text, nil, roots[1:2], at); ok {
		t.Fatal("thirty lines away is another point")
	}
}

func TestExternalRoots_SkipsBotAndPrismRoots(t *testing.T) {
	yes := true
	var d PRDump
	mk := func(id int64, login, body string, resolved *bool, reply bool) {
		var th Thread
		th.Path, th.OriginalLine, th.IsResolved = "a.go", 10, resolved
		c := ThreadComment{DatabaseID: FlexInt64(id), Author: &Actor{Login: login}, Body: body, CreatedAt: time.Date(2026, 9, 1, 0, 0, int(id), 0, time.UTC)}
		if reply {
			c.ReplyTo = &struct {
				DatabaseID FlexInt64 `json:"databaseId"`
			}{DatabaseID: 1}
		}
		th.Comments.Nodes = []ThreadComment{c}
		d.ReviewThreads.Nodes = append(d.ReviewThreads.Nodes, th)
	}
	mk(3, "greptile-apps[bot]", "<b>Later root</b>", &yes, false)
	mk(1, "octocat", "A human root", nil, false)
	mk(2, "acme-bot", "<!-- prism:finding:a.go:1:abc --> own root", nil, false)
	mk(4, "octocat", "a reply", nil, true)
	roots := d.ExternalRoots(map[string]bool{"acme-bot": true})
	if len(roots) != 2 || roots[0].CommentID != 1 || roots[0].ResolvedKnown || roots[1].CommentID != 3 || !roots[1].Resolved || roots[1].Text != "Later root" {
		t.Fatalf("roots = %+v", roots)
	}
}
