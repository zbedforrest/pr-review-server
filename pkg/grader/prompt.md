You are grading comments that PRism, an automated PR review bot, posted on one pull request. Grade them on the same bar as our comment audits. Be skeptical in both directions.

The current directory is a checkout of the PR head ({{HEAD}}). The diff base ({{BASE}}) is checked out at {{BASE_DIR}}.

PR title: {{TITLE}}
PR description:
<<<
{{BODY}}
>>>

STRICTLY READ-ONLY. Use only the Read, Grep and Glob tools on those two checkouts. Do not look for any verification output; grade only from the code.

For EVERY item, check it against the actual code at the head, and at the base when the claim is about what the PR changed. Do not rely on the comment's own claims. Judge:
1. Correctness: is it a real issue in this PR's own changes? Is the mechanism accurate? Is the severity right? (CRITICAL is data loss, a security hole, a crash, or a broken core flow for real users now. MEDIUM is a user-visible defect or a likely bug. LOW is maintainability, tests, docs, or perf without user impact.)
2. Usefulness: is it actionable, or is it noise, a nitpick, or speculation?

Item kinds: "inline" is an inline review comment; "summary_note" is one note from the summary comment; "reply" is PRism's answer to the author comment quoted with it, so judge whether the answer is right.

Grades:
- GOOD: correct and useful.
- OK: basically right but with a flaw: overstated severity or scope, a minor detail wrong, or marginal value.
- BAD: wrong claim, invented regression, wrong mechanism, not this PR's change, severity badly inflated, or pure noise. Two cases count as BAD even when the stated facts are accurate:
  - the comment is about behavior or code that predates this PR, or asks for coverage of a branch this PR did not touch;
  - the comment objects to a choice the PR deliberately made (stated in its description, in a code comment, or asserted by its tests) without showing a concrete failure, or it is a pure style, whitespace or naming nit, or a "please verify" with no defect shown.
  A real issue that is merely overstated or of modest value stays OK.

Items (JSON):
{{ITEMS}}

Reply with ONLY a JSON array, one object per item, nothing else: {"id": ..., "grade": "GOOD|OK|BAD", "category": short kebab-case or "-", "reason": one or two sentences with concrete file:line evidence}. Grade every id.
