package types

import "testing"

func TestQualityGateSuppressionFollowsTheAppliedGrade(t *testing.T) {
	cases := []struct {
		v    *Verification
		want bool
	}{
		{&Verification{Gate: "BAD", GateApplied: true}, true},
		{&Verification{Gate: "BAD", GateApplied: true, GateDemoted: true}, false},
		{&Verification{Gate: "BAD"}, false},
		{&Verification{Gate: "OK", GateApplied: true}, false},
	}
	for i, c := range cases {
		if got := c.v.Suppresses(); got != c.want {
			t.Errorf("case %d: Suppresses = %v, want %v", i, got, c.want)
		}
	}
}
