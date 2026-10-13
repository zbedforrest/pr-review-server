# W6-A: fixed needs a hunk, one status note per thread, clamps with a marker

Replay harness: `go run ./cmd/publishreplay --dumps <data>/prs --sidecars <data>/sidecars --offline`
(the rebuilt dumps, 731 PRs) and the same over the 14 PRs of the 2026-10-13 audit (`<data>/audit-prs`).
The compare cache was refetched at schema 4 so every entry carries hunks; all three columns were measured
against that cache (master 201057c ignores the hunks). The "file rule" column runs this branch with
`--fixed-by-file --legacy-status-notes --legacy-clamp`, which is the before for the two new counters (they
did not exist on master).

Metric | master 201057c | file rule (kill switches) | this branch
--- | --- | --- | ---
rebuilt dumps: fixed | 206 | 206 | 151
rebuilt dumps: fixed_without_hunk_change | n/a | 63 | 0
rebuilt dumps: fixed_hunk_unknown | n/a | 0 | 0
rebuilt dumps: double_notes | n/a | 6 | 0
rebuilt dumps: in_thread_replies | 224 | 213 | 151
rebuilt dumps: threads_resolved / unresolved | 175 / 22 | 175 / 22 | 130 / 11
rebuilt dumps: prism_resolved_share | 0.324 | 0.324 | 0.252
rebuilt dumps: same_marker / same_defect reposts | 0 / 10 | 0 / 10 | 0 / 10
rebuilt dumps: roots_posted | 472 | 472 | 472
audited PRs: fixed | 18 | 18 | 14
audited PRs: fixed_without_hunk_change | n/a | 3 | 0
audited PRs: double_notes | n/a | 1 | 0
audited PRs: in_thread_replies | 19 | 16 | 10
audited PRs: threads_resolved / unresolved | 10 / 4 | 10 / 4 | 7 / 2

The master-to-file-rule drift in notes (224 to 213) comes from the severity clamp, which now judges the
anchor by hunk instead of by file. The three audited hunk-less fixes are the
header-template PR's two false "Back at" rows and its "Not seen" row after the force-pushed merge; the
drawer PR's "Not seen" on unchanged lines is reproduced as a unit test (the replay does not reach it
because the sidecar at that commit still carries the finding).

Of the 63 fixes the file rule granted without a hunk near the cited line, the nearest hunk was a median
42 lines away (p25 17, p75 124); 11 were within 10 lines and 23 within 25. A wider margin would recover
few of them, so the margin stays at three lines plus the hunk's own context.

The replay cannot show the clamp (no renderer metric counts cuts); the audited cuts are unit tests
(`render_clamp_test.go`, `render_clamp_audit_test.go`).

Follow-up after the peer verification of the branch: a compare that carries no hunks at all now leaves
every absent finding open (`Report.HunksUnknown`, logged once per round) instead of falling back to the
file rule; the kill switch still restores the file rule. Every entry in the compare cache carries hunks,
so both replay tables above are unchanged by it. The audited "Not seen" on the workflow-lint PR is not a
cache gap: the push between the two reviewed heads rewrote old lines 12 to 29 of the cited workflow (a
comment two lines above the cited line 28 was deleted), so the hunk rule holds there by design; the
finding itself was a false positive, which no line rule can tell from a fix. The nine summary-only audited PRs have no review rounds in the dumps (rounds
come from the bot's review submissions), so they replay as zero rounds.

## Relabel benchmark (commentbench)

The relabel cases were rebuilt from the same labels with compare hunks (`relabel/cases-hunks`, built on this
branch; the original `relabel/cases` carries only changed files, so the hunk rule has nothing to judge by
and both builds score the same there). Master on the rebuilt cases reproduces its baseline exactly.
`go run ./cmd/commentbench run -cases <relabel>/cases-hunks -anonymise`, gold | accepted:

Metric | master 201057c | this branch
--- | --- | ---
fixed flips / wrong / unconfirmed | 8 / 0 / 2 \| 115 / 21 / 17 | 7 / 0 / 1 \| 85 / 14 / 13
summary-count correctness | 0.294 of 17 \| 0.374 of 206 | 0.294 of 17 \| 0.354 of 206
thread-resolution recall | 0.591 of 44 \| 0.560 of 218 | 0.591 of 44 \| 0.491 of 218
post precision / recall | 0.642 / 0.895 \| 0.719 / 0.790 | unchanged
suppression recall, reposts, reply accuracy | 0.269 \| 0.628, 1 \| 17, 0.508 \| 0.345 | unchanged

Wrong fixed flips (the class 1 metric) fall from 21 to 14 on the accepted set; the seven that remain are
flips the labels call wrong although a hunk sits within three lines of the cited line (the fix moved the
code rather than the bug). Thread-resolution recall drops with them: a fix the hunk rule withholds is a
thread left open, and the 2 percentage points of summary-count correctness lost are rounds whose
"fixed" count the label took from the file rule.
