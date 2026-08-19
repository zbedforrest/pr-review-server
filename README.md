# PR Review Server

A self-hostable code review dashboard for GitHub pull requests, with an optional two-stage AI review pipeline.

**Dashboard** — polls GitHub for PRs assigned to you (or your org), shows CI status, draft/ready state, and merged/closed indicators, with filtering and search mirrored into the URL for shareable views. Rows can be hidden into a collapsed section, and reviews you request by pasting any PR URL land in a "Requested by Me" section.

**AI reviews** — a Gemini pass generates review comments; optionally a Claude Code or Codex/OpenRouter agent stage clones the PR, verifies and refines those comments against the real code, and produces the final report. Reports are rendered as HTML with per-comment deep links (`#comment-N`), J/K keyboard navigation, and a Markdown export optimized for coding agents. A deterministic layer (mechanical gates + a "bug memory" of past bug patterns) can inject forced checks into the agent's review.

**Review API** — reviews are also exposed as structured JSON (findings with file/line, diff hunks, and source context) at `/api/review/{owner}/{repo}/{pr}`, and can be generated on demand for any PR — including merged and closed ones — via `POST /api/prs/generate-review`. A bundled Claude Code skill (`skills/claude/prism-review/`) consumes this API.

## Prerequisites

- **Go 1.25+** with CGO enabled (the SQLite driver requires it)
- **Node 20+** and npm (the dashboard is built with Vite and embedded into the Go binary)
- **git** on `PATH`
- Optional: **Docker** (recommended run path), or the **`claude` / `codex` CLI** required by your selected agent backend

## Quick Start (Docker)

```bash
cp .env.example .env   # set GITHUB_TOKEN and GITHUB_USERNAME at minimum
make start             # docker-compose up -d
```

Dashboard: <http://localhost:7769>. Use `make logs`, `make status`, `make restart`, `make rebuild`, and `make stop` to manage it.

## Building from source

The frontend must be built first — `server/dist` is embedded into the binary via `go:embed` and is not checked in, so a bare `go build` on a fresh clone fails without this step:

```bash
cd frontend && npm ci && npm run build   # outputs to ../server/dist
cd .. && go build -o pr-review-server .

export GITHUB_TOKEN=your_token       # PAT with repo + read:org scopes
export GITHUB_USERNAME=your_username
export GEMINI_API_KEY=your_key       # optional, enables AI reviews

./pr-review-server                   # dashboard at http://localhost:8080
```

## Deployment options

| Resource | Options | Notes |
|----------|---------|-------|
| **Hosting** | Any server, VM, or container platform | Dockerfile and docker-compose included |
| **Database** | SQLite (default) or PostgreSQL | Set `DATABASE_URL` for PostgreSQL; SQLite works for small teams |
| **GitHub auth** | PAT (single-user dev mode) or GitHub App | GitHub App provides OAuth login and org-wide PR access for multi-user deployments |
| **Gemini API** | Optional | Required for AI-generated reviews |
| **Agent runtime** | Optional | Claude Code + Anthropic auth, or Codex + an OpenRouter key, for `AGENTIC_REVIEWS=true` |
| **GCS bucket** | Optional | Stores review artifacts (HTML/Markdown + JSON findings) in cloud deployments; defaults to local disk |

Minimal deployment: a single VM with Docker and a GitHub App.

### Auth modes

- **Dev mode** (default when no GitHub App is configured): authenticates to GitHub with `GITHUB_TOKEN` and auto-logs-in `GITHUB_USERNAME`. Good for single-user/local use.
- **Multi-user mode**: set the `GITHUB_APP_*` variables, `OAUTH_CALLBACK_URL`, and `SESSION_SECRET`. Users log in via GitHub OAuth; org membership (`GITHUB_ORG_NAME`) gates access. The OAuth callback path is `/auth/github/callback`.

## Environment variables

The most common ones:

| Variable | Required | Description |
|----------|----------|-------------|
| `GITHUB_TOKEN` | Dev mode | GitHub PAT with `repo` and `read:org` scopes |
| `GITHUB_USERNAME` | Dev mode | Auto-login user and poller identity |
| `GITHUB_APP_*`, `OAUTH_CALLBACK_URL`, `SESSION_SECRET` | Multi-user mode | GitHub App auth (see `.env.example`) |
| `GEMINI_API_KEY` | No | Enables AI-generated reviews |
| `AGENTIC_REVIEWS` | No | Pipe reviews through the selected agent stage after Gemini |
| `AGENT_BACKEND` | No | `claude` (default) or `openrouter` |
| `AGENT_MODEL` | No | Backend model; OpenRouter defaults to `openai/gpt-5.6-sol` |
| `OPENROUTER_API_KEY` | OpenRouter backend | Authenticates Codex requests routed through OpenRouter |
| `DATABASE_URL` | No | PostgreSQL connection string; unset = SQLite at `DB_PATH` |
| `SERVER_PORT` | No | Default `8080` (docker-compose publishes it on `7769`) |
| `POLLING_INTERVAL` | No | GitHub poll cadence, default `1m` |
| `DISABLE_POLLING` | No | Run purely as an on-demand review API |

See `.env.example` for the full reference, including agent tuning (`AGENT_*`), deterministic gates (`GATE_*`), bug memory, and feature flags.

To try GPT-5.6 Sol through OpenRouter while retaining Gemini as the first pass:

```bash
AGENTIC_REVIEWS=true
AGENT_BACKEND=openrouter
OPENROUTER_API_KEY=sk-or-...
# AGENT_MODEL=openai/gpt-5.6-sol  # this is already the backend default
```

The OpenRouter path runs `codex exec` in a read-only sandbox with ephemeral state and filters server credentials out of model-invoked shell commands. Its CLI JSONL currently does not report the serving model, so the review records the exact pinned request model; Claude fallback detection remains stream-verified.

Review-config schema v2 makes the `max_turns` unit backend-specific and records it in every run. Claude counts assistant stream events; OpenRouter/Codex counts completed non-reasoning work items such as tool commands and file changes, excluding the terminal answer. Their defaults and unset ceilings are 40 and 200 respectively; when a request switches backend without specifying `max_turns`, Prism derives the selected backend's default, bounded by its advertised ceiling. A separate generous consecutive budget-exempt-item ceiling still terminates malformed message-only or reasoning-only streams without penalizing productive work. Existing OpenRouter deployments upgrading from schema v1 should review explicit `AGENT_MAX_TURNS` and `REVIEW_MAX_TURNS` values because the new unit is intentionally more granular.

### Recommended configuration

Gates and feature flags all default to off, and the defaults are deliberately conservative. For the strongest reviews, enable the deterministic layer and force the agent to answer it:

```bash
GATE_WIRING=true
GATE_EFFECT_CLEANUP=true
GATE_MIGRATION_RESIDUE=true
GATE_TOPIC_BE_DIR=backend/           # adjust both to your repo layout
GATE_TOPIC_FE_DIR=frontend/
GATE_TASK_PRODUCER_GLOB=**/tasks.py
REQUIRED_CHECKS=true
BUG_MEMORY_OBJECT=bug-memory/bug-memory.json
```

The gates contribute mechanical findings from the diff with no LLM involved, and `REQUIRED_CHECKS=true` is what gives them teeth: each fired gate and bug-memory hit becomes a check the agent must explicitly answer with a VIOLATED / SAFE / NOT-APPLICABLE verdict instead of silently ignoring. The task gate needs only the producer glob; `GATE_TASK_CONSUMER_GLOB` is an optional narrowing. Bug memory pays off once you have a distilled library of past bugs to point it at; start one early.

The remaining feature flags (`SURFACE_ALERTS`, `CARRY_FORWARD_FINDINGS`, `FINDING_OUTCOMES_ENABLED`, `REVIEW_HISTORY_ARCHIVE`) are dashboard and workflow conveniences. They are independent of review quality; enable them as needed.

## API

- `GET /api/review/{owner}/{repo}/{pr}` — structured review JSON (`?format=html` / `?format=md` for rendered output, `?sha=` to pin a commit, or `?sha=<sha>&run_id=<id>` to fetch one immutable execution)
- `POST /api/prs/generate-review` — trigger a review for any PR by reference, including merged/closed PRs
- `GET /api/status` — health check

## Auxiliary tools

- `go run ./cmd/gatecheck <worktree-dir> <base-branch>` — offline report of what the deterministic layer (gates + bug memory) would contribute for a diff; no LLM calls or API keys needed
- `go run ./cmd/seed-users` — seed user rows from GitHub (PostgreSQL only)
- `skills/claude/prism-review/` — Claude Code skill for fetching and acting on reviews; install by copying it to `~/.claude/skills/prism-review/`

## Development

```bash
go test -race ./...                                        # backend tests
golangci-lint run --timeout=5m                             # lint
cd frontend && npm run lint && npm run type-check && npm test
```

For frontend work, `npm run dev` in `frontend/` starts a Vite dev server on port 3000 that proxies API calls to the Go server (`BACKEND_URL`, default `http://localhost:7769`).
