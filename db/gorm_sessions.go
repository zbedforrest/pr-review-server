package db

import (
	"time"

	"gorm.io/gorm"
)

// sessionModelToSession converts a SessionModel to a Session interface type
func sessionModelToSession(m *SessionModel) *Session {
	if m == nil {
		return nil
	}

	return &Session{
		ID:                    m.ID,
		UserID:                int(m.UserID),
		ExpiresAt:             m.ExpiresAt,
		CreatedAt:             m.CreatedAt,
		GitHubTokenEnc:        m.GitHubTokenEnc,
		GitHubRefreshTokenEnc: m.GitHubRefreshTokenEnc,
		GitHubTokenExpiresAt:  m.GitHubTokenExpiresAt,
	}
}

// sessionToSessionModel converts a Session interface type to a SessionModel
func sessionToSessionModel(s *Session) *SessionModel {
	if s == nil {
		return nil
	}

	return &SessionModel{
		ID:                    s.ID,
		UserID:                uint(s.UserID),
		ExpiresAt:             s.ExpiresAt,
		CreatedAt:             s.CreatedAt,
		GitHubTokenEnc:        s.GitHubTokenEnc,
		GitHubRefreshTokenEnc: s.GitHubRefreshTokenEnc,
		GitHubTokenExpiresAt:  s.GitHubTokenExpiresAt,
	}
}

// CreateSession creates a new session
func (g *GormDB) CreateSession(session *Session) error {
	model := sessionToSessionModel(session)
	if err := g.db.Create(model).Error; err != nil {
		return err
	}
	// Update the session struct with the generated CreatedAt
	session.CreatedAt = model.CreatedAt
	return nil
}

// GetSession retrieves a session by ID
func (g *GormDB) GetSession(id string) (*Session, error) {
	var model SessionModel
	result := g.db.Where("id = ?", id).First(&model)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return sessionModelToSession(&model), nil
}

// DeleteSession removes a session by ID
func (g *GormDB) DeleteSession(id string) error {
	return g.db.Where("id = ?", id).Delete(&SessionModel{}).Error
}

// UpdateSessionGitHubToken replaces the sealed GitHub tokens on a session,
// but only while the row still holds expectedEnc: two instances refreshing
// the same rotating token must not overwrite each other's result. Empty
// values clear the tokens. Returns whether the row was written.
func (g *GormDB) UpdateSessionGitHubToken(id, expectedEnc, enc, refreshEnc string, expiresAt *time.Time) (bool, error) {
	res := g.db.Model(&SessionModel{}).
		Where("id = ? AND github_token_enc = ?", id, expectedEnc).
		Updates(map[string]interface{}{
			"github_token_enc":         enc,
			"github_refresh_token_enc": refreshEnc,
			"github_token_expires_at":  expiresAt,
		})
	return res.RowsAffected > 0, res.Error
}
