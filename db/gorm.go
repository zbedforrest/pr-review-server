package db

import (
	"fmt"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GormDB implements the Database interface using GORM
type GormDB struct {
	db *gorm.DB
}

// Ensure GormDB implements Database interface
var _ Database = (*GormDB)(nil)

// NewGormDB creates a new GormDB with the given GORM dialector
func NewGormDB(dialector gorm.Dialector) (*GormDB, error) {
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	gormDB := &GormDB{db: db}

	// Run auto-migrations
	if err := gormDB.AutoMigrate(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Initialize default settings
	if err := gormDB.initDefaultSettings(); err != nil {
		return nil, fmt.Errorf("failed to initialize settings: %w", err)
	}

	return gormDB, nil
}

// NewGormSQLite creates a new GormDB with SQLite
func NewGormSQLite(dbPath string) (*GormDB, error) {
	dialector := sqlite.Open(dbPath)
	return NewGormDB(dialector)
}

// NewGormPostgres creates a new GormDB with PostgreSQL
func NewGormPostgres(databaseURL string) (*GormDB, error) {
	dialector := postgres.Open(databaseURL)
	return NewGormDB(dialector)
}

// AutoMigrate creates or updates the database schema
func (g *GormDB) AutoMigrate() error {
	return g.db.AutoMigrate(
		&UserModel{},
		&SessionModel{},
		&PRModel{},
		&UserPRViewModel{},
		&SettingModel{},
		&TelemetryEventModel{},
	)
}

// Close closes the database connection
func (g *GormDB) Close() error {
	sqlDB, err := g.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// initDefaultSettings initializes default settings if they don't exist
func (g *GormDB) initDefaultSettings() error {
	defaults := map[string]string{
		"auto_review_requested_prs": "true",
		"review_n_requests":         "3",
		"generate_html":             "true",
	}

	for key, value := range defaults {
		var setting SettingModel
		result := g.db.Where("key = ?", key).First(&setting)
		if result.Error == gorm.ErrRecordNotFound {
			setting = SettingModel{Key: key, Value: value}
			if err := g.db.Create(&setting).Error; err != nil {
				return err
			}
		} else if result.Error != nil {
			return result.Error
		}
	}

	return nil
}

// DB returns the underlying GORM database connection (for testing)
func (g *GormDB) DB() *gorm.DB {
	return g.db
}
