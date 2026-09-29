# Approval candidates operations

Approval candidates is disabled by default. It creates private, user-scoped recommendations and never posts reviews, approves, merges, resolves threads, or executes repository code. The [frozen specification](approval-candidates-spec.md) defines the product and policy.

## Configuration

Set these environment variables on each server replica before enabling the feature:

| Variable | Value |
| --- | --- |
| `APPROVAL_CANDIDATES_ENABLED` | `false` initially; `true` enables admission and the UI |
| `APPROVAL_CANDIDATES_PROVIDER` | `anthropic` or `openrouter` |
| `APPROVAL_CANDIDATES_MODEL` | Explicit model ID supported by that provider |
| `APPROVAL_CANDIDATES_DAILY_INPUT_TOKENS` | Deployment-wide UTC allocation budget, at least `600000` |
| `APPROVAL_CANDIDATES_DAILY_OUTPUT_TOKENS` | Deployment-wide UTC allocation budget, at least `12000` |
| `APPROVAL_CANDIDATES_CACHE_DIR` | Private writable directory, default `data/approval-cache` |
| `APPROVAL_CANDIDATES_PROVIDER_IDENTITIES` | JSON array of verified Greptile/Copilot numeric actor IDs |

The runtime uses the existing `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY` setting. A CLI subscription does not supply these credentials. Use the same configuration on all replicas. `BASE_URL` must be the browser application's origin for browser POST validation.

An identity configuration has this shape, using example IDs only:

```json
[{"provider":"greptile","actor_id":12345},{"provider":"copilot","actor_id":67890}]
```

Resolve and verify the actual GitHub numeric bot account IDs before configuration. A matching display name, login substring, or comment marker never establishes trust. Actors must also have GitHub's `Bot` type. Nonzero `app_id` constraints make this runtime unavailable because GitHub review payloads do not expose enough app identity; configure verified actor IDs instead. Missing allowlists leave those artifacts visible as generic evidence. Local PRism evidence is verified against durable completed runs and immutable sidecars.

## Runtime and storage

The investigator runs inside the Go server and uses native model HTTP APIs. A database lease limits deployment concurrency to two investigations and one per user. Two separate read-only slots handle explicit evidence revalidation. Browser navigation does not stop jobs.

The model can only list/read frozen evidence and list/read/search/diff registered git revisions. The code reader uses a separate private bare repository, no checkout, and an ephemeral credential pipe. It disables git hooks, external diff, text conversion, ambient config, credential helpers and submodule recursion. No repository commands or tests run. Temporary repositories are removed at task completion. Startup and hourly maintenance remove abandoned managed repositories using filesystem locks, including after a process is killed. Active workers and surviving Git processes retain their repository leases. Linux and macOS are supported; shared cache mounts must provide coherent flock locking across replicas. Unmarked directories from older versions are preserved: remove those only after stopping all workers running the older version.

Dedicated database tables retain scans, snapshots, assessments, leases, current projections and usage. Evidence is never published as a review artifact. Existing authenticated user inventory and live repository access are checked before returning it. Results are retained for 30 days; hourly pruning removes terminal histories and dependent records.

Every target reserves its maximum allowance before model execution. Trusted usage releases unused allowance. Missing or invalid usage retains the conservative reservation. An in-flight target keeps its original UTC allocation day across midnight and recovery; new targets allocate against the new day. These are resource allocation caps, not dollar budgets or an exact per-calendar-day billing report.

Admission snapshots provider, model and policy/runtime versions. Configuration drift blocks execution or freshness renewal. Changing credentials without changing provider/model remains possible. The provider cannot silently fall back to a different model.

## Rollout and rollback

1. Merge the complete draft stack and apply the additive migrations with the feature disabled. Existing ordinary reviews continue independently.
2. Configure an explicit provider/model and small daily budgets. Confirm authenticated `GET /api/v1/approval-capabilities` reports availability without revealing credentials.
3. Enable in a pilot deployment. Start with a few PRs covering PRism, Greptile, Copilot, a changed HEAD, unresolved concerns, and missing evidence. Compare every recommendation with a human review.
4. Confirm tool use and cumulative usage in retained results, test cancellation, and confirm observations or expiration withdraw recommendations.
5. Expand only after checking false-positive recommendations and actual resource use. Do not automatically approve pilot results.

Set `APPROVAL_CANDIDATES_ENABLED=false` and restart replicas to roll back. The UI disappears, admission stops, and workers cancel remaining scans while retaining protected history. No model review is triggered by GitHub updates. Observed PR update events conservatively invalidate existing candidates, including when a broad update signal contains changes unrelated to review content.

## Validation and limits

Automated fixtures cover both native API adapters, required artifact reads, anchored concern provenance, provider identity, nested pagination failures, incomplete coverage, code citation checks, tool boundaries, cancellation, recovery, budget reservations, generation fencing, SQLite/PostgreSQL transactions, API ownership, and UI freshness. Browser fixtures exercise desktop/mobile themes and keyboard focus.

The implementation was verified with controlled HTTP/provider fixtures. A paid live-model pilot and a production rollout have not been performed. API compatibility tests do not measure recommendation quality on a deployed repository.

Historical legacy or cache-restored PRism runs without immutable full-revision sidecars remain evidence gaps even after a newer review succeeds; the runtime does not silently discard or override them. Reviews without immutable PRism sidecars, unknown reviewed revisions, incomplete pagination, missing provider identity, large evidence sets, or exhausted tool/model budgets produce gaps or failed investigations. The dashboard may lack the current user's actual reviewed SHA; it then keeps that PR in the launch pool and admission resolves eligibility using live review metadata. Safe concern dispositions require source anchors; unanchored concerns remain insufficient evidence. Collection ceilings do not guarantee that every artifact fits the smaller investigation budget. File coverage is labeled unreported unless the source supplies it. Positive CI does not assert branch-rule compliance, and code inspection does not assert tests were executed.
