# Publisher replay baseline

Produced by `go run ./cmd/publishreplay` on master at `b5b37a7` (publisher code unchanged by the harness
commits) over the 551 PR dumps the comment-system audit collected, with their review sidecars. Date: 2026-10-02,
after the harness started treating a compare at the endpoint's 300-file cap as unknown (18 resolutions moved
from the known columns; see limits). Policy: inline cap 3, minimum severity medium, unverified claims folded
(the shipped defaults).

Every later publisher PR reports the same JSON block before and after its change. Targets are from the
program spec: `same_marker_reposts` 0, `same_defect_reposts_per_pr` at most 0.05, `fixed_without_file_change`
0, `same_commit_resolves` 0.

## Metrics (master)

| metric | value | note |
|---|---:|---|
| prs | 551 | dumps replayed |
| prs_with_rounds | 193 | PRs where the bot submitted at least one review |
| rounds | 439 | bot review submissions, in time order |
| rounds_replayed | 433 | rounds whose sidecar the review server still had |
| rounds_missing_sidecar | 6 | spread over 3 PRs; the rounds are skipped |
| same_commit_rounds | 63 | a round whose head equals the previous round's head |
| roots_posted | 485 | inline root comments the publisher would create (498 observed) |
| same_marker_reposts | 11 | target 0 |
| same_defect_reposts | 39 | aliases a root from an earlier round: same file, line within 10, raw Jaccard >= 0.20 or any shared subject (26 under the tightened clause below) |
| same_defect_reposts_per_pr | 0.202 | over prs_with_rounds; target <= 0.05 |
| same_round_duplicates | 5 | two roots in one round that alias each other; ledger aliasing cannot remove these |
| prs_with_reposts | 31 | |
| fixed | 245 | ledger rows flipped out of open |
| fixed_without_file_change | 48 | cited file untouched between LastSeenSHA and HeadSHA; target 0 |
| fixed_file_change_unknown | 18 | the compare listed 300 files, the endpoint's cap, so the cited file may be missing from it |
| same_commit_resolves | 0 | structurally 0 in replay, see limits; `same_commit_rounds` is the exposure |
| comments_per_push_p50 | 1 | inline comments per replayed round |
| rounds_per_pr_p50 | 1 | over prs_with_rounds |
| prism_resolved_share | 0 | the publisher cannot resolve threads yet |

### Renderer and round-guard metrics (added with W1-2)

Measured with `--legacy-titles --same-commit-guard=false`, which reproduces master's publisher on the same
harness commit; every other metric above is unchanged under those flags.

| metric | value | note |
|---|---:|---|
| same_commit_rounds_skipped | 0 | rounds the publisher refused because the head was already published; target: every same-commit round whose earlier round completed |
| nil_impact_bullets | 713 | summary bullets (over every summary render) opening with a nil-impact phrase such as "None today" or "No user impact"; target 0 |
| bare_label_titles | 348 | inline comments titled by their bare kind label ("Behavior change", "Security", ...); target 0 |

Observed in the dumps, for calibration: 498 roots, 10 same-marker reposts, 90 same-defect reposts (rendered
prose, so boilerplate words inflate the overlap), 209 of 498 threads resolved (0.42, all by humans).

## Where the reposts and false fixes concentrate

PR numbers only, repositories omitted. Top reposts (same marker + same defect / rounds): #31655 11 / 9,
#142 3 / 8, #31365 3 / 4, #160 2 / 10. Top false fixes (fixed_without_file_change / fixed): #31944 5 / 6,
#32308 4 / 7, #160 3 / 7, #31655 3 / 14. Unknown compares concentrate on #30624 (4 / 8) and #31365 (4 / 9). The full per-PR table is the `--csv` output; it names real repositories, so write it outside the repo and
never commit it.

## How to reproduce

```
PRISM_BASE_URL=<review server> go run ./cmd/publishreplay \
  --dumps <audit folder>/prs --sidecars <audit folder>/sidecars \
  --csv <audit folder>/replay.csv --out <audit folder>/replay.json
```

The first run fetches missing sidecars (`PRISM_TOKEN` or the gh CLI token) and the GitHub compares between
round heads through `gh api`, caching both under `--sidecars`. Later runs can pass `--offline`. `--pr
repo#number` replays one PR, `--limit N` the first N dumps.

## Fixture expectations

The three fixtures under `cmd/publishreplay/testdata` (a same-marker repost, a rewording, a return after a
fix) are replayed under both policies: `TestRun_FixtureMetricsLegacy` pins master's numbers and
`TestRun_FixtureMetrics` the ledger policy's. `W1-1.md` carries the first before-and-after run.

## Limits of the measurement

- A round is a review submission; a round that posted no inline comment leaves no review and is invisible.
- Same-commit rounds replay the same sidecar twice, so the replay cannot resolve anything on them; the live
  system did, because each API re-run overwrote the sidecar at the same key. `same_commit_rounds` (63) is the
  exposure, so a same-commit guard is evidenced by that count, not by `same_commit_resolves`.
- A transient sidecar or compare fetch failure is logged and counted as `rounds_missing_sidecar` or
  `fixed_file_change_unknown`; only a 404 is remembered with a `.missing` marker.
- The subject clause of the alias rule was tightened with W1-1 to mirror the publisher's key: same
  `finding_kind` and the same sorted subject set, or a shared `symbol` or `selector` subject that is not
  the only subject on either side. One shared enclosing function is not a shared defect. The 39 above was
  measured under the earlier "any shared subject" clause; the same master code reads 26 (0.135 per PR) under
  the current one. `--alias-text-only` drops the subject clause altogether.
- Line translation across pushes is not modelled; the alias check uses the cited lines as emitted.
- Commentable lines come from the hunks stored with each finding, not the full PR patch set.
- Observed same-defect reposts use text overlap only (the dumps carry rendered prose, not contract subjects), so
  the observed column is a calibration aid, not the same rule as the replayed one.
- A 404 sidecar is remembered with a `.missing` marker; delete the marker to ask the server again.
- Changed files come from a three-dot compare, which after a rebase spans the whole PR diff and undercounts
  `fixed_without_file_change`. The endpoint lists at most 300 files and does not page them, so a compare at
  that size cannot prove the cited file untouched and is counted in `fixed_file_change_unknown`.
