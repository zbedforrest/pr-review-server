#!/bin/bash
# Prod-like mode locally:
# - Uses PostgreSQL (via Cloud SQL Proxy)
# - Uses GCS for review storage
# - Uses dev mode auth (auto-login based on GITHUB_USERNAME)
#
# To test with full OAuth, also set:
#   GITHUB_APP_CLIENT_ID, GITHUB_APP_CLIENT_SECRET, etc.

set -e

# Load .env file if it exists
if [ -f .env ]; then
  source .env
fi

# Kill any existing process
pkill -f "pr-review-server" 2>/dev/null || true
sleep 0.5

# ==========================================
# REQUIRED CONFIGURATION
# ==========================================
export GITHUB_TOKEN="${GITHUB_TOKEN:?GITHUB_TOKEN is required}"
export GITHUB_USERNAME="${GITHUB_USERNAME:?GITHUB_USERNAME is required}"
export SERVER_PORT="${SERVER_PORT:-8080}"
export SKIP_DB_MIGRATIONS="${SKIP_DB_MIGRATIONS:-true}"

# PostgreSQL - try Secret Manager if not set
if [ -z "$DATABASE_URL" ]; then
    echo "Fetching DATABASE_URL from Secret Manager..."
    if DATABASE_URL=$(gcloud secrets versions access latest --secret="pr-review-db-url" --project="${GOOGLE_PROJECT_ID:?GOOGLE_PROJECT_ID is required}" 2>/dev/null); then
        export DATABASE_URL
        echo "Fetched DATABASE_URL from Secret Manager"
    else
        echo "Warning: Could not fetch DATABASE_URL from Secret Manager"
    fi
fi
export DATABASE_URL="${DATABASE_URL:?DATABASE_URL is required}"

# GCS bucket for review storage
export GCS_BUCKET="${GCS_BUCKET:?GCS_BUCKET is required}"

# ==========================================
# CLOUD SQL PROXY
# ==========================================
PROXY_PID=""
cleanup() {
    echo "Cleaning up..."
    if [ -n "$PROXY_PID" ]; then
        echo "Stopping Cloud SQL Proxy (PID: $PROXY_PID)..."
        kill "$PROXY_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Kill any existing process on port 5432 (stale proxy, local postgres, etc.)
if lsof -i :5432 >/dev/null 2>&1; then
    echo "Killing existing process on port 5432..."
    lsof -ti :5432 | xargs kill -9 2>/dev/null || true
    sleep 1
fi

echo "Starting Cloud SQL Proxy..."
if [ ! -f ./cloud-sql-proxy ]; then
    echo "Error: ./cloud-sql-proxy binary not found"
    echo "Download from: https://cloud.google.com/sql/docs/postgres/sql-proxy"
    exit 1
fi

./cloud-sql-proxy "${CLOUD_SQL_INSTANCE:?CLOUD_SQL_INSTANCE is required}" > cloud-sql-proxy.log 2>&1 &
PROXY_PID=$!

echo "Waiting for proxy..."
for i in {1..30}; do
    if grep -q "The proxy has started successfully" cloud-sql-proxy.log 2>/dev/null; then
        echo "Cloud SQL Proxy started"
        break
    fi
    if ! ps -p $PROXY_PID > /dev/null 2>&1; then
        echo "Cloud SQL Proxy failed. Check cloud-sql-proxy.log"
        exit 1
    fi
    sleep 1
done

# ==========================================
# BUILD AND RUN
# ==========================================
echo ""
echo "Building frontend..."
cd frontend && npm run build && cd ..

echo ""
echo "Building backend..."
go mod download
go build -o pr-review-server .

echo ""
echo "Starting server (prod-like mode)..."
echo "  Database: PostgreSQL"
echo "  Storage:  GCS ($GCS_BUCKET)"
echo "  Port:     $SERVER_PORT"
echo ""

./pr-review-server
