package db

import (
	"time"

	"gorm.io/gorm"
)

// userModelToUser converts a UserModel to a User interface type
func userModelToUser(m *UserModel) *User {
	if m == nil {
		return nil
	}

	return &User{
		ID:              int(m.ID),
		GitHubID:        m.GitHubID,
		GitHubUsername:  m.GitHubUsername,
		GitHubAvatarURL: m.GitHubAvatar,
		CreatedAt:       m.CreatedAt,
		LastLoginAt:     m.LastLoginAt,
	}
}

// userToUserModel converts a User interface type to a UserModel
func userToUserModel(u *User) *UserModel {
	if u == nil {
		return nil
	}

	return &UserModel{
		ID:             uint(u.ID),
		GitHubID:       u.GitHubID,
		GitHubUsername: u.GitHubUsername,
		GitHubAvatar:   u.GitHubAvatarURL,
		CreatedAt:      u.CreatedAt,
		LastLoginAt:    u.LastLoginAt,
	}
}

// GetUserByGitHubID retrieves a user by their GitHub ID
func (g *GormDB) GetUserByGitHubID(githubID int64) (*User, error) {
	var model UserModel
	result := g.db.Where("github_id = ?", githubID).First(&model)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return userModelToUser(&model), nil
}

// GetUserByID retrieves a user by their internal ID
func (g *GormDB) GetUserByID(id int) (*User, error) {
	var model UserModel
	result := g.db.First(&model, id)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return userModelToUser(&model), nil
}

// CreateUser creates a new user
func (g *GormDB) CreateUser(user *User) error {
	model := userToUserModel(user)
	if err := g.db.Create(model).Error; err != nil {
		return err
	}
	// Update the user struct with the generated ID and CreatedAt
	user.ID = int(model.ID)
	user.CreatedAt = model.CreatedAt
	return nil
}

// UpdateUserLastLogin updates the last login timestamp for a user
func (g *GormDB) UpdateUserLastLogin(userID int) error {
	now := time.Now().UTC()
	return g.db.Model(&UserModel{}).Where("id = ?", userID).Update("last_login_at", now).Error
}

// GetAllUsers returns all users in the database
func (g *GormDB) GetAllUsers() ([]User, error) {
	var models []UserModel
	if err := g.db.Find(&models).Error; err != nil {
		return nil, err
	}
	users := make([]User, len(models))
	for i, m := range models {
		users[i] = *userModelToUser(&m)
	}
	return users, nil
}

// GetUserByUsername retrieves a user by their GitHub username
func (g *GormDB) GetUserByUsername(username string) (*User, error) {
	var model UserModel
	result := g.db.Where("github_username = ?", username).First(&model)

	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}

	return userModelToUser(&model), nil
}
