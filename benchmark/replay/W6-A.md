# W6-A: fixed needs a hunk, one status note per thread, clamps with a marker

Replay harness: `go run ./cmd/publishreplay --dumps <data>/prs --sidecars <data>/sidecars --offline`
(the rebuilt dumps, 731 PRs) and the same over the 14 PRs of the 2026-10-13 audit (`<data>/audit-prs`).
The compare cache was refetched at schema 4 so every entry carries hunks; the master column was measured
on master 201057c against the older cache, the other two on this branch against the new one. The
"file rule" column runs this branch with `--fixed-by-file --legacy-status-notes --legacy-clamp`, which is
the before for the two new counters (they did not exist on master).

Metric | master 201057c | file rule (kill switches) | this branch
--- | --- | --- | ---
rebuilt dumps: fixed | 206 | 206 | 151
rebuilt dumps: fixed_without_hunk_change | n/a | 63 | 0
rebuilt dumps: fixed_hunk_unknown | n/a | 0 | 0
rebuilt dumps: double_notes | n/a | 6 | 0
rebuilt dumps: in_thread_replies | 225 | 213 | 151
rebuilt dumps: threads_resolved / unresolved | 175 / 23 | 175 / 22 | 130 / 11
rebuilt dumps: prism_resolved_share | 0.319 | 0.324 | 0.252
rebuilt dumps: same_marker / same_defect reposts | 0 / 10 | 0 / 10 | 0 / 10
rebuilt dumps: roots_posted | 476 | 472 | 472
audited PRs: fixed | 18 | 18 | 14
audited PRs: fixed_without_hunk_change | n/a | 3 | 0
audited PRs: double_notes | n/a | 1 | 0
audited PRs: in_thread_replies | 19 | 16 | 10
audited PRs: threads_resolved / unresolved | 10 / 4 | 10 / 4 | 7 / 2

The master-to-file-rule drift (609 to 601 rounds replayed, 476 to 472 roots, 225 to 213 notes) comes
from the refetched compares: eight more pushes read as merge-only and are skipped, and the severity
clamp now judges the anchor by hunk instead of by file. The three audited hunk-less fixes are the
header-template PR's two false "Back at" rows and its "Not seen" row after the force-pushed merge; the
drawer PR's "Not seen" on unchanged lines is reproduced as a unit test (the replay does not reach it
because the sidecar at that commit still carries the finding).

Of the 63 fixes the file rule granted without a hunk near the cited line, the nearest hunk was a median
42 lines away (p25 17, p75 124); 11 were within 10 lines and 23 within 25. A wider margin would recover
few of them, so the margin stays at three lines plus the hunk's own context.

The replay cannot show the clamp (no renderer metric counts cuts); the audited cuts are unit tests
(`render_clamp_test.go`). The nine summary-only audited PRs have no review rounds in the dumps (rounds
come from the bot's review submissions), so they replay as zero rounds.
