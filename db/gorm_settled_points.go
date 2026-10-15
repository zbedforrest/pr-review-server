package db

import (
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

func settledPointModelToDomain(m *SettledPointModel) SettledPoint {
	return SettledPoint{
		ID: int(m.ID), RepoOwner: m.RepoOwner, RepoName: m.RepoName, PRNumber: m.PRNumber, CommentID: m.CommentID,
		Author: m.Author, Source: m.Source, Fingerprint: m.Fingerprint, File: m.File, FindingKind: m.FindingKind,
		Subjects: m.Subjects, Headline: m.Headline, Text: m.Text, Reason: m.Reason, SettledAt: m.SettledAt,
	}
}

// RecordSettledPoint stores a settled point once per thread: a second
// record for the same (owner, repo, pr, comment) is a no-op, so the first
// settlement's time and reason stand.
func (g *GormDB) RecordSettledPoint(p *SettledPoint) error {
	model := SettledPointModel{
		RepoOwner: p.RepoOwner, RepoName: p.RepoName, PRNumber: p.PRNumber, CommentID: p.CommentID,
		Author: strings.ToLower(strings.TrimSpace(p.Author)), Source: p.Source, Fingerprint: p.Fingerprint, File: p.File,
		FindingKind: p.FindingKind, Subjects: p.Subjects, Headline: p.Headline, Text: p.Text, Reason: p.Reason, SettledAt: p.SettledAt,
	}
	return g.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model).Error
}

// ListSettledPoints returns the points an author settled in a repo with a
// settlement time in (since, until], newest first. Besides the recorded
// rows it derives one from every dismissed ledger row of the author's PRs
// whose reply ledger holds the author's verdict or PRism's concession, so
// points settled before the table existed are remembered too; a recorded
// row for the same thread wins.
func (g *GormDB) ListSettledPoints(owner, repo, author string, since, until time.Time) ([]SettledPoint, error) {
	author = strings.ToLower(strings.TrimSpace(author))
	var models []SettledPointModel
	err := g.db.Where("repo_owner = ? AND repo_name = ? AND author = ? AND settled_at > ? AND settled_at <= ?", owner, repo, author, since, until).
		Order("settled_at DESC, id DESC").Find(&models).Error
	if err != nil {
		return nil, err
	}
	out := make([]SettledPoint, 0, len(models))
	seen := map[string]bool{}
	for i := range models {
		p := settledPointModelToDomain(&models[i])
		seen[settledKey(p.PRNumber, p.Fingerprint, p.CommentID)] = true
		out = append(out, p)
	}
	derived, err := g.derivedSettledPoints(owner, repo, author, since, until)
	if err != nil {
		return nil, err
	}
	for _, p := range derived {
		if !seen[settledKey(p.PRNumber, p.Fingerprint, p.CommentID)] {
			out = append(out, p)
		}
	}
	return out, nil
}

func settledKey(pr int, fingerprint string, commentID int64) string {
	if commentID != 0 {
		return strconv.Itoa(pr) + ":c:" + strconv.FormatInt(commentID, 10)
	}
	return strconv.Itoa(pr) + ":f:" + fingerprint
}

// derivedSettledPoints reads the author's dismissed ledger rows posted
// since the window opened, each dated by the reply that settled it.
func (g *GormDB) derivedSettledPoints(owner, repo, author string, since, until time.Time) ([]SettledPoint, error) {
	var rows []PublishedFindingModel
	err := g.db.Table("published_findings AS pf").
		Select("pf.*").
		Joins("JOIN prs ON prs.repo_owner = pf.repo_owner AND prs.repo_name = pf.repo_name AND prs.pr_number = pf.pr_number").
		Where("pf.repo_owner = ? AND pf.repo_name = ? AND LOWER(prs.author) = ? AND pf.state = ? AND pf.kind IN ? AND pf.published_at > ?",
			owner, repo, author, PublishedStateDismissed, []string{PublishedKindFinding, PublishedKindAnnotation}, since).
		Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var out []SettledPoint
	for i := range rows {
		row := &rows[i]
		var replies []PublishedReplyModel
		if err := g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND fingerprint = ?", owner, repo, row.PRNumber, row.Fingerprint).
			Order("created_at ASC, id ASC").Find(&replies).Error; err != nil {
			return nil, err
		}
		var settledAt time.Time
		reason, source := "", ""
		for _, r := range replies {
			switch {
			case r.Class == "verdict":
				settledAt, reason, source = r.CreatedAt, r.Body, SettledSourceVerdict
			case r.Decision == "concede" || r.Decision == "withdraw":
				settledAt, reason, source = r.CreatedAt, r.Body, SettledSourceConceded
			}
		}
		if settledAt.IsZero() || !settledAt.After(since) || settledAt.After(until) {
			continue
		}
		out = append(out, SettledPoint{
			RepoOwner: owner, RepoName: repo, PRNumber: row.PRNumber, CommentID: row.CommentID, Author: author, Source: source,
			Fingerprint: row.Fingerprint, File: FingerprintFile(row.Fingerprint), FindingKind: row.FindingKind, Subjects: row.Subjects,
			Text: strings.TrimSpace(row.CommentText + " " + reason), Reason: reason, SettledAt: settledAt,
		})
	}
	return out, nil
}

// FingerprintFile reads the file out of a file:bucket:hash fingerprint.
func FingerprintFile(fp string) string {
	i := strings.LastIndex(fp, ":")
	if i < 0 {
		return fp
	}
	j := strings.LastIndex(fp[:i], ":")
	if j < 0 {
		return fp[:i]
	}
	return fp[:j]
}
