# PR Review Server

A local server that automatically monitors GitHub PRs where you're a requested reviewer and generates reviews using `cbpr`.

## Features

- Polls GitHub every minute for PRs requesting your review
- Tracks commit history for each PR
- Automatically generates new reviews when commits are added
- Web dashboard to view all PRs and their reviews
- SQLite database for persistent tracking

## Prerequisites

- Go 1.24 or later
- `cbpr` command-line tool (from the cbpr directory)
- GitHub personal access token with `repo` scope

## Setup

1. Clone or navigate to this repository:
```bash
cd ~/pr-review-server
```

2. Create a `.env` file with your configuration:
```bash
cp .env.example .env
```

3. Edit `.env` and set your values:
```bash
GITHUB_TOKEN=your_github_token_here
GITHUB_USERNAME=your_github_username
```

4. Build the server:
```bash
go build -o pr-review-server
```

5. Run the server:
```bash
source .env
./pr-review-server
```

Or run directly:
```bash
GITHUB_TOKEN=xxx GITHUB_USERNAME=xxx go run main.go
```

## Configuration

Environment variables:

- `GITHUB_TOKEN` (required) - GitHub personal access token
- `GITHUB_USERNAME` (required) - Your GitHub username
- `POLLING_INTERVAL` (optional) - How often to poll, default: `1m`
- `SERVER_PORT` (optional) - HTTP server port, default: `8080`
- `CBPR_PATH` (optional) - Path to cbpr binary, default: `cbpr` (assumes in PATH)

## Usage

1. Start the server (see Setup above)
2. Open http://localhost:8080 in your browser
3. The dashboard will show all PRs requesting your review
4. Reviews are automatically generated and updated when new commits arrive
5. Click "View Review" to see the generated HTML review

## Project Structure

```
.
├── config/         # Configuration loading
├── db/             # SQLite database layer
├── github/         # GitHub API client
├── poller/         # Polling service and review generator
├── server/         # HTTP server and web UI
├── reviews/        # Generated HTML review files
├── main.go         # Application entry point
└── pr-review.db    # SQLite database (created on first run)
```

## How It Works

1. **Polling**: Every minute, the server queries GitHub for open PRs where you're a requested reviewer
2. **Commit Tracking**: For each PR, it fetches the latest commit SHA from the PR branch
3. **Review Generation**: If the commit SHA has changed (or it's a new PR), it runs:
   ```bash
   cbpr review --repo-name=<owner/repo> -n 3 -p <pr_number> --html
   ```
4. **Storage**: The HTML output is saved to `./reviews/` and tracked in the database
5. **Web UI**: The dashboard displays all tracked PRs with links to GitHub and the review HTML

## Notes

- The server runs locally and stores data in a local SQLite database
- HTML reviews are stored in the `./reviews` directory
- The poller runs immediately on startup, then every minute thereafter
- The web UI auto-refreshes every 30 seconds
