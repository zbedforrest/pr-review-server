package config

import (
	"log"
	"math"
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
	defaultClaudeAgentModel          = "claude-fable-5-1"
	defaultOpenRouterAgentModel      = "openai/gpt-5.6-sol"
	defaultAgentEffort               = "medium"

	// First-pass provider default models, mirroring the llm package defaults
	// so this package stays dependency-light.
	defaultFirstPassGeminiModel     = "gemini-3.1-pro-preview"
	defaultFirstPassClaudeModel     = "claude-sonnet-5"
	defaultFirstPassClaudeCodeModel = "claude-fable-5-1"
	defaultFirstPassOpenRouterModel = "openai/gpt-5.6-sol"

	defaultFirstPassCacheStaggerSec = 8
	// Ten minutes: the audit of 2026-10-14 saw PRs reviewed up to 13 times a
	// day because every push outside a 90 s window became a round.
	defaultAutoReviewSyncDebounceSec = 120
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
	// GitHubWebhookSecret verifies X-Hub-Signature-256 on POST /webhooks/github;
	// empty leaves the endpoint disabled.
	GitHubWebhookSecret string

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

	// First-pass (sampled single-shot review) provider selection. Empty or
	// "gemini" preserves the historical Gemini-only behavior; the
	// classification stage always stays on Gemini flash.
	FirstPassProvider string // gemini (default), claude, claude-code, or openrouter
	FirstPassModel    string // empty = provider default
	FirstPassThinking string // gemini/claude-code thinking level: low, medium, high; empty = provider default
	// FirstPassCacheStaggerSec delays each subsequent claude first-pass sample
	// so sample 1's prompt-cache prefill completes first. 0 disables.
	FirstPassCacheStaggerSec int

	// Agent review (Claude Code or Codex/OpenRouter subprocess).
	AgenticReviews     bool
	AgentCloneRootDir  string
	AgentLogsDir       string
	AgentWallClockSec  int
	AgentMaxTurns      int
	AgentMaxConcurrent int // <=0 uses the poller's safe five-process fallback
	// AgentCloneCacheMaxGB caps the repo clone caches under AgentCloneRootDir;
	// least recently used idle caches are evicted past it. 0 is unlimited.
	AgentCloneCacheMaxGB int
	AgentBackend         string // claude (default) or openrouter
	AgentModel           string // backend model id for agent reviews (empty = backend default)
	AgentEffort          string // backend reasoning effort for agent reviews (empty = service default)
	AgentPinBase         bool   // review against a fix-free checkout pinned at the PR's own base (benchmarks)
	// EnsembleMaxAgents caps concurrent ensemble agent processes per server,
	// on top of the one agent slot each ensemble review holds.
	EnsembleMaxAgents int
	// EnsembleVerify is the repo-evidence verifier's mode for ensemble
	// reviews: off, shadow (decisions recorded, nothing applied) or on. On
	// marks decisions applied; what posts changes only with the publisher
	// half that reads them (PR #207).
	EnsembleVerify string
	// VerifyModel judges findings; VerifyWallClockSec bounds one call;
	// VerifyMaxInputTokens skips packets estimated larger; once
	// VerifyDailyBudgetUSD is spent (UTC day) verification stops until the
	// next day, 0 meaning no cap (a negative value keeps the default).
	// VerifyShadowSample is the share of dashboard-only reviews verified in
	// shadow mode.
	VerifyModel          string
	VerifyWallClockSec   int
	VerifyMaxInputTokens int
	VerifyDailyBudgetUSD float64
	VerifyShadowSample   float64
	// VerifyEscalate sends findings the packet check marks unavailable to a
	// read-only agent run (VerifyAgent*; backend and model default to the
	// review agent's backend with Opus 5.5).
	VerifyEscalate bool
	// VerifyPacketCall runs the one-call packet check first; off (the
	// default, the configuration the fresh-window test measured), every
	// finding goes straight to the verify agent.
	VerifyPacketCall bool
	// VerifyDemoteMedium posts a MEDIUM the verify agent judged not worth
	// posting as a LOW note (default on).
	VerifyDemoteMedium      bool
	VerifyAgentBackend      string
	VerifyAgentModel        string
	VerifyAgentWallClockSec int
	VerifyAgentMaxTurns     int
	// QualityGate grades every finding about to post with the grader rubric
	// before publishing: off, shadow (record only) or on (a BAD grade keeps
	// the finding off GitHub; a CRITICAL posts as a LOW note). Gate errors
	// publish as before. QualityGateBudgetUSD caps gate spend per UTC day.
	QualityGate          string
	QualityGateBudgetUSD float64
	// PublishRewrite rewrites each finding about to post inline into a short
	// title and a direct body (trigger, consequence, fix) before publishing:
	// off, shadow (log the rewrite only) or on (the comment shows it; the
	// full finding moves into its details). Errors publish as before.
	// PublishRewriteBudgetUSD caps rewrite spend per UTC day, shadow included;
	// 0 means no cap.
	PublishRewrite             string
	PublishRewriteModel        string
	PublishRewriteWallClockSec int
	PublishRewriteBudgetUSD    float64
	// GraderEnabled lets the scheduled job (POST /api/health/grade) grade
	// comments posted in the last week that have no grade yet, oldest first;
	// GraderDailyBudgetUSD caps grading spend per UTC day and GraderMaxItems
	// the comments graded per run.
	GraderEnabled        bool
	GraderModel          string
	GraderDailyBudgetUSD float64
	GraderMaxItems       int
	GraderWallClockSec   int
	GraderMaxTurns       int
	// AgentPrewarmRepos are owner/repo clone caches built at startup; empty
	// warms the most-reviewed repos instead.
	AgentPrewarmRepos []string
	// New Relic custom events (optional): account, one of the two keys, and
	// the region (US or EU).
	NewRelicAccountID  string
	NewRelicInsertKey  string
	NewRelicLicenseKey string
	NewRelicRegion     string
	// Author-reply model (pkg/reviewer/service/reply.go). Same backend and
	// credentials as the review agent; smaller budgets, own concurrency cap.
	ReplyModel    string // empty = the review agent's model
	ReplyPolicyV2 bool   // REPLY_POLICY_V2, default on; false restores the fixed budget notice and uncontested holds
	// ReplyVerdictFastPath settles an author's "intentional" / "won't fix"
	// reply with a reaction and a dismissed finding instead of a model reply.
	ReplyVerdictFastPath bool // REPLY_VERDICT_FAST_PATH, default on
	ReplyWallClockSec    int
	ReplyMaxTurns        int
	ReplyMaxConcurrent   int
	// MentionHandle is the App login authors mention to request a review
	// ("@<handle> review"); empty disables mention triggers.
	MentionHandle string
	// MentionPublishDrafts lets a mention on a draft PR post its review;
	// automatic and poller reviews of drafts stay dashboard-only.
	MentionPublishDrafts bool
	// DisableSettleFooter drops the settle and opt-out footer from posted
	// comments (PUBLISH_SETTLE_FOOTER=false).
	DisableSettleFooter bool
	// HealthJobToken authenticates the scheduled daily health report
	// (POST /api/health/daily); empty disables the job endpoint.
	HealthJobToken string
	// FeedbackDigest lets the daily health run scan GitHub for author
	// feedback on PRism comments. Off unless FEEDBACK_DIGEST=true because the
	// scan classifies each item with a model call.
	FeedbackDigest bool
	// AdminLogins are bootstrap settings admins (lowercased GitHub logins), kept
	// outside the writable settings API so admins can never lock themselves out.
	AdminLogins []string
	// AnthropicAPIKey is optional for the agent pass (Claude OAuth remains
	// supported) but required when FirstPassProvider is "claude".
	AnthropicAPIKey   string
	OpenRouterAPIKey  string // OpenRouter credential; deployment-only, never exposed in capabilities
	OpenRouterBaseURL string // OpenRouter API root (empty = service default)
	// OpenRouterProviderOrder and OpenRouterProviderIgnore are OpenRouter
	// provider preferences for models starting with OpenRouterProviderMatch,
	// applied by a loopback proxy at startup (fallbacks stay allowed).
	OpenRouterProviderOrder  []string
	OpenRouterProviderIgnore []string
	OpenRouterProviderMatch  string
	BugMemoryPath            string // local path to a bug-memory library JSON (dev/benchmark)
	BugMemoryObject          string // GCS object name of the library (prod); Path wins if both set
	RequiredChecks           bool   // convert fired gates/memory entries into forced-choice agent checks (pkg/reviewer/service/checks.go)
	// AgentPriorFindings shows the review agent what PRism already raised on
	// the PR, from the publication ledger, so a restatement names its prior
	// (AGENT_PRIOR_FINDINGS=true; off by default, a prompt change).
	AgentPriorFindings bool

	// Publisher kill switches; the zero value is the fixed behaviour and each
	// env var set to false restores the old one. PUBLISH_RENDER_V2 titles
	// comments and bullets by headline instead of impact sentence,
	// PUBLISH_SAME_COMMIT_GUARD refuses a second round for a published head,
	// PUBLISH_REFRESH_SUMMARY re-renders the summary after a concession.
	PublishLegacyTitles        bool
	PublishRepublishSameCommit bool
	PublishSkipSummaryRefresh  bool
	PublishNoInlinePRBudget    bool
	PublishNoPRMute            bool
	// PublishNoPriorIDAlias (PUBLISH_PRIOR_ID_ALIAS=false) stops the publisher
	// from aliasing a finding to the ledger row its prior_id names.
	PublishNoPriorIDAlias bool
	// PublishResolveNotes (PUBLISH_RESOLVE_NOTES=true) restores the one-line
	// reply the publisher used to post into a thread it resolves.
	PublishResolveNotes bool
	// PublishFixedByFile (PUBLISH_FIXED_NEEDS_HUNK=false) judges an absent
	// finding fixed when its file changed anywhere; PublishLegacyStatusNotes
	// (PUBLISH_STATUS_NOTES_V2=false) posts a return and a severity change as
	// two thread notes and reopens on any text match; PublishLegacyClamp
	// (PUBLISH_RENDER_CLAMP_V2=false) cuts bullets and headlines at a word
	// boundary with "...".
	PublishFixedByFile       bool
	PublishLegacyStatusNotes bool
	PublishLegacyClamp       bool
	// PublishNoScopeGuard (PUBLISH_SCOPE_GUARD=false) lets fixed, return and
	// severity transitions through on inherited paths, paths outside the PR
	// diff and after a merge-base change.
	PublishNoScopeGuard bool
	// PublishNoLibraryClaimHold (PUBLISH_LIBRARY_CLAIM_HOLD=false) posts a
	// finding that rests on third-party behaviour the agent did not read;
	// PublishNoRegressionEvidence (PUBLISH_REGRESSION_EVIDENCE=false) posts a
	// regression claim the cited hunk contradicts. Both holds are on by
	// default; PUBLISH_RENDER_HYGIENE is read by the publisher itself.
	PublishNoLibraryClaimHold   bool
	PublishNoRegressionEvidence bool
	// AutoReviewSyncDebounceSec of 0 admits every push at once.
	AutoReviewSyncDebounceSec   int
	AutoReviewSkipUnchangedDiff bool
	// AutoReviewNoDailyRoundCap (AUTO_REVIEW_DAILY_ROUND_CAP=false) admits
	// every push whatever auto_review_daily_round_cap says.
	AutoReviewNoDailyRoundCap bool
	// ReviewDefaultProfile is the profile used when a caller names none:
	// automatic reviews, the Generate button, and profile-less API requests.
	ReviewDefaultProfile string

	// Linked ticket context for the agent prompt (pkg/reviewer/tickets). The
	// feature is on only when JiraEnabled(); JiraProjectKeys optionally
	// restricts which project keys count as ticket references.
	JiraBaseURL     string
	JiraEmail       string
	JiraAPIToken    string
	JiraProjectKeys []string

	// Caller-customization policy. These allowlists and ceilings are owned by
	// the deployment operator; per-review API overrides must remain within them.
	// Credentials, provider endpoints, filesystem paths, and concurrency remain
	// deployment-only settings.
	ReviewAgentModelsClaude         []string
	ReviewAgentModelsOpenRouter     []string
	ReviewAgentEffortsClaude        []string
	ReviewAgentEffortsOpenRouter    []string
	ReviewFirstPassModelsGemini     []string
	ReviewFirstPassModelsClaude     []string
	ReviewFirstPassModelsClaudeCode []string
	ReviewFirstPassModelsOpenRouter []string
	ReviewMaxWallClockSec           int
	ReviewMaxTurns                  int
	ReviewMaxTurnsConfigured        bool
	ReviewMaxFirstPassSamples       int
	ReviewMaxFirstPassConcurrent    int
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

// FirstPassAPIKey returns the credential the configured first-pass provider
// authenticates with.
func (c *Config) FirstPassAPIKey() string {
	return c.FirstPassProviderAPIKey(c.FirstPassProvider)
}

// FirstPassProviderAPIKey returns the credential a specific first-pass
// provider authenticates with.
func (c *Config) FirstPassProviderAPIKey(provider string) string {
	switch provider {
	case "claude":
		return c.AnthropicAPIKey
	case "claude-code":
		return ""
	case "openrouter":
		return c.OpenRouterAPIKey
	default:
		return c.GeminiAPIKey
	}
}

// JiraEnabled reports whether linked-ticket fetching is configured.
func (c *Config) JiraEnabled() bool {
	return c.JiraBaseURL != "" && c.JiraEmail != "" && c.JiraAPIToken != ""
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
		[]string{defaultClaudeAgentModel, "claude-fable-5", "claude-opus-4-8"}, normalizeModel)
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

	firstPassProvider := normalizeFirstPassProvider(getEnvOrDefault("FIRST_PASS_PROVIDER", "gemini"))
	firstPassModel := strings.TrimSpace(os.Getenv("FIRST_PASS_MODEL"))
	// The gemini first-pass default follows the same env override the llm
	// package honors, so the allowlist always admits the model actually run.
	firstPassGeminiDefault := getEnvOrDefault("GEMINI_PRO_MODEL", defaultFirstPassGeminiModel)
	firstPassModelsGemini := getEnvListOrDefault("REVIEW_FIRST_PASS_MODELS_GEMINI",
		[]string{firstPassGeminiDefault}, normalizeModel)
	firstPassModelsClaude := getEnvListOrDefault("REVIEW_FIRST_PASS_MODELS_CLAUDE",
		[]string{defaultFirstPassClaudeModel}, normalizeModel)
	firstPassModelsClaudeCode := getEnvListOrDefault("REVIEW_FIRST_PASS_MODELS_CLAUDE_CODE",
		[]string{defaultFirstPassClaudeCodeModel}, normalizeModel)
	firstPassModelsOpenRouter := getEnvListOrDefault("REVIEW_FIRST_PASS_MODELS_OPENROUTER",
		[]string{defaultFirstPassOpenRouterModel}, normalizeModel)
	activeFirstPassModel := firstPassModel
	if activeFirstPassModel == "" {
		switch firstPassProvider {
		case "claude":
			activeFirstPassModel = defaultFirstPassClaudeModel
		case "claude-code":
			activeFirstPassModel = defaultFirstPassClaudeCodeModel
		case "openrouter":
			activeFirstPassModel = defaultFirstPassOpenRouterModel
		default:
			activeFirstPassModel = firstPassGeminiDefault
		}
	}
	switch firstPassProvider {
	case "claude":
		firstPassModelsClaude = appendUnique(firstPassModelsClaude, activeFirstPassModel)
	case "claude-code":
		firstPassModelsClaudeCode = appendUnique(firstPassModelsClaudeCode, activeFirstPassModel)
	case "openrouter":
		firstPassModelsOpenRouter = appendUnique(firstPassModelsOpenRouter, activeFirstPassModel)
	case "gemini":
		firstPassModelsGemini = appendUnique(firstPassModelsGemini, activeFirstPassModel)
	}

	maxWallClockDefault := positiveOrDefault(agentWallClockSec, defaultAgentWallClockSec)
	reviewMaxTurns, reviewMaxTurnsConfigured := getPositiveEnvInt("REVIEW_MAX_TURNS")

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
		GitHubWebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),

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

		FirstPassProvider:        firstPassProvider,
		FirstPassModel:           firstPassModel,
		FirstPassThinking:        strings.ToLower(strings.TrimSpace(os.Getenv("FIRST_PASS_THINKING"))),
		FirstPassCacheStaggerSec: getNonNegativeEnvIntOrDefault("FIRST_PASS_CACHE_STAGGER_SEC", defaultFirstPassCacheStaggerSec),

		AgenticReviews:              os.Getenv("AGENTIC_REVIEWS") == "true",
		AgentCloneRootDir:           getEnvOrDefault("AGENT_CLONE_ROOT_DIR", "./data/agent-clones"),
		AgentLogsDir:                getEnvOrDefault("AGENT_LOGS_DIR", "./data/agent-logs"),
		AgentWallClockSec:           agentWallClockSec,
		AgentMaxTurns:               agentMaxTurns,
		AgentMaxConcurrent:          getEnvIntOrDefault("AGENT_MAX_CONCURRENT", 2),
		AgentCloneCacheMaxGB:        getEnvIntOrDefault("AGENT_CLONE_CACHE_MAX_GB", 0),
		EnsembleMaxAgents:           getEnvIntOrDefault("ENSEMBLE_MAX_AGENTS", 25),
		EnsembleVerify:              verifyMode(os.Getenv("ENSEMBLE_VERIFY")),
		VerifyModel:                 getEnvOrDefault("VERIFY_MODEL", "anthropic/claude-fable-5.1"),
		VerifyWallClockSec:          getPositiveEnvIntOrDefault("VERIFY_WALL_CLOCK_SEC", 180),
		VerifyMaxInputTokens:        getEnvIntOrDefault("VERIFY_MAX_INPUT_TOKENS", 150000),
		VerifyDailyBudgetUSD:        verifyBudget(getEnvFloatOrDefault("VERIFY_DAILY_BUDGET_USD", 150)),
		VerifyShadowSample:          min(max(getEnvFloatOrDefault("VERIFY_SHADOW_SAMPLE", 1), 0), 1),
		VerifyEscalate:              getEnvBoolOrDefault("VERIFY_ESCALATE", true),
		VerifyPacketCall:            getEnvBoolOrDefault("VERIFY_PACKET_CALL", false),
		VerifyDemoteMedium:          getEnvBoolOrDefault("VERIFY_DEMOTE_MEDIUM", true),
		VerifyAgentBackend:          os.Getenv("VERIFY_AGENT_BACKEND"),
		VerifyAgentModel:            os.Getenv("VERIFY_AGENT_MODEL"),
		VerifyAgentWallClockSec:     getPositiveEnvIntOrDefault("VERIFY_AGENT_WALL_CLOCK_SEC", 420),
		VerifyAgentMaxTurns:         getPositiveEnvIntOrDefault("VERIFY_AGENT_MAX_TURNS", 100),
		QualityGate:                 qualityGateMode(os.Getenv("QUALITY_GATE")),
		QualityGateBudgetUSD:        qualityGateBudget(getEnvFloatOrDefault("QUALITY_GATE_DAILY_BUDGET_USD", 40)),
		PublishRewrite:              stageMode("PUBLISH_REWRITE", os.Getenv("PUBLISH_REWRITE")),
		PublishRewriteModel:         getEnvOrDefault("PUBLISH_REWRITE_MODEL", "claude-sonnet-5-5"),
		PublishRewriteWallClockSec:  getPositiveEnvIntOrDefault("PUBLISH_REWRITE_WALL_CLOCK_SEC", 120),
		PublishRewriteBudgetUSD:     max(getEnvFloatOrDefault("PUBLISH_REWRITE_DAILY_BUDGET_USD", 10), 0),
		GraderEnabled:               getEnvBoolOrDefault("GRADER_ENABLED", false),
		GraderModel:                 getEnvOrDefault("GRADER_MODEL", "claude-fable-5-1"),
		GraderDailyBudgetUSD:        graderBudget(getEnvFloatOrDefault("GRADER_DAILY_BUDGET_USD", 20)),
		GraderMaxItems:              getPositiveEnvIntOrDefault("GRADER_MAX_ITEMS", 60),
		GraderWallClockSec:          getPositiveEnvIntOrDefault("GRADER_WALL_CLOCK_SEC", 900),
		GraderMaxTurns:              getPositiveEnvIntOrDefault("GRADER_MAX_TURNS", 120),
		AgentPrewarmRepos:           getEnvListOrDefault("AGENT_PREWARM_REPOS", nil, strings.TrimSpace),
		NewRelicAccountID:           os.Getenv("NEW_RELIC_ACCOUNT_ID"),
		NewRelicInsertKey:           os.Getenv("NEW_RELIC_INSERT_KEY"),
		NewRelicLicenseKey:          os.Getenv("NEW_RELIC_LICENSE_KEY"),
		NewRelicRegion:              os.Getenv("NEW_RELIC_REGION"),
		AgentBackend:                agentBackend,
		AgentModel:                  agentModel,
		AgentEffort:                 agentEffort,
		AgentPinBase:                strings.EqualFold(strings.TrimSpace(os.Getenv("AGENT_PIN_BASE")), "true"),
		ReplyModel:                  os.Getenv("REPLY_MODEL"),
		ReplyWallClockSec:           getPositiveEnvIntOrDefault("REPLY_WALL_CLOCK_SEC", 180),
		ReplyMaxTurns:               getPositiveEnvIntOrDefault("REPLY_MAX_TURNS", 20),
		ReplyMaxConcurrent:          getPositiveEnvIntOrDefault("REPLY_MAX_CONCURRENT", 2),
		ReplyPolicyV2:               !strings.EqualFold(strings.TrimSpace(os.Getenv("REPLY_POLICY_V2")), "false"),
		ReplyVerdictFastPath:        !strings.EqualFold(strings.TrimSpace(os.Getenv("REPLY_VERDICT_FAST_PATH")), "false"),
		MentionHandle:               strings.TrimSpace(getEnvOrDefaultAllowEmpty("MENTION_HANDLE", "prism-pr-review-server")),
		MentionPublishDrafts:        strings.EqualFold(strings.TrimSpace(os.Getenv("MENTION_PUBLISH_DRAFTS")), "true"),
		DisableSettleFooter:         strings.EqualFold(strings.TrimSpace(os.Getenv("PUBLISH_SETTLE_FOOTER")), "false"),
		HealthJobToken:              os.Getenv("HEALTH_JOB_TOKEN"),
		FeedbackDigest:              strings.EqualFold(strings.TrimSpace(os.Getenv("FEEDBACK_DIGEST")), "true"),
		AdminLogins:                 getEnvListOrDefault("ADMIN_LOGINS", nil, normalizeLogin),
		AnthropicAPIKey:             os.Getenv("ANTHROPIC_API_KEY"),
		OpenRouterAPIKey:            os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterBaseURL:           os.Getenv("OPENROUTER_BASE_URL"),
		OpenRouterProviderOrder:     getEnvListOrDefault("OPENROUTER_PROVIDER_ORDER", nil, strings.TrimSpace),
		OpenRouterProviderIgnore:    getEnvListOrDefault("OPENROUTER_PROVIDER_IGNORE", nil, strings.TrimSpace),
		OpenRouterProviderMatch:     strings.TrimSpace(getEnvOrDefault("OPENROUTER_PROVIDER_MATCH", "deepseek/")),
		BugMemoryPath:               os.Getenv("BUG_MEMORY_PATH"),
		BugMemoryObject:             os.Getenv("BUG_MEMORY_OBJECT"),
		RequiredChecks:              os.Getenv("REQUIRED_CHECKS") == "true",
		AgentPriorFindings:          getEnvBoolOrDefault("AGENT_PRIOR_FINDINGS", false),
		PublishNoPriorIDAlias:       !getEnvBoolOrDefault("PUBLISH_PRIOR_ID_ALIAS", true),
		PublishNoLibraryClaimHold:   !getEnvBoolOrDefault("PUBLISH_LIBRARY_CLAIM_HOLD", true),
		PublishNoRegressionEvidence: !getEnvBoolOrDefault("PUBLISH_REGRESSION_EVIDENCE", true),
		PublishResolveNotes:         getEnvBoolOrDefault("PUBLISH_RESOLVE_NOTES", false),
		PublishFixedByFile:          !getEnvBoolOrDefault("PUBLISH_FIXED_NEEDS_HUNK", true),
		PublishLegacyStatusNotes:    !getEnvBoolOrDefault("PUBLISH_STATUS_NOTES_V2", true),
		PublishLegacyClamp:          !getEnvBoolOrDefault("PUBLISH_RENDER_CLAMP_V2", true),
		PublishNoScopeGuard:         !getEnvBoolOrDefault("PUBLISH_SCOPE_GUARD", true),
		PublishLegacyTitles:         !getEnvBoolOrDefault("PUBLISH_RENDER_V2", true),
		PublishRepublishSameCommit:  !getEnvBoolOrDefault("PUBLISH_SAME_COMMIT_GUARD", true),
		PublishSkipSummaryRefresh:   !getEnvBoolOrDefault("PUBLISH_REFRESH_SUMMARY", true),
		ReviewDefaultProfile:        strings.ToLower(strings.TrimSpace(getEnvOrDefault("REVIEW_DEFAULT_PROFILE", "full"))),
		JiraBaseURL:                 strings.TrimRight(strings.TrimSpace(os.Getenv("JIRA_BASE_URL")), "/"),
		JiraEmail:                   strings.TrimSpace(os.Getenv("JIRA_EMAIL")),
		JiraAPIToken:                strings.TrimSpace(os.Getenv("JIRA_API_TOKEN")),
		JiraProjectKeys:             getEnvListOrDefault("JIRA_PROJECT_KEYS", nil, normalizeProjectKey),

		ReviewAgentModelsClaude:         claudeModels,
		ReviewAgentModelsOpenRouter:     openRouterModels,
		ReviewAgentEffortsClaude:        claudeEfforts,
		ReviewAgentEffortsOpenRouter:    openRouterEfforts,
		ReviewFirstPassModelsGemini:     firstPassModelsGemini,
		ReviewFirstPassModelsClaude:     firstPassModelsClaude,
		ReviewFirstPassModelsClaudeCode: firstPassModelsClaudeCode,
		ReviewFirstPassModelsOpenRouter: firstPassModelsOpenRouter,
		ReviewMaxWallClockSec:           getPositiveEnvIntOrDefault("REVIEW_MAX_WALL_CLOCK_SEC", maxWallClockDefault),
		ReviewMaxTurns:                  reviewMaxTurns,
		ReviewMaxTurnsConfigured:        reviewMaxTurnsConfigured,
		ReviewMaxFirstPassSamples:       getPositiveEnvIntOrDefault("REVIEW_MAX_FIRST_PASS_SAMPLES", defaultReviewFirstPassSamples),
		ReviewMaxFirstPassConcurrent:    getPositiveEnvIntOrDefault("REVIEW_MAX_FIRST_PASS_CONCURRENT", defaultReviewFirstPassConcurrent),

		PublishNoInlinePRBudget:     !getEnvBoolOrDefault("PUBLISH_INLINE_PR_BUDGET", true),
		PublishNoPRMute:             !getEnvBoolOrDefault("PUBLISH_PR_MUTE", true),
		AutoReviewSyncDebounceSec:   getNonNegativeEnvIntOrDefault("AUTO_REVIEW_SYNC_DEBOUNCE_SEC", defaultAutoReviewSyncDebounceSec),
		AutoReviewSkipUnchangedDiff: getEnvBoolOrDefault("AUTO_REVIEW_SKIP_UNCHANGED_DIFF", true),
		AutoReviewNoDailyRoundCap:   !getEnvBoolOrDefault("AUTO_REVIEW_DAILY_ROUND_CAP", true),
	}
}

// getEnvBoolOrDefault parses the usual true/false spellings; unset or
// unparsable values keep the default, so a typo never flips a kill switch.
func getEnvBoolOrDefault(key string, defaultValue bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if value, err := strconv.ParseBool(raw); err == nil {
		return value
	}
	if raw != "" {
		log.Printf("[CONFIG] %s=%q is not a boolean, keeping the default %t", key, raw, defaultValue)
	}
	return defaultValue
}

// qualityGateBudget keeps the default cap for a negative
// QUALITY_GATE_DAILY_BUDGET_USD, so a typo cannot lift the cap.
func qualityGateBudget(usd float64) float64 {
	if usd < 0 {
		log.Printf("[CONFIG] QUALITY_GATE_DAILY_BUDGET_USD=%g is negative, keeping the default 40", usd)
		return 40
	}
	return usd
}

// qualityGateMode accepts off, shadow or on; anything else is off.
func qualityGateMode(raw string) string {
	return stageMode("QUALITY_GATE", raw)
}

// stageMode reads an off/shadow/on switch, logging any other value as off.
func stageMode(name, raw string) string {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "shadow", "on":
		return m
	case "", "off":
	default:
		log.Printf("[CONFIG] %s=%q is not off, shadow or on; it is off", name, raw)
	}
	return "off"
}

// graderBudget keeps the default cap for a negative GRADER_DAILY_BUDGET_USD,
// so a typo cannot lift the cap.
func graderBudget(usd float64) float64 {
	if usd < 0 {
		log.Printf("[CONFIG] GRADER_DAILY_BUDGET_USD=%g is negative, keeping the default 20", usd)
		return 20
	}
	return usd
}

// verifyBudget keeps the default cap for a negative VERIFY_DAILY_BUDGET_USD,
// which would otherwise read as "no cap".
func verifyBudget(usd float64) float64 {
	if usd < 0 {
		log.Printf("[CONFIG] VERIFY_DAILY_BUDGET_USD=%g is negative, keeping the default 150", usd)
		return 150
	}
	return usd
}

// verifyMode reads ENSEMBLE_VERIFY; anything but shadow or on is off.
func verifyMode(raw string) string {
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case "shadow", "on":
		return mode
	case "", "off":
	default:
		log.Printf("[CONFIG] ENSEMBLE_VERIFY=%q is not off, shadow or on; verification is off", raw)
	}
	return "off"
}

func getEnvFloatOrDefault(key string, defaultValue float64) float64 {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		log.Printf("[CONFIG] %s=%q is not a number, keeping the default %g", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvIntOrDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return defaultValue
}

// getEnvOrDefaultAllowEmpty returns defaultValue only when key is unset; an
// explicitly empty value is returned as-is so it can disable a feature.
func getEnvOrDefaultAllowEmpty(key, defaultValue string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
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

// getNonNegativeEnvIntOrDefault allows an explicit 0 (feature off) but falls
// back to the default on unset, malformed, or negative values.
func getNonNegativeEnvIntOrDefault(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return defaultValue
	}
	return n
}

func getPositiveEnvInt(key string) (int, bool) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
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

func normalizeProjectKey(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func normalizeModel(value string) string {
	// Provider model IDs are case-sensitive. Whitespace is formatting noise,
	// but case must be preserved for exact policy matching.
	return strings.TrimSpace(value)
}

// normalizeFirstPassProvider mirrors the alias handling of llm.ParseProvider
// so every provider switch in this package and main sees the canonical name.
func normalizeFirstPassProvider(value string) string {
	provider := strings.ToLower(strings.TrimSpace(value))
	switch provider {
	case "claude_code", "claudecode":
		return "claude-code"
	default:
		return provider
	}
}

func normalizeEffort(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeLogin(value string) string {
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
