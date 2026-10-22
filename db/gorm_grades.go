package db

import (
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm/clause"

	"pr-review-server/pkg/health"
)

// CommentGradeModel is the daily grader's verdict on one posted comment.
// Ref names the ledger row it grades ("f:<published_findings.id>" or
// "r:<published_replies.id>"), so a comment is graded once.
type CommentGradeModel struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	Ref        string    `gorm:"size:64;not null;uniqueIndex"`
	RepoOwner  string    `gorm:"size:255;not null"`
	RepoName   string    `gorm:"size:255;not null"`
	PRNumber   int       `gorm:"not null"`
	Kind       string    `gorm:"size:16;not null"`
	Severity   string    `gorm:"size:16;not null;default:''"`
	HeadSHA    string    `gorm:"size:40;not null;default:''"`
	CommentURL string    `gorm:"type:text"`
	PostedAt   time.Time `gorm:"not null"`
	Grade      string    `gorm:"size:8;not null"`
	Category   string    `gorm:"size:64;not null;default:''"`
	Reason     string    `gorm:"type:text"`
	Model      string    `gorm:"size:64;not null;default:''"`
	CostUSD    float64   `gorm:"not null;default:0"`
	GradedAt   time.Time `gorm:"not null;index"`
}

func (CommentGradeModel) TableName() string { return "comment_grades" }

// GradeCandidate is one posted comment the grader may grade.
type GradeCandidate struct {
	Ref              string
	Kind             string // "inline", "summary_note" or "reply"
	RepoOwner        string
	RepoName         string
	PRNumber         int
	Fingerprint      string
	Severity         string
	Line             int
	Text             string
	AuthorText       string
	HeadSHA          string
	MergeBase        string
	CommentID        int64
	SummaryCommentID int64
	PostedAt         time.Time
}

// GradeCandidates lists comments first posted in [start, end) that carry
// text and have no grade yet: inline findings, summary notes and PRism's
// text replies to authors, oldest first.
func (g *GormDB) GradeCandidates(start, end time.Time) ([]GradeCandidate, error) {
	var rows []PublishedFindingModel
	err := g.db.Where("published_at >= ? AND published_at < ? AND comment_text <> ''", start, end).
		Where("(kind = ? AND comment_id <> 0) OR kind = ?", PublishedKindFinding, PublishedKindAnnotation).
		Where("NOT EXISTS (SELECT 1 FROM comment_grades cg WHERE cg.ref = 'f:' || CAST(published_findings.id AS TEXT))").
		Order("published_at, id").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	summaries := map[[3]any]PublishedFindingModel{}
	summaryOf := func(owner, repo string, number int) (PublishedFindingModel, error) {
		key := [3]any{owner, repo, number}
		if s, ok := summaries[key]; ok {
			return s, nil
		}
		var s PublishedFindingModel
		err := g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND kind = ?", owner, repo, number, PublishedKindSummary).
			Limit(1).Find(&s).Error
		summaries[key] = s
		return s, err
	}
	var out []GradeCandidate
	for _, r := range rows {
		s, err := summaryOf(r.RepoOwner, r.RepoName, r.PRNumber)
		if err != nil {
			return nil, err
		}
		kind := "inline"
		if r.Kind == PublishedKindAnnotation {
			kind = "summary_note"
		}
		out = append(out, GradeCandidate{
			Ref: refFinding(r.ID), Kind: kind, RepoOwner: r.RepoOwner, RepoName: r.RepoName, PRNumber: r.PRNumber,
			Fingerprint: r.Fingerprint, Severity: r.Severity, Line: r.Line, Text: r.CommentText,
			HeadSHA: r.ReviewedSHA, MergeBase: s.MergeBase, CommentID: r.CommentID, SummaryCommentID: s.CommentID, PostedAt: r.PublishedAt,
		})
	}

	var replies []PublishedReplyModel
	err = g.db.Where("replied_at >= ? AND replied_at < ? AND reply_body <> '' AND reply_comment_id <> 0", start, end).
		Where("NOT EXISTS (SELECT 1 FROM comment_grades cg WHERE cg.ref = 'r:' || CAST(published_reply_models.id AS TEXT))").
		Order("replied_at, id").Find(&replies).Error
	if err != nil {
		return nil, err
	}
	for _, r := range replies {
		s, err := summaryOf(r.RepoOwner, r.RepoName, r.PRNumber)
		if err != nil {
			return nil, err
		}
		head := r.DecisionHead
		if head == "" {
			head = s.LastSeenSHA
		}
		out = append(out, GradeCandidate{
			Ref: refReply(r.ID), Kind: "reply", RepoOwner: r.RepoOwner, RepoName: r.RepoName, PRNumber: r.PRNumber,
			Fingerprint: r.Fingerprint, Text: r.ReplyBody, AuthorText: r.Body,
			HeadSHA: head, MergeBase: s.MergeBase, CommentID: r.ReplyCommentID, PostedAt: *r.RepliedAt,
		})
	}
	return out, nil
}

func refFinding(id uint) string { return "f:" + strconv.FormatUint(uint64(id), 10) }
func refReply(id uint) string   { return "r:" + strconv.FormatUint(uint64(id), 10) }

// SaveCommentGrades stores grades; a comment already graded keeps its first grade.
func (g *GormDB) SaveCommentGrades(grades []CommentGradeModel) error {
	if len(grades) == 0 {
		return nil
	}
	return g.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "ref"}}, DoNothing: true}).Create(&grades).Error
}

// GradeSpendSince is what grading has cost since t.
func (g *GormDB) GradeSpendSince(t time.Time) (float64, error) {
	var total float64
	err := g.db.Model(&CommentGradeModel{}).Where("graded_at >= ?", t).Select("COALESCE(SUM(cost_usd), 0)").Scan(&total).Error
	return total, err
}

// qualityMetrics summarises grades made in [start, end) plus a daily BAD
// rate, by the day comments were posted, for the seven days ending at end.
func (g *GormDB) qualityMetrics(start, end time.Time) (health.QualityMetrics, error) {
	q := health.QualityMetrics{ByGrade: map[string]int{}, ByKind: map[string]map[string]int{}, BadCategories: map[string]int{}}
	var rows []CommentGradeModel
	if err := g.db.Where("graded_at >= ? AND graded_at < ?", start, end).Order("graded_at").Find(&rows).Error; err != nil {
		return q, err
	}
	for _, r := range rows {
		q.Graded++
		q.CostUSD += r.CostUSD
		q.ByGrade[r.Grade]++
		if q.ByKind[r.Kind] == nil {
			q.ByKind[r.Kind] = map[string]int{}
		}
		q.ByKind[r.Kind][r.Grade]++
		if r.Grade == "BAD" {
			if r.Category != "" && r.Category != "-" {
				q.BadCategories[r.Category]++
			}
			q.Worst = append(q.Worst, health.QualityExample{Kind: r.Kind, Severity: r.Severity, Category: r.Category, Reason: r.Reason, URL: r.CommentURL})
		}
	}
	sort.SliceStable(q.Worst, func(i, j int) bool { return severityRank(q.Worst[i].Severity) > severityRank(q.Worst[j].Severity) })
	if len(q.Worst) > 3 {
		q.Worst = q.Worst[:3]
	}
	for d := 6; d >= 0; d-- {
		dayEnd := end.Add(-time.Duration(d) * 24 * time.Hour)
		var total, bad int64
		day := g.db.Model(&CommentGradeModel{}).Where("posted_at >= ? AND posted_at < ?", dayEnd.Add(-24*time.Hour), dayEnd)
		if err := day.Count(&total).Error; err != nil {
			return q, err
		}
		if err := g.db.Model(&CommentGradeModel{}).Where("posted_at >= ? AND posted_at < ? AND grade = ?", dayEnd.Add(-24*time.Hour), dayEnd, "BAD").Count(&bad).Error; err != nil {
			return q, err
		}
		q.Trend = append(q.Trend, health.QualityDay{Date: dayEnd.Add(-time.Second).UTC().Format("2006-01-02"), Graded: int(total), Bad: int(bad)})
	}
	return q, nil
}

func severityRank(s string) int {
	switch s {
	case "critical", "CRITICAL":
		return 3
	case "medium", "MEDIUM":
		return 2
	case "low", "LOW":
		return 1
	}
	return 0
}
