# PR Review Server

A self-hostable code review dashboard for GitHub pull requests, with an optional multi-stage AI review pipeline.

**Dashboard** — polls GitHub for PRs assigned to you (or your org), shows CI status, draft/ready state, and merged/closed indicators, with filtering and search mirrored into the URL for shareable views. Rows can be hidden into a collapsed section, and reviews you request by pasting any PR URL land in a "Requested by Me" section.

**AI reviews** — a sampled first pass generates review comments from the diff and surrounding context in a single prompt (no tools), a classification stage scores those comments, and optionally a Claude Code or Codex/OpenRouter agent stage clones the PR, verifies and reconciles the comments against the real code, and produces the final report. The first pass runs on Gemini, Claude, or an OpenRouter model; the agent stage runs on Claude Code or Codex. Reports are rendered as HTML with per-comment deep links (`#comment-N`), J/K keyboard navigation, and a Markdown export optimized for coding agents. A deterministic layer (mechanical gates + a "bug memory" of past bug patterns) can inject forced checks into the agent's review.

**Review API** — authenticated callers can inspect deployment capabilities, create an exact-commit review with per-run model and budget choices, poll it by a unique run ID, and query history. Requested/effective configuration, model usage, fallback detection, and provider-attempt telemetry are durable metadata. The legacy structured findings endpoint remains available, and a bundled Claude Code skill (`skills/claude/prism-review/`) supports both contracts.

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
| **Gemini API** | Required for AI reviews | The classification stage always runs on Gemini flash, whatever the first-pass provider |
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
| `GEMINI_API_KEY` | For AI reviews | Enables the review pipeline; required even when the first pass runs on another provider |
| `AGENTIC_REVIEWS` | No | Pipe reviews through the selected agent stage after the first pass |
| `AGENT_BACKEND` | No | `claude` (default) or `openrouter` |
| `AGENT_MODEL` | No | Backend model; OpenRouter defaults to `openai/gpt-5.6-sol` |
| `OPENROUTER_API_KEY` | OpenRouter backend | Authenticates Codex requests routed through OpenRouter |
| `REVIEW_AGENT_MODELS_*`, `REVIEW_AGENT_EFFORTS_*` | No | Per-backend allowlists for authenticated review API callers |
| `REVIEW_MAX_WALL_CLOCK_SEC`, `REVIEW_MAX_TURNS`, `REVIEW_MAX_FIRST_PASS_SAMPLES` | No | Operator-owned ceilings for per-review overrides |
| `DATABASE_URL` | No | PostgreSQL connection string; unset = SQLite at `DB_PATH` |
| `SERVER_PORT` | No | Default `8080` (docker-compose publishes it on `7769`) |
| `POLLING_INTERVAL` | No | GitHub poll cadence, default `1m` |
| `DISABLE_POLLING` | No | Run purely as an on-demand review API |

### First-pass provider

The first pass is provider-selectable, independent of the agent stage:

| Variable | Purpose |
|----------|---------|
| `FIRST_PASS_PROVIDER` | `gemini` (default), `claude`, or `openrouter` |
| `FIRST_PASS_MODEL` | Model for that provider; defaults per provider |
| `FIRST_PASS_THINKING` | `low`/`medium`/`high` thinking level (Gemini only; unset uses the provider default) |
| `FIRST_PASS_CACHE_STAGGER_SEC` | Seconds between sampled requests so later samples read the first sample's prompt cache (Claude only, default 8) |
| `REVIEW_FIRST_PASS_MODELS_{GEMINI,CLAUDE,OPENROUTER}` | Per-provider allowlists for callers selecting a model per run |

Benchmarking across model combinations put the strongest pairing at a
`gpt-5.6-sol` first pass with a `claude-fable-5-1` agent stage: different model
families on either side beat using one family for both, because the first pass
casts a wide net and the agent stage is the precision filter over it. Whatever
the first-pass provider, `GEMINI_API_KEY` is still required for the
classification stage.

See `.env.example` for the full reference, including agent tuning (`AGENT_*`), deterministic gates (`GATE_*`), bug memory, and feature flags.

To try GPT-5.6 Sol as the agent stage while retaining Gemini as the first pass:

```bash
AGENTIC_REVIEWS=true
AGENT_BACKEND=openrouter
OPENROUTER_API_KEY=sk-or-...
# AGENT_MODEL=openai/gpt-5.6-sol  # this is already the backend default
```

The OpenRouter path runs `codex exec` in a read-only sandbox with ephemeral state. Agent child environments are constructed from a backend-specific, default-deny allowlist: OpenRouter credentials never enter Claude children, and Anthropic credentials never enter OpenRouter children. OpenRouter's CLI JSONL currently does not report the serving model, so the review records the exact pinned request model as unverified; Claude fallback detection remains stream-verified.

Changing a single review does not change these deployment defaults. If both runtimes are installed and their credentials and policy allowlists are configured, a caller can select OpenRouter for one run while automatic reviews continue using Claude. Query `GET /api/v1/review-capabilities` first; it reports whether each backend is actually ready without exposing credential values.

Configuration snapshots use schema version 3. `max_turns` is explicitly backend-specific: Claude counts assistant stream events, while OpenRouter/Codex counts completed non-reasoning work items and excludes the terminal answer. Capabilities and every durable run expose `turn_budget_unit` and `turn_budget_version`, so clients must not compare raw turn counts across backends as though they represented the same event.

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

- `GET /api/v1/review-capabilities` — defaults, backend readiness, model/effort allowlists, turn-budget semantics, and override ceilings
- `POST /api/v1/review-runs` — create an exact-target run with optional model, effort, wall-clock, turn, sample, agent, first-pass provider/model, and required-check overrides
- `GET /api/v1/review-runs/{run_id}` — exact status, immutable configuration snapshot, result, models, per-stage timings, and provider-stage attempts
- `GET /api/v1/review-runs?owner=...&repo=...&pull_request=...` — cursor-paginated history; optional `commit_sha` and `status` filters
- `GET /api/review/{owner}/{repo}/{pr}` — legacy/latest structured review JSON (`?format=html` / `?format=md`, `?sha=`, or `?sha=<sha>&run_id=<id>`)
- `POST /api/prs/generate-review` — backward-compatible review creation for callers that do not need customization
- `GET /api/status` — health check

Create a customized exact-head run with the bundled client:

```bash
export PRISM_BASE_URL=https://prism.example.com

skills/claude/prism-review/scripts/prism.sh capabilities
created=$(skills/claude/prism-review/scripts/prism.sh create acme/widgets#42 \
  --backend openrouter \
  --model openai/gpt-5.6-sol \
  --effort high \
  --wall-clock-seconds 900 \
  --max-turns 120)
skills/claude/prism-review/scripts/prism.sh wait "$(printf '%s' "$created" | jq -r .run_id)"
```

The client resolves the full current GitHub HEAD and sends it as
`expected_head_sha`; the API rejects a moved target with `409`. Creation
accepts an `Idempotency-Key`, returns `202` with `Location` and `Retry-After`,
and persists both requested and effective configuration before execution.
Separate executions of the same PR commit are distinguished by opaque unique
`run_id` values. Successful artifacts are immutable run-scoped objects, while
the PR row remains only the latest published projection.

## Auxiliary tools

- `go run ./cmd/gatecheck <worktree-dir> <base-branch>` — offline report of what the deterministic layer (gates + bug memory) would contribute for a diff; no LLM calls or API keys needed
- `skills/claude/prism-review/` — Claude Code skill for fetching and acting on reviews; install by copying it to `~/.claude/skills/prism-review/`

## Development

```bash
go test -race ./...                                        # backend tests
golangci-lint run --timeout=5m                             # lint
cd frontend && npm run lint && npm run type-check && npm test
```

For frontend work, `npm run dev` in `frontend/` starts a Vite dev server on port 3000 that proxies API calls to the Go server (`BACKEND_URL`, default `http://localhost:7769`).
