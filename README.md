# PR Review Server

An automated local server that monitors GitHub PRs where you're a requested reviewer, automatically generates reviews using [cbpr](https://github.com/google/cbpr), and provides a web dashboard to view them.

## Features

- **Automated Monitoring**: Polls GitHub every minute for PRs requesting your review
- **Automatic Review Generation**: Generates comprehensive reviews using cbpr when new commits are pushed
- **Track Your Own PRs**: Monitors PRs you've created and their review status
- **Web Dashboard**: Clean interface to view all PRs and their generated reviews
- **Smart Prioritization**: Includes a CLI tool to intelligently prioritize which PRs to review next
- **Self-Healing**: Automatically recovers from errors and handles stale reviews
- **Persistent Storage**: SQLite database tracks PR history and review status

## Prerequisites

- **For Docker Installation (Recommended)**:
  - Docker Desktop
  - `cbpr` binary for Linux (optional but recommended - see setup instructions)
  - GitHub personal access token with `repo` scope

- **For Manual Installation**:
  - Go 1.24 or later
  - Node.js 18+ and npm
  - `cbpr` command-line tool (optional but recommended)
  - GitHub personal access token with `repo` scope

**Note**: The server will start and run without `cbpr`, but AI review generation will fail gracefully. PRs will appear in the dashboard with "Error" status if cbpr is not available.

## Quick Start

### Installation Option 1: Docker (Recommended)

Docker installation is recommended for most users as it handles all dependencies automatically and runs reliably in the background.

1. **Clone the repository**:
   ```bash
   git clone <repository-url>
   cd pr-review-server
   ```

2. **Set up environment variables**:
   ```bash
   cp .env.example .env
   ```

   Edit `.env` and add your GitHub credentials:
   ```bash
   GITHUB_TOKEN=ghp_your_token_here
   GITHUB_USERNAME=your_github_username
   ```

3. **Build cbpr for Linux** (required for Docker):
   ```bash
   ./build-cbpr-linux.sh
   ```

   Note: This requires access to cbpr source code. If you don't have it, the server will run but won't generate reviews.

4. **Build and start the server**:
   ```bash
   docker-compose up --build -d
   ```

5. **Access the dashboard**:
   Open http://localhost:7769 in your browser.

For detailed Docker usage, see [docs/DOCKER-SETUP.md](./docs/DOCKER-SETUP.md).

### Installation Option 2: Manual Installation

Use manual installation if you're developing the tool or prefer not to use Docker.

1. **Clone the repository**:
   ```bash
   git clone <repository-url>
   cd pr-review-server
   ```

2. **Set up environment variables**:
   ```bash
   cp .env.example .env
   ```

   Edit `.env` and add your GitHub credentials:
   ```bash
   GITHUB_TOKEN=ghp_your_token_here
   GITHUB_USERNAME=your_github_username
   ```

3. **Build the frontend**:
   ```bash
   cd frontend
   npm install
   npm run build
   cd ..
   ```

4. **Build the server**:
   ```bash
   go build -o pr-review-server
   ```

5. **Run the server**:
   ```bash
   source .env
   ./pr-review-server
   ```

   Or run directly without building:
   ```bash
   GITHUB_TOKEN=xxx GITHUB_USERNAME=xxx go run main.go
   ```

6. **Access the dashboard**:
   Open http://localhost:8080 in your browser (or your configured SERVER_PORT).

**Important**: If you modify frontend code, you must rebuild it (step 3) before rebuilding the Go server.

## Usage

### Web Dashboard

The dashboard displays:
- **Review PRs**: PRs requesting your review with status badges
- **My PRs**: PRs you've created and their review status
- **CI Status**: Build status for each PR
- **Priority Indicators**: Visual cues for which PRs need attention

Features:
- Auto-refreshes every 30 seconds
- Click "View Review" to see generated cbpr analysis
- Click PR titles to open on GitHub
- Status indicators show review progress (pending/generating/completed/error)

### PR Prioritization Tool

Use the `review-next.sh` script to intelligently prioritize which PRs to review:

```bash
# Show top 3 PRs to review
./review-next.sh

# Show top 5 PRs
./review-next.sh --top 5

# Show all PRs with scores
./review-next.sh --show-all

# Filter to specific repository
./review-next.sh --repo owner/repo-name
```

The prioritization algorithm considers:
- PR age (older PRs get higher priority)
- Approval gap (many reviews but no approvals = needs attention)
- Size vs attention (large PRs with few reviews)
- Explicit review requests
- Your previous review activity

For details on the prioritization algorithm, see [docs/PR_PRIORITIZATION.md](./docs/PR_PRIORITIZATION.md).

### Utility Scripts

- **Check status**: `./status.sh` - Shows server status and PR statistics
- **Simple startup**: `./start.sh` - Starts the server with environment loaded from `.env`

## Configuration

Create a `.env` file from the example:

```bash
cp .env.example .env
```

### Required Environment Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `GITHUB_TOKEN` | GitHub personal access token with `repo` scope | `ghp_xxxxxxxxxxxx` |
| `GITHUB_USERNAME` | Your GitHub username | `yourusername` |

Get a GitHub token at: https://github.com/settings/tokens

### Optional Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `POLLING_INTERVAL` | `1m` | How often to check for PR updates (e.g., `30s`, `1m`, `5m`) |
| `SERVER_PORT` | `8080` | Port for the web dashboard |
| `CBPR_PATH` | `cbpr` | Path to cbpr binary (if not in PATH) |
| `DEV_MODE` | `false` | Enable development mode (for contributors) |

## How It Works

1. **Polling**: Every minute (configurable), the server queries GitHub for:
   - Open PRs where you're a requested reviewer
   - Open PRs you've created

2. **Change Detection**: For each PR, it tracks:
   - Latest commit SHA
   - Review status
   - CI status
   - Approval count

3. **Review Generation**: When changes are detected:
   - Runs cbpr to generate comprehensive code review
   - Saves HTML output to `./reviews/` directory
   - Updates database with completion status
   - **Graceful Degradation**: If cbpr is not available, PRs are marked as "error" and the dashboard continues to function

4. **Self-Healing**:
   - Resets stale "generating" PRs after 2 minutes
   - Retries failed reviews after 5 minutes (including those without cbpr)
   - Removes closed/merged PRs automatically
   - Detects outdated reviews and regenerates when new commits arrive

5. **Dashboard**: Serves all tracked PRs with:
   - Real-time status updates
   - Links to GitHub and generated reviews
   - Priority indicators
   - Review statistics

## Project Structure

```
.
├── config/              # Configuration loading
├── db/                  # SQLite database layer
├── github/              # GitHub API client
├── poller/              # Polling service and review generator
├── prioritization/      # PR prioritization logic
├── server/              # HTTP server and web UI
├── frontend/            # React dashboard
│   ├── src/
│   └── package.json
├── reviews/             # Generated HTML review files (created at runtime)
├── main.go              # Application entry point
├── Dockerfile           # Docker image definition
├── docker-compose.yml   # Docker Compose configuration
├── Makefile             # Docker management commands
└── review-next.sh       # PR prioritization CLI tool
```

## Additional Documentation

- **[docs/DOCKER-SETUP.md](./docs/DOCKER-SETUP.md)**: Detailed Docker usage guide
  - Monday morning workflow
  - Daily operations
  - Troubleshooting
  - Advanced auto-startup setup

- **[docs/PR_PRIORITIZATION.md](./docs/PR_PRIORITIZATION.md)**: Prioritization algorithm details
  - Scoring methodology
  - Usage examples
  - Configuration options

- **[docs/AUDIO_SETUP.md](./docs/AUDIO_SETUP.md)**: Optional voice notifications (Docker on macOS)
  - PulseAudio setup
  - Text-to-speech configuration

- **[docs/SCRIPTS.md](./docs/SCRIPTS.md)**: Reference for all utility scripts
  - User scripts (start.sh, status.sh, review-next.sh)
  - Docker scripts (start-week.sh, build-cbpr-linux.sh)
  - Developer scripts (dev.sh, prod-local.sh)

- **[CONTRIBUTING.md](./CONTRIBUTING.md)**: Developer guide
  - Development setup
  - Code style guidelines
  - Testing procedures
  - Contribution workflow

## Development

For contributors:

1. **Development mode** (Go backend only):
   ```bash
   ./dev.sh
   ```
   Then separately start the React dev server:
   ```bash
   cd frontend && npm run dev
   ```

2. **Production mode locally** (tests the full embedded build):
   ```bash
   ./prod-local.sh
   ```

## Troubleshooting

### "No matching files found" error when building

**Error**: `server/server.go:22:12: pattern dist/*: no matching files found`

**Solution**: Build the frontend first:
```bash
cd frontend
npm install
npm run build
cd ..
go build -o pr-review-server
```

### Server won't start (Docker)

```bash
# Check Docker is running
docker info

# Check logs for errors
docker-compose logs -f

# Try rebuilding
docker-compose up --build -d
```

### Server won't start (Manual)

```bash
# Check environment variables are set
echo $GITHUB_TOKEN
echo $GITHUB_USERNAME

# Check cbpr is accessible
which cbpr

# Check logs
tail -f server.log
```

### Reviews not generating

**Important**: The server will start and run even if cbpr is not installed. PRs will show "Error" status in the dashboard when cbpr fails to generate reviews.

1. **Check if cbpr is installed**:
   ```bash
   cbpr --version
   ```

   If cbpr is not installed, reviews cannot be generated but the dashboard will still work.

2. **Check server startup logs** for cbpr warnings:
   ```bash
   # Look for warnings like:
   # ⚠️  WARNING: cbpr not found at 'cbpr' or in PATH
   tail -20 server.log
   ```

3. **Check cbpr has necessary authentication configured**

4. **Review server logs for cbpr errors**:
   ```bash
   tail -f server.log | grep cbpr
   ```

5. **PRs in error state**:
   - PRs marked as "Error" in the dashboard had review generation failures
   - The system will automatically retry failed PRs after 5 minutes
   - You can also manually trigger a retry by deleting the PR from the system (it will be re-added on next poll)

### Dashboard not loading

1. Check the server is running:
   ```bash
   # Docker
   docker-compose ps

   # Manual
   ./status.sh
   ```

2. Verify the port isn't in use:
   ```bash
   lsof -i :8080  # or your SERVER_PORT
   ```

3. Check for errors in browser console (F12)

## Database Schema

SQLite database schema (for reference):

```sql
CREATE TABLE prs (
    id INTEGER PRIMARY KEY,
    repo_owner TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    title TEXT,
    author TEXT,
    last_commit_sha TEXT,
    last_reviewed_at TIMESTAMP,
    review_html_path TEXT,
    status TEXT DEFAULT 'pending',
    generating_since TIMESTAMP,
    is_mine BOOLEAN DEFAULT 0,
    draft BOOLEAN DEFAULT 0,
    approval_count INTEGER DEFAULT 0,
    my_review_status TEXT,
    ci_status TEXT,
    UNIQUE(repo_owner, repo_name, pr_number)
);
```

## License

[Add your license here]

## Contributing

[Add contribution guidelines if applicable]
