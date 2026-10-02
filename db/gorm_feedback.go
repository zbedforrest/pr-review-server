package db

import (
	"time"

	"gorm.io/gorm/clause"
)

// FeedbackItemModel is one piece of author feedback on a PRism comment: a
// reply under an inline finding, a conversation comment that names the bot,
// or a reaction on a PRism comment. One row per GitHub item, so the daily
// scan can be re-run without re-classifying.
type FeedbackItemModel struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	Source     string    `gorm:"size:16;not null;uniqueIndex:idx_feedback_items_unique"`
	ItemID     int64     `gorm:"not null;uniqueIndex:idx_feedback_items_unique"`
	CommentID  int64     `gorm:"not null;default:0"`
	RepoOwner  string    `gorm:"size:255;not null"`
	RepoName   string    `gorm:"size:255;not null"`
	PRNumber   int       `gorm:"not null"`
	Author     string    `gorm:"size:255;not null"`
	Body       string    `gorm:"type:text"`
	ReplyClass string    `gorm:"size:16;not null;default:''"`
	Reaction   string    `gorm:"size:16;not null;default:''"`
	Label      string    `gorm:"size:16;not null;index"`
	Classifier string    `gorm:"size:64;not null;default:''"`
	URL        string    `gorm:"size:512;not null;default:''"`
	CreatedAt  time.Time `gorm:"not null;index"`
	ObservedAt time.Time `gorm:"not null"`
}

func (FeedbackItemModel) TableName() string { return "feedback_items" }

type FeedbackItem struct {
	ID         uint
	Source     string
	ItemID     int64
	CommentID  int64
	RepoOwner  string
	RepoName   string
	PRNumber   int
	Author     string
	Body       string
	ReplyClass string
	Reaction   string
	Label      string
	Classifier string
	URL        string
	CreatedAt  time.Time
	ObservedAt time.Time
}

// FeedbackKey identifies one stored item: the source and GitHub's id for it.
type FeedbackKey struct {
	Source string
	ItemID int64
}

// FeedbackTarget is a PR the publication ledger knows, with the GitHub ids of
// the inline roots and summary comments PRism posted there and the reply
// classes the reply ledger already assigned to author comments.
type FeedbackTarget struct {
	RepoOwner    string
	RepoName     string
	PRNumber     int
	Roots        map[int64]bool
	Summaries    map[int64]bool
	ReplyClasses map[int64]string
}

// ListFeedbackTargets returns the PRs worth scanning for feedback: every open
// PR with PRism comments, and closed ones that got a PRism comment after
// recentSince.
func (g *GormDB) ListFeedbackTargets(recentSince time.Time) ([]FeedbackTarget, error) {
	var rows []struct {
		RepoOwner string
		RepoName  string
		PRNumber  int
		Kind      string
		CommentID int64
	}
	err := g.db.Table("published_findings AS pf").
		Select("pf.repo_owner, pf.repo_name, pf.pr_number, pf.kind, pf.comment_id").
		Joins("JOIN prs ON prs.repo_owner = pf.repo_owner AND prs.repo_name = pf.repo_name AND prs.pr_number = pf.pr_number").
		Where("pf.comment_id <> 0 AND pf.kind IN ? AND (LOWER(prs.pr_state) = 'open' OR pf.published_at >= ?)", []string{PublishedKindSummary, PublishedKindFinding}, recentSince).
		Order("pf.repo_owner, pf.repo_name, pf.pr_number, pf.comment_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	var out []FeedbackTarget
	index := map[[3]any]int{}
	for _, row := range rows {
		key := [3]any{row.RepoOwner, row.RepoName, row.PRNumber}
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, FeedbackTarget{RepoOwner: row.RepoOwner, RepoName: row.RepoName, PRNumber: row.PRNumber,
				Roots: map[int64]bool{}, Summaries: map[int64]bool{}, ReplyClasses: map[int64]string{}})
		}
		if row.Kind == PublishedKindSummary {
			out[i].Summaries[row.CommentID] = true
		} else {
			out[i].Roots[row.CommentID] = true
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	var replies []struct {
		RepoOwner       string
		RepoName        string
		PRNumber        int
		AuthorCommentID int64
		Class           string
	}
	if err := g.db.Model(&PublishedReplyModel{}).Where("created_at >= ?", recentSince).
		Select("repo_owner, repo_name, pr_number, author_comment_id, class").Scan(&replies).Error; err != nil {
		return nil, err
	}
	for _, r := range replies {
		if i, ok := index[[3]any{r.RepoOwner, r.RepoName, r.PRNumber}]; ok {
			out[i].ReplyClasses[r.AuthorCommentID] = r.Class
		}
	}
	return out, nil
}

// KnownFeedbackItems returns the keys already stored for a PR.
func (g *GormDB) KnownFeedbackItems(owner, repo string, number int) (map[FeedbackKey]bool, error) {
	var keys []FeedbackKey
	err := g.db.Model(&FeedbackItemModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, number).
		Select("source, item_id").Scan(&keys).Error
	if err != nil {
		return nil, err
	}
	known := make(map[FeedbackKey]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	return known, nil
}

// SaveFeedbackItems stores new items and reports how many were new; an item
// already stored under its key is left as it was.
func (g *GormDB) SaveFeedbackItems(items []FeedbackItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	models := make([]FeedbackItemModel, 0, len(items))
	for _, it := range items {
		models = append(models, FeedbackItemModel{
			Source: it.Source, ItemID: it.ItemID, CommentID: it.CommentID,
			RepoOwner: it.RepoOwner, RepoName: it.RepoName, PRNumber: it.PRNumber,
			Author: it.Author, Body: it.Body, ReplyClass: it.ReplyClass, Reaction: it.Reaction,
			Label: it.Label, Classifier: it.Classifier, URL: it.URL,
			CreatedAt: it.CreatedAt, ObservedAt: it.ObservedAt,
		})
	}
	res := g.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&models)
	return int(res.RowsAffected), res.Error
}

// ListFeedbackItems returns the items whose GitHub time falls in [start, end),
// newest first.
func (g *GormDB) ListFeedbackItems(start, end time.Time) ([]FeedbackItem, error) {
	var models []FeedbackItemModel
	if err := g.db.Where("created_at >= ? AND created_at < ?", start, end).Order("created_at DESC, id DESC").Find(&models).Error; err != nil {
		return nil, err
	}
	out := make([]FeedbackItem, 0, len(models))
	for _, m := range models {
		out = append(out, FeedbackItem(m))
	}
	return out, nil
}
