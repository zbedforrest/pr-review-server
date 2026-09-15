package db

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WebhookDeliveryModel is one accepted GitHub webhook delivery, keyed by the
// X-GitHub-Delivery id so a redelivery is a no-op.
type WebhookDeliveryModel struct {
	DeliveryID     string    `gorm:"column:delivery_id;primaryKey;size:64"`
	Event          string    `gorm:"size:64;not null;default:''"`
	Action         string    `gorm:"size:64;not null;default:''"`
	InstallationID int64     `gorm:"column:installation_id;not null;default:0"`
	RepoOwner      string    `gorm:"size:255;not null"`
	RepoName       string    `gorm:"size:255;not null"`
	PRNumber       int       `gorm:"not null"`
	HeadSHA        string    `gorm:"column:head_sha;size:40;not null;default:''"`
	BaseSHA        string    `gorm:"column:base_sha;size:40;not null;default:''"`
	Author         string    `gorm:"size:255;not null;default:''"`
	Title          string    `gorm:"type:text"`
	Draft          bool      `gorm:"not null;default:false"`
	State          string    `gorm:"size:16;not null;default:''"`
	ReceivedAt     time.Time `gorm:"column:received_at;not null;index"`
}

func (WebhookDeliveryModel) TableName() string { return "webhook_deliveries" }

// AutoReviewIntentModel is one automatic review owed to a PR head. The unique
// target index is the dedup key shared by the webhook and the poll fallback;
// owner and repo are stored lowercased because GitHub treats them
// case-insensitively.
type AutoReviewIntentModel struct {
	ID          uint      `gorm:"primaryKey;autoIncrement"`
	RepoOwner   string    `gorm:"size:255;not null;uniqueIndex:idx_auto_review_intents_target,priority:1"`
	RepoName    string    `gorm:"size:255;not null;uniqueIndex:idx_auto_review_intents_target,priority:2"`
	PRNumber    int       `gorm:"not null;uniqueIndex:idx_auto_review_intents_target,priority:3"`
	HeadSHA     string    `gorm:"column:head_sha;size:40;not null;uniqueIndex:idx_auto_review_intents_target,priority:4"`
	Trigger     string    `gorm:"size:32;not null"`
	DeliveryID  string    `gorm:"column:delivery_id;size:64;not null;default:''"`
	Status      string    `gorm:"size:16;not null;index"`
	RunID       string    `gorm:"column:run_id;size:36;not null;default:'';index"`
	Publication string    `gorm:"size:128;not null;default:''"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

func (AutoReviewIntentModel) TableName() string { return "auto_review_intents" }

func autoReviewIntentFromModel(m AutoReviewIntentModel) AutoReviewIntent {
	return AutoReviewIntent{
		ID: m.ID, RepoOwner: m.RepoOwner, RepoName: m.RepoName, PRNumber: m.PRNumber, HeadSHA: m.HeadSHA,
		Trigger: m.Trigger, DeliveryID: m.DeliveryID, Status: m.Status, RunID: m.RunID, Publication: m.Publication,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func intentTarget(owner, repo string) (string, string) {
	return strings.ToLower(owner), strings.ToLower(repo)
}

// CreateWebhookDelivery records a delivery once. It reports false when the
// delivery id was already stored.
func (g *GormDB) CreateWebhookDelivery(d *WebhookDelivery) (bool, error) {
	if d.DeliveryID == "" {
		return false, fmt.Errorf("create webhook delivery: delivery id is required")
	}
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = time.Now().UTC()
	}
	row := WebhookDeliveryModel{
		DeliveryID: d.DeliveryID, Event: d.Event, Action: d.Action, InstallationID: d.InstallationID,
		RepoOwner: d.RepoOwner, RepoName: d.RepoName, PRNumber: d.PRNumber, HeadSHA: d.HeadSHA, BaseSHA: d.BaseSHA,
		Author: d.Author, Title: d.Title, Draft: d.Draft, State: d.State, ReceivedAt: d.ReceivedAt,
	}
	res := g.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "delivery_id"}}, DoNothing: true}).Create(&row)
	if res.Error != nil {
		return false, fmt.Errorf("create webhook delivery %s: %w", d.DeliveryID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteWebhookDelivery forgets a delivery whose processing failed before it
// was acknowledged, so a redelivery is not rejected as a duplicate.
func (g *GormDB) DeleteWebhookDelivery(deliveryID string) error {
	if err := g.db.Where("delivery_id = ?", deliveryID).Delete(&WebhookDeliveryModel{}).Error; err != nil {
		return fmt.Errorf("delete webhook delivery %s: %w", deliveryID, err)
	}
	return nil
}

// DeleteTerminalAutoReviewIntentsBefore prunes done and superseded intents
// not touched since cutoff. A still-open head that loses its done row is
// re-seeded from the publication ledger, so nothing is reviewed twice. Failed
// rows are kept: a failed head may carry comments the ledger never recorded,
// and the row is what stops the poll fallback from reviewing it again.
func (g *GormDB) DeleteTerminalAutoReviewIntentsBefore(cutoff time.Time) (int64, error) {
	res := g.db.Where("status IN ? AND updated_at < ?",
		[]string{AutoReviewIntentDone, AutoReviewIntentSuperseded}, cutoff.UTC()).
		Delete(&AutoReviewIntentModel{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune terminal auto review intents: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteWebhookDeliveriesBefore prunes deliveries older than cutoff; the
// dedup key only needs to outlive GitHub's redelivery window.
func (g *GormDB) DeleteWebhookDeliveriesBefore(cutoff time.Time) (int64, error) {
	res := g.db.Where("received_at < ?", cutoff.UTC()).Delete(&WebhookDeliveryModel{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// GetWebhookStatus summarizes deliveries received since the given time and
// the automatic review backlog.
func (g *GormDB) GetWebhookStatus(since time.Time) (WebhookStatus, error) {
	var status WebhookStatus
	var deliveries int64
	// Stored timestamps are UTC; SQLite compares them as text, so the bound
	// must be UTC too.
	if err := g.db.Model(&WebhookDeliveryModel{}).Where("received_at >= ?", since.UTC()).Count(&deliveries).Error; err != nil {
		return status, fmt.Errorf("count webhook deliveries: %w", err)
	}
	status.Deliveries = int(deliveries)
	var last WebhookDeliveryModel
	res := g.db.Order("received_at DESC").Limit(1).Find(&last)
	if res.Error != nil {
		return status, fmt.Errorf("latest webhook delivery: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		at := last.ReceivedAt
		status.LastDeliveryAt = &at
	}
	var queued int64
	if err := g.db.Model(&AutoReviewIntentModel{}).Where("status = ?", AutoReviewIntentQueued).Count(&queued).Error; err != nil {
		return status, fmt.Errorf("count queued auto review intents: %w", err)
	}
	status.IntentsQueued = int(queued)
	return status, nil
}

// EnsureAutoReviewIntent inserts an intent for the target head (queued unless
// intent.Status seeds another status), or moves an existing row whose status
// is one of requeueFrom to the seeded status. The intent is filled from the
// stored row on return; created reports whether this call produced the intent.
func (g *GormDB) EnsureAutoReviewIntent(intent *AutoReviewIntent, requeueFrom []string) (bool, error) {
	if intent.RepoOwner == "" || intent.RepoName == "" || intent.PRNumber <= 0 || intent.HeadSHA == "" {
		return false, fmt.Errorf("ensure auto review intent: complete PR target and head are required")
	}
	intent.RepoOwner, intent.RepoName = intentTarget(intent.RepoOwner, intent.RepoName)
	if intent.Status == "" {
		intent.Status = AutoReviewIntentQueued
	}
	now := time.Now().UTC()
	row := AutoReviewIntentModel{
		RepoOwner: intent.RepoOwner, RepoName: intent.RepoName, PRNumber: intent.PRNumber, HeadSHA: intent.HeadSHA,
		Trigger: intent.Trigger, DeliveryID: intent.DeliveryID, Status: intent.Status, RunID: intent.RunID,
		Publication: intent.Publication, CreatedAt: now, UpdatedAt: now,
	}
	onConflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "repo_owner"}, {Name: "repo_name"}, {Name: "pr_number"}, {Name: "head_sha"}},
	}
	if len(requeueFrom) == 0 {
		onConflict.DoNothing = true
	} else {
		onConflict.Where = clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "auto_review_intents.status IN ?", Vars: []interface{}{requeueFrom}}}}
		onConflict.DoUpdates = clause.Assignments(map[string]interface{}{
			"status": intent.Status, "trigger": intent.Trigger, "delivery_id": intent.DeliveryID,
			"run_id": intent.RunID, "publication": intent.Publication, "updated_at": now,
		})
	}
	res := g.db.Clauses(onConflict).Create(&row)
	if res.Error != nil {
		return false, fmt.Errorf("ensure auto review intent for %s/%s#%d@%s: %w", intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA, res.Error)
	}
	var stored AutoReviewIntentModel
	if err := g.db.Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND head_sha = ?",
		intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA).First(&stored).Error; err != nil {
		return false, fmt.Errorf("load auto review intent for %s/%s#%d@%s: %w", intent.RepoOwner, intent.RepoName, intent.PRNumber, intent.HeadSHA, err)
	}
	*intent = autoReviewIntentFromModel(stored)
	return res.RowsAffected > 0, nil
}

// ListAutoReviewIntents returns intents matching the filter, oldest first.
func (g *GormDB) ListAutoReviewIntents(filter AutoReviewIntentFilter) ([]AutoReviewIntent, error) {
	query := g.db.Model(&AutoReviewIntentModel{})
	owner, repo := intentTarget(filter.RepoOwner, filter.RepoName)
	if owner != "" {
		query = query.Where("repo_owner = ?", owner)
	}
	if repo != "" {
		query = query.Where("repo_name = ?", repo)
	}
	if filter.PRNumber > 0 {
		query = query.Where("pr_number = ?", filter.PRNumber)
	}
	if filter.HeadSHA != "" {
		query = query.Where("head_sha = ?", filter.HeadSHA)
	}
	if len(filter.Statuses) > 0 {
		query = query.Where("status IN ?", filter.Statuses)
	}
	var models []AutoReviewIntentModel
	if err := query.Order("id ASC").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("list auto review intents: %w", err)
	}
	intents := make([]AutoReviewIntent, len(models))
	for i, m := range models {
		intents[i] = autoReviewIntentFromModel(m)
	}
	return intents, nil
}

// UpdateAutoReviewIntentStatus moves one intent from any of the from statuses
// to the given status and sets its run link (empty clears it). It reports
// whether the row was changed.
func (g *GormDB) UpdateAutoReviewIntentStatus(id uint, from []string, to, runID string) (bool, error) {
	updates := map[string]interface{}{"status": to, "run_id": runID, "updated_at": time.Now().UTC()}
	query := g.db.Model(&AutoReviewIntentModel{}).Where("id = ?", id)
	if len(from) > 0 {
		query = query.Where("status IN ?", from)
	}
	res := query.Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("update auto review intent %d: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// SetAutoReviewIntentPublicationByRun records how the run's GitHub
// publication ended on the intent it served; a run no intent links to is a
// no-op.
func (g *GormDB) SetAutoReviewIntentPublicationByRun(runID, outcome string) error {
	if runID == "" {
		return nil
	}
	err := g.db.Model(&AutoReviewIntentModel{}).Where("run_id = ?", runID).
		Updates(map[string]interface{}{"publication": outcome, "updated_at": time.Now().UTC()}).Error
	if err != nil {
		return fmt.Errorf("record publication for run %s: %w", runID, err)
	}
	return nil
}

// SupersedeQueuedAutoReviewIntents marks the PR's queued intents superseded,
// except the one for keepHeadSHA when given. Running work is left alone.
func (g *GormDB) SupersedeQueuedAutoReviewIntents(owner, repo string, number int, keepHeadSHA string) (int, error) {
	owner, repo = intentTarget(owner, repo)
	query := g.db.Model(&AutoReviewIntentModel{}).
		Where("repo_owner = ? AND repo_name = ? AND pr_number = ? AND status = ?", owner, repo, number, AutoReviewIntentQueued)
	if keepHeadSHA != "" {
		query = query.Where("head_sha <> ?", keepHeadSHA)
	}
	res := query.Updates(map[string]interface{}{"status": AutoReviewIntentSuperseded, "updated_at": time.Now().UTC()})
	if res.Error != nil && res.Error != gorm.ErrRecordNotFound {
		return 0, fmt.Errorf("supersede auto review intents for %s/%s#%d: %w", owner, repo, number, res.Error)
	}
	return int(res.RowsAffected), nil
}
