package poller

import (
	"context"
	"strings"

	"pr-review-server/db"
	"pr-review-server/github"
)

// SettingPublishOptOutAuthors lists the logins who asked PRism to stop
// posting on their PRs. Deny wins over every entry of publish_enabled_authors,
// so comments, replies, reactions and mention responses all stop at once;
// dashboard reviews are unaffected.
const SettingPublishOptOutAuthors = "publish_opt_out_authors"

// authorAllowed is the one matcher for the author allowlists
// (publish_enabled_authors, auto_review_authors, auto_review_lite_authors): a
// plain login entry or "*" matches as before, and a team: entry matches a
// member of that team through the resolver's cache. Without a resolver (dev
// mode with no org, or tests) team entries match nobody.
func (p *Poller) authorAllowed(list, author string) bool {
	return AdmittingEntry(context.Background(), p.teams, list, author) != ""
}

// AdmittingEntry names the most specific entry of an author allowlist that
// admits author: "login" for the login itself, then the slug of a team the
// author belongs to, then "*"; empty when nothing admits them. The gates and
// the settings API share it so they can never disagree.
func AdmittingEntry(ctx context.Context, teams *github.TeamResolver, list, author string) string {
	member := func(slug string) bool { return teams.IsMember(ctx, slug, author) }
	return admittingEntry(teams, list, author, member)
}

// CachedAdmittingEntry is AdmittingEntry answered from the membership cache
// alone, for request paths that must not wait on a team fetch; a team the
// poller has not resolved yet admits nobody.
func CachedAdmittingEntry(teams *github.TeamResolver, list, author string) string {
	member := func(slug string) bool { return teams.CachedMember(slug, author) }
	return admittingEntry(teams, list, author, member)
}

func admittingEntry(teams *github.TeamResolver, list, author string, member func(slug string) bool) string {
	author = strings.TrimSpace(author)
	if author == "" {
		return ""
	}
	wildcard := false
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "*":
			wildcard = true
		case strings.EqualFold(entry, author):
			return "login"
		}
	}
	if teams != nil {
		for _, slug := range github.TeamSlugs(list, teams.Org()) {
			if member(slug) {
				return slug
			}
		}
	}
	if wildcard {
		return "*"
	}
	return ""
}

// optedOut reports whether author is on the opt-out list (logins only).
func optedOut(author, optOutCSV string) bool {
	author = strings.ToLower(strings.TrimSpace(author))
	return author != "" && db.ParseLoginCSV(optOutCSV)[author]
}

// PublishGate is the one gate on posting anything to a PR, built from both
// publication lists read once. Every path that writes to GitHub as PRism
// (review rounds, inline comments, reply reactions and text, mention
// responses, maintenance tools) asks it; the review gates do not, so an
// opted-out author keeps their dashboard reviews.
type PublishGate struct {
	teams   *github.TeamResolver
	enabled string
	optOut  string
}

// NewPublishGate builds the gate from the two publication lists for a caller
// outside the poller, so a tool can never disagree with the server.
func NewPublishGate(teams *github.TeamResolver, enabled, optOut string) PublishGate {
	return PublishGate{teams: teams, enabled: enabled, optOut: optOut}
}

// publishGate reads the lists. A read error denies, like the gate always
// has: the returned gate still matches nobody.
func (p *Poller) publishGate() (PublishGate, error) {
	g := PublishGate{teams: p.teams}
	enabled, err := p.db.GetSetting(settingPublishEnabledAuthors)
	if err != nil {
		return g, err
	}
	optOut, err := p.db.GetSetting(SettingPublishOptOutAuthors)
	if err != nil {
		return g, err
	}
	g.enabled, g.optOut = enabled, optOut
	return g, nil
}

// OptedOut reports whether the author asked for silence.
func (g PublishGate) OptedOut(author string) bool {
	return optedOut(author, g.optOut)
}

// Allowed is the matcher the posting paths share: an opted-out login is
// denied before any login, team or "*" entry of publish_enabled_authors can
// match it.
func (g PublishGate) Allowed(author string) bool {
	return !g.OptedOut(author) && AdmittingEntry(context.Background(), g.teams, g.enabled, author) != ""
}

// TeamResolver exposes the poller's membership cache so the settings API
// validates and reports teams against the same state the gates use.
func (p *Poller) TeamResolver() *github.TeamResolver {
	return p.teams
}
