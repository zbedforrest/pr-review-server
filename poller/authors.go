package poller

import (
	"context"

	"pr-review-server/github"
)

// authorAllowed is the one matcher for the two author allowlists
// (publish_enabled_authors, auto_review_lite_authors): a plain login entry
// or "*" matches as before, and a team: entry matches a member of that team
// through the resolver's cache. Without a resolver (dev mode with no org, or
// tests) team entries match nobody.
func (p *Poller) authorAllowed(list, author string) bool {
	if publishEnabledFor(author, list) {
		return true
	}
	if p.teams == nil {
		return false
	}
	slugs := github.TeamSlugs(list, p.teams.Org())
	if len(slugs) == 0 {
		return false
	}
	return p.teams.AnyMember(context.Background(), slugs, author)
}

// authorMatcher binds a list read once to the matcher, for callers that
// hand a predicate to another component.
func (p *Poller) authorMatcher(list string) func(login string) bool {
	return func(login string) bool { return p.authorAllowed(list, login) }
}

// TeamResolver exposes the poller's membership cache so the settings API
// validates and reports teams against the same state the gates use.
func (p *Poller) TeamResolver() *github.TeamResolver {
	return p.teams
}
