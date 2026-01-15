# Scripts Reference

This document describes the utility scripts available in the project root.

## User Scripts

### `start.sh`
Simple startup script that loads environment variables from `.env` and starts the built server binary.

**Usage**:
```bash
./start.sh
```

**Prerequisites**: You must have already built the server (`go build -o pr-review-server`)

### `status.sh`
Checks the status of the running server and displays statistics.

**Usage**:
```bash
./status.sh
```

**Output**:
- Server process status (running/not running)
- Web interface health check
- PR statistics by status (completed/generating/pending/error)
- Currently processing PR (if any)
- Recent activity from logs

### `review-next.sh`
Intelligently prioritizes PRs to help you decide what to review next. See [PR_PRIORITIZATION.md](../PR_PRIORITIZATION.md) for detailed documentation.

**Usage**:
```bash
# Show top 3 PRs
./review-next.sh

# Show top 5 PRs
./review-next.sh --top 5

# Show all PRs with scores
./review-next.sh --show-all

# Filter by repository
./review-next.sh --repo owner/repo-name
```

## Docker Scripts

### `start-week.sh`
Monday morning workflow script that starts the Docker container for the week.

**Usage**:
```bash
./start-week.sh
```

This is equivalent to `docker-compose up -d` but with helpful status messages.

### `build-cbpr-linux.sh`
Builds cbpr for Linux (required for Docker containers).

**Usage**:
```bash
./build-cbpr-linux.sh
```

**Prerequisites**: Requires cbpr source code to be available at `~/cbpr`

**Output**: Creates `bin/cbpr-linux` which is copied into the Docker image

## Developer Scripts

### `dev.sh`
Starts the Go backend in development mode with `DEV_MODE=true`. This disables the embedded React frontend and expects you to run the React dev server separately.

**Usage**:
```bash
# Terminal 1: Start Go backend
./dev.sh

# Terminal 2: Start React dev server
cd frontend && npm run dev
```

**Benefits**:
- Hot module reloading for React
- Faster development cycle
- Better error messages

### `prod-local.sh`
Tests the full production build locally with embedded React frontend.

**Usage**:
```bash
./prod-local.sh
```

**What it does**:
1. Builds React frontend (`npm run build`)
2. Runs Go server in production mode with embedded assets
3. Starts server on configured port (default: 7769)

**Use this to**: Test the production build before creating a Docker image or deploying.

## Script Environment Variables

Most scripts use these environment variables (loaded from `.env`):

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `GITHUB_TOKEN` | Yes | - | GitHub personal access token |
| `GITHUB_USERNAME` | Yes | - | Your GitHub username |
| `SERVER_PORT` | No | 7769 | Port for web dashboard |
| `POLLING_INTERVAL` | No | 1m | How often to poll GitHub |
| `CBPR_PATH` | No | cbpr | Path to cbpr binary |

## Common Workflows

### Daily Development
```bash
# Start development servers
./dev.sh
cd frontend && npm run dev

# Make changes, test with hot reload

# Test production build
./prod-local.sh
```

### Testing Docker Build
```bash
# Build cbpr for Linux (once)
./build-cbpr-linux.sh

# Build and start container
docker-compose up --build -d

# View logs
docker-compose logs -f

# Check status
./status.sh

# Stop container
docker-compose down
```

### Monday Morning (Production Use)
```bash
# Start server for the week
./start-week.sh

# Check it's running
./status.sh

# View dashboard
open http://localhost:7769
```

### Checking What to Review
```bash
# See top priority PRs
./review-next.sh

# Check detailed stats
./status.sh
```
