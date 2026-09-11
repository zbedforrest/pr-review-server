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
}

// handleGetUser returns information about the currently logged-in user
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	response := UserResponse{
		ID:              user.ID,
		GitHubUsername:  user.GitHubUsername,
		GitHubAvatarURL: user.GitHubAvatarURL,
		IsAdmin:         s.isAdmin(user),
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
