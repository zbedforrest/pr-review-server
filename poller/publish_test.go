package poller

import (
	"testing"

	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/reviewer/payload"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishEnabledFor(t *testing.T) {
	cases := []struct {
		author, enabled string
		want            bool
	}{
		{"alice", "", false},
		{"", "*", false},
		{"alice", "*", true},
		{"alice", "bob, Alice ,carol", true},
		{"dave", "bob,alice", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, publishEnabledFor(c.author, c.enabled), "author=%q enabled=%q", c.author, c.enabled)
	}
}

const greptileBody = `<a href="#"><img alt="P1" src="https://greptile-static-assets.s3.amazonaws.com/badges/p1.svg?v=9" align="top"></a> **Nil map write on first session**

The sessions map is assigned before init so the first write panics with a nil map.

<a href="https://app.greptile.com/x"><picture><img alt="Fix"></picture></a>`

func TestBuildPublishRound_TagsReconciledFindingsAndBuildsLinks(t *testing.T) {
	pr := github.PullRequest{Owner: "acme", Repo: "example", Number: 7, CommitSHA: "abcdef1234567890", Author: "alice"}
	pl := payload.Payload{Findings: []payload.Finding{
		{ID: "sum", Severity: "unknown", File: "SUMMARY", Comment: "Verdict: approve with suggestions."},
		{ID: "f1", Severity: "critical", File: "pkg/store/sessions.go", Line: 42, Comment: "Nil map write: sessions[id] is assigned before the map is initialised, so the first write panics."},
		{ID: "f2", Severity: "medium", File: "pkg/api/handler.go", Line: 8, Comment: "Missing error check on Decode."},
	}}
	comments := []github.ReviewCommentInfo{
		{ID: 900, Author: "greptile-apps[bot]", Body: greptileBody, Path: "pkg/store/sessions.go", Line: 44},
		{ID: 901, Author: "greptile-apps[bot]", Body: greptileBody, Path: "pkg/other/unrelated.go", Line: 3},
		{ID: 902, Author: "human", Body: "looks fine", Path: "pkg/api/handler.go", Line: 8},
	}
	patches := map[string]string{
		"pkg/store/sessions.go": "@@ -40,3 +40,4 @@\n a\n+b\n+c\n d\n",
	}
	previous := []db.PublishedFinding{{Kind: db.PublishedKindSummary, Fingerprint: "summary", Rounds: 3}}

	r := buildPublishRound(pr, pl, comments, patches, previous, "https://prism.example")

	assert.Equal(t, "acme", r.Owner)
	assert.Equal(t, 7, r.Number)
	assert.Equal(t, "abcdef1234567890", r.HeadSHA)
	assert.Equal(t, "both", r.SourceTags["f1"], "PRism finding near Greptile's comment on the same file")
	assert.Empty(t, r.SourceTags["f2"], "unmatched PRism finding stays prism-only")
	require.Len(t, r.GreptileOnly, 1)
	assert.Equal(t, int64(901), r.GreptileOnly[0].CommentID)
	assert.Equal(t, "pkg/other/unrelated.go", r.GreptileOnly[0].File)
	assert.True(t, r.Commentable["pkg/store/sessions.go"][41], "added line inside the hunk is commentable")
	assert.False(t, r.Commentable["pkg/store/sessions.go"][99])
	assert.Equal(t, previous, r.Previous)
	assert.Equal(t, "https://prism.example/go/agent?o=acme&r=example&n=7", r.AgentLinkBase)
	assert.Equal(t, "https://prism.example/api/review/acme/example/7?format=html", r.DashboardURL)
}

func TestBuildPublishRound_NoBaseURLDisablesLinks(t *testing.T) {
	r := buildPublishRound(github.PullRequest{Owner: "a", Repo: "b", Number: 1}, payload.Payload{}, nil, nil, nil, "")
	assert.Empty(t, r.AgentLinkBase)
	assert.Empty(t, r.DashboardURL)
}

func TestBuildPublishRound_CarriesRequiredCheckViolation(t *testing.T) {
	pr := github.PullRequest{Owner: "a", Repo: "b", Number: 1}
	quiet := buildPublishRound(pr, payload.Payload{RequiredChecks: &payload.RequiredChecksInfo{Issued: 2, Violated: 0}}, nil, nil, nil, "")
	assert.False(t, quiet.RequiredCheckViolated)
	loud := buildPublishRound(pr, payload.Payload{RequiredChecks: &payload.RequiredChecksInfo{Issued: 2, Violated: 1}}, nil, nil, nil, "")
	assert.True(t, loud.RequiredCheckViolated)
}
