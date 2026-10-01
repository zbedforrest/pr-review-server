# Approval candidates operations

Approval candidates is disabled by default. It creates private, user-scoped recommendations and never posts reviews, approves, merges, resolves threads, or executes repository code. The [frozen specification](approval-candidates-spec.md) defines the product and policy.

## Configuration

Set these environment variables on each server replica before enabling the feature:

| Variable | Value |
| --- | --- |
| `APPROVAL_CANDIDATES_ENABLED` | `false` initially; `true` enables admission and the UI |
| `APPROVAL_CANDIDATES_PROVIDER` | `anthropic` or `openrouter` |
| `APPROVAL_CANDIDATES_MODEL` | Explicit model ID supported by that provider |
| `APPROVAL_CANDIDATES_REASONING_EFFORT` | OpenRouter reasoning effort: `minimal`, `low` (default), `medium` or `high`; `off` sends no reasoning setting. Any other value makes the feature unavailable |
| `APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS` | Optional deployment-wide UTC allocation budget, at least one target allowance (2,000,000) when set; unset means unlimited |
| `APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS` | Optional deployment-wide UTC allocation budget, at least one target allowance (160,000) when set; unset means unlimited |
| `APPROVAL_CANDIDATES_CACHE_DIR` | Private writable directory, default `data/approval-cache` |
| `APPROVAL_CANDIDATES_PROVIDER_IDENTITIES` | JSON array of verified Greptile/Copilot numeric actor IDs |
| `APPROVAL_CANDIDATES_SLOTS` | Concurrent investigations across the deployment and for one user, default 50, clamped to 1 through 100 |
| `APPROVAL_CANDIDATES_MODEL_CONCURRENCY` | In-flight model requests per process, default the slot count |
| `DB_MAX_OPEN_CONNS` | PostgreSQL connections per process, default 10; idle connections are kept at half |

The runtime uses the existing `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY` setting. A CLI subscription does not supply these credentials. Use the same configuration on all replicas. `BASE_URL` must be the browser application's origin for browser POST validation.

An identity configuration has this shape, using example IDs only:

```json
[{"provider":"greptile","actor_id":12345},{"provider":"copilot","actor_id":67890}]
```

Resolve and verify the actual GitHub numeric bot account IDs before configuration. A matching display name, login substring, or comment marker never establishes trust. Actors must also have GitHub's `Bot` type. Nonzero `app_id` constraints make this runtime unavailable because GitHub review payloads do not expose enough app identity; configure verified actor IDs instead. Missing allowlists leave those artifacts visible as generic evidence. Local PRism evidence is verified against durable completed runs and immutable sidecars.

## Runtime and storage

The investigator runs inside the Go server and uses native model HTTP APIs. Database leases limit deployment concurrency to `APPROVAL_CANDIDATES_SLOTS` investigations (default 50), all usable by one user. Two separate read-only slots handle explicit evidence revalidation. Browser navigation does not stop jobs.

The model can only list/read frozen evidence and list/read/search/diff registered git revisions. The code reader uses a separate private bare repository, no checkout, and an ephemeral credential pipe. It disables git hooks, external diff, text conversion, ambient config, credential helpers and submodule recursion. No repository commands or tests run. Temporary repositories are removed at task completion. Startup and hourly maintenance remove abandoned managed repositories using filesystem locks, including after a process is killed. Active workers and surviving Git processes retain their repository leases. Linux and macOS are supported; shared cache mounts must provide coherent flock locking across replicas. Unmarked directories from older versions are preserved: remove those only after stopping all workers running the older version.

Dedicated database tables retain scans, snapshots, assessments, leases, current projections and usage. Evidence is never published as a review artifact. Existing authenticated user inventory and live repository access are checked before returning it. Results are retained for 30 days; hourly pruning removes terminal histories and dependent records.

Every target reserves its maximum allowance before model execution. Trusted usage releases unused allowance. Missing or invalid usage retains the conservative reservation. An in-flight target keeps its original UTC allocation day across midnight and recovery; new targets allocate against the new day. These are resource allocation caps, not dollar budgets or an exact per-calendar-day billing report.

Admission snapshots provider, model and policy/runtime versions. Configuration drift blocks execution or freshness renewal. Changing credentials without changing provider/model remains possible. The provider cannot silently fall back to a different model.

## Investigation context

The server preloads every review artifact, every extracted concern, the merge-base to head diff (up to 200,000 bytes) and the code at review anchors (up to 240,000 bytes) into the first model request, under short aliases such as `E3`, `C2` and `H`. The model returns only verdicts, discovered concerns, a list of artifacts without concerns and gaps; the server derives provenance and artifact dispositions and verifies every citation. Read tools remain for code that is not shown, with at most one tool round per attempt, so most targets finish in one or two model calls. Evidence over 600,000 bytes stops the target with `evidence_limit` before any model call.

Each model call reserves 400,000 input and up to 16,000 output tokens (6,000 for a follow-up that asks only for missing entries), and one request that runs past 120 seconds stops the target on its limit; a target allows 2,000,000 input and 160,000 output tokens across recovery attempts. Server logs record each round's duration, input, output and reasoning tokens. Pinned file and diff reads are cached per repository, so preloading, paging and citation checks do not repeat git work.

## Live progress

The collapsed approval bar shows an animated prism, a ten-word-or-shorter activity line, and the number of finished PRs. The fill represents finished targets, including failures, rather than estimated time remaining. Reduced-motion preferences disable the animation.

While a scan runs, the browser polls its authenticated `/api/v1/approval-scans/{scan_id}/progress` endpoint every two seconds. This returns owner-scoped aggregate progress from the database without GitHub or model calls. Evidence lists refresh every ten seconds. Progress updates never publish PRism review documents or GitHub comments.

Workers report observed stages and tool names without exposing source text, file paths, review bodies, model reasoning, or tool arguments. An optional Gemini Flash-Lite call summarizes up to eight recent activity labels at most once every ten seconds when activity changes. It uses the existing Gemini key when available, otherwise OpenRouter, and falls back to a deterministic activity label when unavailable. Each request has a four-second timeout and a 64-token output limit; each target attempt permits at most 18 summary requests. These small presentation calls are separate from investigation token reservations and cannot change the assessment.

Set `APPROVAL_CANDIDATES_PROGRESS_SUMMARIES=false` to disable summary model calls while retaining live progress. `APPROVAL_CANDIDATES_PROGRESS_MODEL` overrides the default `gemini-2.5-flash-lite` (Gemini API) or `google/gemini-2.5-flash-lite` (OpenRouter); use the model ID format for the selected provider.

## Deterministic gates and reuse

Before reserving daily budget or opening the repository, each target is checked for an outcome the model cannot change:

- An ineligible snapshot (closed, own, outside the visible scope) finishes `excluded`.
- A full scan whose snapshot is identical to an earlier completed investigation by the same user reuses that assessment. The assessment records `origin: reused` and the source target in `reused_from`, reports zero usage and is re-decided by the current policy. The reuse index (`approval_reuse_index`) is keyed by user and a hash of the snapshot digest, provider, model, reasoning effort and versions, and is pruned with its scan. Recheck scans never reuse.
- Failing CI on the head, a standing human change request or a head provider change request finishes `needs_attention` with `origin: gate`. The summary names the blocker and says no investigation ran.

These targets make no model call and leave the daily budget untouched, so a dashboard of mostly blocked or unchanged PRs finishes in seconds. Drafts are investigated and always carry `pr_draft`, which keeps them out of candidates. A missing current review is not a gate: those targets are investigated and end `insufficient_evidence` at worst.

A non-candidate investigation result rechecks access and configuration and then finalizes without collecting evidence a second time; only candidates pay for the final re-collection. Changing the provider, model, reasoning effort, or any policy, prompt or runtime version changes the reuse key, so older assessments stop being reused. A reuse lookup failure is logged as `[APPROVAL] reuse lookup` and treated as a miss.

## Rollout and rollback

1. Merge the complete draft stack and apply the additive migrations with the feature disabled. Existing ordinary reviews continue independently.
2. Configure an explicit provider/model and small daily budgets. Confirm authenticated `GET /api/v1/approval-capabilities` reports availability without revealing credentials.
3. Enable in a pilot deployment. Start with a few PRs covering PRism, Greptile, Copilot, a changed HEAD, unresolved concerns, and missing evidence. Compare every recommendation with a human review.
4. Confirm tool use and cumulative usage in retained results, test cancellation, and confirm observations or expiration withdraw recommendations.
5. Expand only after checking false-positive recommendations and actual resource use. Do not automatically approve pilot results.

Set `APPROVAL_CANDIDATES_ENABLED=false` and restart replicas to roll back. The UI disappears, admission stops, and workers cancel remaining scans while retaining protected history. No model review is triggered by GitHub updates. Observed PR update events conservatively invalidate existing candidates, including when a broad update signal contains changes unrelated to review content.

## Validation and limits

Automated fixtures cover both native API adapters, required artifact reads, anchored concern provenance, provider identity, nested pagination failures, incomplete coverage, code citation checks, tool boundaries, cancellation, recovery, budget reservations, generation fencing, SQLite/PostgreSQL transactions, API ownership, and UI freshness. Browser fixtures exercise desktop/mobile themes and keyboard focus.

Controlled HTTP/provider fixtures validate API compatibility. They do not establish recommendation quality on a deployed repository or replace a live-model pilot before production rollout.

Historical legacy or cache-restored PRism runs without immutable full-revision sidecars remain evidence gaps even after a newer review succeeds; the runtime does not silently discard or override them. Reviews without immutable PRism sidecars, unknown reviewed revisions, incomplete pagination, missing provider identity, large evidence sets, or exhausted tool/model budgets produce gaps or failed investigations. The dashboard may lack the current user's actual reviewed SHA; it then keeps that PR in the launch pool and admission resolves eligibility using live review metadata. Safe concern dispositions require source anchors; unanchored concerns remain insufficient evidence. Collection ceilings do not guarantee that every artifact fits the smaller investigation budget. File coverage is labeled unreported unless the source supplies it. Positive CI does not assert branch-rule compliance, and code inspection does not assert tests were executed.

## GitHub rate limits and failure diagnosis

Approval work shares the server's GitHub quota. A rate-limited response (REST or GraphQL, primary or secondary) pauses target claims until GitHub's reported reset, or for one minute when GitHub gives none, capped at one hour. A target that hits the limit mid-run, including during evidence collection, stops sending requests, waits for the reset and retries once when its deadline allows, and otherwise finishes as `github_rate_limited` rather than as an access failure or an incomplete snapshot. The candidate feed reads live PR state only for candidate rows; other rows reuse PR reads for up to 30 seconds. Admission's PR reads fill the same cache, and a worker's access checks reuse a PR read for the length of one target, so each target reads its PR for access once.

Failed targets keep their cause: the stored summary carries a bounded error message and the server logs the full error with the target ID. Investigations stopped by a resource ceiling record which one (`budget_exhausted`, `conversation_limit`, `evidence_limit` or `tool_result_limit`). A final answer that is not a JSON object, or that misses concern verdicts or artifact classifications, gets one follow-up asking for exactly what is missing; validation is never relaxed and the follow-up spends the same budget. After that the server settles the rest without the model: missing verdicts become unresolved, unclassified artifacts become coverage gaps, unverifiable citations are dropped with their dispositions marked uncertain, and an answer that still fails validation ends as `invalid_assessment`. Unfinished check suites without check runs are not treated as pending CI.

Before a broad scan, validate one PR that already has a completed review at its current head. Local testing needs authentic review history (a read-only connection to deployed review runs or imported fixtures): a database with no review runs cannot produce candidates.

## Concurrency and Cloud Run

Each process runs one dispatcher that claims every free slot in a single transaction, so a 50-PR scan starts all of its targets at once. Background workers need CPU outside requests: deploy with `--no-cpu-throttling` and at least one minimum instance. HTTP `--concurrency` does not limit workers. For 50 slots, size instances at 2 vCPU and 4 GiB: each investigation holds a prompt of about 1 MB, and repository setup runs git processes (at most 16 at once per process).

Run Cloud Run in the same region as Cloud SQL. A cross-region statement costs 40 to 50 ms, and round trips dominate the gated writes. Keep instances times `DB_MAX_OPEN_CONNS` below the Cloud SQL connection limit with headroom for maintenance and other clients.

GitHub's secondary limits allow about 100 concurrent requests and about 900 REST points per minute. A 50-PR scan issues about 500 collection requests (one per evidence endpoint per PR, reviewer requests read once), plus re-collection for candidates; the shared rate-limit pause handles bursts. Model requests share a process-wide limiter: a 429, 502, 503 or 529 (or a 200 body carrying a 429, 502 or 503 error) is retried with `Retry-After` or jittered exponential backoff, up to six attempts, and a 429 pauses every target together. A request is not retried when its wait plus 45 seconds would pass the target deadline; the target then fails with the provider status.

Daily token caps, when set, must cover the slot count times the target allowance (2,000,000 input and 160,000 output tokens) for full parallelism; otherwise later targets fail with `budget_exhausted` until running ones release unused allowance.
