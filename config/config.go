package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAgentWallClockSec         = 360
	defaultAgentMaxTurns             = 40
	defaultOpenRouterAgentMaxTurns   = 200
	defaultReviewFirstPassSamples    = 3
	defaultReviewFirstPassConcurrent = 5
	defaultClaudeAgentModel          = "claude-opus-4-8"
	defaultOpenRouterAgentModel      = "openai/gpt-5.6-sol"
	defaultAgentEffort               = "medium"
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
	DisablePolling  bool   // skip initial+scheduled polls (benchmark/on-demand deployments)
	ReviewsDir      string // Deprecated: use GCSBucket instead
	GCSBucket       string
	ServerPort      string
	ReviewerEnabled bool
	GeminiAPIKey    string

	// Agent review (Claude Code or Codex/OpenRouter subprocess).
	AgenticReviews     bool
	AgentCloneRootDir  string
	AgentLogsDir       string
	AgentWallClockSec  int
	AgentMaxTurns      int
	AgentMaxConcurrent int    // <=0 disables the cap (unlimited concurrency)
	AgentBackend       string // claude (default) or openrouter
	AgentModel         string // backend model id for agent reviews (empty = backend default)
	AgentEffort        string // backend reasoning effort for agent reviews (empty = service default)
	AnthropicAPIKey    string // frozen readiness signal; Claude retains its CLI-native auth flow
	OpenRouterAPIKey   string // OpenRouter credential; deployment-only, never exposed in capabilities
	OpenRouterBaseURL  string // OpenRouter API root (empty = service default)
	BugMemoryPath      string // local path to a bug-memory library JSON (dev/benchmark)
	BugMemoryObject    string // GCS object name of the library (prod); Path wins if both set
	RequiredChecks     bool   // convert fired gates/memory entries into forced-choice agent checks (service/checks.go)

	// Caller-customization policy. These allowlists and ceilings are owned by
	// the deployment operator; per-review API overrides must remain within them.
	// Credentials, provider endpoints, filesystem paths, and concurrency remain
	// deployment-only settings.
	ReviewAgentModelsClaude      []string
	ReviewAgentModelsOpenRouter  []string
	ReviewAgentEffortsClaude     []string
	ReviewAgentEffortsOpenRouter []string
	ReviewMaxWallClockSec        int
	ReviewMaxTurns               int
	ReviewMaxTurnsConfigured     bool
	ReviewMaxFirstPassSamples    int
	ReviewMaxFirstPassConcurrent int
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

	agentBackend := getEnvOrDefault("AGENT_BACKEND", "claude")
	agentWallClockSec := getEnvIntOrDefault("AGENT_WALL_CLOCK_SEC", defaultAgentWallClockSec)
	agentMaxTurnsDefault := defaultAgentMaxTurns
	if strings.EqualFold(strings.TrimSpace(agentBackend), "openrouter") {
		agentMaxTurnsDefault = defaultOpenRouterAgentMaxTurns
	}
	agentMaxTurns := getEnvIntOrDefault("AGENT_MAX_TURNS", agentMaxTurnsDefault)
	agentModel := os.Getenv("AGENT_MODEL")
	agentEffort := os.Getenv("AGENT_EFFORT")

	claudeModels := getEnvListOrDefault("REVIEW_AGENT_MODELS_CLAUDE",
		[]string{defaultClaudeAgentModel, "claude-fable-5"}, normalizeModel)
	openRouterModels := getEnvListOrDefault("REVIEW_AGENT_MODELS_OPENROUTER",
		[]string{defaultOpenRouterAgentModel}, normalizeModel)
	claudeEfforts := getEnvListOrDefault("REVIEW_AGENT_EFFORTS_CLAUDE",
		[]string{"low", "medium", "high"}, normalizeEffort)
	openRouterEfforts := getEnvListOrDefault("REVIEW_AGENT_EFFORTS_OPENROUTER",
		[]string{"low", "medium", "high", "xhigh", "max"}, normalizeEffort)

	// Operator allowlists must never invalidate the deployment's currently
	// selected backend/model/effort. Keep Config's active values untouched and
	// append only their resolved runtime values to the matching policy list.
	activeBackend := strings.ToLower(strings.TrimSpace(agentBackend))
	activeModel := strings.TrimSpace(agentModel)
	if activeModel == "" {
		if activeBackend == "openrouter" {
			activeModel = defaultOpenRouterAgentModel
		} else {
			activeModel = defaultClaudeAgentModel
		}
	}
	activeEffort := normalizeEffort(agentEffort)
	if activeEffort == "" {
		activeEffort = defaultAgentEffort
	}
	switch activeBackend {
	case "claude":
		claudeModels = appendUnique(claudeModels, activeModel)
		claudeEfforts = appendUnique(claudeEfforts, activeEffort)
	case "openrouter":
		openRouterModels = appendUnique(openRouterModels, activeModel)
		openRouterEfforts = appendUnique(openRouterEfforts, activeEffort)
	}

	maxWallClockDefault := positiveOrDefault(agentWallClockSec, defaultAgentWallClockSec)
	maxTurnsDefault := positiveOrDefault(agentMaxTurns, agentMaxTurnsDefault)
	reviewMaxTurnsDefault := maxTurnsDefault
	if reviewMaxTurnsDefault < defaultOpenRouterAgentMaxTurns {
		// The deployment default remains backend-specific, but an unset caller
		// ceiling must leave room for every selectable backend's default unit.
		reviewMaxTurnsDefault = defaultOpenRouterAgentMaxTurns
	}
	reviewMaxTurns, reviewMaxTurnsConfigured := getPositiveEnvInt("REVIEW_MAX_TURNS")
	if !reviewMaxTurnsConfigured {
		reviewMaxTurns = reviewMaxTurnsDefault
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
		DisablePolling:  os.Getenv("DISABLE_POLLING") == "true",
		ReviewsDir:      getEnvOrDefault("REVIEWS_DIR", "./reviews"), // Deprecated
		GCSBucket:       os.Getenv("GCS_BUCKET"),                     // Required for cloud storage
		ServerPort:      getEnvOrDefault("SERVER_PORT", "8080"),
		ReviewerEnabled: false, // Will be set to true in main.go if API key is available
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),

		AgenticReviews:     os.Getenv("AGENTIC_REVIEWS") == "true",
		AgentCloneRootDir:  getEnvOrDefault("AGENT_CLONE_ROOT_DIR", "./data/agent-clones"),
		AgentLogsDir:       getEnvOrDefault("AGENT_LOGS_DIR", "./data/agent-logs"),
		AgentWallClockSec:  agentWallClockSec,
		AgentMaxTurns:      agentMaxTurns,
		AgentMaxConcurrent: getEnvIntOrDefault("AGENT_MAX_CONCURRENT", 2),
		AgentBackend:       agentBackend,
		AgentModel:         agentModel,
		AgentEffort:        agentEffort,
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		OpenRouterAPIKey:   os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterBaseURL:  os.Getenv("OPENROUTER_BASE_URL"),
		BugMemoryPath:      os.Getenv("BUG_MEMORY_PATH"),
		BugMemoryObject:    os.Getenv("BUG_MEMORY_OBJECT"),
		RequiredChecks:     os.Getenv("REQUIRED_CHECKS") == "true",

		ReviewAgentModelsClaude:      claudeModels,
		ReviewAgentModelsOpenRouter:  openRouterModels,
		ReviewAgentEffortsClaude:     claudeEfforts,
		ReviewAgentEffortsOpenRouter: openRouterEfforts,
		ReviewMaxWallClockSec:        getPositiveEnvIntOrDefault("REVIEW_MAX_WALL_CLOCK_SEC", maxWallClockDefault),
		ReviewMaxTurns:               reviewMaxTurns,
		ReviewMaxTurnsConfigured:     reviewMaxTurnsConfigured,
		ReviewMaxFirstPassSamples:    getPositiveEnvIntOrDefault("REVIEW_MAX_FIRST_PASS_SAMPLES", defaultReviewFirstPassSamples),
		ReviewMaxFirstPassConcurrent: getPositiveEnvIntOrDefault("REVIEW_MAX_FIRST_PASS_CONCURRENT", defaultReviewFirstPassConcurrent),
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

// getPositiveEnvIntOrDefault is used for hard safety ceilings. Invalid,
// zero, and negative values fall back to a positive default rather than
// accidentally disabling a limit.
func getPositiveEnvIntOrDefault(key string, defaultValue int) int {
	if value, ok := getPositiveEnvInt(key); ok {
		return value
	}
	return defaultValue
}

func getPositiveEnvInt(key string) (int, bool) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	return n, err == nil && n > 0
}

func positiveOrDefault(value, defaultValue int) int {
	if value > 0 {
		return value
	}
	return defaultValue
}

// getEnvListOrDefault parses a comma-separated deployment setting in stable
// first-seen order, dropping blank and duplicate entries after normalization.
// An unset or blank setting uses the supplied defaults.
func getEnvListOrDefault(key string, defaults []string, normalize func(string) string) []string {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		raw = strings.Join(defaults, ",")
	}

	values := make([]string, 0, len(defaults))
	for _, item := range strings.Split(raw, ",") {
		value := normalize(item)
		if value != "" {
			values = appendUnique(values, value)
		}
	}
	return values
}

func normalizeModel(value string) string {
	// Provider model IDs are case-sensitive. Whitespace is formatting noise,
	// but case must be preserved for exact policy matching.
	return strings.TrimSpace(value)
}

func normalizeEffort(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
