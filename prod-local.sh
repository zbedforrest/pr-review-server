#!/bin/bash
# Production mode locally: Build React and run Go with embedded build

set -e

echo "Building React frontend..."
cd frontend
npm run build
cd ..

echo "Starting Go backend in PRODUCTION mode..."
export DEV_MODE=false
export GITHUB_TOKEN="${GITHUB_TOKEN:?Environment variable GITHUB_TOKEN is required}"
export GITHUB_USERNAME="${GITHUB_USERNAME:?Environment variable GITHUB_USERNAME is required}"
export SERVER_PORT="${SERVER_PORT:-7769}"

go run .
