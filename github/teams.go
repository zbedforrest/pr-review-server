package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gogithub "github.com/google/go-github/v57/github"
)

// Author allowlists accept "team:<slug>" next to plain logins and "*"; the
// slug names a team of the installation's organization and membership is
// resolved through the installation token, so a list follows the team.
const TeamEntryPrefix = "team:"

var (
	ErrTeamNotFound = errors.New("team not found")
	// ErrTeamForbidden means the installation token cannot list team
	// members: the App lacks the Organization members: read permission, or
	// the installation has not accepted it yet.
	ErrTeamForbidden = errors.New("team members not readable: the GitHub App needs Organization members (read)")

	validTeamSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$`)
)

// ParseTeamEntry recognises the team forms of an allowlist entry:
// "team:<slug>" and "@<org>/<slug>". Both normalise to a lowercase slug; the
// "@" form is only accepted for the installation's own organization. Plain
// logins and "*" return isTeam=false with no error.
func ParseTeamEntry(entry, org string) (slug string, isTeam bool, err error) {
	entry = strings.TrimSpace(entry)
	lower := strings.ToLower(entry)
	switch {
	case strings.HasPrefix(lower, TeamEntryPrefix):
		slug = strings.TrimPrefix(lower, TeamEntryPrefix)
	case strings.HasPrefix(lower, "@"):
		entryOrg, rest, ok := strings.Cut(strings.TrimPrefix(lower, "@"), "/")
		if !ok {
			return "", true, fmt.Errorf("%q must be @org/team-slug", entry)
		}
		if org == "" {
			return "", true, fmt.Errorf("%q cannot be checked without a configured organization; use team:<slug>", entry)
		}
		if !strings.EqualFold(entryOrg, org) {
			return "", true, fmt.Errorf("%q names another organization; teams must belong to %q", entry, org)
		}
		slug = rest
	default:
		return "", false, nil
	}
	if !validTeamSlug.MatchString(slug) {
		return "", true, fmt.Errorf("%q is not a valid team slug", entry)
	}
	return slug, true, nil
}

// TeamSlugs returns the distinct team slugs named in a comma-separated
// allowlist, in first-seen order. Malformed team entries are skipped: they
// cannot match anyone, and validation happens on save, not on match.
func TeamSlugs(csv, org string) []string {
	var slugs []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(csv, ",") {
		slug, isTeam, err := ParseTeamEntry(entry, org)
		if !isTeam || err != nil || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	return slugs
}

// TeamMemberLister is the one GitHub call the resolver needs.
type TeamMemberLister interface {
	GetOrgTeamMembers(ctx context.Context, orgName, teamSlug string) ([]string, error)
}

// TeamMembership is a team's resolved member list as the cache holds it.
type TeamMembership struct {
	Slug       string
	Logins     []string
	ResolvedAt time.Time
}

type teamEntry struct {
	membership TeamMembership
	members    map[string]bool
	resolved   bool // membership holds a successful fetch
	stale      bool // the last refresh of a resolved list failed
	err        error
	checkedAt  time.Time
}

// Defaults for the resolver's cache. A failed lookup with nothing cached is
// retried sooner than a good one expires so an outage does not pin a team
// closed for a full TTL, while a match loop still cannot hammer GitHub.
const (
	teamCacheTTL   = 10 * time.Minute
	teamRetryAfter = time.Minute
	teamFetchWait  = 20 * time.Second
)

// TeamResolver answers "is this login on that team" from a per-slug cache in
// front of the members endpoint. Lookups are lazy (nothing is fetched until
// an allowlist with a team entry is matched), a stale entry is served while a
// refresh fails, and a team that does not exist, cannot be read, or has never
// been resolved matches nobody.
type TeamResolver struct {
	org    string
	lister TeamMemberLister
	ttl    time.Duration
	retry  time.Duration
	now    func() time.Time

	mu       sync.Mutex
	teams    map[string]*teamEntry
	inflight map[string]bool
}

func NewTeamResolver(org string, lister TeamMemberLister) *TeamResolver {
	return &TeamResolver{
		org:      org,
		lister:   lister,
		ttl:      teamCacheTTL,
		retry:    teamRetryAfter,
		now:      time.Now,
		teams:    map[string]*teamEntry{},
		inflight: map[string]bool{},
	}
}

func (r *TeamResolver) Org() string { return r.org }

// Members returns the team's member logins, refreshing the cache when it is
// older than the TTL. A refresh that fails transiently returns the previous
// list; ErrTeamNotFound and ErrTeamForbidden are returned as such.
func (r *TeamResolver) Members(ctx context.Context, slug string) (TeamMembership, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	entry := r.entry(ctx, slug)
	if entry.resolved && entry.err == nil {
		return entry.membership, nil
	}
	if entry.err == nil {
		entry.err = errors.New("team not resolved")
	}
	return entry.membership, entry.err
}

// IsMember reports whether login belongs to the team; false on any failure.
func (r *TeamResolver) IsMember(ctx context.Context, slug, login string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return false
	}
	entry := r.entry(ctx, strings.ToLower(strings.TrimSpace(slug)))
	return entry.resolved && entry.members[login]
}

// AnyMember reports whether login belongs to at least one of the teams.
func (r *TeamResolver) AnyMember(ctx context.Context, slugs []string, login string) bool {
	for _, slug := range slugs {
		if r.IsMember(ctx, slug, login) {
			return true
		}
	}
	return false
}

// entry returns a copy of the cached state for slug, fetching first when
// nothing usable is cached or the cache is due. The fetch runs outside the
// lock, and one fetch per slug at a time: callers arriving during it answer
// from the cached state (a stale list, or nothing) instead of piling on.
func (r *TeamResolver) entry(ctx context.Context, slug string) teamEntry {
	now := r.now()
	r.mu.Lock()
	cached, ok := r.teams[slug]
	if ok && !r.due(cached, now) {
		r.mu.Unlock()
		return *cached
	}
	if r.inflight[slug] {
		r.mu.Unlock()
		if ok {
			return *cached
		}
		return teamEntry{err: errors.New("team not resolved"), checkedAt: now}
	}
	r.inflight[slug] = true
	r.mu.Unlock()

	// The cache is shared with the poller's gates, so a fetch must outlive
	// the request that happened to trigger it; only the timeout applies.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teamFetchWait)
	logins, err := r.lister.GetOrgTeamMembers(fetchCtx, r.org, slug)
	cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, slug)
	next := &teamEntry{checkedAt: now}
	if prev, ok := r.teams[slug]; ok {
		*next = *prev
		next.checkedAt = now
	}
	switch classified := classifyTeamError(err); {
	case classified == nil:
		sort.Strings(logins)
		members := make(map[string]bool, len(logins))
		for _, l := range logins {
			members[strings.ToLower(l)] = true
		}
		next.membership = TeamMembership{Slug: slug, Logins: logins, ResolvedAt: now}
		next.members = members
		next.resolved = true
		next.stale = false
		next.err = nil
	case errors.Is(classified, ErrTeamNotFound), errors.Is(classified, ErrTeamForbidden):
		// A definite answer from GitHub replaces whatever was cached: a
		// deleted team or a revoked permission must stop matching now.
		log.Printf("[TEAMS] %s/%s: %v; team entry matches nobody until it resolves", r.org, slug, classified)
		next = &teamEntry{err: classified, checkedAt: now}
	case next.resolved:
		log.Printf("[TEAMS] %s/%s: refresh failed (%v); serving %d members resolved at %s", r.org, slug, classified, len(next.membership.Logins), next.membership.ResolvedAt.Format(time.RFC3339))
		next.stale = true
		next.err = nil
	default:
		log.Printf("[TEAMS] %s/%s: lookup failed (%v); team entry matches nobody until it resolves", r.org, slug, classified)
		next.err = classified
	}
	r.teams[slug] = next
	return *next
}

// due says whether a cached entry should be refreshed: fresh lists and
// definite misses (404, 403) after the TTL, transient failures (with a stale
// list or none) after the shorter retry interval.
func (r *TeamResolver) due(e *teamEntry, now time.Time) bool {
	switch {
	case e.resolved && !e.stale, errors.Is(e.err, ErrTeamNotFound), errors.Is(e.err, ErrTeamForbidden):
		return now.Sub(e.checkedAt) >= r.ttl
	default:
		return now.Sub(e.checkedAt) >= r.retry
	}
}

func classifyTeamError(err error) error {
	if err == nil {
		return nil
	}
	var ghErr *gogithub.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusNotFound:
			return ErrTeamNotFound
		case http.StatusForbidden:
			return ErrTeamForbidden
		}
	}
	return err
}
