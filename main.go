package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"pr-review-server/config"
	"pr-review-server/db"
	"pr-review-server/github"
	"pr-review-server/poller"
	"pr-review-server/server"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Validate required config
	if cfg.GitHubToken == "" {
		log.Fatal("GITHUB_TOKEN environment variable is required")
	}
	if cfg.GitHubUsername == "" {
		log.Fatal("GITHUB_USERNAME environment variable is required")
	}

	log.Printf("Starting PR Review Server...")
	log.Printf("GitHub Username: %s", cfg.GitHubUsername)
	log.Printf("Polling Interval: %s", cfg.PollingInterval)
	log.Printf("Server Port: %s", cfg.ServerPort)
	log.Printf("Reviews Directory: %s", cfg.ReviewsDir)
	log.Printf("CBPR Path: %s", cfg.CbprPath)

	// Initialize database
	database, err := db.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()
	log.Printf("Database initialized at %s", cfg.DBPath)

	// Initialize GitHub client
	ghClient := github.NewClient(cfg.GitHubToken, cfg.GitHubUsername)
	log.Println("GitHub client initialized")

	// Initialize server first (so poller can update its cache)
	srv := server.New(cfg, database, ghClient)

	// Initialize poller
	p := poller.New(cfg, database, ghClient)

	// Wire poller to update server's cache
	p.SetCacheUpdateFunc(srv.UpdatePRCache)

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
