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

## PRism Review Skill (Claude Code)

`prism-review` is a [Claude Code](https://claude.com/claude-code) skill that
pulls a PR's AI review from this server straight into your Claude Code
session. It fetches the structured findings, opens the cited code, and reports
an independent assessment with suggested next steps. By default it is
**read-only** — it won't edit your files unless you ask it to apply the fixes.

### One-time install

The skill ships in this repo under `skills/claude/prism-review/`. Symlink it
into your Claude Code skills directory so a `git pull` keeps it current:

```bash
ln -s "$(pwd)/skills/claude/prism-review" ~/.claude/skills/prism-review
```

(Prefer to pin a version? Copy instead: `cp -r skills/claude/prism-review ~/.claude/skills/`.)

You also need, on your `PATH`:

- The `gh` CLI, **authenticated** (`gh auth login`). The skill sends your
  GitHub token as a Bearer credential; your GitHub login must already be a
  registered user on the server (log into the dashboard once, or get seeded).
- `jq` and `curl`.

### Configure the server URL

Point the skill at your deployment by exporting `PRISM_BASE_URL` (it is
**required** — the skill errors out if unset). Ask your team for the value:

```bash
export PRISM_BASE_URL="https://prism.example.com"   # prod
# export PRISM_BASE_URL="http://localhost:8080"     # against a local server
```

Add it to your shell profile so it persists across sessions.

### Use it

In Claude Code, just ask:

> read the prism review for PR 42

> fetch the prism review for acme/example#6

The PR reference can be a bare number (resolved from the current repo's
`origin` remote), `owner/repo#N`, or a full GitHub PR URL. For the complete
reference — review status states, pinning to a commit with `PRISM_SHA`, and
failure modes — see [`skills/claude/prism-review/SKILL.md`](skills/claude/prism-review/SKILL.md).
