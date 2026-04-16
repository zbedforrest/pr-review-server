package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"pr-review-server/auth"
	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/gcs"
	"pr-review-server/github"
	"pr-review-server/poller"
	"pr-review-server/server"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Unified startup - always use the same architecture
	if cfg.IsDevMode() {
		log.Println("=== Starting PR Review Server (Dev Mode) ===")
	} else {
		log.Println("=== Starting PR Review Server (Production Mode) ===")
	}
	start(cfg)
}

// start runs the server with unified architecture.
// In dev mode, users are auto-logged-in based on GITHUB_USERNAME.
// In prod mode, users authenticate via OAuth.
func start(cfg *config.Config) {
	// Validate required config
	if cfg.IsDevMode() {
		// Dev mode requires GitHub token and username
		if cfg.GitHubToken == "" {
			log.Fatal("GITHUB_TOKEN environment variable is required")
		}
		if cfg.GitHubUsername == "" {
			log.Fatal("GITHUB_USERNAME environment variable is required")
		}
		log.Printf("GitHub Username: %s", cfg.GitHubUsername)
	} else {
		// Prod mode requires GitHub App and OAuth config
		if cfg.GitHubAppID == "" {
			log.Fatal("GITHUB_APP_ID environment variable is required for production mode")
		}
		if cfg.GitHubAppPrivateKeyPath == "" {
			log.Fatal("GITHUB_APP_PRIVATE_KEY_PATH environment variable is required for production mode")
		}
		if cfg.GitHubAppClientID == "" {
			log.Fatal("GITHUB_APP_CLIENT_ID environment variable is required for production mode")
		}
		if cfg.GitHubAppClientSecret == "" {
			log.Fatal("GITHUB_APP_CLIENT_SECRET environment variable is required for production mode")
		}
		if cfg.GitHubAppInstallationID == "" {
			log.Fatal("GITHUB_APP_INSTALLATION_ID environment variable is required for production mode")
		}
		if cfg.OAuthCallbackURL == "" {
			log.Fatal("OAUTH_CALLBACK_URL environment variable is required for production mode")
		}
		if cfg.SessionSecret == "" {
			log.Fatal("SESSION_SECRET environment variable is required for production mode")
		}
		log.Printf("GitHub App ID: %s", cfg.GitHubAppID)
		if cfg.GitHubOrgName != "" {
			log.Printf("GitHub Org: %s", cfg.GitHubOrgName)
		}
	}

	log.Printf("Polling Interval: %s", cfg.PollingInterval)
	log.Printf("Server Port: %s", cfg.ServerPort)

	// Check if AI review is configured
	if cfg.GeminiAPIKey == "" {
		log.Println("ⓘ  INFO: GEMINI_API_KEY is not set. AI review generation is disabled.")
	} else {
		log.Println("✅ GEMINI_API_KEY found. AI review generation is enabled.")
		cfg.ReviewerEnabled = true
	}

	// Create reviews directory if using local storage
	if cfg.GCSBucket == "" {
		if err := os.MkdirAll(cfg.ReviewsDir, 0755); err != nil {
			log.Fatalf("Failed to create reviews directory: %v", err)
		}
	}

	// Initialize database
	var database db.Database
	var err error

	if cfg.UsePostgreSQL() {
		database, err = db.NewGormPostgres(cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("Failed to initialize PostgreSQL database: %v", err)
		}
		log.Println("PostgreSQL database initialized")
	} else {
		// Create data directory for SQLite
		dbDir := filepath.Dir(cfg.DBPath)
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			log.Fatalf("Failed to create data directory: %v", err)
		}
		database, err = db.NewGormSQLite(cfg.DBPath)
		if err != nil {
			log.Fatalf("Failed to initialize SQLite database: %v", err)
		}
		log.Printf("SQLite database initialized at %s", cfg.DBPath)
	}
	defer database.Close()

	// Initialize GitHub client
	var ghClient *github.Client

	if cfg.IsDevMode() {
		// Dev mode: Use personal access token
		ghClient = github.NewClient(cfg.GitHubToken, cfg.GitHubUsername)
		log.Println("GitHub client initialized with personal access token")
	} else {
		// Prod mode: Use GitHub App installation token
		appClient, err := github.NewAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath, cfg.GitHubAppInstallationID)
		if err != nil {
			log.Fatalf("Failed to initialize GitHub App client: %v", err)
		}
		log.Println("GitHub App client initialized")

		ctx := context.Background()
		token, err := appClient.GetToken(ctx)
		if err != nil {
			log.Fatalf("Failed to get GitHub App installation token: %v", err)
		}
		ghClient = github.NewClient(token, "")
		ghClient.SetAppClient(appClient)
		log.Println("GitHub client initialized with installation token (multi-user mode)")
		if cfg.GitHubOrgName != "" {
			log.Printf("Polling all open PRs for org: %s", cfg.GitHubOrgName)
		} else {
			log.Println("WARNING: GITHUB_ORG_NAME not set, poller will not find PRs in multi-user mode")
		}
	}

	// Initialize GCS client (optional in dev mode)
	var gcsClient *gcs.Client
	if cfg.GCSBucket != "" {
		gcsClient, err = gcs.NewClient(context.Background(), cfg.GCSBucket)
		if err != nil {
			log.Fatalf("Failed to initialize GCS client: %v", err)
		}
		log.Printf("GCS client initialized for bucket: %s", cfg.GCSBucket)
	} else {
		log.Println("No GCS bucket configured, using local storage for reviews")
	}

	// Initialize auth handler (used for both dev and prod modes)
	authHandler := auth.NewAuth(cfg, database)
	log.Println("Auth handler initialized")

	// Initialize server
	srv := server.New(cfg, database, ghClient, gcsClient)
	srv.SetAuth(authHandler)

	// Initialize poller
	p := poller.New(cfg, database, ghClient, gcsClient)

	// Wire components together
	p.SetCacheUpdateFunc(srv.UpdatePRCache)
	srv.SetPollTrigger(p.Trigger)
	srv.SetPoller(p)
	p.EventFunc = srv.BroadcastEvent
	p.StatusEventFunc = func() {
		srv.BroadcastStatusSnapshot(context.Background())
	}

	// In dev mode, set up the dev user for the poller to use
	if cfg.IsDevMode() {
		devUser, err := authHandler.GetDevUser()
		if err != nil {
			log.Fatalf("Failed to get dev user: %v", err)
		}
		p.SetDevUser(devUser)
		log.Printf("Dev user configured for poller: %s (ID: %d)", devUser.GitHubUsername, devUser.ID)
	}

	// Start poller in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Start(ctx)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
		os.Exit(0)
	}()

	// Start server (blocking)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
