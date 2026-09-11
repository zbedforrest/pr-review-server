package service

import (
	"testing"

	"pr-review-server/pkg/reviewer/types"
)

func TestVerdictFromComments(t *testing.T) {
	summary := func(body string) []types.LineComment {
		return []types.LineComment{
			{FilePath: "a/b.ts", LineNumber: 10, Importance: "MEDIUM", CommentBody: "a finding"},
			{FilePath: "SUMMARY", LineNumber: 0, CommentBody: body},
		}
	}

	cases := []struct {
		name     string
		comments []types.LineComment
		want     string
	}{
		{
			name:     "request changes",
			comments: summary("Overall: **Request changes** — the cache key omits the tenant id."),
			want:     VerdictRequestChanges,
		},
		{
			name:     "request-changes hyphenated",
			comments: summary("verdict: request-changes"),
			want:     VerdictRequestChanges,
		},
		{
			name:     "approve with suggestions",
			comments: summary("Verdict: Approve with suggestions. Two minor cleanups noted."),
			want:     VerdictApproveSuggestions,
		},
		{
			name:     "approve with suggestion singular",
			comments: summary("APPROVE WITH SUGGESTION — see the LOW item."),
			want:     VerdictApproveSuggestions,
		},
		{
			name:     "plain approve",
			comments: summary("Small, well-tested change. Verdict: Approve."),
			want:     VerdictApprove,
		},
		{
			// Ordering mirrors the benchmark parser: "request changes"
			// anywhere in the body wins even if "approve" also appears.
			name:     "request changes outranks approve",
			comments: summary("I'd approve the refactor alone, but the auth regression means: request changes."),
			want:     VerdictRequestChanges,
		},
		{
			// "approve with suggestions" contains "approve"; the more
			// specific phrase must be checked first.
			name:     "approve with suggestions not downgraded to approve",
			comments: summary("approve with suggestions"),
			want:     VerdictApproveSuggestions,
		},
		{
			// A ledger-bearing SUMMARY (EnforceRequiredChecks appends the
			// required-checks table): VIOLATED rows are check verdicts, not
			// review verdicts, and must not flip an approve body.
			name: "ledger VIOLATED rows do not misparse",
			comments: summary("Solid change overall. Verdict: Approve.\n\n---\n" +
				"_Required checks (id | verdict | evidence)_\n" +
				"- CHK-gate-1 | VIOLATED | overlay/portal.tsx\n" +
				"- CHK-mem-2 | SAFE | handlers/topics.py\n"),
			want: VerdictApprove,
		},
		{
			name: "ledger under request changes still request changes",
			comments: summary("Verdict: Request changes (testing).\n\n---\n" +
				"_Required checks (id | verdict | evidence)_\n" +
				"- CHK-gate-1 | UNANSWERED | -\n"),
			want: VerdictRequestChanges,
		},
		{
			name:     "no recognizable verdict",
			comments: summary("The diff touches three files; here is a prose dump with no decision."),
			want:     "",
		},
		{
			name: "no SUMMARY entry",
			comments: []types.LineComment{
				{FilePath: "a/b.ts", LineNumber: 10, CommentBody: "request changes here"},
			},
			want: "",
		},
		{
			name:     "nil comments",
			comments: nil,
			want:     "",
		},
		{
			// Only the first SUMMARY body is read, mirroring the benchmark
			// parser (MergeFindings keeps only the canonical set's SUMMARY).
			name: "first SUMMARY wins",
			comments: []types.LineComment{
				{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Verdict: approve"},
				{FilePath: "SUMMARY", LineNumber: 0, CommentBody: "Verdict: request changes"},
			},
			want: VerdictApprove,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerdictFromComments(tc.comments); got != tc.want {
				t.Errorf("VerdictFromComments() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerdictFromComments_PrefersTheStructuredVerdict(t *testing.T) {
	comments := []types.LineComment{{FilePath: "SUMMARY",
		CommentBody: "Verdict: approve.\n\nNothing here would make me request changes.",
		Summary:     &types.SummaryBlock{Verdict: "approve"}}}
	if got := VerdictFromComments(comments); got != VerdictApprove {
		t.Fatalf("structured verdict must win over prose that happens to contain another verdict's words: %q", got)
	}
	prose := []types.LineComment{{FilePath: "SUMMARY", CommentBody: "Verdict: request changes. The retry is unbounded."}}
	if got := VerdictFromComments(prose); got != VerdictRequestChanges {
		t.Fatalf("prose summaries still parse: %q", got)
	}
}
