package main

import (
	"fmt"
	"sort"
	"time"

	"pr-review-server/internal/replaykit"
)

type prState string

const (
	prOpen   prState = "open"
	prMerged prState = "merged"
	prClosed prState = "closed"
)

// thread is one PRism root review thread as a source reads it.
type thread struct {
	RootCommentID int64
	NodeID        string
	FindingID     string
	Path          string
	PostedSHA     string
	PostedAt      time.Time
	Resolved      bool
	// ResolvedKnown is false for a dump whose resolution flags were never
	// filled; such a thread is neither open nor resolved to this tool.
	ResolvedKnown bool
	Outdated      bool
}

type pr struct {
	Owner   string
	Repo    string
	Number  int
	Author  string
	State   prState
	HeadSHA string
	Threads []thread
}

func (p *pr) ref() string { return fmt.Sprintf("%s/%s#%d", p.Owner, p.Repo, p.Number) }

// verdict is what the tool decided about one thread. The stale verdicts
// name why the thread should close; the skip verdicts name why it stays.
type verdict string

const (
	stalePRMerged     verdict = "pr_merged"
	stalePRClosed     verdict = "pr_closed"
	staleFileChanged  verdict = "file_changed"
	skipResolved      verdict = "already_resolved"
	skipUnknownState  verdict = "resolution_unknown"
	skipUnchanged     verdict = "open_unchanged"
	skipChangeUnknown verdict = "change_unknown"
)

func (v verdict) stale() bool {
	return v == stalePRMerged || v == stalePRClosed || v == staleFileChanged
}

type decision struct {
	PR      *pr
	Thread  thread
	Verdict verdict
}

// changedFiles lists the files changed between two commits of a repository;
// known is false when the compare is unavailable or hit the endpoint's cap.
type changedFiles func(owner, repo, base, head string) (files []string, known bool)

// classify decides every thread of a PR. A thread on a merged or closed PR
// is stale as it stands; on an open PR it is stale once its cited file
// changed between the commit it was posted against and the head.
func classify(p *pr, changed changedFiles) []decision {
	out := make([]decision, 0, len(p.Threads))
	for _, t := range p.Threads {
		out = append(out, decision{PR: p, Thread: t, Verdict: classifyThread(p, t, changed)})
	}
	return out
}

func classifyThread(p *pr, t thread, changed changedFiles) verdict {
	switch {
	case !t.ResolvedKnown:
		return skipUnknownState
	case t.Resolved:
		return skipResolved
	case p.State == prMerged:
		return stalePRMerged
	case p.State == prClosed:
		return stalePRClosed
	case t.PostedSHA == "" || p.HeadSHA == "":
		return skipChangeUnknown
	case t.PostedSHA == p.HeadSHA:
		return skipUnchanged
	}
	files, known := changed(p.Owner, p.Repo, t.PostedSHA, p.HeadSHA)
	switch {
	case replaykit.FileChanged(files, t.Path):
		return staleFileChanged
	case !known:
		return skipChangeUnknown
	}
	return skipUnchanged
}

// summary is the count block a run prints; it names no repository.
type summary struct {
	PRs               int `json:"prs"`
	PRsOpen           int `json:"prs_open"`
	PRsMerged         int `json:"prs_merged"`
	PRsClosed         int `json:"prs_closed"`
	PrismRoots        int `json:"prism_roots"`
	AlreadyResolved   int `json:"already_resolved"`
	ResolutionUnknown int `json:"resolution_unknown"`
	Stale             int `json:"stale"`
	StalePRMerged     int `json:"stale_pr_merged"`
	StalePRClosed     int `json:"stale_pr_closed"`
	StaleFileChanged  int `json:"stale_file_changed"`
	// StaleOutdated counts stale threads GitHub also marks outdated, for
	// calibration against the W2-3 corroboration rule.
	StaleOutdated int `json:"stale_outdated"`
	OpenUnchanged int `json:"open_unchanged"`
	ChangeUnknown int `json:"change_unknown"`
	PRsWithStale  int `json:"prs_with_stale"`
	// Applied is present in live mode only.
	Applied *applied `json:"applied,omitempty"`
}

func summarize(prs []*pr, decisions []decision) summary {
	s := summary{PRs: len(prs)}
	for _, p := range prs {
		switch p.State {
		case prMerged:
			s.PRsMerged++
		case prClosed:
			s.PRsClosed++
		default:
			s.PRsOpen++
		}
	}
	withStale := map[*pr]bool{}
	for _, d := range decisions {
		s.PrismRoots++
		switch d.Verdict {
		case skipResolved:
			s.AlreadyResolved++
		case skipUnknownState:
			s.ResolutionUnknown++
		case skipUnchanged:
			s.OpenUnchanged++
		case skipChangeUnknown:
			s.ChangeUnknown++
		case stalePRMerged:
			s.StalePRMerged++
		case stalePRClosed:
			s.StalePRClosed++
		case staleFileChanged:
			s.StaleFileChanged++
		}
		if d.Verdict.stale() {
			s.Stale++
			withStale[d.PR] = true
			if d.Thread.Outdated {
				s.StaleOutdated++
			}
		}
	}
	s.PRsWithStale = len(withStale)
	return s
}

// staleOnly keeps the wanted stale decisions in PR, then posting order.
func staleOnly(decisions []decision, wanted map[verdict]bool) []decision {
	var out []decision
	for _, d := range decisions {
		if wanted[d.Verdict] {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.PR != b.PR {
			return a.PR.ref() < b.PR.ref()
		}
		return a.Thread.PostedAt.Before(b.Thread.PostedAt)
	})
	return out
}
