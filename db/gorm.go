package db

import (
	"fmt"
	"os"
	"time"

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
		// Translation is process-wide and lets CreateReviewRun expose one
		// dialect-independent idempotency conflict contract on SQLite/Postgres.
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	gormDB := &GormDB{db: db}

	// Configure connection pool to stay within Cloud SQL limits (db-f1-micro = 25 max)
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(10)                  // Leave headroom for Cloud SQL overhead + local dev
		sqlDB.SetMaxIdleConns(5)                   // Keep a few warm connections
		sqlDB.SetConnMaxLifetime(30 * time.Minute) // Recycle connections to avoid stale sockets
	}

	// Run auto-migrations
	if os.Getenv("SKIP_DB_MIGRATIONS") != "true" {
		if err := gormDB.AutoMigrate(); err != nil {
			return nil, fmt.Errorf("failed to run migrations: %w", err)
		}
	} else {
		// Even when full migrations are disabled, apply trivial idempotent
		// column additions. These are safe to re-run and keep the schema
		// compatible with newly-added model fields.
		if err := gormDB.ensureIdempotentColumns(); err != nil {
			return nil, fmt.Errorf("failed to apply idempotent column adds: %w", err)
		}
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
	if err := g.db.AutoMigrate(
		&UserModel{},
		&SessionModel{},
		&PRModel{},
		&UserPRViewModel{},
		&SettingModel{},
		&TelemetryEventModel{},
		&PollerLeaseModel{},
		&FindingOutcomeModel{},
		&ReviewRunModel{},
		&ReviewStageAttemptModel{},
	); err != nil {
		return err
	}

	// Clean up wrongly-named column from GORM default naming (git_hub_updated_at vs github_updated_at)
	g.db.Exec("ALTER TABLE prs DROP COLUMN IF EXISTS git_hub_updated_at")

	// Explicit column additions (AutoMigrate can miss new columns).
	return g.ensureIdempotentColumns()
}

// ensureIdempotentColumns applies `ADD COLUMN IF NOT EXISTS` for columns that
// were added after the initial schema. Runs even when SKIP_DB_MIGRATIONS=true
// because these are trivial and safe to re-execute on every boot.
//
// Postgres-only: SQLite (used by the dev/test path) doesn't support
// `ADD COLUMN IF NOT EXISTS` and doesn't have the `timestamptz` type. SQLite
// goes through full AutoMigrate when migrations are enabled, so this no-op
// is the correct behavior.
func (g *GormDB) ensureIdempotentColumns() error {
	// Whole-table creation for tables added after the initial schema. Unlike
	// the column adds below this is dialect-agnostic: HasTable+CreateTable is
	// idempotent and safe on every boot, and it keeps SKIP_DB_MIGRATIONS
	// deployments working when a release introduces a brand-new table.
	if !g.db.Migrator().HasTable(&FindingOutcomeModel{}) {
		if err := g.db.Migrator().CreateTable(&FindingOutcomeModel{}); err != nil {
			return fmt.Errorf("create finding_outcomes: %w", err)
		}
	}
	if !g.db.Migrator().HasTable(&ReviewRunModel{}) {
		if err := g.db.Migrator().CreateTable(&ReviewRunModel{}); err != nil {
			return fmt.Errorf("create review_runs: %w", err)
		}
	}
	if !g.db.Migrator().HasTable(&ReviewStageAttemptModel{}) {
		if err := g.db.Migrator().CreateTable(&ReviewStageAttemptModel{}); err != nil {
			return fmt.Errorf("create review_stage_attempts: %w", err)
		}
	}
	// Older revisions did not enforce one live run per target. Keep the most
	// recently accepted row so adding the invariant cannot boot-loop on an
	// unexpectedly dirty database.
	if err := g.db.Exec(`UPDATE review_runs
		SET status = 'timed_out', terminal_code = 'migration_deduped', failure_stage = 'migration',
			error_summary = 'terminalized by one-live-run-per-PR migration', completed_at = CURRENT_TIMESTAMP,
			lease_holder = '', lease_expires_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status IN ('queued', 'running')
		  AND EXISTS (
			SELECT 1 FROM review_runs newer
			WHERE newer.repo_owner = review_runs.repo_owner
			  AND newer.repo_name = review_runs.repo_name
			  AND newer.pr_number = review_runs.pr_number
			  AND newer.status IN ('queued', 'running')
			  AND (newer.accepted_at > review_runs.accepted_at
			       OR (newer.accepted_at = review_runs.accepted_at AND newer.run_id > review_runs.run_id))
		  )`).Error; err != nil {
		return fmt.Errorf("dedupe live review runs: %w", err)
	}
	if err := g.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_runs_one_live_per_pr
		ON review_runs(repo_owner, repo_name, pr_number)
		WHERE status IN ('queued', 'running')`).Error; err != nil {
		return fmt.Errorf("index one live review run per PR: %w", err)
	}
	if g.db.Dialector.Name() != "postgres" {
		if !g.db.Migrator().HasTable(&PRModel{}) {
			return nil
		}
		if !g.db.Migrator().HasColumn(&PRModel{}, "projection_run_id") {
			if err := g.db.Migrator().AddColumn(&PRModel{}, "ProjectionRunID"); err != nil {
				return fmt.Errorf("add projection_run_id: %w", err)
			}
		}
		if err := g.db.Exec("UPDATE prs SET projection_run_id = '' WHERE projection_run_id IS NULL").Error; err != nil {
			return fmt.Errorf("backfill projection_run_id: %w", err)
		}
		return nil
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS github_updated_at timestamptz").Error; err != nil {
		return fmt.Errorf("add github_updated_at: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS error_message text").Error; err != nil {
		return fmt.Errorf("add error_message: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS error_retry_count integer NOT NULL DEFAULT 0").Error; err != nil {
		return fmt.Errorf("add error_retry_count: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS review_verdict varchar(32)").Error; err != nil {
		return fmt.Errorf("add review_verdict: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE user_pr_views ADD COLUMN IF NOT EXISTS user_hidden boolean NOT NULL DEFAULT false").Error; err != nil {
		return fmt.Errorf("add user_hidden: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE user_pr_views ADD COLUMN IF NOT EXISTS via_manual boolean NOT NULL DEFAULT false").Error; err != nil {
		return fmt.Errorf("add via_manual: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS pr_state varchar(16) NOT NULL DEFAULT 'open'").Error; err != nil {
		return fmt.Errorf("add pr_state: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS model_fallback boolean NOT NULL DEFAULT false").Error; err != nil {
		return fmt.Errorf("add model_fallback: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS review_run_id varchar(36)").Error; err != nil {
		return fmt.Errorf("add review_run_id: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS review_run_json text").Error; err != nil {
		return fmt.Errorf("add review_run_json: %w", err)
	}
	if err := g.db.Exec("ALTER TABLE prs ADD COLUMN IF NOT EXISTS projection_run_id varchar(36)").Error; err != nil {
		return fmt.Errorf("add projection_run_id: %w", err)
	}
	if err := g.db.Exec("UPDATE prs SET projection_run_id = '' WHERE projection_run_id IS NULL").Error; err != nil {
		return fmt.Errorf("backfill projection_run_id: %w", err)
	}
	if err := g.db.Exec("CREATE INDEX IF NOT EXISTS idx_prs_review_run_id ON prs(review_run_id)").Error; err != nil {
		return fmt.Errorf("index review_run_id: %w", err)
	}
	if err := g.db.Exec("CREATE INDEX IF NOT EXISTS idx_review_runs_status_lease ON review_runs(status, lease_expires_at)").Error; err != nil {
		return fmt.Errorf("index review_runs status/lease: %w", err)
	}
	if err := g.db.Exec("CREATE INDEX IF NOT EXISTS idx_review_runs_status_queue ON review_runs(status, queued_at)").Error; err != nil {
		return fmt.Errorf("index review_runs status/queue: %w", err)
	}
	return nil
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
		"auto_review_requested_prs": "false",
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
