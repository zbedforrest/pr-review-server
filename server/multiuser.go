package server

import (
	"encoding/json"
	"net/http"

	"pr-review-server/auth"
)

// UserResponse represents the current user for API responses
type UserResponse struct {
	ID              int    `json:"id"`
	GitHubUsername  string `json:"github_username"`
	GitHubAvatarURL string `json:"github_avatar_url"`
	IsAdmin         bool   `json:"is_admin"`
	// QuickActionsEnabled mirrors QUICK_ACTIONS_ENABLED (and the admin-only
	// stage); GitHubActionsAvailable is whether this request carries a usable
	// GitHub token to act as the human.
	QuickActionsEnabled    bool `json:"quick_actions_enabled"`
	GitHubActionsAvailable bool `json:"github_actions_available"`
}

// handleGetUser returns information about the currently logged-in user
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	_, _, tokenOK := auth.GitHubUserToken(r)
	response := UserResponse{
		ID:                     user.ID,
		GitHubUsername:         user.GitHubUsername,
		GitHubAvatarURL:        user.GitHubAvatarURL,
		IsAdmin:                s.isAdmin(user),
		QuickActionsEnabled:    s.quickActionsAvailableTo(user),
		GitHubActionsAvailable: tokenOK,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response) // nolint:errcheck
}

// GetUserHandler returns a handler function for /api/user
func (s *Server) GetUserHandler() http.HandlerFunc {
	return s.handleGetUser
}

// GetPRsHandler returns a handler function for /api/prs
func (s *Server) GetPRsHandler() http.HandlerFunc {
	return s.handleGetPRs
}
