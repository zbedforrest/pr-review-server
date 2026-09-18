package github

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// partialErrorSummary folds the per-node GraphQL errors of a batched query
// into one log line. A batch of 50 PRs whose check contexts the App cannot
// read otherwise produces hundreds of identical lines every poll cycle.
type partialErrorSummary struct {
	mu     sync.Mutex
	total  int
	prs    map[string]struct{}
	groups map[string]*partialErrorGroup
}

type partialErrorGroup struct {
	typ, path, message, samplePR string
	count                        int
	prs                          map[string]struct{}
}

func newPartialErrorSummary() *partialErrorSummary {
	return &partialErrorSummary{prs: map[string]struct{}{}, groups: map[string]*partialErrorGroup{}}
}

// add records the errors of one batch; batch resolves the prN aliases in
// error paths back to PRs.
func (s *partialErrorSummary) add(errs []GraphQLPartialError, batch []PRInfo) {
	if len(errs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range errs {
		path := normalizeGraphQLPath(e.Path)
		key := e.Type + "@" + path
		g := s.groups[key]
		if g == nil {
			g = &partialErrorGroup{typ: e.Type, path: path, message: e.Message, prs: map[string]struct{}{}}
			s.groups[key] = g
		}
		g.count++
		s.total++
		if pr := aliasedPR(e.Path, batch); pr != "" {
			if g.samplePR == "" {
				g.samplePR = pr
			}
			g.prs[pr] = struct{}{}
			s.prs[pr] = struct{}{}
		}
	}
}

// log emits the single summary line, or nothing when no errors were recorded.
func (s *partialErrorSummary) log(label string, totalPRs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total == 0 {
		return
	}
	groups := make([]*partialErrorGroup, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].count != groups[j].count {
			return groups[i].count > groups[j].count
		}
		return groups[i].path < groups[j].path
	})
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		part := fmt.Sprintf("%s at %s x%d on %d PRs", g.typ, g.path, g.count, len(g.prs))
		if g.samplePR != "" {
			part += " (e.g. " + g.samplePR + ")"
		}
		part += ": " + g.message
		if g.typ == "FORBIDDEN" && strings.Contains(g.path, "statusCheckRollup.contexts") {
			part += " [the App needs Checks: Read for CheckRun and Commit statuses: Read for StatusContext]"
		}
		parts = append(parts, part)
	}
	log.Printf("[GRAPHQL] %s: %d partial errors on %d/%d PRs; %s",
		label, s.total, len(s.prs), totalPRs, strings.Join(parts, "; "))
}

// normalizeGraphQLPath drops the per-PR alias and list indices from an error
// path so identical failures on different PRs and nodes group together:
// [pr48 pullRequest commits nodes 0 commit statusCheckRollup contexts nodes 54]
// becomes pullRequest.commits.nodes.commit.statusCheckRollup.contexts.nodes.
func normalizeGraphQLPath(path []interface{}) string {
	parts := make([]string, 0, len(path))
	for i, elem := range path {
		seg, ok := elem.(string)
		if !ok || (i == 0 && aliasIndex(seg) >= 0) {
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ".")
}

// aliasIndex returns N for a prN batch alias, or -1 for any other segment.
func aliasIndex(seg string) int {
	if !strings.HasPrefix(seg, "pr") {
		return -1
	}
	idx, err := strconv.Atoi(seg[2:])
	if err != nil || idx < 0 {
		return -1
	}
	return idx
}

// aliasedPR resolves the leading prN alias of an error path to owner/repo#number.
func aliasedPR(path []interface{}, batch []PRInfo) string {
	if len(path) == 0 {
		return ""
	}
	alias, ok := path[0].(string)
	if !ok {
		return ""
	}
	idx := aliasIndex(alias)
	if idx < 0 || idx >= len(batch) {
		return ""
	}
	pr := batch[idx]
	return fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)
}
