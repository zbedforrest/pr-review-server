package server

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"pr-review-server/db"
)

const settingAdminLogins = "admin_logins"

var validLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)

// "*" widens the publish allowlist but never the admin list.
func (s *Server) isAdmin(u *db.User) bool {
	if u == nil {
		return false
	}
	if s.cfg.IsDevMode() {
		return true
	}
	row, err := s.db.GetSetting(settingAdminLogins)
	if err != nil {
		log.Printf("[SETTINGS] %s unreadable, denying admin to %s: %v", settingAdminLogins, u.GitHubUsername, err)
		return false
	}
	for _, login := range append(splitLogins(row), s.cfg.AdminLogins...) {
		if login != "*" && strings.EqualFold(login, u.GitHubUsername) {
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

func normalizeLoginCSV(raw string, allowStar bool) (string, error) {
	logins := splitLogins(raw)
	for _, login := range logins {
		if login == "*" && allowStar {
			continue
		}
		if !validLogin.MatchString(login) {
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
