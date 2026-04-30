#!/bin/bash
# Prod-like mode locally — runs the server inside the same Docker image we
# deploy to Cloud Run, so the local dev loop matches prod as closely as
# possible.
#
# - Image: prism-local:dev (built from ./Dockerfile, includes claude CLI + git)
# - Database: PostgreSQL via Cloud SQL Proxy on the host (reached from the
#   container as host.docker.internal:5432)
# - GCS:    uses your gcloud Application Default Credentials, mounted in
# - Auth (Claude): uses ANTHROPIC_API_KEY (NOT the local `claude login` OAuth)
# - Auth (app):    dev mode auto-login based on GITHUB_USERNAME

set -e

if [ -f .env ]; then
  # shellcheck disable=SC1091
  source .env
fi

# ==========================================
# REQUIRED CONFIGURATION
# ==========================================
: "${GITHUB_TOKEN:?GITHUB_TOKEN is required}"
: "${GITHUB_USERNAME:?GITHUB_USERNAME is required}"
: "${GCS_BUCKET:?GCS_BUCKET is required}"
: "${CLOUD_SQL_INSTANCE:?CLOUD_SQL_INSTANCE is required}"
: "${GOOGLE_PROJECT_ID:?GOOGLE_PROJECT_ID is required}"
: "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY is required (export it in your shell)}"
: "${GEMINI_API_KEY:?GEMINI_API_KEY is required (the agent stage runs after a Gemini pass)}"

export SERVER_PORT="${SERVER_PORT:-8080}"
export SKIP_DB_MIGRATIONS="${SKIP_DB_MIGRATIONS:-true}"
export AGENTIC_REVIEWS="${AGENTIC_REVIEWS:-false}"
# Inside the container, route the agent's data dirs at fixed paths regardless
# of what's in .env — those .env defaults are for the bare-host dev path.
AGENT_CLONE_ROOT_DIR_CONTAINER="/app/data/agent-clones"
AGENT_LOGS_DIR_CONTAINER="/app/data/agent-logs"
export AGENT_WALL_CLOCK_SEC="${AGENT_WALL_CLOCK_SEC:-180}"
export AGENT_MAX_TURNS="${AGENT_MAX_TURNS:-40}"

# Resolve DATABASE_URL via Secret Manager if not already set
if [ -z "$DATABASE_URL" ]; then
    echo "Fetching DATABASE_URL from Secret Manager..."
    if DATABASE_URL=$(gcloud secrets versions access latest --secret="pr-review-db-url" --project="$GOOGLE_PROJECT_ID" 2>/dev/null); then
        export DATABASE_URL
        echo "Fetched DATABASE_URL from Secret Manager"
    else
        echo "Warning: Could not fetch DATABASE_URL from Secret Manager"
    fi
fi
: "${DATABASE_URL:?DATABASE_URL is required}"

# The proxy listens on the host at 127.0.0.1:5432; from inside the container,
# rewrite the host portion to host.docker.internal so the connection lands.
DATABASE_URL_FOR_CONTAINER=$(printf '%s' "$DATABASE_URL" | sed -E 's#@(127\.0\.0\.1|localhost):#@host.docker.internal:#')

# Application Default Credentials path on the host (gcloud auth ADC default)
ADC_HOST_DIR="${HOME}/.config/gcloud"
if [ ! -f "${ADC_HOST_DIR}/application_default_credentials.json" ]; then
    echo "ERROR: ${ADC_HOST_DIR}/application_default_credentials.json not found."
    echo "Run: gcloud auth application-default login"
    exit 1
fi

# ==========================================
# CLOUD SQL PROXY (host)
# ==========================================
PROXY_PID=""
CONTAINER_NAME="prism-local"
cleanup() {
    echo ""
    echo "Cleaning up..."
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    if [ -n "$PROXY_PID" ]; then
        echo "Stopping Cloud SQL Proxy (PID: $PROXY_PID)..."
        kill "$PROXY_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

if lsof -i :5432 >/dev/null 2>&1; then
    echo "Killing existing process on port 5432..."
    lsof -ti :5432 | xargs kill -9 2>/dev/null || true
    sleep 1
fi

if [ ! -f ./cloud-sql-proxy ]; then
    echo "Error: ./cloud-sql-proxy binary not found"
    echo "Download from: https://cloud.google.com/sql/docs/postgres/sql-proxy"
    exit 1
fi

echo "Starting Cloud SQL Proxy..."
./cloud-sql-proxy "$CLOUD_SQL_INSTANCE" > cloud-sql-proxy.log 2>&1 &
PROXY_PID=$!

echo "Waiting for proxy..."
for _ in {1..30}; do
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
# BUILD IMAGE
# ==========================================
IMAGE_TAG="prism-local:dev"
echo ""
echo "Building Docker image $IMAGE_TAG..."
docker build -t "$IMAGE_TAG" . | tail -5

# ==========================================
# RUN CONTAINER
# ==========================================
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

mkdir -p ./data/agent-clones ./data/agent-logs ./reviews

echo ""
echo "Starting server (prod-like, in container)..."
echo "  Image:       $IMAGE_TAG"
echo "  Container:   $CONTAINER_NAME"
echo "  Database:    PostgreSQL (via host.docker.internal:5432)"
echo "  Storage:     GCS ($GCS_BUCKET)"
echo "  Claude auth: ANTHROPIC_API_KEY"
echo "  Port:        $SERVER_PORT"
echo "  Logs:        ./server.log (also streamed to this terminal)"
echo ""

docker run --rm \
  --name "$CONTAINER_NAME" \
  -p "${SERVER_PORT}:8080" \
  -e "GITHUB_TOKEN=$GITHUB_TOKEN" \
  -e "GITHUB_USERNAME=$GITHUB_USERNAME" \
  -e "DATABASE_URL=$DATABASE_URL_FOR_CONTAINER" \
  -e "GCS_BUCKET=$GCS_BUCKET" \
  -e "GOOGLE_PROJECT_ID=$GOOGLE_PROJECT_ID" \
  -e "GOOGLE_CLOUD_PROJECT=$GOOGLE_PROJECT_ID" \
  -e "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY" \
  -e "GEMINI_API_KEY=$GEMINI_API_KEY" \
  -e "AGENTIC_REVIEWS=$AGENTIC_REVIEWS" \
  -e "AGENT_CLONE_ROOT_DIR=$AGENT_CLONE_ROOT_DIR_CONTAINER" \
  -e "AGENT_LOGS_DIR=$AGENT_LOGS_DIR_CONTAINER" \
  -e "AGENT_WALL_CLOCK_SEC=$AGENT_WALL_CLOCK_SEC" \
  -e "AGENT_MAX_TURNS=$AGENT_MAX_TURNS" \
  -e "SERVER_PORT=8080" \
  -e "SKIP_DB_MIGRATIONS=$SKIP_DB_MIGRATIONS" \
  -e "GOOGLE_APPLICATION_CREDENTIALS=/home/appuser/.config/gcloud/application_default_credentials.json" \
  -v "${ADC_HOST_DIR}:/home/appuser/.config/gcloud:ro" \
  -v "$(pwd)/data:/app/data" \
  -v "$(pwd)/reviews:/app/reviews" \
  --add-host=host.docker.internal:host-gateway \
  "$IMAGE_TAG" 2>&1 | tee server.log
