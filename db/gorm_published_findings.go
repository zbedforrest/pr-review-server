package db

import (
	"gorm.io/gorm/clause"
)

// Publication ledger persistence. Like finding outcomes, these methods are
// deliberately not on the shared Database interface; the publisher
// type-asserts the narrow capability it needs.

func publishedFindingModelToDomain(m *PublishedFindingModel) PublishedFinding {
	return PublishedFinding{
		ID:           int(m.ID),
		RepoOwner:    m.RepoOwner,
		RepoName:     m.RepoName,
		PRNumber:     m.PRNumber,
		Kind:         m.Kind,
		Fingerprint:  m.Fingerprint,
		SourceTag:    m.SourceTag,
		Severity:     m.Severity,
		ReviewedSHA:  m.ReviewedSHA,
		LastSeenSHA:  m.LastSeenSHA,
		CommentID:    m.CommentID,
		ThreadNodeID: m.ThreadNodeID,
		ReviewID:     m.ReviewID,
		CheckRunID:   m.CheckRunID,
		State:        m.State,
		PublishedAt:  m.PublishedAt,
	}
}

// UpsertPublishedFinding inserts the ledger row for a finding or, when the
// same (owner, repo, pr, fingerprint) was already published, updates its
// current state. ReviewedSHA (first publication) is preserved on conflict;
// GitHub ids are refreshed only when the caller supplies non-zero values so a
// later round that re-sees the finding cannot erase where it was posted.
func (g *GormDB) UpsertPublishedFinding(p *PublishedFinding) error {
	model := PublishedFindingModel{
		RepoOwner:    p.RepoOwner,
		RepoName:     p.RepoName,
		PRNumber:     p.PRNumber,
		Kind:         p.Kind,
		Fingerprint:  p.Fingerprint,
		SourceTag:    p.SourceTag,
		Severity:     p.Severity,
		ReviewedSHA:  p.ReviewedSHA,
		LastSeenSHA:  p.LastSeenSHA,
		CommentID:    p.CommentID,
		ThreadNodeID: p.ThreadNodeID,
		ReviewID:     p.ReviewID,
		CheckRunID:   p.CheckRunID,
		State:        p.State,
		PublishedAt:  p.PublishedAt,
	}
	updates := map[string]interface{}{
		"kind":          model.Kind,
		"source_tag":    model.SourceTag,
		"severity":      model.Severity,
		"last_seen_sha": model.LastSeenSHA,
		"state":         model.State,
	}
	if model.CommentID != 0 {
		updates["comment_id"] = model.CommentID
	}
	if model.ThreadNodeID != "" {
		updates["thread_node_id"] = model.ThreadNodeID
	}
	if model.ReviewID != 0 {
		updates["review_id"] = model.ReviewID
	}
	if model.CheckRunID != 0 {
		updates["check_run_id"] = model.CheckRunID
	}
	return g.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "repo_owner"},
			{Name: "repo_name"},
			{Name: "pr_number"},
			{Name: "fingerprint"},
		},
		DoUpdates: clause.Assignments(updates),
	}).Create(&model).Error
}

// GetPublishedFindingsForPR returns every ledger row for a PR (summary row
// included), oldest publication first.
func (g *GormDB) GetPublishedFindingsForPR(owner, repo string, prNumber int) ([]PublishedFinding, error) {
	var models []PublishedFindingModel
	err := g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ?", owner, repo, prNumber).
		Order("published_at ASC, id ASC").
		Find(&models).Error
	if err != nil {
		return nil, err
	}
	out := make([]PublishedFinding, 0, len(models))
	for i := range models {
		out = append(out, publishedFindingModelToDomain(&models[i]))
	}
	return out, nil
}
