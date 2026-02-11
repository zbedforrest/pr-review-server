# PR Review Server

Code review dashboard for GitHub pull requests.  

## Team Deployment Requirements

| Resource | Options | Notes |
|----------|---------|-------|
| **Hosting** | Any server, VM, or container platform | Dockerfile included |
| **Database** | SQLite (default) or PostgreSQL | SQLite works for small teams; PostgreSQL recommended for production |
| **GitHub App** | Required for multi-user | Provides OAuth login and org-wide PR access |
| **Gemini API** | Optional | Required for AI-generated reviews |
| **GCS Bucket** | Optional | For storing review HTML in cloud deployments |

Minimal deployment: A single VM with Docker and a GitHub App.

## Quick Start

```bash
export GITHUB_TOKEN=your_token
export GITHUB_USERNAME=your_username
export GEMINI_API_KEY=your_gemini_key  # optional, for AI reviews

go run .
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `GITHUB_TOKEN` | Yes | GitHub PAT with `repo` and `read:org` scopes |
| `GITHUB_USERNAME` | Yes | Your GitHub username |
| `GEMINI_API_KEY` | No | For AI-generated reviews |
| `SERVER_PORT` | No | Default: 8080 |
| `POLLING_INTERVAL` | No | Default: 1m |

See `.env.example` for the full list including multi-user OAuth and cloud deployment options.
