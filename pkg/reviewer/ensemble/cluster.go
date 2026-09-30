// Package ensemble merges the findings of several independent agent reviews
// of one pull request into a single review.
//
// The merge never trusts a single step. Clusters are only hints for the
// merge author; the guard then checks, finding by finding, that every input
// finding is accounted for, and reinserts any that are not. Deterministic
// merges one finding per cluster when no author is available.
package ensemble

import (
	"fmt"
	"sort"
	"strings"

	"pr-review-server/pkg/reviewer/types"
)

// DefaultLineWindow matches the single-review merge: two findings on the same
// file within this many lines are treated as candidates for the same issue.
const DefaultLineWindow = 10

// Member is one finding from one run, with an id unique across the ensemble.
type Member struct {
	ID      string
	Run     int
	Finding types.LineComment
}

// Cluster groups members that probably describe the same issue.
type Cluster struct {
	ID      string
	Members []Member
}

// Support is the number of distinct runs that raised a finding in the cluster.
func (c Cluster) Support() int {
	runs := map[int]bool{}
	for _, m := range c.Members {
		runs[m.Run] = true
	}
	return len(runs)
}

// Members flattens the runs' active findings and namespaces their ids by run
// ("r2:A-1"), so ids that every run reuses cannot collide. SUMMARY entries and
// inactive records are not claims and are left out. Runs are numbered from 1.
func Members(runs [][]types.LineComment) []Member {
	var out []Member
	for r, findings := range runs {
		run := r + 1
		n := 0
		for _, f := range findings {
			if isSummary(f) || f.Inactive {
				continue
			}
			n++
			label := f.ID
			if label == "" {
				label = fmt.Sprintf("F-%d", n)
			}
			out = append(out, Member{ID: fmt.Sprintf("r%d:%s", run, label), Run: run, Finding: f})
		}
	}
	return dedupeIDs(out)
}

// dedupeIDs suffixes repeated ids within a run; an agent can reuse a label.
func dedupeIDs(ms []Member) []Member {
	seen := map[string]int{}
	for i := range ms {
		seen[ms[i].ID]++
		if n := seen[ms[i].ID]; n > 1 {
			ms[i].ID = fmt.Sprintf("%s#%d", ms[i].ID, n)
		}
	}
	return ms
}

// ClusterMembers groups members by single linkage: two members join when they
// name the same file and their lines are within lineWindow of each other.
// Whole-file findings (line 0) join only other whole-file findings on the
// same file. Mechanical findings never join anything. Clusters are ordered by
// file, then first line, so ids are stable for a given input.
func ClusterMembers(ms []Member, lineWindow int) []Cluster {
	parent := make([]int, len(ms))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for i := range ms {
		for j := i + 1; j < len(ms); j++ {
			if related(ms[i].Finding, ms[j].Finding, lineWindow) {
				parent[find(i)] = find(j)
			}
		}
	}
	groups := map[int][]Member{}
	var roots []int
	for i, m := range ms {
		r := find(i)
		if _, ok := groups[r]; !ok {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], m)
	}
	clusters := make([]Cluster, 0, len(roots))
	for _, r := range roots {
		members := groups[r]
		sort.SliceStable(members, func(a, b int) bool {
			return members[a].Finding.LineNumber < members[b].Finding.LineNumber
		})
		clusters = append(clusters, Cluster{Members: members})
	}
	sort.SliceStable(clusters, func(a, b int) bool {
		fa, fb := clusters[a].Members[0].Finding, clusters[b].Members[0].Finding
		if fa.FilePath != fb.FilePath {
			return fa.FilePath < fb.FilePath
		}
		return fa.LineNumber < fb.LineNumber
	})
	for i := range clusters {
		clusters[i].ID = fmt.Sprintf("C-%d", i+1)
	}
	return clusters
}

func related(a, b types.LineComment, lineWindow int) bool {
	if a.Provenance == "mechanical" || b.Provenance == "mechanical" || normPath(a.FilePath) != normPath(b.FilePath) {
		return false
	}
	if a.LineNumber == 0 || b.LineNumber == 0 {
		return a.LineNumber == b.LineNumber
	}
	d := a.LineNumber - b.LineNumber
	if d < 0 {
		d = -d
	}
	return d <= lineWindow
}

// normPath compares paths as the runs emit them: every run of one ensemble
// reviews the same checkout, so only cosmetic prefixes differ.
func normPath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "./")
}

func isSummary(f types.LineComment) bool {
	return f.FilePath == "SUMMARY"
}

// ImportanceRank orders severities; unknown or empty ranks lowest.
func ImportanceRank(imp string) int {
	switch strings.ToUpper(strings.TrimSpace(imp)) {
	case "CRITICAL":
		return 3
	case "MEDIUM":
		return 2
	case "LOW":
		return 1
	default:
		return 0
	}
}
