package poller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/reviewer/service"
	"pr-review-server/pkg/reviewer/types"
)

func TestReportWithholdMatchesWhatThePublisherHolds(t *testing.T) {
	database, err := db.NewGormSQLite(":memory:")
	require.NoError(t, err)
	defer database.Close()
	p := &Poller{cfg: &config.Config{}, db: database}
	t.Setenv("publisher.GateEndorsedInlineEnv, "true")

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
	require.NotNil(t, view.Withhold)
	for _, tc := range cases {
		assert.Equal(t, tc.held, view.Withhold(tc.c), tc.name)
	}
	assert.Equal(t, publisher.MergeConfidence(0, 1, false), view.Confidence, "only the endorsed MEDIUM counts; held ones do not")

	t.Setenv(ReportHideWithheldEnv, "off")
	assert.Nil(t, p.reportPublishView(result).Withhold)
}
