package main

import (
	"testing"
	"time"
)

func summaryDump(reviews string, created, updated time.Time) *prDump {
	d := &prDump{Number: 5, Owner: "acme", Repo: "example", HeadRefOid: "5a5a5a5555555555555555555555555555555555"}
	d.Commits.Nodes = []dumpCommit{{}}
	d.Commits.Nodes[0].Commit.Oid = "5a5a5a5555555555555555555555555555555555"
	d.Comments.Nodes = []dumpIssueComment{{DatabaseID: 1, Author: &dumpActor{Login: "acme-bot"}, CreatedAt: created, UpdatedAt: updated,
		Body: "<!-- prism:summary:v1 -->\n### PRism review\n\n<sub>Reviews (" + reviews + ") · reviewed 5a5a5a5 · 2 changed files</sub>\n"}}
	return d
}

func TestRounds_SummaryOnlyRoundComesFromTheStickyComment(t *testing.T) {
	created, updated := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	d := summaryDump("1", created, updated)
	bots := d.botLogins()
	if !bots["acme-bot"] {
		t.Fatalf("the summary's author is the bot: %v", bots)
	}
	rounds := d.rounds(bots)
	if len(rounds) != 1 || rounds[0].SHA7 != "5a5a5a5" || rounds[0].SHA != d.HeadRefOid || !rounds[0].At.Equal(created) {
		t.Fatalf("a single-round summary is timed at its creation and widened to the full sha: %+v", rounds)
	}
	if rounds := summaryDump("3", created, updated).rounds(bots); len(rounds) != 1 || !rounds[0].At.Equal(updated) {
		t.Fatalf("a rewritten summary records only its last round, at the last edit: %+v", rounds)
	}
}

func TestRounds_SummaryRoundNeverDuplicatesAReview(t *testing.T) {
	created := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	d := summaryDump("1", created, created)
	d.Reviews.Nodes = []dumpReview{{Author: &dumpActor{Login: "acme-bot"}, State: "COMMENTED", SubmittedAt: created.Add(-time.Minute)}}
	d.Reviews.Nodes[0].Commit.Oid = d.HeadRefOid
	if rounds := d.rounds(d.botLogins()); len(rounds) != 1 || !rounds[0].At.Equal(created.Add(-time.Minute)) {
		t.Fatalf("the review's round wins over the summary's: %+v", rounds)
	}
}

func TestSummaryNotes_ParsesFileLinkedBulletsOnly(t *testing.T) {
	body := "- **[MEDIUM]** Inline one — [`a.go:10`](https://github.com/acme/example/pull/5#discussion_r77)\n" +
		"<details><summary>2 folded notes</summary>\n\n" +
		"- <img alt=\"LOW\" src=\"x\"> The mapping keys on `.id` — [`mapping.yml:175`](https://github.com/acme/example/blob/5a5a5a5/svc/mapping.yml#L175)\n" +
		"- **[LOW]** Another note — [`b.go:3`](https://github.com/acme/example/blob/5a5a5a5/pkg/b.go#L3)\n</details>\n"
	notes := summaryNotes(body)
	if len(notes) != 2 || notes[0].File != "svc/mapping.yml" || notes[0].Line != 175 || notes[0].Text != "The mapping keys on `.id`" || notes[1].File != "pkg/b.go" || notes[1].Line != 3 {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestExternalThreadsBefore_ResolvedOnlyOnceSomeoneReplied(t *testing.T) {
	yes := true
	d := &prDump{Number: 9, Owner: "acme", Repo: "example"}
	t0 := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	mk := func(id int64, login string, resolved *bool, replyAt time.Time) {
		var th dumpThread
		th.Path, th.OriginalLine, th.IsResolved = "a.go", 10, resolved
		th.Comments.Nodes = []dumpThreadComment{{DatabaseID: flexInt64(id), Author: &dumpActor{Login: login}, Body: "root", CreatedAt: t0}}
		if !replyAt.IsZero() {
			reply := dumpThreadComment{DatabaseID: flexInt64(id + 1), Author: &dumpActor{Login: "dev"}, Body: "answered", CreatedAt: replyAt}
			reply.ReplyTo = &struct {
				DatabaseID flexInt64 `json:"databaseId"`
			}{DatabaseID: flexInt64(id)}
			th.Comments.Nodes = append(th.Comments.Nodes, reply)
		}
		d.ReviewThreads.Nodes = append(d.ReviewThreads.Nodes, th)
	}
	mk(1, "greptile-apps[bot]", &yes, t0.Add(10*time.Minute))
	mk(10, "greptile-apps[bot]", &yes, time.Time{})
	mk(20, "greptile-apps[bot]", &yes, t0.Add(2*time.Hour))
	mk(30, "acme-bot", &yes, t0.Add(10*time.Minute))
	threads := externalThreadsBefore(d, map[string]bool{"acme-bot": true}, t0.Add(time.Hour))
	if len(threads) != 3 || threads[0].RootCommentID != 1 || !threads[0].Resolved || threads[1].RootCommentID != 10 || threads[1].Resolved || threads[2].RootCommentID != 20 || threads[2].Resolved {
		t.Fatalf("threads = %+v", threads)
	}
}
