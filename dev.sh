#!/bin/bash
# Development mode: Start Go backend in dev mode

export DEV_MODE=true
export GITHUB_TOKEN="${GITHUB_TOKEN:?Environment variable GITHUB_TOKEN is required}"
export GITHUB_USERNAME="${GITHUB_USERNAME:?Environment variable GITHUB_USERNAME is required}"
export SERVER_PORT="${SERVER_PORT:-7769}"

echo "Starting Go backend in DEV mode on port $SERVER_PORT"
echo "API endpoints available at http://localhost:$SERVER_PORT/api/"
echo ""
echo "To start the React frontend:"
echo "  cd frontend && npm run dev"
echo ""

go run .
