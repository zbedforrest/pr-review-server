package config

import (
	"os"
	"time"
)

type Config struct {
	GitHubToken              string
	GitHubUsername           string
	PollingInterval          time.Duration
	DBPath                   string
	ReviewsDir               string
	ServerPort               string
	CbprPath                 string
	EnableVoiceNotifications bool
	DevMode                  bool
}

func Load() *Config {
	pollingInterval := 1 * time.Minute
	if interval := os.Getenv("POLLING_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			pollingInterval = d
		}
	}

	cbprPath := os.Getenv("CBPR_PATH")
	if cbprPath == "" {
		cbprPath = "cbpr" // assume it's in PATH
	}

	// Enable voice notifications by default (can be disabled with ENABLE_VOICE_NOTIFICATIONS=false)
	enableVoice := getEnvOrDefault("ENABLE_VOICE_NOTIFICATIONS", "true") == "true"

	// Dev mode for local development with Vite dev server
	devMode := getEnvOrDefault("DEV_MODE", "false") == "true"

	return &Config{
		GitHubToken:              os.Getenv("GITHUB_TOKEN"),
		GitHubUsername:           os.Getenv("GITHUB_USERNAME"),
		PollingInterval:          pollingInterval,
		DBPath:                   getEnvOrDefault("DB_PATH", "./data/pr-review.db"),
		ReviewsDir:               getEnvOrDefault("REVIEWS_DIR", "./reviews"),
		ServerPort:               getEnvOrDefault("SERVER_PORT", "8080"),
		CbprPath:                 cbprPath,
		EnableVoiceNotifications: enableVoice,
		DevMode:                  devMode,
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
