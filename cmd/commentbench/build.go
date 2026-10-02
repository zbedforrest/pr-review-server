package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"pr-review-server/internal/replaykit"
	"pr-review-server/pkg/publisher"
	"pr-review-server/pkg/publisher/replytext"
	"pr-review-server/pkg/reviewer/payload"
	"pr-review-server/pkg/reviewer/reconcile"
)

// BuildOptions are the builder's inputs. Store supplies sidecars and
// compares (cache first, then the network when it has a fetcher).
type BuildOptions struct {
	Audit       *audit
	DumpsDir    string
	Store       *replaykit.Store
	ReplyLedger map[int64]RecordedDecision
	Logf        func(string, ...any)
}

func (o BuildOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// ManifestEntry is one line of manifest.json.
type ManifestEntry struct {
	ID             string   `json:"id"`
	File           string   `json:"file,omitempty"`
	Tier           string   `json:"tier"`
	Why            []string `json:"why,omitempty"`
	Rounds         int      `json:"rounds"`
	RoundsReplay   int      `json:"rounds_with_sidecar"`
	ExpectPost     int      `json:"expect_post"`
	ExpectSuppress int      `json:"expect_suppress"`
	Unmapped       int      `json:"findings_unmapped"`
	Summaries      int      `json:"summaries"`
	Resolutions    int      `json:"resolutions"`
	Replies        int      `json:"replies"`
	Summary        string   `json:"summary"`
}

type Manifest struct {
	Generated time.Time       `json:"generated"`
	Counts    map[string]int  `json:"counts"`
	Cases     []ManifestEntry `json:"cases"`
}

// buildAll writes one case file per audited PR plus manifest.json into out.
func buildAll(o BuildOptions, out string) (Manifest, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Join(out, "cases"), 0o755); err != nil {
		return Manifest{}, err
	}
	ex := indexExamples(o.Audit.Verified)
	o.Logf = logf
	m := Manifest{Generated: time.Now().UTC(), Counts: map[string]int{}}
	for _, apr := range o.Audit.PerPR {
		c, entry, err := buildCase(o, apr, ex)
		if err != nil {
			logf("%s: %v", apr.Key, err)
			entry = ManifestEntry{ID: apr.Key, Tier: TierExcluded, Why: []string{err.Error()}}
		}
		if c != nil {
			name := strings.NewReplacer("/", "__", "#", "__").Replace(apr.Key) + ".json"
			entry.File = filepath.Join("cases", name)
			if err := writeJSON(filepath.Join(out, entry.File), c); err != nil {
				return m, err
			}
		}
		m.Counts[entry.Tier]++
		m.Cases = append(m.Cases, entry)
	}
	return m, writeJSON(filepath.Join(out, "manifest.json"), m)
}

var auditKeyRe = regexp.MustCompile(`^([^/]+)/([^#]+)#(\d+)$`)

func buildCase(o BuildOptions, apr auditPR, ex examples) (*Case, ManifestEntry, error) {
	entry := ManifestEntry{ID: apr.Key}
	km := auditKeyRe.FindStringSubmatch(apr.Key)
	if km == nil {
		return nil, entry, fmt.Errorf("bad key")
	}
	owner, repo := km[1], km[2]
	d, err := replaykit.LoadDump(filepath.Join(o.DumpsDir, fmt.Sprintf("%s__%s__%s.json", owner, repo, km[3])))
	if err != nil {
		return nil, entry, fmt.Errorf("dump: %w", err)
	}
	c := &Case{ID: apr.Key, Owner: owner, Repo: repo, Number: d.Number, Dump: *d, Sidecars: map[string]json.RawMessage{}, Compares: map[string][]string{}}
	bots := d.BotLogins()
	names := make([]string, 0, len(bots))
	for b := range bots {
		names = append(names, b)
	}
	sort.Strings(names)
	if len(names) > 1 {
		return nil, entry, fmt.Errorf("more than one PRism login: %v", names)
	}
	if len(names) == 1 {
		c.Bot = names[0]
	}
	if !apr.ReadOK {
		c.TierWhy = append(c.TierWhy, "read_ok false")
	}
	rounds := d.Rounds(bots)
	if len(rounds) == 0 {
		return nil, entry, fmt.Errorf("no PRism review rounds")
	}
	payloads := make([]*payload.Payload, len(rounds))
	for i, rd := range rounds {
		info := RoundInfo{Index: i, SHA: rd.SHA, At: rd.At}
		raw, ok, err := o.Store.Sidecar(owner, repo, d.Number, rd.SHA7)
		if err != nil {
			o.logf("%s: sidecar %s: %v; round left without a sidecar", apr.Key, rd.SHA7, err)
		}
		if err == nil && ok {
			if pl, derr := payload.Decode(raw); derr == nil {
				c.Sidecars[rd.SHA7] = raw
				payloads[i] = &pl
				info.HasSidecar = true
				entry.RoundsReplay++
			}
		}
		for _, cm := range d.ExternalCommentsBefore(bots, rd.At) {
			info.CommentsPresent = append(info.CommentsPresent, cm.ID)
		}
		if i > 0 {
			prev := rounds[i-1]
			for _, cm := range d.Commits.Nodes {
				if t := cm.Commit.CommittedDate; t.After(prev.At) && !t.After(rd.At) {
					info.CommitsSincePrev = append(info.CommitsSincePrev, replaykit.Short(cm.Commit.Oid))
				}
			}
			for _, p := range rounds[:i] {
				if p.SHA == rd.SHA {
					continue
				}
				cmp, known, err := o.Store.Compare(owner, repo, p.SHA, rd.SHA)
				if err != nil {
					o.logf("%s: compare %s...%s: %v; changed files unknown", apr.Key, replaykit.Short(p.SHA), rd.SHA7, err)
				}
				if err == nil && known {
					c.Compares[compareKey(p.SHA, rd.SHA)] = cmp.Files
				}
			}
			if files, ok := c.Compares[compareKey(prev.SHA, rd.SHA)]; ok {
				info.ChangedSincePrev = files
			}
		}
		c.Rounds = append(c.Rounds, info)
	}
	entry.Rounds = len(rounds)
	if entry.RoundsReplay == 0 {
		return nil, entry, fmt.Errorf("no sidecar for any round")
	}
	for _, t := range d.ReviewThreads.Nodes {
		for _, cm := range t.Comments.Nodes {
			if rec, ok := o.ReplyLedger[int64(cm.DatabaseID)]; ok {
				if c.Recorded == nil {
					c.Recorded = map[int64]RecordedDecision{}
				}
				c.Recorded[int64(cm.DatabaseID)] = rec
			}
		}
	}

	prismIDs := map[int64]bool{}
	for _, ac := range apr.Comments {
		prismIDs[ac.CommentID] = true
	}
	confirmed := normaliseExamples(ex.confirmed[apr.Key], prismIDs)
	refuted := normaliseExamples(ex.refuted[apr.Key], prismIDs)
	if wholePR(refuted) && len(confirmed) == 0 {
		c.TierWhy = append(c.TierWhy, "refuted by a verifier: "+strings.Join(covers(refuted, 0), ","))
	}

	b := caseBuilder{c: c, d: d, apr: apr, rounds: rounds, payloads: payloads, bots: bots, confirmed: confirmed, refuted: refuted}
	b.findings()
	b.replies()
	b.resolutions()
	b.summaries()
	sort.SliceStable(c.Expect.Resolutions, func(i, j int) bool { return c.Expect.Resolutions[i].CommentID < c.Expect.Resolutions[j].CommentID })
	b.tier()

	for _, f := range c.Expect.Findings {
		switch {
		case !f.mapped():
			entry.Unmapped++
		case f.Expect == ExpectPost:
			entry.ExpectPost++
		default:
			entry.ExpectSuppress++
		}
	}
	entry.Tier, entry.Why = c.Tier, c.TierWhy
	entry.Summaries, entry.Resolutions, entry.Replies = len(c.Expect.Summaries), len(c.Expect.Resolutions), len(c.Expect.Replies)
	entry.Summary = fmt.Sprintf("%d rounds; findings post %d / suppress %d (%s); replies %d; resolutions %d",
		entry.Rounds, entry.ExpectPost, entry.ExpectSuppress, reasonCounts(c.Expect.Findings), entry.Replies, entry.Resolutions)
	return c, entry, nil
}

func reasonCounts(fs []FindingExpect) string {
	n := map[string]int{}
	for _, f := range fs {
		if f.Expect == ExpectSuppress {
			n[f.Reason]++
		}
	}
	var parts []string
	for k, v := range n {
		parts = append(parts, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// normaliseExamples keeps only the PRism comment ids an example names. An
// example naming no id covers the PR; one naming only other reviewers'
// comments covers nothing here.
func normaliseExamples(list []example, prism map[int64]bool) []example {
	out := make([]example, 0, len(list))
	for _, e := range list {
		ids := map[int64]bool{}
		for id := range e.IDs {
			if prism[id] {
				ids[id] = true
			}
		}
		if len(e.IDs) > 0 && len(ids) == 0 {
			continue
		}
		out = append(out, example{Pattern: e.Pattern, IDs: ids})
	}
	return out
}

type caseBuilder struct {
	c         *Case
	d         *replaykit.PRDump
	apr       auditPR
	rounds    []replaykit.Round
	payloads  []*payload.Payload
	bots      map[string]bool
	confirmed []example
	refuted   []example

	byComment map[int64]*FindingExpect
	excluded  int
}

func threadOfComment(d *replaykit.PRDump, id int64) *replaykit.Thread {
	for i := range d.ReviewThreads.Nodes {
		for _, c := range d.ReviewThreads.Nodes[i].Comments.Nodes {
			if int64(c.DatabaseID) == id {
				return &d.ReviewThreads.Nodes[i]
			}
		}
	}
	return nil
}

func rootOf(t *replaykit.Thread) *replaykit.ThreadComment {
	if t == nil || len(t.Comments.Nodes) == 0 {
		return nil
	}
	return &t.Comments.Nodes[0]
}

// roundAt is the round whose submission is closest to t; review comments
// carry their review's submission time.
func (b *caseBuilder) roundAt(t time.Time) int {
	best, bestGap := -1, time.Duration(1<<62)
	for i, rd := range b.rounds {
		gap := rd.At.Sub(t)
		if gap < 0 {
			gap = -gap
		}
		if gap < bestGap {
			best, bestGap = i, gap
		}
	}
	if bestGap > 10*time.Minute {
		return -1
	}
	return best
}

var settlingClasses = map[string]bool{"fix_claim": true, "intentional_behavior": true, "pushback": true}

func (b *caseBuilder) byAuthor(hr auditResponse) bool {
	return strings.EqualFold(hr.Login, b.d.Author.Login)
}

// settles reports a reply that decides a finding: a fix claim from anyone
// (the commit settles it), a verdict or pushback from the PR author only.
func (b *caseBuilder) settles(hr auditResponse) bool {
	return settlingClasses[hr.Class] && (hr.Class == "fix_claim" || b.byAuthor(hr))
}

func (b *caseBuilder) findings() {
	b.byComment = map[int64]*FindingExpect{}
	labels := map[int64]auditComment{}
	for _, ac := range b.apr.Comments {
		labels[ac.CommentID] = ac
	}
	repliesByRoot := map[int64][]auditResponse{}
	for _, hr := range b.apr.HumanResponses {
		if root := rootOf(threadOfComment(b.d, hr.CommentID)); root != nil {
			repliesByRoot[int64(root.DatabaseID)] = append(repliesByRoot[int64(root.DatabaseID)], hr)
		}
	}
	defectOf := func(id int64) int64 {
		seen := map[int64]bool{}
		for {
			l, ok := labels[id]
			if !ok || !l.RedundantRepost || l.RepostOfCommentID == 0 || seen[id] {
				return id
			}
			seen[id] = true
			id = l.RepostOfCommentID
		}
	}
	for _, ac := range b.apr.Comments {
		fe := FindingExpect{CommentID: ac.CommentID, Round: -1, File: ac.Path, Line: ac.Line, Headline: ac.Headline, Defect: defectOf(ac.CommentID)}
		fe.Expect, fe.Reason, fe.Detail = expectationFor(ac)
		if t := threadOfComment(b.d, ac.CommentID); t != nil {
			root := rootOf(t)
			fe.Text = replaykit.StripMarkup(root.Body)
			if r := b.roundAt(root.CreatedAt); r >= 0 {
				fe.Round = r
				marker, _ := publisher.FindingIDFromBody(root.Body)
				var loose bool
				fe.FindingID, fe.Text, fe.Subjects, loose = b.mapFinding(r, marker, fe.File, fe.Line, fe.Text)
				if loose {
					fe.MappedBy = "alias"
				}
			}
		}
		if pats := covers(b.refuted, ac.CommentID); len(pats) > 0 && len(covers(b.confirmed, ac.CommentID)) == 0 {
			b.excluded++
			b.c.TierWhy = appendOnce(b.c.TierWhy, "comment expectations refuted: "+strings.Join(pats, ","))
			continue
		}
		for _, p := range covers(b.confirmed, ac.CommentID) {
			fe.Backed = appendOnce(fe.Backed, "confirmed:"+p)
		}
		for _, hr := range repliesByRoot[ac.CommentID] {
			if b.settles(hr) {
				fe.Backed = appendOnce(fe.Backed, "reply:"+hr.Class)
			}
		}
		fe.Gold = len(fe.Backed) > 0 && fe.mapped() && fe.MappedBy == ""
		b.c.Expect.Findings = append(b.c.Expect.Findings, fe)
	}
	sort.SliceStable(b.c.Expect.Findings, func(i, j int) bool {
		fi, fj := b.c.Expect.Findings[i], b.c.Expect.Findings[j]
		if fi.Round != fj.Round {
			return fi.Round < fj.Round
		}
		return fi.CommentID < fj.CommentID
	})
	for i := range b.c.Expect.Findings {
		b.byComment[b.c.Expect.Findings[i].CommentID] = &b.c.Expect.Findings[i]
	}
}

// expectationFor turns the analyst labels into post or suppress. A comment
// is worth posting only when it adds context, is not a repeat, was not
// already addressed, was first raised by PRism and is not wrong.
func expectationFor(ac auditComment) (expect, reason, detail string) {
	switch {
	case ac.RedundantRepost:
		return ExpectSuppress, ReasonRepost, fmt.Sprintf("repost of %d", ac.RepostOfCommentID)
	case ac.AddressedBeforePost:
		return ExpectSuppress, ReasonAddressed, ""
	case ac.FirstRaisedBy != "" && ac.FirstRaisedBy != "prism":
		return ExpectSuppress, ReasonExternal, "first raised by " + ac.FirstRaisedBy
	case ac.Correctness == "incorrect":
		return ExpectSuppress, ReasonIncorrect, ""
	case !ac.AddsContext:
		return ExpectSuppress, ReasonNoContext, ac.Correctness
	}
	return ExpectPost, "", ""
}

// mapFinding finds the sidecar finding a historical root was rendered from:
// the marker id when the round's sidecar still has it, else (loose) the
// closest finding on the same file and nearby line. The rendered body scores
// lower against raw prose than raw against raw, hence half the alias bar;
// a loose match never counts as gold.
func (b *caseBuilder) mapFinding(round int, marker, file string, line int, text string) (id, raw string, subjects []string, loose bool) {
	pl := b.payloads[round]
	if pl == nil {
		return "", text, nil, false
	}
	for _, f := range pl.Findings {
		if f.ID == marker {
			return f.ID, f.Comment, replaykit.Subjects(f), false
		}
	}
	best, bestScore := -1, 0.0
	for i, f := range pl.Findings {
		if !replaykit.SameFile(f.File, file) || (line > 0 && f.Line > 0 && absInt(f.Line-line) > replaykit.AliasLineTolerance) {
			continue
		}
		if s := reconcile.Similarity(text, f.Comment); s > bestScore {
			best, bestScore = i, s
		}
	}
	if best >= 0 && bestScore >= replaykit.AliasSimilarity/2 {
		f := pl.Findings[best]
		return f.ID, f.Comment, replaykit.Subjects(f), true
	}
	return "", text, nil, false
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// acceptedActions is what PRism should have done about a human reply, from
// the analyst's verdict on what it did and the reply class. A verdict
// ("intentional", "by design") settles the thread: dismissed, and a
// reaction is enough.
func acceptedActions(hr auditResponse, rec *RecordedDecision, label *auditComment) (accept []string, wantDismissed bool) {
	intent := hr.Class == "intentional_behavior"
	switch hr.PrismReplyQuality {
	case "unneeded":
		return []string{ActionReact}, intent
	case "held_wrongly":
		return []string{ActionConcede, ActionWithdraw}, true
	case "conceded_wrongly":
		return []string{ActionHold}, false
	case "argued_against_itself":
		return []string{ActionWithdraw}, true
	case "good":
		if intent {
			return []string{ActionReact, ActionConcede, ActionWithdraw}, true
		}
		if rec != nil && rec.Decision != "" && rec.Decision != publisher.DecisionAbstain && !rec.BudgetExhausted {
			return []string{rec.Decision}, false
		}
		classRule, _ := acceptedActions(auditResponse{Class: hr.Class, PrismReplyQuality: "missing_when_needed"}, nil, label)
		return append([]string{ActionReact}, classRule...), false
	}
	switch hr.Class {
	case "fix_claim":
		return []string{ActionConcede}, false
	case "intentional_behavior":
		return []string{ActionReact, ActionConcede, ActionWithdraw}, true
	case "pushback":
		if label != nil && (label.Correctness == "incorrect" || label.Correctness == "true_but_immaterial") {
			return []string{ActionConcede, ActionWithdraw}, true
		}
		return []string{ActionHold}, false
	case "question":
		return []string{ActionAnswer}, false
	}
	return []string{ActionReact}, false
}

func (b *caseBuilder) replies() {
	labels := map[int64]auditComment{}
	for _, ac := range b.apr.Comments {
		labels[ac.CommentID] = ac
	}
	for _, hr := range b.apr.HumanResponses {
		root := rootOf(threadOfComment(b.d, hr.CommentID))
		if root == nil || int64(root.DatabaseID) == hr.CommentID || !b.bots[replaykit.BareLogin(replaykit.ActorLogin(root.Author))] {
			continue
		}
		rootID := int64(root.DatabaseID)
		var rec *RecordedDecision
		if r, ok := b.c.Recorded[hr.CommentID]; ok {
			rec = &r
		}
		var label *auditComment
		if l, ok := labels[rootID]; ok {
			label = &l
		}
		if hr.PrismReplyQuality == "n/a" {
			continue
		}
		re := ReplyExpect{CommentID: hr.CommentID, RootCommentID: rootID, Author: hr.Login, At: hr.Created, Class: hr.Class, Quality: hr.PrismReplyQuality}
		re.Accept, re.WantDismissed = acceptedActions(hr, rec, label)
		re.ClassAccept, _ = acceptedActions(auditResponse{Class: hr.Class, PrismReplyQuality: "missing_when_needed"}, nil, label)
		if settlingClasses[hr.Class] || hr.Class == "ack" {
			re.Backed = append(re.Backed, "reply:"+hr.Class)
		}
		for _, p := range covers(b.confirmed, rootID) {
			re.Backed = appendOnce(re.Backed, "confirmed:"+p)
		}
		if len(covers(b.refuted, rootID)) > 0 && len(covers(b.confirmed, rootID)) == 0 {
			b.excluded++
			continue
		}
		re.Gold = len(re.Backed) > 0
		b.c.Expect.Replies = append(b.c.Expect.Replies, re)
	}
	sort.SliceStable(b.c.Expect.Replies, func(i, j int) bool { return b.c.Expect.Replies[i].At.Before(b.c.Expect.Replies[j].At) })
}

var shaRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// fixTime is when the fix a reply claims landed: the commit it names when
// the dump has it, else the reply itself.
func (b *caseBuilder) fixTime(hr auditResponse) (time.Time, string) {
	for _, m := range shaRe.FindAllString(strings.ToLower(hr.Quote), -1) {
		for _, cm := range b.d.Commits.Nodes {
			if strings.HasPrefix(cm.Commit.Oid, m) {
				return cm.Commit.CommittedDate, replaykit.Short(cm.Commit.Oid)
			}
		}
	}
	return hr.Created, ""
}

func (b *caseBuilder) resolutions() {
	seen := map[int64]bool{}
	for _, hr := range b.apr.HumanResponses {
		root := rootOf(threadOfComment(b.d, hr.CommentID))
		if root == nil {
			continue
		}
		fe := b.byComment[int64(root.DatabaseID)]
		if fe == nil || seen[fe.Defect] {
			continue
		}
		var res ResolutionExpect
		switch {
		case hr.Class == "fix_claim":
			at, sha := b.fixTime(hr)
			res = ResolutionExpect{Defect: fe.Defect, CommentID: fe.CommentID, FixedBy: -1, Why: "fix claim", Gold: sha != ""}
			if sha != "" {
				res.Why = "fix commit " + sha
			}
			for i, rd := range b.rounds {
				if rd.At.After(at) {
					res.FixedBy = i
					break
				}
			}
		case b.byAuthor(hr) && (hr.Class == "intentional_behavior" || replytext.AssertsIntent(hr.Quote)):
			res = ResolutionExpect{Defect: fe.Defect, CommentID: fe.CommentID, FixedBy: -1, Why: "author verdict", Gold: true}
		default:
			continue
		}
		seen[fe.Defect] = true
		b.c.Expect.Resolutions = append(b.c.Expect.Resolutions, res)
	}
}

// summaries derives the "Since last review" counts of each later round from
// the expectations: new is the defects that round should post; fixed is the
// posted defects whose fix landed since the previous round, or that are
// absent from the round's review while their cited file changed and do not
// come back later (policy R5); still open is the rest of the posted defects
// that no verdict settled. A round is scored only when every PRism root up to
// it is labelled. A fix found by absence also expects its thread resolved.
func (b *caseBuilder) summaries() {
	labelled := map[int64]bool{}
	for _, f := range b.c.Expect.Findings {
		labelled[f.CommentID] = f.mapped()
	}
	completeUpTo := len(b.rounds) - 1
	for _, t := range b.d.ReviewThreads.Nodes {
		root := rootOf(&t)
		if root == nil || root.ReplyTo != nil || !b.bots[replaykit.BareLogin(replaykit.ActorLogin(root.Author))] {
			continue
		}
		if _, ok := publisher.FindingIDFromBody(root.Body); !ok {
			continue
		}
		if !labelled[int64(root.DatabaseID)] {
			if r := b.roundAt(root.CreatedAt); r >= 0 && r-1 < completeUpTo {
				completeUpTo = r - 1
			}
		}
	}
	fixedAt := map[int64]int{}
	settledAt := map[int64]time.Time{}
	hasResolution := map[int64]bool{}
	for _, r := range b.c.Expect.Resolutions {
		hasResolution[r.Defect] = true
		if r.Why == "author verdict" {
			for _, re := range b.c.Expect.Replies {
				if fe := b.byComment[re.RootCommentID]; fe != nil && fe.Defect == r.Defect && re.WantDismissed {
					settledAt[r.Defect] = re.At
					break
				}
			}
			continue
		}
		if r.FixedBy >= 0 {
			fixedAt[r.Defect] = r.FixedBy
		}
	}
	first := map[int64]*FindingExpect{}
	lastRound := map[int64]int{}
	for i := range b.c.Expect.Findings {
		f := &b.c.Expect.Findings[i]
		if !f.mapped() {
			continue
		}
		if first[f.Defect] == nil {
			first[f.Defect] = f
		}
		if f.Round > lastRound[f.Defect] {
			lastRound[f.Defect] = f.Round
		}
	}
	lastSeen := map[int64]int{}
	for i := 1; i < len(b.rounds); i++ {
		if b.payloads[i] == nil {
			continue
		}
		posted := map[int64]bool{}
		gold := true
		for _, f := range b.c.Expect.Findings {
			if f.mapped() && f.Round < i && f.Expect == ExpectPost {
				posted[f.Defect] = true
				gold = gold && f.Gold
				if _, ok := lastSeen[f.Defect]; !ok {
					lastSeen[f.Defect] = f.Round
				}
			}
		}
		se := SummaryExpect{Round: i}
		for _, f := range b.c.Expect.Findings {
			if f.mapped() && f.Round == i && f.Expect == ExpectPost && !posted[f.Defect] {
				se.New++
				gold = gold && f.Gold
			}
		}
		for defect := range posted {
			if t, ok := settledAt[defect]; ok && t.Before(b.rounds[i].At) {
				continue
			}
			if by, ok := fixedAt[defect]; ok && by < i {
				continue
			}
			if by, ok := fixedAt[defect]; ok && by == i {
				se.Fixed++
				continue
			}
			fe := first[defect]
			if b.present(i, fe) {
				lastSeen[defect] = i
				se.StillOpen++
				continue
			}
			files, known := b.c.Compares[compareKey(b.rounds[lastSeen[defect]].SHA, b.rounds[i].SHA)]
			if lastRound[defect] <= i && known && replaykit.FileChanged(files, fe.File) {
				fixedAt[defect] = i
				se.Fixed++
				b.fixedByAbsence(defect, fe, i, hasResolution[defect])
				hasResolution[defect] = true
				continue
			}
			se.StillOpen++
		}
		se.Gold = gold
		if i <= completeUpTo {
			b.c.Expect.Summaries = append(b.c.Expect.Summaries, se)
		}
	}
}

// fixedByAbsence records a fix the review's silence corroborates: a new
// resolution, or an earlier round for a claimed fix whose commit the claim
// only named later.
func (b *caseBuilder) fixedByAbsence(defect int64, fe *FindingExpect, i int, has bool) {
	if !has {
		b.c.Expect.Resolutions = append(b.c.Expect.Resolutions, ResolutionExpect{Defect: defect, CommentID: fe.CommentID, FixedBy: i, Why: "absent and cited file changed", Gold: fe.Gold})
		return
	}
	for j := range b.c.Expect.Resolutions {
		r := &b.c.Expect.Resolutions[j]
		if r.Defect == defect && r.Why != "author verdict" && (r.FixedBy < 0 || r.FixedBy > i) {
			r.FixedBy = i
		}
	}
}

// present reports whether round i's review still raises the defect.
func (b *caseBuilder) present(i int, fe *FindingExpect) bool { return raises(b.payloads[i], fe) }

// raises reports whether a review payload raises the expectation's defect:
// its finding id or an active finding the alias rule matches.
func raises(pl *payload.Payload, fe *FindingExpect) bool {
	if pl == nil {
		return false
	}
	for _, f := range pl.Findings {
		if !f.Active && pl.SchemaVersion == payload.CurrentSchemaVersion {
			continue
		}
		if f.ID == fe.FindingID || replaykit.SameDefect(f.File, f.Line, f.Comment, replaykit.Subjects(f), fe.File, fe.Line, fe.Text, fe.Subjects) {
			return true
		}
	}
	return false
}

func (b *caseBuilder) tier() {
	c := b.c
	switch {
	case len(c.TierWhy) > 0 && (strings.HasPrefix(c.TierWhy[0], "read_ok") || strings.HasPrefix(c.TierWhy[0], "refuted")):
		c.Tier = TierExcluded
		return
	}
	scored := 0
	gold := b.excluded == 0
	for _, f := range c.Expect.Findings {
		if !f.mapped() {
			continue
		}
		scored++
		gold = gold && f.Gold
	}
	for _, r := range c.Expect.Replies {
		scored++
		gold = gold && r.Gold
	}
	for _, r := range c.Expect.Resolutions {
		gold = gold && r.Gold
	}
	for _, s := range c.Expect.Summaries {
		gold = gold && s.Gold
	}
	switch {
	case scored == 0:
		c.Tier = TierExcluded
		c.TierWhy = append(c.TierWhy, "no scorable expectation")
	case gold:
		c.Tier = TierGold
	default:
		c.Tier = TierAccepted
	}
}

// loadReplyLedger reads the reply ledger export (recent rows with the model's
// decision per author comment id).
func loadReplyLedger(path string) (map[int64]RecordedDecision, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exp struct {
		Recent []struct {
			AuthorCommentID int64  `json:"author_comment_id"`
			Decision        string `json:"decision"`
			ReplyBody       string `json:"reply_body"`
			Cited           string `json:"cited"`
			Note            string `json:"note"`
			Action          string `json:"action"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		return nil, err
	}
	out := map[int64]RecordedDecision{}
	for _, r := range exp.Recent {
		if r.Decision == "" {
			continue
		}
		d := RecordedDecision{Decision: r.Decision, Reply: r.ReplyBody, React: r.Action == publisher.ReplyActionReacted, BudgetExhausted: r.Note == publisher.NoteBudgetExhausted}
		if r.Cited != "" {
			_ = json.Unmarshal([]byte(r.Cited), &d.Cited)
		}
		out[r.AuthorCommentID] = d
	}
	return out, nil
}
