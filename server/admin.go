package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/github"
)

const settingAdminLogins = "admin_logins"

const maxLoginLen = 39

var validLogin = regexp.MustCompile(`^[a-z0-9](?:-?[a-z0-9]){0,38}$`)

// Bootstrap admins are checked before the settings row so they never depend
// on the database. "*" widens the publish allowlist but never the admin list.
func (s *Server) isAdmin(u *db.User) bool {
	if u == nil {
		return false
	}
	if s.cfg.IsDevMode() || containsLogin(s.cfg.AdminLogins, u.GitHubUsername) {
		return true
	}
	row, err := s.db.GetSetting(settingAdminLogins)
	if err != nil {
		log.Printf("[SETTINGS] %s unreadable, denying admin to %s: %v", settingAdminLogins, u.GitHubUsername, err)
		return false
	}
	return containsLogin(splitLogins(row), u.GitHubUsername)
}

func containsLogin(logins []string, login string) bool {
	for _, l := range logins {
		if l != "*" && strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

func splitLogins(csv string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		login := strings.ToLower(strings.TrimSpace(part))
		if login == "" || seen[login] {
			continue
		}
		seen[login] = true
		out = append(out, login)
	}
	return out
}

// normalizeLoginCSV validates a login list. A PR author list also accepts
// "*" and GitHub App authors such as "dependabot[bot]"; admins are people.
func normalizeLoginCSV(raw string, authors bool) (string, error) {
	logins := splitLogins(raw)
	for _, login := range logins {
		if authors && login == "*" {
			continue
		}
		name := strings.TrimSuffix(login, "[bot]")
		bot := name != login
		if (bot && !authors) || len(name) > maxLoginLen || !validLogin.MatchString(name) {
			return "", fmt.Errorf("%q is not a valid login", login)
		}
	}
	return strings.Join(logins, ","), nil
}

// normalizeAuthorCSV validates one of the two author allowlists
// (publish_enabled_authors, auto_review_lite_authors): logins as
// normalizeLoginCSV accepts them, plus team entries, which come back in
// canonical "team:<slug>" form. The slugs are returned so the caller can
// check that each team exists before the value is stored.
func normalizeAuthorCSV(raw, org string) (csv string, teams []string, err error) {
	var entries []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.ToLower(strings.TrimSpace(part))
		if entry == "" {
			continue
		}
		slug, isTeam, parseErr := github.ParseTeamEntry(entry, org)
		if parseErr != nil {
			return "", nil, parseErr
		}
		if isTeam {
			entry = github.TeamEntryPrefix + slug
		} else if _, loginErr := normalizeLoginCSV(entry, true); loginErr != nil {
			return "", nil, loginErr
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
		if isTeam {
			teams = append(teams, slug)
		}
	}
	return strings.Join(entries, ","), teams, nil
}

// verifyAuthorTeams rejects a save that names a team the installation
// cannot resolve, with the HTTP status the caller should answer with: 400
// for a team that does not exist or is not readable, 502 when GitHub could
// not be asked.
func (s *Server) verifyAuthorTeams(ctx context.Context, field string, slugs []string) (int, error) {
	if len(slugs) == 0 {
		return 0, nil
	}
	if s.teams == nil {
		return http.StatusBadRequest, fmt.Errorf("%s: team entries need GITHUB_ORG_NAME and a GitHub client", field)
	}
	for _, slug := range slugs {
		_, err := s.teams.Members(ctx, slug)
		switch {
		case err == nil:
		case errors.Is(err, github.ErrTeamNotFound):
			return http.StatusBadRequest, fmt.Errorf("%s: team %q does not exist in %s", field, slug, s.teams.Org())
		case errors.Is(err, github.ErrTeamForbidden):
			return http.StatusBadRequest, fmt.Errorf("%s: team %q: %v", field, slug, err)
		default:
			return http.StatusBadGateway, fmt.Errorf("%s: could not look up team %q: %v", field, slug, err)
		}
	}
	return 0, nil
}

// addAuthorListTeams reports every team named by either author list with
// its resolved members, so the settings page can show counts and the
// dashboard can apply the publish gate to team members client-side.
func (s *Server) addAuthorListTeams(ctx context.Context, response map[string]interface{}, lists ...string) {
	teams := map[string]interface{}{}
	org := ""
	if s.teams != nil {
		org = s.teams.Org()
	}
	for _, list := range lists {
		for _, slug := range github.TeamSlugs(list, org) {
			if _, done := teams[slug]; done {
				continue
			}
			teams[slug] = s.authorListTeam(ctx, slug)
		}
	}
	response["author_list_teams"] = teams
}

func (s *Server) authorListTeam(ctx context.Context, slug string) map[string]interface{} {
	if s.teams == nil {
		return map[string]interface{}{"members": []string{}, "resolved_at": "", "error": "team lookup unavailable"}
	}
	membership, err := s.teams.Members(ctx, slug)
	out := map[string]interface{}{"members": append([]string{}, membership.Logins...), "resolved_at": ""}
	if !membership.ResolvedAt.IsZero() {
		out["resolved_at"] = membership.ResolvedAt.UTC().Format(time.RFC3339)
	}
	switch {
	case err == nil:
	case errors.Is(err, github.ErrTeamNotFound), errors.Is(err, github.ErrTeamForbidden):
		out["error"] = err.Error()
	default:
		// The wrapped client error names the request; the chip only needs
		// to know the lookup has not succeeded yet.
		out["error"] = "team lookup failed; retrying"
	}
	return out
}

func (s *Server) addAdminSettings(response map[string]interface{}) {
	row, _ := s.db.GetSetting(settingAdminLogins)
	response[settingAdminLogins] = strings.Join(splitLogins(row), ",")
	response["admin_logins_fixed"] = append([]string{}, s.cfg.AdminLogins...)
	excluded, _ := s.db.GetSetting(db.SettingCIStatusExcludeAuthors)
	response[db.SettingCIStatusExcludeAuthors] = strings.Join(splitLogins(excluded), ",")
}

func (s *Server) writeSetting(actor, key, value string) error {
	old, err := s.db.GetSetting(key)
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	if err := s.db.SetSetting(key, value); err != nil {
		return err
	}
	log.Printf("[SETTINGS] actor=%s key=%s old=%q new=%q", actor, key, old, value)
	return nil
}
