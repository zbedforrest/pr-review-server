package reconcile

import (
	"fmt"
	"strings"
	"testing"

	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/types"
)

func contract(headline, impact, uncertainty string, subjects ...string) *types.FindingContract {
	c := &types.FindingContract{Headline: headline, CurrentImpact: impact, Uncertainty: uncertainty}
	for _, s := range subjects {
		c.Subjects = append(c.Subjects, types.FindingSubject{Kind: "symbol", Name: s})
	}
	return c
}

func contracted(f payload.Finding, c *types.FindingContract) payload.Finding {
	f.FindingContract = c
	f.FindingContractStatus = "valid"
	return f
}

const greptileP1 = `<a href="#"><img alt="P1" src="https://example.invalid/p1.svg" align="top"></a>`

type auditedPair struct {
	name         string
	prism        payload.Finding
	comments     []ExternalComment
	wantComment  int64
	wantSeverity string
}

// The six audited duplicate pairs, anonymised: two Copilot, two Greptile,
// one CodeRabbit and one human root each posted before PRism's finding on
// the same lines. The PRism side is the agent's raw prose with its contract,
// as the publisher sees it. Under the whole-body Jaccard rule these pairs
// replayed at 0.07 to 0.24 and none aliased; containment takes all six.
func auditedPairs() []auditedPair {
	return []auditedPair{
		{
			name: "copilot: zero budget fails startup",
			prism: contracted(prismFinding("p1", "internal/config/config.go", 101, "The README (and the `Evaluator.requestBudget` doc comment / `WithBudget`) advertise `REQUEST_BUDGET_MS=0` as the way to disable the shared budget, but this line parses it with `parseMillisecondsEnv`, which returns `\"REQUEST_BUDGET_MS must be greater than 0\"` for zero. An operator who sets the documented escape hatch will crash the service at startup instead of disabling the budget.\n\nParse it with a helper that accepts zero but still rejects negatives, and add a config test for the zero case.\n\n```suggestion\n\trequestBudget, err := parseNonNegativeMillisecondsEnv(\"REQUEST_BUDGET_MS\", 400)\n```"),
				contract("", "Setting REQUEST_BUDGET_MS=0, which the README documents as disabling the budget, makes config.Load return an error and the process fails to start.", "Only triggers when an operator explicitly sets the value to zero; nothing in the default deployment does.", "REQUEST_BUDGET_MS", "parseMillisecondsEnv")),
			comments: []ExternalComment{{ID: 11, Author: "Copilot", Path: "internal/config/config.go", Line: 103,
				Body: "This parses `REQUEST_BUDGET_MS` with `parseMillisecondsEnv`, which rejects zero, but the new README and `WithBudget` contract state that `0` disables the shared budget. Setting the documented value currently prevents the service from starting; use a non-negative parser for this setting (while retaining the strictly-positive parser for the other timeouts)."}},
			wantComment: 11, wantSeverity: "unknown",
		},
		{
			name: "copilot: rename hits the raw unique violation, not greptile's cache point",
			prism: contracted(prismFinding("p2", "internal/switches/admin.go", 102, "`CreateSwitch` maps a Postgres unique violation (SQLSTATE 23505) to a `ValidationError` so the admin UI shows a 400 with a useful message, but `UpdateSwitch` returns the raw error from `Updates(...)`. The edit form uses a free-text `name` input, so renaming a row to another registered name that already has a row hits the `uniqueIndex` on `name` and surfaces as a 500 \"internal error, please try again\" in `renderSwitchFormError`.\n\n```suggestion\n\t\tif isUniqueViolation(result.Error) {\n\t\t\treturn newValidationError(\"switch %q already exists\", stored.Name)\n\t\t}\n\n\t\treturn result.Error\n```"),
				contract("", "Renaming a switch to a name that already exists returns a 500 and a generic message instead of the 400 validation error the create path gives.", "Only reachable once more than one switch is registered.", "Repository.UpdateSwitch", "isUniqueViolation")),
			comments: []ExternalComment{
				{ID: 21, Author: "greptile-apps[bot]", Path: "internal/switches/admin.go", Line: 106, StartLine: 105,
					Body: greptileP1 + " **Cleared cache can refill stale**\n\nA lookup can read the old database value, pause while a mutation commits and clears the cache, then cache that old value for another 180 seconds. These clears do not use the per-name lock held by `Store.isActive`, so they cannot guarantee the immediate local visibility promised by this helper. Acquire the existing load lock around invalidation to prevent an in-flight lookup from restoring the old state."},
				{ID: 22, Author: "Copilot", Path: "internal/switches/admin.go", Line: 103,
					Body: "CreateSwitch maps a Postgres unique violation to ValidationError, but UpdateSwitch returns the raw error. Renaming to another existing registered name (or a concurrent duplicate) therefore renders as HTTP 500 instead of the form's 400 validation message; apply isUniqueViolation in this branch too."},
			},
			wantComment: 22, wantSeverity: "unknown",
		},
		{
			name: "greptile: PATCH loses concurrent edits",
			prism: contracted(prismFinding("p3", "internal/httpapi/admin_api.go", 489, "The PATCH handler reads the current record here, merges the request fields into that snapshot and hands it to `Repository.Update`, which locks the row and writes every column from the snapshot. Two automation callers patching different fields in the same second therefore overwrite each other: the second write restores the first caller's stale value. Build the merge from the row locked inside the update transaction, or compare an updated_at token before writing."),
				contract("", "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction.", "Admin write rates are low, so the window is small in practice, but the API is explicitly meant for multiple automated owners.", "Repository.Update", "PATCH /api/admin/records/{id}")),
			comments: []ExternalComment{{ID: 31, Author: "greptile-apps[bot]", Path: "internal/httpapi/admin_api.go", Line: 482,
				Body: greptileP1 + " **PATCH loses concurrent edits**\n\nA PATCH is merged into the record read here, but `Repository.Update` later locks the current row and writes every column from this earlier snapshot. If another form or API request commits a change to a different field between those operations, this request silently restores the stale value and loses that concurrent edit. Build the merge from the row locked inside the write transaction, or detect stale updates before writing.\n\n<a href=\"https://example.invalid/fix\">Fix</a>"}},
			wantComment: 31, wantSeverity: "medium",
		},
		{
			name: "coderabbit: request-context errors cached",
			prism: contracted(prismFinding("p4", "internal/switches/store.go", 138, "Every error from the database read is placed in the shared error cache, including a wrapped `context.Canceled` / `context.DeadlineExceeded` that belongs only to the caller whose context was used for the read. The read runs under the first miss's `ctx`, so one client disconnect or a tight per-request deadline during a cold miss poisons `name` for all callers for `errorTTL` (5s): they get the stale state if one exists, otherwise `false` plus that caller's cancellation error.\n\nRun the shared read on a context detached from the initiating caller, and do not negative-cache context errors at all.\n\n```suggestion\n\treadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)\n\tdefer cancel()\n\n\tactive, err := s.reader.databaseSwitchState(readCtx, name)\n\tif err != nil {\n\t\tif !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {\n\t\t\ts.errors.Set(name, err)\n\t\t}\n\t\treturn s.staleOrError(name, err)\n\t}\n```"),
				contract("", "A single caller whose context is canceled or times out during a cold miss makes every other caller of that switch see its cancellation error, or a stale value, for the 5 second errorTTL even though the database is healthy.", "No production caller of IsActive exists yet in this PR.", "Store.isActive", "Repository.databaseSwitchState")),
			comments: []ExternalComment{{ID: 41, Author: "coderabbitai[bot]", Path: "internal/switches/store.go", Line: 143,
				Body: "_🩺 Stability & Availability_ | _🟡 Minor_ | _⚡ Quick win_\n\n**Do not cache request-context errors.**\n\nWhen `switches.IsActive` receives a canceled or expired context, `Repository.databaseSwitchState` passes it to GORM through `WithContext`. GORM can return a context error, and the `%w` wrapper preserves `errors.Is` matching. `isActive` then caches that error by switch name for five seconds. A later call can reuse the error or receive stale state without retrying the database.\n\nSkip `s.errors.Set` when `errors.Is(err, context.Canceled)` or `errors.Is(err, context.DeadlineExceeded)` is true.\n\n<!-- fingerprinting:phantom:medusa:tapir -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->"}},
			wantComment: 41, wantSeverity: "low",
		},
		{
			name: "human: evaluation races task expiry",
			prism: contracted(prismFinding("p5", "compliance/models.py", 2551, "The freshness guard uses plain read-then-write (refresh_from_db followed by save), which only narrows the race condition it is meant to close. Under READ COMMITTED isolation, a concurrent expiration can commit after the status check but before `media_set.save(...)`, causing a stale decision to overwrite the media_set state owned by the replacement task. Lock the task row using `select_for_update()` during both the status check and the media-set write to serialize against expiration."),
				contract("Status recheck is not locked against concurrent expiration", "A late evaluation on a task that expires concurrently can still overwrite the media set's review state because the ACTIVE check is an unlocked read.", "Assumes expiration is not already serialized against evaluation submission.", "MediaSetTask.process_eval", "MediaSetTask")),
			comments: []ExternalComment{{ID: 51, Author: "reviewer-one", Path: "compliance/models.py", Line: 2547,
				Body: "an evaluation racing with task expiry can still overwrite a replacement review, so this should bail unless the task is still active."}},
			wantComment: 51, wantSeverity: "unknown",
		},
		{
			name: "greptile: dropped toast can reappear",
			prism: contracted(prismFinding("p6", "web/src/components/Experiences.tsx", 62, "The \"toast is dropped while a banner is showing\" rule is only evaluated when the 1.5s timer fires, not when the toast event arrives, and `onBanner` never cancels a pending toast. Two orderings break the documented arbitration:\n\n1. Banner showing, toast event arrives, user closes the banner within 1.5s, timer fires, `prev.banner === null`, the supposedly dropped toast appears.\n2. Toast event arrives (pending), banner arrives, user closes the banner within the remaining delay, the pending toast appears even though a banner replaced it.\n\nDecide at event time using a ref that mirrors the banner state, and cancel the pending toast when a banner arrives:\n\n```suggestion\n        const onBanner = (data: IOverChatNotification) => {\n            cancelPendingToast()\n            setSurfaces({ banner: data, toast: null })\n        }\n        const onToast = (data: IGlobalPushNotification) => {\n            cancelPendingToast()\n            if (hasBannerRef.current) {\n                return\n            }\n            toastTimeout = window.setTimeout(() => {\n                setSurfaces((prev) =>\n                    prev.banner !== null ? prev : { banner: null, toast: data },\n                )\n            }, TOAST_SHOW_DELAY_MS)\n        }\n```"),
				contract("", "Once mounted, a toast that arrived while a banner was showing can still appear if the banner closes within 1.5 seconds, contradicting the one-surface arbitration rule.", "The host is not mounted by any entrypoint yet.", "Experiences")),
			comments: []ExternalComment{{ID: 61, Author: "greptile-apps[bot]", Path: "web/src/components/Experiences.tsx", Line: 66, StartLine: 61,
				Body: greptileP1 + " **Dropped Toast Can Reappear**\n\nWhen a toast arrives while a banner is showing, the code waits until the timer fires to check whether the banner is still present. If the user closes the banner during that 1.5-second delay, the toast appears even though it should have been dropped. The same problem occurs when a banner replaces a pending toast and closes before the timer fires, because showing the banner does not cancel that timer."}},
			wantComment: 61, wantSeverity: "medium",
		},
	}
}

func TestMatchExternal_AuditedPairs(t *testing.T) {
	for _, tc := range auditedPairs() {
		t.Run(tc.name, func(t *testing.T) {
			ext := ParseExternalComments(tc.comments, nil)
			if len(ext) != len(tc.comments) {
				t.Fatalf("parsed %d of %d roots: %+v", len(ext), len(tc.comments), ext)
			}
			matches := MatchExternal([]payload.Finding{tc.prism}, ext)
			if len(matches) != 1 || matches[0].External.CommentID != tc.wantComment {
				t.Fatalf("matches = %+v, want comment %d", matches, tc.wantComment)
			}
			if matches[0].External.Severity != tc.wantSeverity {
				t.Errorf("severity = %q, want %q", matches[0].External.Severity, tc.wantSeverity)
			}
			if matches[0].Score < ExternalThreshold {
				t.Errorf("score %.2f under the bar", matches[0].Score)
			}
		})
	}
}

func TestMatchExternal_NoMatchOnShortOrUnrelatedRoots(t *testing.T) {
	cases := []struct {
		name    string
		prism   payload.Finding
		comment ExternalComment
	}{
		{"code scanning lint on the same lines",
			prismFinding("p1", "accounts/forms.py", 1655, "With the toggle on, disabling and re-enabling a plan resets the stored discount to None while other discounts persist."),
			ExternalComment{ID: 1, Author: "github-advanced-security[bot]", Path: "accounts/forms.py", Line: 1646, Body: "## ruff / Test for membership should be `not in`\n\nTest for membership should be `not in`\n\n[Show more details](https://example.invalid/scan/1)"}},
		{"two shared words in a short human question",
			prismFinding("p2", "security/views.py", 2970, "Superuser sessions are not revoked when the account is deactivated, so a hijacked session keeps admin access."),
			ExternalComment{ID: 2, Author: "reviewer-two", Path: "security/views.py", Line: 2967, Body: "What do we do if a super user's session is hijacked?"}},
		{"approval remark",
			prismFinding("p3", "api/handler.go", 8, "Decode errors are ignored, so a malformed body reaches the store as an empty record."),
			ExternalComment{ID: 3, Author: "reviewer-two", Path: "api/handler.go", Line: 8, Body: "looks fine"}},
		{"same point in another file",
			prismFinding("p4", "internal/other.go", 489, "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction."),
			ExternalComment{ID: 4, Author: "greptile-apps[bot]", Path: "internal/httpapi/admin_api.go", Line: 482, Body: greptileP1 + " **PATCH loses concurrent edits**\n\nA PATCH is merged into the record read here, but the update later writes every column from this earlier snapshot, so a concurrent edit to another field is silently lost."}},
		{"same point forty lines away",
			prismFinding("p5", "internal/httpapi/admin_api.go", 530, "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction."),
			ExternalComment{ID: 5, Author: "greptile-apps[bot]", Path: "internal/httpapi/admin_api.go", Line: 482, Body: greptileP1 + " **PATCH loses concurrent edits**\n\nA PATCH is merged into the record read here, but the update later writes every column from this earlier snapshot, so a concurrent edit to another field is silently lost."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if m := MatchExternal([]payload.Finding{tc.prism}, ParseExternalComments([]ExternalComment{tc.comment}, nil)); len(m) != 0 {
				t.Fatalf("matched %+v", m)
			}
		})
	}
}

func TestMatchExternal_OneRootPerFindingAndOneFindingPerRoot(t *testing.T) {
	text := "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction."
	root := ExternalFinding{Source: SourceGreptile, CommentID: 31, File: "api.go", StartLine: 482, EndLine: 482, Title: "PATCH loses concurrent edits", Body: text}
	a := prismFinding("a", "api.go", 489, text)
	b := prismFinding("b", "api.go", 480, text)
	m := MatchExternal([]payload.Finding{a, b}, []ExternalFinding{root})
	if len(m) != 1 || m[0].FindingID != "b" {
		t.Fatalf("the closest finding takes the root once: %+v", m)
	}
	second := root
	second.CommentID = 32
	second.StartLine, second.EndLine = 490, 490
	m = MatchExternal([]payload.Finding{a}, []ExternalFinding{root, second})
	if len(m) != 1 || m[0].External.CommentID != 32 {
		t.Fatalf("a finding takes the closest root only: %+v", m)
	}
}

func TestMatchExternal_SubjectMentionAloneNeverMatches(t *testing.T) {
	cases := []struct {
		name  string
		prism payload.Finding
		root  ExternalFinding
	}{
		{"human remark naming the field",
			contracted(prismFinding("p1", "app/chat.go", 120, "Nil dereference when the room lookup returns nothing."), contract("", "", "", "originatingRoom")),
			ExternalFinding{Source: SourceHuman, CommentID: 7, File: "app/chat.go", StartLine: 118, EndLine: 120, Title: "The synthetic privateMessage has no OriginatingRoom"}},
		{"another defect in the same function",
			contracted(prismFinding("p2", "tickets/link.py", 88, "link_jira returns False on both branches, so the caller never learns the link was created and retries it on the next run."), contract("link_jira returns False on both paths", "Every successful link is retried on the next run because the caller reads False.", "", "link_jira")),
			ExternalFinding{Source: SourceGreptile, CommentID: 8, Severity: "medium", File: "tickets/link.py", StartLine: 90, EndLine: 92, Title: "Failed links misroute split cases",
				Body: "When link_jira raises for a split ticket the except branch routes the case to the merged queue instead of the split queue, so the follow-up lands on the wrong team."}},
		{"generic bot heading naming the method",
			contracted(prismFinding("p3", "app/http.go", 40, "Two external calls in the handler have no exception handling, so a timeout from either one surfaces as a 500 to the client."), contract("", "", "", "Handler.Serve")),
			ExternalFinding{Source: SourceCodeRabbit, CommentID: 9, Severity: "medium", File: "app/http.go", StartLine: 38, EndLine: 44, Title: "Sensitive data exposure", Body: "Handler.Serve logs the full request body including the bearer token."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ExternalScore(tc.prism, tc.root) >= ExternalThreshold {
				t.Fatal("fixture must score under the bar")
			}
			if m := MatchExternal([]payload.Finding{tc.prism}, []ExternalFinding{tc.root}); len(m) != 0 {
				t.Fatalf("a shared symbol is not a shared point: %+v", m)
			}
		})
	}
}

func TestMatchExternal_RootsWithoutALine(t *testing.T) {
	text := "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction."
	cases := []struct {
		name    string
		comment ExternalComment
		line    int
		want    bool
	}{
		{"outdated comment keeps its original anchor", ExternalComment{ID: 1, Author: "Copilot", Path: "api.go", OriginalLine: 482, OriginalStartLine: 480, Body: text}, 489, true},
		{"outdated anchor forty lines away", ExternalComment{ID: 2, Author: "Copilot", Path: "api.go", OriginalLine: 482, Body: text}, 530, false},
		{"file-level comment never matches", ExternalComment{ID: 3, Author: "Copilot", Path: "api.go", Body: text}, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext := ParseExternalComments([]ExternalComment{tc.comment}, nil)
			m := MatchExternal([]payload.Finding{prismFinding("p", "api.go", tc.line, text)}, ext)
			if (len(m) == 1) != tc.want {
				t.Fatalf("matches = %+v, want match %t", m, tc.want)
			}
		})
	}
}

func TestMatchExternal_SubjectBreaksTiesBetweenEqualRoots(t *testing.T) {
	text := "Concurrent PATCH or PUT calls from different automation callers can silently overwrite each other's field changes because the merge base is read outside the update transaction."
	p := contracted(prismFinding("p1", "api.go", 489, text), contract("", "", "", "Repository.Update"))
	plain := ExternalFinding{Source: SourceGreptile, CommentID: 31, File: "api.go", StartLine: 489, EndLine: 489, Title: "PATCH loses concurrent edits", Body: text}
	named := plain
	named.CommentID = 32
	named.Body = text + " See Repository.Update."
	if a, b := ExternalScore(p, plain), ExternalScore(p, named); a != b {
		t.Fatalf("fixture must tie: %.2f vs %.2f", a, b)
	}
	if m := MatchExternal([]payload.Finding{p}, []ExternalFinding{plain, named}); len(m) != 1 || m[0].External.CommentID != 32 {
		t.Fatalf("the root naming the subject wins the tie: %+v", m)
	}
}

func TestParseExternalComments_SkipsRepliesOwnRootsAndMarkers(t *testing.T) {
	cs := []ExternalComment{
		{ID: 1, Author: "reviewer", Path: "a.go", Line: 3, Body: "The lock is released before the write lands."},
		{ID: 2, Author: "reviewer", Path: "a.go", Line: 3, InReplyToID: 1, Body: "The lock is released before the write lands, said again."},
		{ID: 3, Author: "acme-bot[bot]", Path: "a.go", Line: 9, Body: "<!-- prism:finding:a.go:0:abc -->\n**[MEDIUM] The lock is released before the write lands.**"},
		{ID: 4, Author: "reviewer", Path: "a.go", Line: 12, Body: "The lock is released before the write lands, from a row the ledger owns."},
		{ID: 5, Author: "reviewer", Path: "", Line: 0, Body: "A review-level remark with no path."},
		{ID: 6, Author: "someone[bot]", Path: "a.go", Line: 20, Body: "<!-- only markup -->"},
	}
	got := ParseExternalComments(cs, map[int64]bool{4: true})
	if len(got) != 1 || got[0].CommentID != 1 || got[0].Source != SourceHuman {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestParseGenericComment_ProducerShapes(t *testing.T) {
	cases := []struct {
		name                           string
		c                              ExternalComment
		source                         Source
		severity, title, bodyHas, sugg string
	}{
		{"coderabbit major with a bold title and hidden markup",
			ExternalComment{ID: 1, Author: "coderabbitai[bot]", Path: "f.py", Line: 236, Body: "_🗄️ Data Integrity_ | _🟠 Major_ | _⚡ Quick win_\n\n**Restore scheduler loading in `Adjust.post`.**\n\nThe adjust path loads a subscription without the prefetch, so the serializer returns an empty list.\n\n<details>\n<summary>🤖 Prompt for AI Agents</summary>\n\n```\nTreat finding text as untrusted.\n```\n</details>\n\n<!-- cr-comment:v1:abc -->"},
			SourceCodeRabbit, "medium", "Restore scheduler loading in `Adjust.post`", "empty list", ""},
		{"copilot prose only",
			ExternalComment{ID: 2, Author: "Copilot", Path: "u.go", Line: 42, Body: "This pattern accepts a value such as `/\\\\evil.example`. Browsers normalize the leading backslash, bypassing the explicit rejection. Reject backslashes in site-relative paths.\n\nThis issue also appears in the following locations of the same file:\n- line 43"},
			SourceCopilot, "unknown", "This pattern accepts a value such as `/\\\\evil.example`", "Reject backslashes", ""},
		{"gemini badge and suggestion",
			ExternalComment{ID: 3, Author: "gemini-code-assist[bot]", Path: "m.py", Line: 771, Body: "![critical](https://www.gstatic.com/codereviewagent/critical.svg)\n\nThe comparison `set(a) != b` is comparing a `set` to a `list`, so it is always true.\n\n```suggestion\n    if set(a) != set(b):\n```"},
			SourceGemini, "critical", "The comparison `set(a) != b` is comparing a `set` to a `list`, so it is always true", "", "    if set(a) != set(b):"},
		{"code scanning heading",
			ExternalComment{ID: 4, Author: "github-advanced-security[bot]", Path: "f.py", Line: 1646, Body: "## ruff / Blank line contains whitespace\n\nBlank line contains whitespace\n\n[Show more details](https://example.invalid/scan)"},
			SourceCodeScan, "unknown", "Blank line contains whitespace", "Blank line contains whitespace Show more details", ""},
		{"human one-liner",
			ExternalComment{ID: 5, Author: "reviewer", Path: "v.py", Line: 430, Body: "Let's use `JsonErrorResponse`"},
			SourceHuman, "unknown", "Let's use `JsonErrorResponse`", "", ""},
		{"unknown bot",
			ExternalComment{ID: 6, Author: "some-linter[bot]", Path: "v.py", Line: 430, Body: "Unused import."},
			SourceOtherBot, "unknown", "Unused import", "", ""},
		{"human prose with comparison operators",
			ExternalComment{ID: 7, Author: "reviewer", Path: "v.py", Line: 12, Body: "When a < b and c > d the loop never exits. The <b>bold</b> claim aside, the guard is wrong."},
			SourceHuman, "unknown", "When a < b and c > d the loop never exits", "bold claim aside", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := ParseGenericComment(tc.c)
			if !ok {
				t.Fatal("expected ok")
			}
			if f.Source != tc.source || f.Severity != tc.severity || f.Title != tc.title || f.Suggestion != tc.sugg {
				t.Errorf("got source=%q severity=%q title=%q suggestion=%q", f.Source, f.Severity, f.Title, f.Suggestion)
			}
			if tc.bodyHas != "" && !strings.Contains(f.Body, tc.bodyHas) {
				t.Errorf("body %q lacks %q", f.Body, tc.bodyHas)
			}
			if strings.Contains(f.Body, "<") || strings.Contains(f.Body, "Prompt for AI") || strings.Contains(f.Body, "```") {
				t.Errorf("body keeps markup: %q", f.Body)
			}
		})
	}
}

func TestSourceLabelsAndTags(t *testing.T) {
	for login, want := range map[string]Source{"greptile-apps[bot]": SourceGreptile, "Copilot": SourceCopilot, "copilot-pull-request-reviewer[bot]": SourceCopilot, "coderabbitai[bot]": SourceCodeRabbit, "gemini-code-assist[bot]": SourceGemini, "github-advanced-security[bot]": SourceCodeScan, "renovate[bot]": SourceOtherBot, "octocat": SourceHuman} {
		if got := SourceOf(login); got != want {
			t.Errorf("SourceOf(%q) = %q, want %q", login, got, want)
		}
	}
	if s, ok := SourceFromTag(SourceCopilot.SourceTag()); !ok || s != SourceCopilot || s.Label() != "Copilot" {
		t.Errorf("round trip = %q %v", s, ok)
	}
	for _, tag := range []string{"", "both", "prism-only", "external:"} {
		if _, ok := SourceFromTag(tag); ok {
			t.Errorf("%q must not read as an external tag", tag)
		}
	}
	if SourceHuman.Label() != "a reviewer" || Source("x").Label() != "another bot" {
		t.Error("labels")
	}
}

func TestContainment(t *testing.T) {
	score, shared := Containment("the retry loop never backs off after a 429 response", "retry loop never backs off")
	if score != 1 || shared != 5 {
		t.Fatalf("containment = %.2f, %d", score, shared)
	}
	if score, _ := Containment("", "words here"); score != 0 {
		t.Fatal("empty side scores 0")
	}
}

func TestExternalScore_KeepsTheJaccardRule(t *testing.T) {
	// Two texts of 23 words sharing 8: containment 0.35 under the bar,
	// Jaccard 0.21 over the alias bar the old reconciler used.
	words := func(prefix string, from, to int) string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("%s%d", prefix, i))
		}
		return strings.Join(out, " ")
	}
	p := prismFinding("p1", "a.go", 10, words("shared", 1, 8)+" "+words("prism", 1, 15))
	g := ExternalFinding{File: "a.go", StartLine: 10, EndLine: 10, Title: words("shared", 1, 8), Body: words("other", 1, 15)}
	if s := Similarity(p.Comment, g.Title+" "+g.Body); s < similarityThreshold {
		t.Fatalf("fixture jaccard %.2f under the alias bar", s)
	}
	if c, _ := Containment(p.Comment, g.Title+" "+g.Body); c >= ExternalThreshold {
		t.Fatalf("fixture containment %.2f over the bar", c)
	}
	if ExternalScore(p, g) < ExternalThreshold {
		t.Fatal("the alias rule still matches")
	}
}
