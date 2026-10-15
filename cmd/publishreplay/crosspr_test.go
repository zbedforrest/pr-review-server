package main

import (
	"context"
	"testing"

	"pr-review-server/pkg/publisher"
)

func crossPRRun(t *testing.T, policy publisher.Policy) Result {
	t.Helper()
	dumps, err := loadDumps("testdata/crosspr/dumps")
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStore("testdata/crosspr/sidecars", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Dumps: dumps, Store: st, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The author called the purchaser-id point intentional on #6; two days
// later #7 raised it again from another file and the bot posted it (the
// observed root). The replayed publisher seeds the verdict and writes a
// settled row instead of the root.
func TestRun_CrossPRSettledPointIsNotPostedAgain(t *testing.T) {
	res := crossPRRun(t, publisher.DefaultPolicy())
	m := res.Metrics
	if m.SettledPointsSeeded != 1 || m.CrossPRSettled != 1 || m.CrossPRResettled != 0 || m.RootsPosted != 1 || m.Observed.CrossPRResettled != 1 {
		t.Fatalf("metrics = seeded %d settled %d resettled %d roots %d observed %d", m.SettledPointsSeeded, m.CrossPRSettled, m.CrossPRResettled, m.RootsPosted, m.Observed.CrossPRResettled)
	}
	if len(res.CrossPRPairs) != 2 || res.CrossPRPairs[0].Outcome != "settled" || res.CrossPRPairs[0].SettledPR != 6 || res.CrossPRPairs[1].Outcome != "observed" || res.CrossPRPairs[1].PR != "example#7" {
		t.Fatalf("pairs = %+v", res.CrossPRPairs)
	}
	if later := res.PerPR[1]; later.PR != "example#7" || later.RootsPosted != 0 || later.CrossPRSettled != 1 {
		t.Fatalf("later PR = %+v", later)
	}
}

func TestRun_CrossPRSettledOffPostsTheRootAndCountsIt(t *testing.T) {
	policy := publisher.DefaultPolicy()
	policy.CrossPRSettled = false
	res := crossPRRun(t, policy)
	m := res.Metrics
	if m.SettledPointsSeeded != 1 || m.CrossPRSettled != 0 || m.CrossPRResettled != 1 || m.RootsPosted != 2 {
		t.Fatalf("metrics = seeded %d settled %d resettled %d roots %d", m.SettledPointsSeeded, m.CrossPRSettled, m.CrossPRResettled, m.RootsPosted)
	}
	if len(res.CrossPRPairs) != 2 || res.CrossPRPairs[0].Outcome != "posted" {
		t.Fatalf("pairs = %+v", res.CrossPRPairs)
	}
}
