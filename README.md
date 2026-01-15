# PR Review Server

An automated local server that monitors GitHub PRs where you're a requested reviewer, provides a web dashboard to track them, and optionally generates automated reviews using [cbpr](https://github.com/google/cbpr).

## Features

- **Automated Monitoring**: Polls GitHub every minute for PRs requesting your review
- **Web Dashboard**: Clean interface to view all PRs and track their status
- **Track Your Own PRs**: Monitors PRs you've created and their review status
- **Smart Prioritization**: Includes a CLI tool to intelligently prioritize which PRs to review next
- **Optional AI Reviews**: Generates comprehensive reviews using cbpr when available (requires Gemini API key)
- **Self-Healing**: Automatically recovers from errors and handles stale reviews
- **Persistent Storage**: SQLite database tracks PR history and review status
- **Graceful Degradation**: Works fully without cbpr - just won't generate AI reviews

## Prerequisites

### Required

- **For Docker Installation (Recommended)**:
  - Docker Desktop
  - GitHub personal access token with `repo` scope

- **For Manual Installation**:
  - Go 1.24 or later
  - Node.js 18+ and npm
  - GitHub personal access token with `repo` scope

### Optional (For AI Review Generation)

To enable automated review generation with cbpr, you'll also need:
- `cbpr` binary (Linux binary for Docker, or native binary for manual installation)
- Gemini API key from Google AI Studio

**Note**: The server works perfectly without cbpr - you'll still get PR tracking, prioritization, and the dashboard. AI review generation simply won't be available.

## Quick Start

### Installing Without cbpr or Gemini API Key

If you don't have cbpr or a Gemini API key, you can still use the server for PR tracking and prioritization:

**Docker Installation (without AI reviews)**:
```bash
# Clone and set up environment
git clone <repository-url>
cd pr-review-server
cp .env.example .env

# Edit .env with just GitHub credentials (skip GEMINI_API_KEY)
# GITHUB_TOKEN=ghp_your_token_here
# GITHUB_USERNAME=your_github_username

# Build and start (skip the cbpr build step)
docker-compose up --build -d
```

**Manual Installation (without AI reviews)**:
```bash
# Clone and set up environment
git clone <repository-url>
cd pr-review-server
cp .env.example .env

# Edit .env with just GitHub credentials

# Build frontend
cd frontend && npm install && npm run build && cd ..

# Build and run server
go build -o pr-review-server
./pr-review-server
```

The dashboard will work normally. PRs will show in the interface, but without "View Review" links. You can still use all tracking and prioritization features.

---

### Installation Option 1: Docker (Recommended with AI Reviews)

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

   Edit `.env` and add your credentials:
   ```bash
   GITHUB_TOKEN=ghp_your_token_here
   GITHUB_USERNAME=your_github_username
   GEMINI_API_KEY=your_gemini_key_here  # Optional - only needed for AI reviews
   ```

3. **Build cbpr for Linux** (optional - only for AI reviews):
   ```bash
   ./build-cbpr-linux.sh
   ```

   **Skip this step** if you don't have cbpr or don't want AI reviews. The server will work fine without it.

4. **Build and start the server**:
   ```bash
   docker-compose up --build -d
   ```

5. **Access the dashboard**:
   Open http://localhost:7769 in your browser.

For detailed Docker usage, see [docs/DOCKER-SETUP.md](./docs/DOCKER-SETUP.md).

### Installation Option 2: Manual Installation (with AI Reviews)

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

   Edit `.env` and add your credentials:
   ```bash
   GITHUB_TOKEN=ghp_your_token_here
   GITHUB_USERNAME=your_github_username
   GEMINI_API_KEY=your_gemini_key_here  # Optional - only needed for AI reviews
   ```

3. **Install cbpr** (optional - only for AI reviews):
   - Install cbpr following its documentation
   - Ensure `cbpr` is in your PATH or set `CBPR_PATH` in `.env`
   - **Skip this step** if you don't want AI reviews

4. **Build the frontend**:
   ```bash
   cd frontend
   npm install
   npm run build
   cd ..
   ```

5. **Build the server**:
   ```bash
   go build -o pr-review-server
   ```

6. **Run the server**:
   ```bash
   source .env
   ./pr-review-server
   ```

   Or run directly without building:
   ```bash
   GITHUB_TOKEN=xxx GITHUB_USERNAME=xxx go run main.go
   ```

7. **Access the dashboard**:
   Open http://localhost:8080 in your browser (or your configured SERVER_PORT).

**Important**: If you modify frontend code, you must rebuild it (step 4) before rebuilding the Go server.

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
| `GEMINI_API_KEY` | (none) | Gemini API key for cbpr AI reviews - **only needed if using cbpr** |
| `CBPR_PATH` | `cbpr` | Path to cbpr binary (if not in PATH) - **only needed if using cbpr** |
| `POLLING_INTERVAL` | `1m` | How often to check for PR updates (e.g., `30s`, `1m`, `5m`) |
| `SERVER_PORT` | `8080` | Port for the web dashboard |
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

3. **Review Generation** (optional - only if cbpr is configured):
   - Runs cbpr to generate comprehensive code review
   - Saves HTML output to `./reviews/` directory
   - Updates database with completion status
   - **Graceful Degradation**: If cbpr is not available, reviews won't be generated but all other features work normally

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

### AI Reviews not generating (cbpr)

**Important**: The server works perfectly without cbpr - you'll still get PR tracking, prioritization, and the dashboard. This section only applies if you're trying to use AI review generation.

1. **Check if cbpr is installed and configured**:
   ```bash
   cbpr --version
   ```

   If cbpr is not installed, AI reviews cannot be generated. Install cbpr and set your `GEMINI_API_KEY` to enable this feature.

2. **Check if GEMINI_API_KEY is set**:
   ```bash
   echo $GEMINI_API_KEY
   ```

   cbpr requires a Gemini API key. Get one from [Google AI Studio](https://aistudio.google.com/app/apikey).

3. **Check cbpr has necessary authentication configured**
   - Ensure cbpr can access your Gemini API key
   - Test cbpr manually: `cbpr review --help`

4. **Review server logs for cbpr errors**:
   ```bash
   tail -f server.log | grep cbpr
   ```

5. **PRs in error state**:
   - If cbpr fails, PRs will show "Error" status in the dashboard
   - The system will automatically retry failed PRs after 5 minutes
   - Without cbpr, PRs will remain in "pending" or "error" state (this is normal)

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
