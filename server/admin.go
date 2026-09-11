package server

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"pr-review-server/db"
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

func (s *Server) addAdminSettings(response map[string]interface{}) {
	row, _ := s.db.GetSetting(settingAdminLogins)
	response[settingAdminLogins] = strings.Join(splitLogins(row), ",")
	response["admin_logins_fixed"] = append([]string{}, s.cfg.AdminLogins...)
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
