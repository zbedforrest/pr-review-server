# PRism review client and API reference

## Client commands

```text
prism.sh capabilities
prism.sh create <pr-ref> [create options]
prism.sh get <run-id>
prism.sh wait <run-id|findings-url> [--timeout-seconds N] [--poll-seconds N]
prism.sh history <pr-ref> [--limit N] [--cursor TOKEN] [--commit-sha SHA] [--status STATUS]
prism.sh fetch <pr-ref> [--sha SHA]
```

`capabilities`, `create`, `get`, and `history` emit JSON. `wait` and `fetch`
render a compact evidence-oriented view of the review findings.

Create options map directly to the v1 request:

| Client option | Request field |
|---|---|
| `--backend` | `config.agent.backend` |
| `--model` | `config.agent.model` |
| `--effort` | `config.agent.effort` |
| `--wall-clock-seconds` | `config.agent.wall_clock_seconds` |
| `--max-turns` | `config.agent.max_turns` |
| `--first-pass-samples` | `config.first_pass.samples` |
| `--agent-enabled` | `config.agent.enabled` |
| `--required-checks` | `config.required_checks` |
| `--expected-head-sha` | `target.expected_head_sha` |
| `--idempotency-key` | `Idempotency-Key` header |

The server validates requested settings against the deployment's capability
policy. A backend switch should normally specify both `--backend` and a model
listed for that backend. `max_turns` uses the backend-specific unit and version
returned by capabilities; it is not assumed to mean the same event type across
Claude and OpenRouter.

## Examples

Use the deployment defaults:

```bash
created=$(prism.sh create acme/widgets#42)
prism.sh wait "$(printf '%s' "$created" | jq -r .run_id)"
```

Run Claude with explicit budgets:

```bash
created=$(prism.sh create acme/widgets#42 \
  --backend claude --model claude-fable-5 --effort medium \
  --wall-clock-seconds 600 --max-turns 60 --first-pass-samples 3)
prism.sh wait "$(printf '%s' "$created" | jq -r .run_id)"
```

Experiment with an enabled OpenRouter model without changing deployment
defaults:

```bash
prism.sh capabilities | jq '.backends.openrouter'
created=$(prism.sh create acme/widgets#42 \
  --backend openrouter --model openai/gpt-5.6-sol --effort high \
  --wall-clock-seconds 900 --max-turns 120)
prism.sh wait "$(printf '%s' "$created" | jq -r .run_id)"
```

Compare all runs on one commit:

```bash
prism.sh history acme/widgets#42 --commit-sha 0123456789abcdef0123456789abcdef01234567
```

## HTTP contract

- `GET /api/v1/review-capabilities` returns defaults, backend readiness,
  allowlisted models/efforts, backend turn-budget semantics, and ceilings.
- `POST /api/v1/review-runs` accepts one exact target and optional overrides.
  It returns `202`, a `Location`, and a `Retry-After` hint while queued.
- `GET /api/v1/review-runs/{run_id}` returns exact immutable configuration,
  result, models, and stage attempts. Live responses include `Retry-After`.
- `GET /api/v1/review-runs?...` lists durable runs with cursor pagination.

Every run response includes `config.requested`, `config.effective`,
configuration sources/hash/schema version, and a unique `run_id`. The detailed
endpoint also includes provider-attempt lifecycle telemetry. Use `run_id` to
distinguish two runs made against the same PR commit.

Creation uses a caller-scoped idempotency key. Reusing a key with an identical
request returns the original run; reusing it with a different request returns
`409 idempotency_key_reused`. Supply your own stable key when a caller may retry
after an ambiguous network failure.

## Status and failures

Run statuses are `queued`, `running`, `completed`, `failed`, `timed_out`, and
`cancelled`. `prism.sh wait` exits successfully only for `completed`.

Client exit codes:

- `2`: invalid command, reference, option, or local value.
- `3`: missing dependency or authentication.
- `4`: HTTP/server error; the response body is printed to stderr.
- `5`: terminal run failure, timeout, or cancellation reported by the server.
- `6`: the deployment lacks a requested v1 capability or safe fallback.
- `7`: client-side polling timeout.

The client may fall back to `POST /api/prs/generate-review` only when no review
configuration or explicit idempotency key was requested. It refuses to silently
drop custom settings or retry guarantees and requires the legacy response to
confirm the exact expected commit. `fetch` continues to use the legacy read
endpoint for backward compatibility.

## Environment

- `PRISM_BASE_URL` (required): deployment root URL.
- `PRISM_TOKEN`: bearer token override; otherwise `gh auth token` is used.
- `PRISM_SHA`: optional default commit selector for `fetch`.
