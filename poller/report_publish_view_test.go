package poller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/html"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

func TestReportHeldMatchesWhatThePublisherHolds(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	p := &Poller{cfg: &config.Config{}, db: database}
	t.Setenv(publisher.GateEndorsedInlineEnv, "true")

	finding := func(line int, sev string, v *types.Verification) types.LineComment {
		return types.LineComment{FilePath: "a.go", LineNumber: line, Importance: sev, State: "confirmed",
			CommentBody: fmt.Sprintf("Handle%d drops the error from Save, so a failed write reports success.", line), Verification: v}
	}
	cases := []struct {
		name string
		c    types.LineComment
		held bool
	}{
		{"kept and graded good", finding(1, "MEDIUM", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "GOOD", GateApplied: true}), false},
		{"unverified by anyone", finding(2, "MEDIUM", nil), false},
		{"refuted", finding(3, "MEDIUM", &types.Verification{Status: types.VerifyDrop, Applied: true}), true},
		{"refuted in shadow", finding(4, "MEDIUM", &types.Verification{Status: types.VerifyDrop}), false},
		{"held", finding(5, "MEDIUM", &types.Verification{Status: types.VerifyHold, Applied: true}), true},
		{"low value", finding(6, "LOW", &types.Verification{Status: types.VerifyKeep, Applied: true, LowValue: true}), true},
		{"graded bad", finding(7, "MEDIUM", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true}), true},
		{"critical graded bad is demoted and posted", finding(8, "LOW", &types.Verification{Status: types.VerifyKeep, Applied: true, Gate: "BAD", GateApplied: true, GateDemoted: true}), false},
	}
	result := &service.ReviewResult{}
	for _, tc := range cases {
		result.Comments = append(result.Comments, tc.c)
	}
	view := p.reportPublishView(result)
	require.NotNil(t, view.Held)
	for _, tc := range cases {
		assert.Equal(t, tc.held, view.Held(tc.c) != "", tc.name)
	}
	assert.Equal(t, "the verifier refuted it", view.Held(cases[2].c))
	assert.Equal(t, "the verifier could not stand behind it as written", view.Held(cases[4].c))
	assert.Equal(t, "the verifier judged it true but not worth posting", view.Held(cases[5].c))
	assert.True(t, view.Inline(cases[0].c), "a gate-endorsed finding posts inline")
	assert.False(t, view.Inline(cases[1].c), "an unverified finding does not")
	assert.Equal(t, publisher.MergeConfidence(0, 1, false), view.Confidence, "only the endorsed MEDIUM counts; held ones do not")
	assert.Equal(t, "Handle1 drops the error from Save, so a failed write reports success.", view.Title(cases[0].c), "the index is titled the way the PR lists the finding")
	assert.Empty(t, view.Title(types.LineComment{FilePath: "z.go", CommentBody: "Unknown."}))

	t.Setenv(ReportHideWithheldEnv, "off")
	assert.Nil(t, p.reportPublishView(result).Held)
}

func TestWithPublishDecisionsFollowsWhatTheRoundDid(t *testing.T) {
	settled := types.LineComment{FilePath: "a.go", LineNumber: 1, CommentBody: "Settled on an earlier PR."}
	threaded := types.LineComment{FilePath: "a.go", LineNumber: 2, CommentBody: "Has an inline thread."}
	refuted := types.LineComment{FilePath: "a.go", LineNumber: 3, CommentBody: "Refuted before publishing."}
	key := func(c types.LineComment) string { return payload.Fingerprint(c.FilePath, c.LineNumber, c.CommentBody) }
	before := html.PublishView{Held: func(c types.LineComment) string {
		if c.CommentBody == refuted.CommentBody {
			return "the verifier refuted it"
		}
		return ""
	}, Confidence: 3}
	rep := &publisher.Report{Confidence: 4, Decisions: &publisher.Decisions{
		Dropped: map[string]bool{key(settled): true}, Inline: map[string]bool{key(threaded): true}}}

	after := withPublishDecisions(before, rep)
	assert.Equal(t, heldDropped, after.Held(settled))
	assert.Equal(t, "the verifier refuted it", after.Held(refuted))
	assert.Empty(t, after.Held(threaded))
	assert.True(t, after.Inline(threaded))
	assert.False(t, after.Inline(settled))
	assert.Equal(t, 4, after.Confidence)

	assert.Equal(t, 3, withPublishDecisions(before, &publisher.Report{}).Confidence)

	rep.Decisions.Unbacked = map[string]bool{key(refuted): true}
	assert.Equal(t, "the verifier refuted it"+heldThreadOpen, withPublishDecisions(before, rep).Held(refuted))
}

func TestReportPublishViewForSaysWhyTheRunWillNotPost(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	p := &Poller{cfg: &config.Config{}, db: database}
	result := &service.ReviewResult{Comments: []types.LineComment{{FilePath: "a.go", LineNumber: 1, Importance: "LOW", CommentBody: "Unused."}}}
	job := func(mutate func(*ReviewJob)) ReviewJob {
		j := ReviewJob{}
		j.PR.Author = "octocat"
		mutate(&j)
		return j
	}

	assert.Equal(t, notPublishedNotAsked, p.reportPublishViewFor(job(func(j *ReviewJob) { j.SkipPublish = true }), result).NotPublished)
	assert.Equal(t, notPublishedDraft, p.reportPublishViewFor(job(func(j *ReviewJob) { j.PR.Draft = true }), result).NotPublished)
	assert.Equal(t, p.reviewPublishBound(job(func(*ReviewJob) {})), p.reportPublishViewFor(job(func(*ReviewJob) {}), result).NotPublished == "",
		"the report and the pipeline agree on whether the run posts")

	t.Setenv(ReportHideWithheldEnv, "off")
	assert.Empty(t, p.reportPublishViewFor(job(func(j *ReviewJob) { j.SkipPublish = true }), result).NotPublished, "a page that hides nothing makes no claim about posting")
}

func TestNotPublishedReason(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		publicationPosted:        "",
		publicationAlreadyPosted: "an earlier run already posted a review of this commit",
		publicationNotAllowed:    notPublishedAuthor,
		publicationMuted:         "the author muted PRism on this pull request",
		publicationUnavailable:   "posting is not set up on this server",
		publicationSkippedPrefix + "pull request head moved past the reviewed commit": "the pull request head moved past the reviewed commit",
		publicationFailedPrefix + "load ledger":                                       "posting to GitHub failed",
	}
	for outcome, want := range cases {
		assert.Equal(t, want, notPublishedReason(outcome), outcome)
	}
}

func TestRefreshReportAfterPublishRendersWhyNothingPosted(t *testing.T) {
	var got []string
	rr := &ReviewResult{RenderAfterPublish: func(rep *publisher.Report, notPublished string) []byte {
		got = append(got, notPublished)
		return nil
	}}
	p := &Poller{cfg: &config.Config{}}
	pr := github.PullRequest{Owner: "acme", Repo: "widgets", Number: 1}

	p.refreshReportAfterPublish(t.Context(), pr, rr, nil, publicationMuted, nil)
	assert.Empty(t, got, "off by default")

	t.Setenv(ReportPublishDecisionsEnv, "true")
	p.refreshReportAfterPublish(t.Context(), pr, rr, nil, publicationMuted, nil)
	p.refreshReportAfterPublish(t.Context(), pr, rr, nil, publicationPosted, nil)
	p.refreshReportAfterPublish(t.Context(), pr, rr, nil, "", nil)
	p.refreshReportAfterPublish(t.Context(), pr, rr, &publisher.Report{Decisions: &publisher.Decisions{}}, publicationPosted, nil)
	assert.Equal(t, []string{"the author muted PRism on this pull request", ""}, got)
}
