package server

import (
	"testing"

	"pr-review-server/db"
)

func TestReadyToMerge(t *testing.T) {
	cases := []struct {
		name string
		pr   db.PR
		want bool
	}{
		{"clean open non-draft", db.PR{PRState: "open", MergeStateStatus: "CLEAN"}, true},
		{"clean with approved decision", db.PR{PRState: "open", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED"}, true},
		{"clean legacy row without pr_state", db.PR{MergeStateStatus: "CLEAN"}, true},
		{"blocked", db.PR{PRState: "open", MergeStateStatus: "BLOCKED"}, false},
		{"behind", db.PR{PRState: "open", MergeStateStatus: "BEHIND"}, false},
		{"dirty", db.PR{PRState: "open", MergeStateStatus: "DIRTY"}, false},
		{"unstable", db.PR{PRState: "open", MergeStateStatus: "UNSTABLE"}, false},
		{"has hooks", db.PR{PRState: "open", MergeStateStatus: "HAS_HOOKS"}, false},
		{"draft status", db.PR{PRState: "open", MergeStateStatus: "DRAFT"}, false},
		{"unknown", db.PR{PRState: "open", MergeStateStatus: "UNKNOWN"}, false},
		{"not yet fetched", db.PR{PRState: "open", MergeStateStatus: ""}, false},
		{"clean but changes requested", db.PR{PRState: "open", MergeStateStatus: "CLEAN", ReviewDecision: "CHANGES_REQUESTED"}, false},
		{"clean but review required", db.PR{PRState: "open", MergeStateStatus: "CLEAN", ReviewDecision: "REVIEW_REQUIRED"}, false},
		{"clean but draft flag", db.PR{PRState: "open", MergeStateStatus: "CLEAN", Draft: true}, false},
		{"clean but merged", db.PR{PRState: "merged", MergeStateStatus: "CLEAN"}, false},
		{"clean but closed", db.PR{PRState: "closed", MergeStateStatus: "CLEAN"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readyToMerge(tc.pr); got != tc.want {
				t.Errorf("readyToMerge(%+v) = %v, want %v", tc.pr, got, tc.want)
			}
		})
	}
}
