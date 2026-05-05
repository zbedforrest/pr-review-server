package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Legacy single-user mode (backwards compatible)
	GitHubToken    string
	GitHubUsername string

	// GitHub App configuration (multi-user mode)
	GitHubAppID             string
	GitHubAppPrivateKeyPath string
	GitHubAppClientID       string
	GitHubAppClientSecret   string
	GitHubAppInstallationID string
	GitHubOrgName           string // Organization name for membership verification and PR polling

	// OAuth configuration
	OAuthCallbackURL string
	SessionSecret    string
	BaseURL          string // Base URL for the application (e.g., https://pr-review.example.com)

	// Database configuration
	DatabaseURL string // PostgreSQL connection URL (if set, uses PostgreSQL instead of SQLite)
	DBPath      string // SQLite path for local development

	// General configuration
	PollingInterval time.Duration
	ReviewsDir      string // Deprecated: use GCSBucket instead
	GCSBucket       string
	ServerPort      string
	ReviewerEnabled bool
	GeminiAPIKey    string

	// Agent review (Claude Code subprocess) — dev-only for now.
	AgenticReviews     bool
	AgentCloneRootDir  string
	AgentLogsDir       string
	AgentWallClockSec  int
	AgentMaxTurns      int
	AgentMaxConcurrent int // <=0 disables the cap (unlimited concurrency)
}

// IsMultiUserMode returns true if the application is configured for multi-user mode (GitHub App)
// Deprecated: Use IsDevMode() instead. The single-user vs multi-user distinction is being removed.
func (c *Config) IsMultiUserMode() bool {
	return c.GitHubAppID != "" && c.GitHubAppPrivateKeyPath != ""
}

// IsDevMode returns true if the application is running in development mode.
// Dev mode: DEV_MODE=true OR no OAuth client ID configured.
// In dev mode, the user is auto-logged-in based on GITHUB_USERNAME.
func (c *Config) IsDevMode() bool {
	// Explicit dev mode flag
	if os.Getenv("DEV_MODE") == "true" {
		return true
	}
	// No OAuth configured means dev mode
	return c.GitHubAppClientID == ""
}

// UsePostgreSQL returns true if the application should use PostgreSQL instead of SQLite
func (c *Config) UsePostgreSQL() bool {
	return c.DatabaseURL != ""
}

func Load() *Config {
	pollingInterval := 1 * time.Minute
	if interval := os.Getenv("POLLING_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			pollingInterval = d
		}
	}

	return &Config{
		// Legacy single-user mode
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		GitHubUsername: os.Getenv("GITHUB_USERNAME"),

		// GitHub App (multi-user mode)
		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppPrivateKeyPath: os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"),
		GitHubAppClientID:       os.Getenv("GITHUB_APP_CLIENT_ID"),
		GitHubAppClientSecret:   os.Getenv("GITHUB_APP_CLIENT_SECRET"),
		GitHubAppInstallationID: os.Getenv("GITHUB_APP_INSTALLATION_ID"),
		GitHubOrgName:           os.Getenv("GITHUB_ORG_NAME"),

		// OAuth
		OAuthCallbackURL: os.Getenv("OAUTH_CALLBACK_URL"),
		SessionSecret:    os.Getenv("SESSION_SECRET"),
		BaseURL:          os.Getenv("BASE_URL"),

		// Database
		DatabaseURL: os.Getenv("DATABASE_URL"),
		DBPath:      getEnvOrDefault("DB_PATH", "./data/pr-review.db"),

		// General
		PollingInterval: pollingInterval,
		ReviewsDir:      getEnvOrDefault("REVIEWS_DIR", "./reviews"), // Deprecated
		GCSBucket:       os.Getenv("GCS_BUCKET"),                     // Required for cloud storage
		ServerPort:      getEnvOrDefault("SERVER_PORT", "8080"),
		ReviewerEnabled: false, // Will be set to true in main.go if API key is available
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),

		AgenticReviews:     os.Getenv("AGENTIC_REVIEWS") == "true",
		AgentCloneRootDir:  getEnvOrDefault("AGENT_CLONE_ROOT_DIR", "./data/agent-clones"),
		AgentLogsDir:       getEnvOrDefault("AGENT_LOGS_DIR", "./data/agent-logs"),
		AgentWallClockSec:  getEnvIntOrDefault("AGENT_WALL_CLOCK_SEC", 360),
		AgentMaxTurns:      getEnvIntOrDefault("AGENT_MAX_TURNS", 40),
		AgentMaxConcurrent: getEnvIntOrDefault("AGENT_MAX_CONCURRENT", 2),
	}
}

func getEnvIntOrDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
