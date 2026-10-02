package main

import (
	"fmt"
	"sort"
	"strings"
)

func fmtRatio(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *v)
}

// metricsTable is the headline table, gold and accepted side by side.
func metricsTable(res Result) string {
	g, a := res.Tiers[TierGold], res.Tiers[TierAccepted]
	var b strings.Builder
	b.WriteString("| metric | gold | accepted |\n|---|---:|---:|\n")
	row := func(name, gv, av string) { fmt.Fprintf(&b, "| %s | %s | %s |\n", name, gv, av) }
	num := func(n int) string { return fmt.Sprint(n) }
	row("cases", num(g.Cases), num(a.Cases))
	row("rounds replayed", num(g.Rounds), num(a.Rounds))
	row("findings that should post / be suppressed", fmt.Sprintf("%d / %d", g.ShouldPost, g.ShouldSuppress), fmt.Sprintf("%d / %d", a.ShouldPost, a.ShouldSuppress))
	row("post precision", fmtRatio(g.PostPrecision), fmtRatio(a.PostPrecision))
	row("post recall", fmtRatio(g.PostRecall), fmtRatio(a.PostRecall))
	row("suppression recall", fmtRatio(g.SuppressionRecall), fmtRatio(a.SuppressionRecall))
	row("reposts", num(g.Reposts), num(a.Reposts))
	row("fixed flips / wrong / unconfirmed", fmt.Sprintf("%d / %d / %d", g.Fixed, g.WrongFixed, g.FixedUnconfirmed), fmt.Sprintf("%d / %d / %d", a.Fixed, a.WrongFixed, a.FixedUnconfirmed))
	row("summary-count correctness", fmt.Sprintf("%s of %d", fmtRatio(g.SummaryCorrect), g.SummariesScored), fmt.Sprintf("%s of %d", fmtRatio(a.SummaryCorrect), a.SummariesScored))
	row("thread-resolution recall", fmt.Sprintf("%s of %d", fmtRatio(g.ResolutionRecall), g.ResolutionsWanted), fmt.Sprintf("%s of %d", fmtRatio(a.ResolutionRecall), a.ResolutionsWanted))
	row("reply decision accuracy", fmt.Sprintf("%s of %d", fmtRatio(g.ReplyAccuracy), g.RepliesScored), fmt.Sprintf("%s of %d", fmtRatio(a.ReplyAccuracy), a.RepliesScored))
	row("replies whose thread the replay did not post", num(g.RepliesNoThread), num(a.RepliesNoThread))
	row("fixed flips on unlabelled rows / reply step errors", fmt.Sprintf("%d / %d", g.FixedUnlabelled, g.ReplyErrors), fmt.Sprintf("%d / %d", a.FixedUnlabelled, a.ReplyErrors))
	row("unlabelled posts / unmapped expectations", fmt.Sprintf("%d / %d", g.UnlabeledPosts, g.UnmappedExpected), fmt.Sprintf("%d / %d", a.UnlabeledPosts, a.UnmappedExpected))
	return b.String()
}

func classTable(res Result) string {
	classes := map[string]bool{}
	for _, t := range res.Tiers {
		for k := range t.ReplyByClass {
			classes[k] = true
		}
	}
	names := make([]string, 0, len(classes))
	for k := range classes {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("| reply class | gold correct / expected | accepted correct / expected |\n|---|---:|---:|\n")
	cell := func(t TierMetrics, k string) string {
		if cs := t.ReplyByClass[k]; cs != nil {
			return fmt.Sprintf("%d / %d", cs.Correct, cs.Expected)
		}
		return "0 / 0"
	}
	for _, k := range names {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", k, cell(res.Tiers[TierGold], k), cell(res.Tiers[TierAccepted], k))
	}
	return b.String()
}

func renderMarkdown(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# commentbench (%s replies)\n\nPublisher capabilities: changed files %t, thread resolution %t.\n\n", res.Replies, res.Capabilities["changed_files"], res.Capabilities["resolve_thread"])
	b.WriteString(metricsTable(res))
	b.WriteString("\n")
	b.WriteString(classTable(res))
	if len(res.Failures) > 0 {
		b.WriteString("\nGold thresholds failed: " + strings.Join(res.Failures, "; ") + "\n")
	}
	b.WriteString("\n## Per-case differences\n\n")
	for _, s := range res.Cases {
		if len(s.Diffs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s (%s)\n\n", s.ID, s.Tier)
		for _, d := range s.Diffs {
			where := fmt.Sprintf("round %d", d.Round+1)
			if d.Round < 0 {
				where = "reply"
			}
			if d.CommentID != 0 {
				where += fmt.Sprintf(", comment %d", d.CommentID)
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", d.Kind, where, d.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}
