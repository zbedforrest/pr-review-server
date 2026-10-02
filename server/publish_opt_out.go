package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"pr-review-server/auth"
	"pr-review-server/db"
	"pr-review-server/poller"
)

// publishOptOutPath is the self-service side of the publish gate: POST adds
// the signed-in login to publish_opt_out_authors, DELETE removes it. Either
// answers with the refreshed /api/user document.
const publishOptOutPath = "/api/me/publish-opt-out"

const settingPublishOptOutAuthors = poller.SettingPublishOptOutAuthors

// publishEnrollment is the signed-in author's standing with the publish gate.
type publishEnrollment struct {
	// Enrolled: publish_enabled_authors admits the login, opt-out aside.
	Enrolled bool `json:"publish_enrolled"`
	// OptedOut: the login is on publish_opt_out_authors, so nothing is posted.
	OptedOut bool `json:"publish_opted_out"`
	// EnrolledVia is the entry that admits the login: "login", the team slug,
	// or "*"; empty when not enrolled.
	EnrolledVia string `json:"enrolled_via"`
}

func (s *Server) publishEnrollmentFor(ctx context.Context, login string) publishEnrollment {
	enabled, _ := s.db.GetSetting(settingPublishEnabledAuthors)
	optOut, _ := s.db.GetSetting(settingPublishOptOutAuthors)
	via := s.enrolledVia(ctx, enabled, login)
	return publishEnrollment{
		Enrolled:    via != "",
		OptedOut:    db.ParseLoginCSV(optOut)[strings.ToLower(strings.TrimSpace(login))],
		EnrolledVia: via,
	}
}

// enrolledVia names the most specific entry of the allowlist that admits the
// login: the login itself, then a team the login belongs to, then "*".
func (s *Server) enrolledVia(ctx context.Context, enabledCSV, login string) string {
	return poller.AdmittingEntry(ctx, s.teams, enabledCSV, login)
}

func (s *Server) handlePublishOptOut(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	var leave bool
	switch r.Method {
	case http.MethodPost:
		leave = true
	case http.MethodDelete:
		leave = false
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.setPublishOptOut(user.GitHubUsername, leave); err != nil {
		http.Error(w, "Failed to update settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.userResponse(r, user)) // nolint:errcheck
}

// setPublishOptOut rewrites the opt-out list with the login added or
// removed, keeping the other entries in their stored order.
func (s *Server) setPublishOptOut(login string, leave bool) error {
	login = strings.ToLower(strings.TrimSpace(login))
	s.publishOptOutMu.Lock()
	defer s.publishOptOutMu.Unlock()
	current, err := s.db.GetSetting(settingPublishOptOutAuthors)
	if err != nil {
		return err
	}
	var next []string
	for _, entry := range splitLogins(current) {
		if entry != login {
			next = append(next, entry)
		}
	}
	if leave {
		next = append(next, login)
	}
	value := strings.Join(next, ",")
	if value == strings.Join(splitLogins(current), ",") {
		return nil
	}
	return s.writeSetting(login, settingPublishOptOutAuthors, value)
}
