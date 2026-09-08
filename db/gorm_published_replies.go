package db

import (
	"time"

	"gorm.io/gorm/clause"
)

// RecordPublishedReply stores an author reply once; a second call for the
// same author comment is a no-op and reports created=false.
func (g *GormDB) RecordPublishedReply(r *PublishedReply) (bool, error) {
	model := PublishedReplyModel{
		RepoOwner:       r.RepoOwner,
		RepoName:        r.RepoName,
		PRNumber:        r.PRNumber,
		AuthorCommentID: r.AuthorCommentID,
		RootCommentID:   r.RootCommentID,
		Fingerprint:     r.Fingerprint,
		AuthorID:        r.AuthorID,
		Class:           r.Class,
		Action:          r.Action,
		Body:            r.Body,
		ReplyCommentID:  r.ReplyCommentID,
		CreatedAt:       r.CreatedAt,
		ProcessedAt:     time.Now().UTC(),
	}
	res := g.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// GetPublishedReplyIDsForPR returns the author comment ids already handled.
func (g *GormDB) GetPublishedReplyIDsForPR(owner, repo string, number int) (map[int64]bool, error) {
	var ids []int64
	err := g.db.Model(&PublishedReplyModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, number).
		Pluck("author_comment_id", &ids).Error
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	return seen, nil
}

// ListPublishedReplyTargets returns every open, non-draft PR with inline
// comments PRism has posted, in any finding state: authors reply under fixed
// findings too. Closed PRs and PRs cleanup already removed are excluded here
// so the scan never grows with history.
func (g *GormDB) ListPublishedReplyTargets() ([]PublishedReplyTarget, error) {
	var rows []struct {
		RepoOwner   string
		RepoName    string
		PRNumber    int
		CommentID   int64
		Fingerprint string
	}
	err := g.db.Table("published_findings AS pf").
		Select("pf.repo_owner, pf.repo_name, pf.pr_number, pf.comment_id, pf.fingerprint").
		Joins("JOIN prs ON prs.repo_owner = pf.repo_owner AND prs.repo_name = pf.repo_name AND prs.pr_number = pf.pr_number").
		Where("pf.kind = ? AND pf.comment_id <> 0 AND LOWER(prs.pr_state) = 'open' AND NOT prs.draft", PublishedKindFinding).
		Order("pf.repo_owner, pf.repo_name, pf.pr_number, pf.comment_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	var out []PublishedReplyTarget
	for _, row := range rows {
		n := len(out)
		if n == 0 || out[n-1].RepoOwner != row.RepoOwner || out[n-1].RepoName != row.RepoName || out[n-1].PRNumber != row.PRNumber {
			out = append(out, PublishedReplyTarget{RepoOwner: row.RepoOwner, RepoName: row.RepoName, PRNumber: row.PRNumber, Roots: map[int64]string{}})
			n++
		}
		out[n-1].Roots[row.CommentID] = row.Fingerprint
	}
	return out, nil
}
