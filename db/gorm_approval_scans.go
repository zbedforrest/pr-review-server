package db

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	mrand "math/rand/v2"
	"path"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ ApprovalStore = (*GormDB)(nil)

type approvalGate struct {
	ID      int `gorm:"primaryKey"`
	Version int64
}
type approvalProjection struct {
	UserID     int    `gorm:"primaryKey"`
	Owner      string `gorm:"primaryKey;size:255"`
	Repo       string `gorm:"primaryKey;size:255"`
	Number     int    `gorm:"primaryKey"`
	TargetID   string `gorm:"index;size:64"`
	Generation int64
}
type approvalSnapshot struct {
	TargetID  string `gorm:"primaryKey;size:64"`
	JSON      string `gorm:"type:text"`
	CreatedAt time.Time
}
type approvalAssessment struct {
	TargetID  string `gorm:"primaryKey;size:64"`
	JSON      string `gorm:"type:text"`
	CreatedAt time.Time
}
type approvalDailyBudget struct {
	Day          string `gorm:"primaryKey;size:10"`
	InputTokens  int64
	OutputTokens int64
}

type approvalUsageReservation struct {
	ID                                   string `gorm:"primaryKey;size:64"`
	TargetID                             string `gorm:"index;size:64"`
	LeaseToken                           string
	InputTokens, OutputTokens, ToolBytes int64
	Rounds, ToolCalls                    int
	Settled                              bool
}

func (approvalGate) TableName() string       { return "approval_mutation_gate" }
func (approvalProjection) TableName() string { return "approval_user_targets" }
func (approvalSnapshot) TableName() string   { return "approval_evidence_snapshots" }
func (approvalAssessment) TableName() string { return "approval_assessments" }

func (g *GormDB) ensureApprovalTables() error {
	if err := g.db.AutoMigrate(&ApprovalScan{}, &ApprovalTarget{}, &approvalGate{}, &approvalProjection{}, &approvalSnapshot{}, &approvalAssessment{}, &approvalDailyBudget{}, &ApprovalValidation{}, &approvalUsageReservation{}, &approvalReuseEntry{}); err != nil {
		return err
	}
	return g.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&approvalGate{ID: 1}).Error
}

func (g *GormDB) approvalTransaction(fn func(*gorm.DB) error, clocks ...*time.Time) error {
	started := time.Now()
	initial := make([]time.Time, len(clocks))
	for index, clock := range clocks {
		initial[index] = *clock
	}
	for attempt := 0; ; attempt++ {
		var gated time.Time
		err := g.db.Transaction(func(tx *gorm.DB) error {
			locked := tx.Model(&approvalGate{}).Where("id = 1").UpdateColumn("version", gorm.Expr("version + 1"))
			if locked.Error != nil {
				return locked.Error
			}
			if locked.RowsAffected != 1 {
				return fmt.Errorf("approval mutation gate unavailable")
			}
			gated = time.Now()
			elapsed := gated.Sub(started)
			for index, clock := range clocks {
				*clock = initial[index].Add(elapsed)
			}
			return fn(tx)
		})
		if total := time.Since(started); total > time.Second {
			caller := "unknown"
			if pc, _, _, ok := runtime.Caller(1); ok {
				caller = path.Base(runtime.FuncForPC(pc).Name())
			}
			wait := total
			if !gated.IsZero() {
				wait = gated.Sub(started)
			}
			log.Printf("[APPROVAL] slow transaction %s: %s total, %s waiting for the gate", caller, total.Round(time.Millisecond), wait.Round(time.Millisecond))
		}
		if err == nil || attempt >= 12 || !approvalRetryable(err) {
			return err
		}
		approvalBackoff(attempt)
	}
}

// approvalWrite runs a write that does not take the mutation gate. fn gets a
// session without gorm's implicit transaction, so a single statement costs
// one round trip; fn opens a transaction itself when it needs several.
func (g *GormDB) approvalWrite(fn func(*gorm.DB) error, clocks ...*time.Time) error {
	started := time.Now()
	initial := make([]time.Time, len(clocks))
	for index, clock := range clocks {
		initial[index] = *clock
	}
	session := g.db.Session(&gorm.Session{SkipDefaultTransaction: true})
	for attempt := 0; ; attempt++ {
		elapsed := time.Since(started)
		for index, clock := range clocks {
			*clock = initial[index].Add(elapsed)
		}
		err := fn(session)
		if err == nil || attempt >= 12 || !approvalRetryable(err) {
			return err
		}
		approvalBackoff(attempt)
	}
}

func approvalRetryable(err error) bool {
	message := err.Error()
	for _, transient := range []string{"database is locked", "database table is locked", "SQLSTATE 40P01", "SQLSTATE 40001"} {
		if strings.Contains(message, transient) {
			return true
		}
	}
	return false
}

func approvalBackoff(attempt int) {
	base := time.Duration(attempt+1) * 5 * time.Millisecond
	time.Sleep(base/2 + mrand.N(base))
}

func approvalToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func approvalTerminal(s string) bool {
	return s == "completed" || s == "failed" || s == "timed_out" || s == "cancelled"
}
func approvalLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 101 {
		return 101
	}
	return n
}
func approvalReason(reason string) string { b, _ := json.Marshal([]string{reason}); return string(b) }

var (
	approvalRunning          = []string{"collecting", "investigating", "validating"}
	approvalTerminalStatuses = []string{"completed", "failed", "timed_out", "cancelled"}
)

func (g *GormDB) AdmitApprovalScan(req ApprovalAdmission) (*ApprovalScan, bool, error) {
	if req.Scan.ID == "" || req.Scan.UserID <= 0 || req.Scan.IdempotencyKey == "" || req.Scan.RequestHash == "" || (req.Scan.Kind != "full" && req.Scan.Kind != "recheck") || len(req.Targets) == 0 || len(req.Targets) > MaxApprovalTargetsPerScan {
		return nil, false, fmt.Errorf("invalid approval admission")
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	var result ApprovalScan
	replayed := false
	err := g.approvalTransaction(func(tx *gorm.DB) error {
		var existing ApprovalScan
		err := tx.Where("user_id = ? AND idempotency_key = ?", req.Scan.UserID, req.Scan.IdempotencyKey).First(&existing).Error
		if err == nil {
			if existing.RequestHash != req.Scan.RequestHash {
				return ErrApprovalIdempotency
			}
			result = existing
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		err = tx.Where("user_id = ? AND status IN ?", req.Scan.UserID, []string{"queued", "running", "cancelling"}).First(&existing).Error
		if err == nil {
			return &ApprovalActiveConflict{ScanID: existing.ID}
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		eligible := 0
		seen := map[string]bool{}
		for _, target := range req.Targets {
			key := strings.ToLower(target.Owner+"/"+target.Repo) + fmt.Sprint("/", target.Number)
			if target.ID == "" || target.Owner == "" || target.Repo == "" || target.Number <= 0 || seen[key] {
				return fmt.Errorf("invalid or duplicate approval target")
			}
			seen[key] = true
			if target.ExecutionStatus != "completed" || target.Decision != "excluded" {
				eligible++
			}
		}
		if eligible == 0 {
			return fmt.Errorf("no_eligible_targets")
		}
		if req.DailyInputLimit > 0 && req.DailyOutputLimit > 0 {
			var budget approvalDailyBudget
			err := tx.First(&budget, "day = ?", req.Now.UTC().Format("2006-01-02")).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if budget.InputTokens+req.TargetInputLimit > req.DailyInputLimit || budget.OutputTokens+req.TargetOutputLimit > req.DailyOutputLimit {
				return ErrApprovalBudget
			}
		}
		result = req.Scan
		result.Status = "queued"
		result.Total = len(req.Targets)
		result.CreatedAt = req.Now
		result.CancelRequested = false
		result.CompletedAt = nil
		if result.Deadline.IsZero() {
			result.Deadline = req.Now.Add(time.Duration(len(req.Targets))*180*time.Second + 10*time.Minute)
		}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		owners, repos := []string{}, []string{}
		for _, input := range req.Targets {
			owners = append(owners, strings.ToLower(input.Owner))
			repos = append(repos, strings.ToLower(input.Repo))
		}
		var existingProjections []approvalProjection
		if err := tx.Where("user_id = ? AND owner IN ? AND repo IN ?", result.UserID, owners, repos).Find(&existingProjections).Error; err != nil {
			return err
		}
		generations := make(map[string]int64, len(existingProjections))
		for _, p := range existingProjections {
			generations[fmt.Sprintf("%s/%s/%d", p.Owner, p.Repo, p.Number)] = p.Generation
		}
		targets := make([]ApprovalTarget, 0, len(req.Targets))
		projections := make([]approvalProjection, 0, len(req.Targets))
		for _, input := range req.Targets {
			t := ApprovalTarget{ID: input.ID, ScanID: result.ID, UserID: result.UserID, Owner: strings.ToLower(input.Owner), Repo: strings.ToLower(input.Repo), Number: input.Number, RepositoryID: input.RepositoryID, AccessPartition: input.AccessPartition, ExpectedHeadSHA: input.ExpectedHeadSHA, CreatedAt: req.Now, ExecutionStatus: "queued", Freshness: "expired", ReasonCodesJSON: "[]"}
			if input.ExecutionStatus == "completed" && input.Decision == "excluded" {
				t.ExecutionStatus = "completed"
				t.Decision = "excluded"
				t.ReasonCodesJSON = input.ReasonCodesJSON
				t.Summary = input.Summary
				t.CompletedAt = &req.Now
			}
			t.Generation = generations[fmt.Sprintf("%s/%s/%d", t.Owner, t.Repo, t.Number)] + 1
			targets = append(targets, t)
			projections = append(projections, approvalProjection{UserID: t.UserID, Owner: t.Owner, Repo: t.Repo, Number: t.Number, TargetID: t.ID, Generation: t.Generation})
		}
		if err := tx.Create(&targets).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "owner"}, {Name: "repo"}, {Name: "number"}}, DoUpdates: clause.AssignmentColumns([]string{"target_id", "generation"})}).Create(&projections).Error
	}, &req.Now)
	if err != nil {
		return nil, false, err
	}
	return &result, replayed, nil
}

func (g *GormDB) GetApprovalScan(user int, id string) (*ApprovalScan, error) {
	var s ApprovalScan
	err := g.db.Where("id = ? AND user_id = ?", id, user).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrApprovalNotFound
	}
	return &s, err
}

func (g *GormDB) GetApprovalScanByIdempotency(user int, key string) (*ApprovalScan, error) {
	var s ApprovalScan
	err := g.db.Where("user_id = ? AND idempotency_key = ?", user, key).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrApprovalNotFound
	}
	return &s, err
}

func (g *GormDB) ReleaseApprovalTargetSlot(id, token string) error {
	if token == "" {
		return nil
	}
	var held int64
	if err := g.db.Model(&ApprovalTarget{}).Where("id = ? AND lease_token = ? AND execution_status IN ?", id, token, approvalTerminalStatuses).Count(&held).Error; err != nil {
		return err
	}
	if held == 0 {
		return nil
	}
	now := time.Now()
	return g.approvalTransaction(func(tx *gorm.DB) error {
		var target ApprovalTarget
		if err := tx.Where("id = ? AND lease_token = ? AND execution_status IN ?", id, token, approvalTerminalStatuses).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		released := tx.Model(&ApprovalTarget{}).Where("id = ? AND lease_token = ? AND execution_status = ?", id, token, target.ExecutionStatus).UpdateColumns(map[string]any{"lease_token": "", "lease_until": nil})
		if released.Error != nil || released.RowsAffected == 0 {
			return released.Error
		}
		return approvalSettleParent(tx, target.ScanID, now)
	}, &now)
}
func (g *GormDB) ListApprovalScans(user int, kind string, limit int, cursor string) ([]ApprovalScan, error) {
	q := g.db.Where("user_id = ?", user)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	if cursor != "" {
		var c ApprovalScan
		if err := g.db.Where("id = ? AND user_id = ?", cursor, user).First(&c).Error; err != nil {
			return nil, ErrApprovalNotFound
		}
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", c.CreatedAt, c.CreatedAt, c.ID)
	}
	var out []ApprovalScan
	err := q.Order("created_at DESC, id DESC").Limit(approvalLimit(limit)).Find(&out).Error
	return out, err
}
func (g *GormDB) GetApprovalTarget(user int, scan, id string) (*ApprovalTarget, error) {
	var t ApprovalTarget
	err := g.db.Where("id = ? AND scan_id = ? AND user_id = ?", id, scan, user).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	var snapshot approvalSnapshot
	err = g.db.First(&snapshot, "target_id = ?", id).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	t.SnapshotJSON = snapshot.JSON
	var assessment approvalAssessment
	err = g.db.First(&assessment, "target_id = ?", id).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	t.AssessmentJSON = assessment.JSON
	return &t, nil
}

// attachApprovalAssessments fills AssessmentJSON for a page of targets with
// one query; listed rows otherwise carry no assessment. Snapshots stay out of
// lists because they are large.
func (g *GormDB) attachApprovalAssessments(targets []ApprovalTarget) error {
	if len(targets) == 0 {
		return nil
	}
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.ID
	}
	var rows []approvalAssessment
	if err := g.db.Where("target_id IN ?", ids).Find(&rows).Error; err != nil {
		return err
	}
	byID := make(map[string]string, len(rows))
	for _, r := range rows {
		byID[r.TargetID] = r.JSON
	}
	for i := range targets {
		targets[i].AssessmentJSON = byID[targets[i].ID]
	}
	return nil
}

func (g *GormDB) ListApprovalTargets(user int, scan string, limit int, cursor string) ([]ApprovalTarget, error) {
	if _, err := g.GetApprovalScan(user, scan); err != nil {
		return nil, err
	}
	q := g.db.Where("scan_id = ? AND user_id = ?", scan, user)
	if cursor != "" {
		q = q.Where("id > ?", cursor)
	}
	var out []ApprovalTarget
	if err := q.Order("id ASC").Limit(approvalLimit(limit)).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, g.attachApprovalAssessments(out)
}
func (g *GormDB) ListCurrentApprovalTargets(user int, limit int, cursor string) ([]ApprovalTarget, error) {
	q := g.db.Model(&ApprovalTarget{}).Select("approval_targets.*").Joins("JOIN approval_user_targets p ON p.target_id = approval_targets.id AND p.generation = approval_targets.generation").Where("p.user_id = ?", user)
	if cursor != "" {
		q = q.Where("approval_targets.id > ?", cursor)
	}
	var out []ApprovalTarget
	if err := q.Order("approval_targets.id ASC").Limit(approvalLimit(limit)).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, g.attachApprovalAssessments(out)
}

// approvalLeased scopes a statement to the caller's target while its lease,
// deadline and scan still let it run.
func approvalLeased(tx *gorm.DB, id, token string, now time.Time) *gorm.DB {
	return tx.Model(&ApprovalTarget{}).Where("id = ? AND lease_token = ? AND lease_until > ? AND deadline > ? AND execution_status IN ? AND NOT EXISTS (SELECT 1 FROM approval_scans s WHERE s.id = approval_targets.scan_id AND s.cancel_requested)", id, token, now, now, approvalRunning)
}

// approvalLockLease locks the caller's target row for the rest of tx and
// returns it, or ErrApprovalLeaseLost when the lease no longer holds.
func approvalLockLease(tx *gorm.DB, id, token string, now time.Time) (*ApprovalTarget, error) {
	if err := approvalSQLiteWriteLock(tx, id); err != nil {
		return nil, err
	}
	var t ApprovalTarget
	if err := approvalLeased(tx, id, token, now).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrApprovalLeaseLost
		}
		return nil, err
	}
	return &t, nil
}

// approvalSQLiteWriteLock takes SQLite's database write lock as the first
// statement: SQLite has no row locks, and a transaction that reads first
// fails its later write once another writer commits.
func approvalSQLiteWriteLock(tx *gorm.DB, id string) error {
	if tx.Dialector.Name() != "sqlite" {
		return nil
	}
	return tx.Exec("UPDATE approval_targets SET lease_token = lease_token WHERE id = ?", id).Error
}

// approvalUpdateLeased is one conditional UPDATE of the caller's target.
func (g *GormDB) approvalUpdateLeased(id, token string, now time.Time, values func(time.Time) map[string]any) error {
	return g.approvalWrite(func(db *gorm.DB) error {
		updated := approvalLeased(db, id, token, now).UpdateColumns(values(now))
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			return ErrApprovalLeaseLost
		}
		return nil
	}, &now)
}

// approvalLeasedTransaction runs fn in an ungated transaction that first
// locks the caller's target row.
func (g *GormDB) approvalLeasedTransaction(id, token string, now *time.Time, fn func(*gorm.DB, *ApprovalTarget) error) error {
	return g.approvalWrite(func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			t, err := approvalLockLease(tx, id, token, *now)
			if err != nil {
				return err
			}
			return fn(tx, t)
		})
	}, now)
}

// approvalDeltas collects daily budget changes so a transaction touches each
// day row once, after all of its target row locks.
type approvalDeltas map[string][2]int64

// release charges the day row with the target's usage beyond what its
// reservation already charged; a deferred reservation charged nothing.
func (d approvalDeltas) release(t *ApprovalTarget) {
	if t.BudgetDay == "" {
		return
	}
	chargedInput, chargedOutput := t.ReservedInput, t.ReservedOutput
	if t.BudgetDeferred {
		chargedInput, chargedOutput = 0, 0
	}
	v := d[t.BudgetDay]
	v[0] += t.InputTokens - chargedInput
	v[1] += t.OutputTokens - chargedOutput
	d[t.BudgetDay] = v
	t.ReservedInput = t.InputTokens
	t.ReservedOutput = t.OutputTokens
	t.BudgetDeferred = false
}

func (d approvalDeltas) apply(tx *gorm.DB) error {
	days := make([]string, 0, len(d))
	for day := range d {
		days = append(days, day)
	}
	sort.Strings(days)
	for _, day := range days {
		input, output := d[day][0], d[day][1]
		var err error
		if input >= 0 && output >= 0 {
			err = approvalChargeDailyBudget(tx, day, input, output)
		} else {
			err = approvalAdjustDailyBudget(tx, day, input, output)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// approvalChargeDailyBudget adds usage to a day row in one statement,
// creating the row when no capped reservation has created it yet.
func approvalChargeDailyBudget(tx *gorm.DB, day string, input, output int64) error {
	if input == 0 && output == 0 {
		return nil
	}
	charged := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "day"}},
		DoUpdates: clause.Assignments(map[string]any{
			"input_tokens":  gorm.Expr("approval_daily_budgets.input_tokens + ?", input),
			"output_tokens": gorm.Expr("approval_daily_budgets.output_tokens + ?", output),
		}),
		Where: clause.Where{Exprs: []clause.Expression{gorm.Expr("approval_daily_budgets.input_tokens BETWEEN 0 AND ? AND approval_daily_budgets.output_tokens BETWEEN 0 AND ?", int64(math.MaxInt64)-input, int64(math.MaxInt64)-output)}},
	}).Create(&approvalDailyBudget{Day: day, InputTokens: input, OutputTokens: output})
	if charged.Error != nil {
		return charged.Error
	}
	if charged.RowsAffected != 1 {
		return ErrApprovalBudget
	}
	return nil
}

func approvalAdjustDailyBudget(tx *gorm.DB, day string, inputDelta, outputDelta int64) error {
	if inputDelta == 0 && outputDelta == 0 {
		return nil
	}
	q := tx.Model(&approvalDailyBudget{}).Where("day = ? AND input_tokens >= 0 AND output_tokens >= 0", day)
	q = approvalDeltaGuard(q, "input_tokens", inputDelta)
	q = approvalDeltaGuard(q, "output_tokens", outputDelta)
	updated := q.UpdateColumns(map[string]any{"input_tokens": gorm.Expr("input_tokens + ?", inputDelta), "output_tokens": gorm.Expr("output_tokens + ?", outputDelta)})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrApprovalBudget
	}
	return nil
}

func approvalDeltaGuard(q *gorm.DB, column string, delta int64) *gorm.DB {
	switch {
	case delta > 0:
		return q.Where(column+" <= ?", int64(math.MaxInt64)-delta)
	case delta < 0:
		return q.Where(column+" >= ?", -delta)
	}
	return q
}

func approvalSettleParent(tx *gorm.DB, id string, now time.Time) error {
	var scan ApprovalScan
	if err := tx.First(&scan, "id = ?", id).Error; err != nil {
		return err
	}
	var targets []ApprovalTarget
	if err := tx.Where("scan_id = ?", id).Find(&targets).Error; err != nil {
		return err
	}
	completed, failed := 0, 0
	active := false
	started := false
	for _, t := range targets {
		if !approvalTerminal(t.ExecutionStatus) || (t.ExecutionStatus == "cancelled" && t.LeaseUntil != nil && t.LeaseUntil.After(now)) {
			active = true
		}
		if t.StartedAt != nil {
			started = true
		}
		if t.ExecutionStatus == "completed" {
			completed++
		}
		if t.ExecutionStatus == "failed" || t.ExecutionStatus == "timed_out" {
			failed++
		}
	}
	if active {
		scan.CompletedAt = nil
		if scan.CancelRequested {
			scan.Status = "cancelling"
		} else if started {
			scan.Status = "running"
		} else {
			scan.Status = "queued"
		}
	} else {
		scan.CompletedAt = &now
		switch {
		case scan.CancelRequested:
			scan.Status = "cancelled"
		case completed == 0:
			scan.Status = "failed"
		case failed > 0:
			scan.Status = "partial"
		default:
			scan.Status = "completed"
		}
	}
	return tx.Save(&scan).Error
}

// approvalActiveChildren matches a scan's targets that still hold work or a
// slot.
const approvalActiveChildren = "(execution_status NOT IN ? OR (execution_status = ? AND lease_until > ?))"

// settleApprovalScanIfDone settles a scan whose last target just finished.
// The count runs without the gate; the claim sweep settles any scan two
// concurrent finishers both missed.
func (g *GormDB) settleApprovalScanIfDone(id string, now time.Time) error {
	var active int64
	if err := g.db.Model(&ApprovalTarget{}).Where("scan_id = ? AND "+approvalActiveChildren, id, approvalTerminalStatuses, "cancelled", now).Count(&active).Error; err != nil || active > 0 {
		return err
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		var scan ApprovalScan
		if err := tx.First(&scan, "id = ?", id).Error; err != nil {
			return err
		}
		if scan.Status != "queued" && scan.Status != "running" && scan.Status != "cancelling" {
			return nil
		}
		return approvalSettleParent(tx, id, now)
	}, &now)
}

// approvalTerminate ends a target whose row the caller holds locked and adds
// its unused token reservation to deltas.
func approvalTerminate(tx *gorm.DB, t *ApprovalTarget, now time.Time, status, reason string, deltas approvalDeltas) error {
	t.ExecutionStatus = status
	t.Decision = ""
	t.Freshness = "expired"
	t.ReasonCodesJSON = approvalReason(reason)
	t.CompletedAt = &now
	if status != "cancelled" {
		t.LeaseToken = ""
		t.LeaseUntil = nil
	}
	t.PendingCallID = ""
	t.PendingInput = 0
	t.PendingOutput = 0
	deltas.release(t)
	return tx.Save(t).Error
}

// approvalRelock locks a target that a gated transaction read without a lock
// and returns its current row, or nil when its holder changed. Ungated stage
// writes may move the status meanwhile, so callers recheck the fresh row.
func approvalRelock(tx *gorm.DB, t ApprovalTarget) (*ApprovalTarget, error) {
	var fresh ApprovalTarget
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND lease_token = ?", t.ID, t.LeaseToken).Take(&fresh).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// approvalExpiry names why housekeeping must end a non-terminal target.
func approvalExpiry(t ApprovalTarget, scan ApprovalScan, now time.Time) (reason, status string) {
	switch {
	case scan.CancelRequested:
		return "cancelled", "cancelled"
	case t.StartedAt == nil && !scan.Deadline.After(now):
		return "queue_deadline", "timed_out"
	case t.Deadline != nil && !t.Deadline.After(now):
		return "target_deadline", "timed_out"
	case t.LeaseUntil != nil && !t.LeaseUntil.After(now) && t.Attempts >= 2:
		return "recovery_exhausted", "failed"
	}
	return "", ""
}

func (g *GormDB) ClaimApprovalTarget(req ApprovalClaim) (*ApprovalTarget, error) {
	req.MaxClaims = 1
	claimed, err := g.ClaimApprovalTargets(req)
	if err != nil || len(claimed) == 0 {
		return nil, err
	}
	return &claimed[0], nil
}

func (g *GormDB) ClaimApprovalTargets(req ApprovalClaim) ([]ApprovalTarget, error) {
	if req.Worker == "" || req.MaxSlots <= 0 || req.LeaseDuration <= 0 || req.TargetDuration <= 0 {
		return nil, fmt.Errorf("invalid approval claim")
	}
	perUser := max(req.MaxPerUser, 1)
	maxClaims := max(req.MaxClaims, 1)
	var claimed []ApprovalTarget
	err := g.approvalTransaction(func(tx *gorm.DB) error {
		claimed = nil
		deltas := approvalDeltas{}
		settle := map[string]bool{}
		var idle []ApprovalScan
		if err := tx.Where("status = ? OR (status IN ? AND NOT EXISTS (SELECT 1 FROM approval_targets WHERE approval_targets.scan_id = approval_scans.id AND "+approvalActiveChildren+"))", "cancelling", []string{"queued", "running"}, approvalTerminalStatuses, "cancelled", req.Now).Find(&idle).Error; err != nil {
			return err
		}
		for _, scan := range idle {
			if err := approvalSettleParent(tx, scan.ID, req.Now); err != nil {
				return err
			}
		}
		var targets []ApprovalTarget
		if err := tx.Where("execution_status IN ?", []string{"queued", "collecting", "investigating", "validating"}).Order("created_at ASC, id ASC").Find(&targets).Error; err != nil {
			return err
		}
		scanIDs := make([]string, 0, len(targets))
		for _, t := range targets {
			scanIDs = append(scanIDs, t.ScanID)
		}
		var scanRows []ApprovalScan
		if len(scanIDs) > 0 {
			if err := tx.Where("id IN ?", scanIDs).Find(&scanRows).Error; err != nil {
				return err
			}
		}
		scans := make(map[string]ApprovalScan, len(scanRows))
		for _, scan := range scanRows {
			scans[scan.ID] = scan
		}
		activeUsers := map[int]int{}
		slots := 0
		ended := map[string]bool{}
		for i := range targets {
			t := &targets[i]
			scan, ok := scans[t.ScanID]
			if !ok {
				return gorm.ErrRecordNotFound
			}
			if reason, _ := approvalExpiry(*t, scan, req.Now); reason != "" {
				fresh, err := approvalRelock(tx, *t)
				if err != nil {
					return err
				}
				if fresh == nil || approvalTerminal(fresh.ExecutionStatus) {
					ended[t.ID] = true
					continue
				}
				*t = *fresh
				if reason, status := approvalExpiry(*t, scan, req.Now); reason != "" {
					if err := approvalTerminate(tx, t, req.Now, status, reason, deltas); err != nil {
						return err
					}
					settle[t.ScanID] = true
					ended[t.ID] = true
					continue
				}
			}
			if t.LeaseUntil != nil && t.LeaseUntil.After(req.Now) {
				slots++
				activeUsers[t.UserID]++
			}
		}
		var cancelled []ApprovalTarget
		if err := tx.Where("execution_status = ? AND lease_until > ?", "cancelled", req.Now).Find(&cancelled).Error; err != nil {
			return err
		}
		for _, t := range cancelled {
			slots++
			activeUsers[t.UserID]++
		}
		prefix := req.Worker + ":" + approvalToken() + ":"
		var fresh, recovered []ApprovalTarget
		for _, t := range targets {
			if len(fresh)+len(recovered) >= min(maxClaims, req.MaxSlots-slots) {
				break
			}
			if ended[t.ID] || approvalTerminal(t.ExecutionStatus) || activeUsers[t.UserID] >= perUser || (t.LeaseUntil != nil && t.LeaseUntil.After(req.Now)) {
				continue
			}
			activeUsers[t.UserID]++
			if t.StartedAt == nil {
				fresh = append(fresh, t)
			} else {
				recovered = append(recovered, t)
			}
		}
		ids := []string{}
		if len(fresh) > 0 {
			deadline := req.Now.Add(req.TargetDuration)
			until := approvalEarlier(req.Now.Add(req.LeaseDuration), deadline)
			for _, t := range fresh {
				ids = append(ids, t.ID)
			}
			if err := tx.Model(&ApprovalTarget{}).Where("id IN ? AND execution_status = ? AND started_at IS NULL AND (lease_until IS NULL OR lease_until <= ?)", ids, "queued", req.Now).UpdateColumns(map[string]any{"started_at": req.Now, "deadline": deadline, "attempts": gorm.Expr("attempts + 1"), "lease_token": gorm.Expr("CAST(? AS TEXT) || id", prefix), "lease_until": until, "execution_status": "collecting", "pending_call_id": "", "pending_input": 0, "pending_output": 0}).Error; err != nil {
				return err
			}
		}
		for _, t := range recovered {
			until := approvalEarlier(req.Now.Add(req.LeaseDuration), *t.Deadline)
			if err := tx.Model(&ApprovalTarget{}).Where("id = ? AND execution_status = ? AND lease_token = ? AND (lease_until IS NULL OR lease_until <= ?)", t.ID, t.ExecutionStatus, t.LeaseToken, req.Now).UpdateColumns(map[string]any{"attempts": gorm.Expr("attempts + 1"), "lease_token": prefix + t.ID, "lease_until": until, "execution_status": "collecting", "pending_call_id": "", "pending_input": 0, "pending_output": 0}).Error; err != nil {
				return err
			}
			ids = append(ids, t.ID)
		}
		if len(ids) > 0 {
			var rows []ApprovalTarget
			if err := tx.Where("id IN ?", ids).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
				return err
			}
			for _, t := range rows {
				if t.LeaseToken == prefix+t.ID {
					claimed = append(claimed, t)
					settle[t.ScanID] = true
				}
			}
		}
		settled := make([]string, 0, len(settle))
		for id := range settle {
			settled = append(settled, id)
		}
		sort.Strings(settled)
		for _, id := range settled {
			if err := approvalSettleParent(tx, id, req.Now); err != nil {
				return err
			}
		}
		return deltas.apply(tx)
	}, &req.Now)
	return claimed, err
}

func approvalEarlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (g *GormDB) HeartbeatApprovalTarget(id, token string, now time.Time, duration time.Duration) error {
	return g.approvalUpdateLeased(id, token, now, func(now time.Time) map[string]any {
		until := now.Add(duration)
		return map[string]any{"lease_until": gorm.Expr("CASE WHEN deadline < ? THEN deadline ELSE ? END", until, until)}
	})
}
func (g *GormDB) SetApprovalTargetStage(id, token, stage string, now time.Time) error {
	if stage != "collecting" && stage != "investigating" && stage != "validating" {
		return fmt.Errorf("invalid approval stage")
	}
	return g.approvalUpdateLeased(id, token, now, func(time.Time) map[string]any { return map[string]any{"execution_status": stage} })
}

func (g *GormDB) SetApprovalTargetProgress(id, token, summary string, now time.Time) error {
	if len(summary) > 160 || len(strings.Fields(summary)) > 10 {
		return fmt.Errorf("invalid approval progress summary")
	}
	return g.approvalUpdateLeased(id, token, now, func(time.Time) map[string]any { return map[string]any{"summary": summary} })
}

func (g *GormDB) SaveApprovalSnapshot(id, token, body string, now time.Time) error {
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("invalid snapshot JSON")
	}
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, _ *ApprovalTarget) error {
		var existing approvalSnapshot
		err := tx.First(&existing, "target_id = ?", id).Error
		if err == nil {
			if existing.JSON == body {
				return nil
			}
			return fmt.Errorf("approval snapshot is immutable")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&approvalSnapshot{TargetID: id, JSON: body, CreatedAt: now}).Error
	})
}

// ReserveApprovalBudget sets the target's token allowance. Without a daily
// cap the day row is charged only with actual usage at release, so parallel
// targets never queue on its row lock; with a cap the day row update is the
// last statement, which holds that lock for one round trip.
func (g *GormDB) ReserveApprovalBudget(id, token string, now time.Time, dailyInput, dailyOutput, targetInput, targetOutput int64) error {
	if targetInput <= 0 || targetOutput <= 0 {
		return ErrApprovalBudget
	}
	capped := dailyInput > 0 && dailyOutput > 0
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, t *ApprovalTarget) error {
		if t.BudgetDay != "" {
			return nil
		}
		day := now.UTC().Format("2006-01-02")
		if err := tx.Model(t).UpdateColumns(map[string]any{"budget_day": day, "reserved_input": targetInput, "reserved_output": targetOutput, "budget_deferred": !capped}).Error; err != nil {
			return err
		}
		if !capped {
			return nil
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&approvalDailyBudget{Day: day}).Error; err != nil {
			return err
		}
		q := tx.Model(&approvalDailyBudget{}).Where("day = ? AND input_tokens >= 0 AND output_tokens >= 0", day)
		q = approvalDeltaGuard(q, "input_tokens", targetInput)
		q = approvalDeltaGuard(q, "output_tokens", targetOutput)
		q = q.Where("input_tokens <= ? AND output_tokens <= ?", dailyInput-targetInput, dailyOutput-targetOutput)
		reserved := q.UpdateColumns(map[string]any{"input_tokens": gorm.Expr("input_tokens + ?", targetInput), "output_tokens": gorm.Expr("output_tokens + ?", targetOutput)})
		if reserved.Error != nil {
			return reserved.Error
		}
		if reserved.RowsAffected != 1 {
			return ErrApprovalBudget
		}
		return nil
	})
}
func (g *GormDB) ReserveApprovalCall(id, token string, now time.Time, r ApprovalCallReservation) error {
	if r.CallID == "" || r.InputTokens < 0 || r.OutputTokens < 0 || r.ToolCalls < 0 {
		return fmt.Errorf("invalid call reservation")
	}
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, t *ApprovalTarget) error {
		if t.BudgetDay == "" || t.PendingCallID != "" || t.InputTokens+r.InputTokens > t.ReservedInput || t.OutputTokens+r.OutputTokens > t.ReservedOutput || t.InputTokens+r.InputTokens > r.MaxInputTokens || t.OutputTokens+r.OutputTokens > r.MaxOutputTokens || t.Rounds+1 > r.MaxRounds || t.ToolCalls+r.ToolCalls > r.MaxToolCalls {
			return ErrApprovalBudget
		}
		t.InputTokens += r.InputTokens
		t.OutputTokens += r.OutputTokens
		t.Rounds++
		t.ToolCalls += r.ToolCalls
		t.PendingCallID = r.CallID
		t.PendingInput = r.InputTokens
		t.PendingOutput = r.OutputTokens
		return tx.Save(t).Error
	})
}
func (g *GormDB) CompleteApprovalCall(id, token, callID string, now time.Time, input, output int64) error {
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, t *ApprovalTarget) error {
		if callID == "" || t.PendingCallID != callID {
			return ErrApprovalLeaseLost
		}
		if input < 0 || output < 0 || input > t.PendingInput || output > t.PendingOutput {
			return ErrApprovalBudget
		}
		t.InputTokens -= t.PendingInput - input
		t.OutputTokens -= t.PendingOutput - output
		t.PendingCallID = ""
		t.PendingInput = 0
		t.PendingOutput = 0
		return tx.Save(t).Error
	})
}

func (g *GormDB) FinalizeApprovalTarget(id, token string, now time.Time, f ApprovalFinalization) error {
	if f.ExecutionStatus != "completed" && f.ExecutionStatus != "failed" && f.ExecutionStatus != "timed_out" {
		return fmt.Errorf("invalid final execution status")
	}
	if f.ExecutionStatus == "completed" && f.Decision != "candidate" && f.Decision != "needs_attention" && f.Decision != "insufficient_evidence" && f.Decision != "excluded" {
		return fmt.Errorf("invalid approval decision")
	}
	if f.AssessmentJSON != "" && !json.Valid([]byte(f.AssessmentJSON)) {
		return fmt.Errorf("invalid assessment JSON")
	}
	scanID := ""
	err := g.approvalWrite(func(db *gorm.DB) error {
		scanID = ""
		return db.Transaction(func(tx *gorm.DB) error {
			deltas := approvalDeltas{}
			if f.ExecutionStatus == "timed_out" {
				if err := approvalSQLiteWriteLock(tx, id); err != nil {
					return err
				}
				var expired ApprovalTarget
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND lease_token = ?", id, token).Take(&expired).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return ErrApprovalLeaseLost
					}
					return err
				}
				if !approvalTerminal(expired.ExecutionStatus) && expired.Deadline != nil && !expired.Deadline.After(now) {
					if err := approvalTerminate(tx, &expired, now, "timed_out", "deadline_exceeded", deltas); err != nil {
						return err
					}
					scanID = expired.ScanID
					return deltas.apply(tx)
				}
			}
			t, err := approvalLockLease(tx, id, token, now)
			if err != nil {
				return err
			}
			if t.PendingCallID != "" && f.Decision == "candidate" {
				return ErrApprovalBudget
			}
			if f.Decision == "candidate" {
				var unsettled int64
				if err := tx.Model(&approvalUsageReservation{}).Where("target_id = ? AND lease_token = ? AND settled = ?", id, token, false).Count(&unsettled).Error; err != nil {
					return err
				}
				if unsettled > 0 {
					return ErrApprovalBudget
				}
				if f.AssessmentJSON == "" || (f.Freshness != "current" && f.Freshness != "stale" && f.Freshness != "expired") {
					return fmt.Errorf("invalid candidate assessment")
				}
				if f.Freshness == "current" && (f.ValidatedAt == nil || f.ValidUntil == nil || f.ValidatedAt.After(now) || !f.ValidUntil.After(now) || f.ValidUntil.After(f.ValidatedAt.Add(5*time.Minute))) {
					return fmt.Errorf("invalid candidate freshness")
				}
			}
			if f.AssessmentJSON != "" {
				if err := tx.Create(&approvalAssessment{TargetID: id, JSON: f.AssessmentJSON, CreatedAt: now}).Error; err != nil {
					return err
				}
			}
			final := f
			t.ExecutionStatus = final.ExecutionStatus
			t.Decision = final.Decision
			if final.ExecutionStatus != "completed" {
				t.Decision = ""
			}
			t.Freshness = final.Freshness
			if final.Freshness == "current" && t.ObservedChangeAt != nil && t.StartedAt != nil && !t.ObservedChangeAt.Before(*t.StartedAt) {
				t.Freshness = "stale"
				final.ValidUntil = &now
				final.ReasonCodesJSON = approvalReason("observed_pr_change")
			}
			t.ReasonCodesJSON = final.ReasonCodesJSON
			t.Summary = final.Summary
			t.ValidatedAt = final.ValidatedAt
			t.ValidUntil = final.ValidUntil
			t.CompletedAt = &now
			t.LeaseToken = ""
			t.LeaseUntil = nil
			t.PendingCallID = ""
			t.PendingInput = 0
			t.PendingOutput = 0
			deltas.release(t)
			if err := tx.Save(t).Error; err != nil {
				return err
			}
			scanID = t.ScanID
			return deltas.apply(tx)
		})
	}, &now)
	if err == nil && scanID != "" {
		if settleErr := g.settleApprovalScanIfDone(scanID, now); settleErr != nil {
			log.Printf("[APPROVAL] settle scan %s: %v", scanID, settleErr)
		}
	}
	return err
}

func (g *GormDB) ReserveApprovalUsage(id, token string, now time.Time, r ApprovalUsageReservation) error {
	c := r.Carry
	if r.ID == "" || r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.ToolBytes < 0 || r.Usage.Rounds < 0 || r.Usage.ToolCalls < 0 || c.InputTokens != 0 || c.OutputTokens != 0 || c.ToolBytes < 0 || c.Rounds < 0 || c.ToolCalls < 0 {
		return fmt.Errorf("invalid usage reservation")
	}
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, t *ApprovalTarget) error {
		t.ToolBytes += c.ToolBytes
		t.Rounds += c.Rounds
		t.ToolCalls += c.ToolCalls
		u := r.Usage
		l := r.Limits
		if (u.InputTokens > 0 || u.OutputTokens > 0) && t.BudgetDay == "" {
			return ErrApprovalBudget
		}
		if t.InputTokens+u.InputTokens > l.InputTokens || t.OutputTokens+u.OutputTokens > l.OutputTokens || t.ToolBytes+u.ToolBytes > l.ToolBytes || t.Rounds+u.Rounds > l.Rounds || t.ToolCalls+u.ToolCalls > l.ToolCalls || t.InputTokens+u.InputTokens > t.ReservedInput || t.OutputTokens+u.OutputTokens > t.ReservedOutput {
			return ErrApprovalBudget
		}
		reservation := approvalUsageReservation{ID: r.ID, TargetID: id, LeaseToken: token, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ToolBytes: u.ToolBytes, Rounds: u.Rounds, ToolCalls: u.ToolCalls}
		if err := tx.Create(&reservation).Error; err != nil {
			return err
		}
		t.InputTokens += u.InputTokens
		t.OutputTokens += u.OutputTokens
		t.ToolBytes += u.ToolBytes
		t.Rounds += u.Rounds
		t.ToolCalls += u.ToolCalls
		return tx.Save(t).Error
	})
}

func (g *GormDB) SettleApprovalUsage(id, token, reservationID string, now time.Time, actual ApprovalUsage) error {
	return g.approvalLeasedTransaction(id, token, &now, func(tx *gorm.DB, t *ApprovalTarget) error {
		var r approvalUsageReservation
		if err := tx.Where("id = ? AND target_id = ? AND lease_token = ? AND settled = ?", reservationID, id, token, false).First(&r).Error; err != nil {
			return ErrApprovalLeaseLost
		}
		if actual.InputTokens < 0 || actual.OutputTokens < 0 || actual.ToolBytes < 0 || actual.Rounds < 0 || actual.ToolCalls < 0 || actual.InputTokens > 1_000_000_000 || actual.OutputTokens > 1_000_000_000 || actual.ToolBytes > r.ToolBytes || actual.Rounds > r.Rounds || actual.ToolCalls > r.ToolCalls || t.InputTokens > math.MaxInt64-actual.InputTokens || t.OutputTokens > math.MaxInt64-actual.OutputTokens {
			return ErrApprovalBudget
		}
		t.InputTokens -= r.InputTokens - actual.InputTokens
		t.OutputTokens -= r.OutputTokens - actual.OutputTokens
		t.ToolBytes -= r.ToolBytes - actual.ToolBytes
		inputOver, outputOver := int64(0), int64(0)
		if t.InputTokens > t.ReservedInput {
			inputOver = t.InputTokens - t.ReservedInput
		}
		if t.OutputTokens > t.ReservedOutput {
			outputOver = t.OutputTokens - t.ReservedOutput
		}
		if (inputOver > 0 || outputOver > 0) && !t.BudgetDeferred {
			if err := approvalAdjustDailyBudget(tx, t.BudgetDay, inputOver, outputOver); err != nil {
				return err
			}
			t.ReservedInput += inputOver
			t.ReservedOutput += outputOver
		}
		if err := tx.Save(t).Error; err != nil {
			return err
		}
		return tx.Model(&r).Update("settled", true).Error
	})
}

// FlushApprovalUsage adds tool and citation usage that was accounted in
// process to the target's durable counters.
func (g *GormDB) FlushApprovalUsage(id, token string, now time.Time, u ApprovalUsage) error {
	if u.InputTokens != 0 || u.OutputTokens != 0 || u.ToolBytes < 0 || u.Rounds < 0 || u.ToolCalls < 0 {
		return fmt.Errorf("invalid usage flush")
	}
	if u == (ApprovalUsage{}) {
		return nil
	}
	return g.approvalUpdateLeased(id, token, now, func(time.Time) map[string]any {
		return map[string]any{"tool_calls": gorm.Expr("tool_calls + ?", u.ToolCalls), "tool_bytes": gorm.Expr("tool_bytes + ?", u.ToolBytes), "rounds": gorm.Expr("rounds + ?", u.Rounds)}
	})
}

func approvalCancel(tx *gorm.DB, user int, id string, now time.Time, deltas approvalDeltas) error {
	var scan ApprovalScan
	err := tx.Where("id = ? AND user_id = ?", id, user).First(&scan).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrApprovalNotFound
	}
	if err != nil {
		return err
	}
	if scan.Status == "completed" || scan.Status == "partial" || scan.Status == "failed" || scan.Status == "cancelled" {
		return nil
	}
	if err := tx.Model(&scan).Updates(map[string]any{"cancel_requested": true, "status": "cancelling"}).Error; err != nil {
		return err
	}
	var targets []ApprovalTarget
	if err := tx.Where("scan_id = ?", id).Find(&targets).Error; err != nil {
		return err
	}
	for i := range targets {
		if approvalTerminal(targets[i].ExecutionStatus) {
			continue
		}
		fresh, err := approvalRelock(tx, targets[i])
		if err != nil {
			return err
		}
		if fresh == nil || approvalTerminal(fresh.ExecutionStatus) {
			continue
		}
		if err := approvalTerminate(tx, fresh, now, "cancelled", "cancelled", deltas); err != nil {
			return err
		}
	}
	return approvalSettleParent(tx, id, now)
}
func (g *GormDB) CancelApprovalScan(user int, id string, now time.Time) error {
	return g.approvalTransaction(func(tx *gorm.DB) error {
		deltas := approvalDeltas{}
		if err := approvalCancel(tx, user, id, now, deltas); err != nil {
			return err
		}
		return deltas.apply(tx)
	}, &now)
}
func (g *GormDB) CancelAllApprovalScans(now time.Time) error {
	var count int64
	if err := g.db.Model(&ApprovalScan{}).Where("status IN ?", []string{"queued", "running", "cancelling"}).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		var scans []ApprovalScan
		if err := tx.Where("status IN ?", []string{"queued", "running", "cancelling"}).Find(&scans).Error; err != nil {
			return err
		}
		deltas := approvalDeltas{}
		for _, s := range scans {
			if err := approvalCancel(tx, s.UserID, s.ID, now, deltas); err != nil {
				return err
			}
		}
		return deltas.apply(tx)
	}, &now)
}

func (g *GormDB) ClaimApprovalValidation(user int, id string, now time.Time, duration time.Duration) (*ApprovalValidation, error) {
	var result *ApprovalValidation
	err := g.approvalTransaction(func(tx *gorm.DB) error {
		var t ApprovalTarget
		if err := tx.Where("id = ? AND user_id = ?", id, user).First(&t).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrApprovalNotFound
			}
			return err
		}
		var p approvalProjection
		if err := tx.Where("user_id = ? AND target_id = ? AND generation = ?", user, id, t.Generation).First(&p).Error; err != nil {
			return ErrApprovalLeaseLost
		}
		if t.ExecutionStatus != "completed" || t.Decision != "candidate" || t.Freshness == "stale" || !t.CreatedAt.Add(30*24*time.Hour).After(now) {
			return ErrApprovalLeaseLost
		}
		var existing ApprovalValidation
		err := tx.First(&existing, "target_id = ?", id).Error
		if err == nil && existing.LeaseUntil.After(now) {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var slots int64
		if err := tx.Model(&ApprovalValidation{}).Where("lease_until > ?", now).Count(&slots).Error; err != nil {
			return err
		}
		if slots >= 2 {
			return nil
		}
		v := ApprovalValidation{TargetID: id, UserID: user, Generation: t.Generation, Token: approvalToken(), LeaseUntil: now.Add(duration)}
		if err := tx.Save(&v).Error; err != nil {
			return err
		}
		result = &v
		return nil
	}, &now)
	return result, err
}
func (g *GormDB) FinishApprovalValidation(user int, id, token string, now time.Time, freshness, reason string) error {
	if freshness != "current" && freshness != "stale" && freshness != "expired" {
		return fmt.Errorf("invalid freshness")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		var v ApprovalValidation
		if err := tx.Where("target_id = ? AND user_id = ? AND token = ? AND lease_until > ?", id, user, token, now).First(&v).Error; err != nil {
			return ErrApprovalLeaseLost
		}
		var p approvalProjection
		if err := tx.Where("user_id = ? AND target_id = ? AND generation = ?", user, id, v.Generation).First(&p).Error; err != nil {
			return ErrApprovalLeaseLost
		}
		var t ApprovalTarget
		if err := tx.First(&t, "id = ?", id).Error; err != nil {
			return err
		}
		if t.Freshness == "stale" || !t.CreatedAt.Add(30*24*time.Hour).After(now) {
			return ErrApprovalLeaseLost
		}
		t.Freshness = freshness
		if freshness == "current" {
			until := now.Add(5 * time.Minute)
			retention := t.CreatedAt.Add(30 * 24 * time.Hour)
			if until.After(retention) {
				until = retention
			}
			t.ValidatedAt = &now
			t.ValidUntil = &until
		} else {
			t.ValidUntil = &now
			t.ReasonCodesJSON = approvalReason(reason)
		}
		if err := tx.Save(&t).Error; err != nil {
			return err
		}
		return tx.Delete(&v).Error
	}, &now)
}
func (g *GormDB) InvalidateApprovalTargets(owner, repo string, number int, reason string, now time.Time) error {
	return g.InvalidateUserApprovalTargets(0, owner, repo, number, reason, now)
}
func (g *GormDB) InvalidateUserApprovalTargets(user int, owner, repo string, number int, reason string, now time.Time) error {
	return g.InvalidateMatchingApprovalTargets(user, owner, repo, number, ApprovalInvalidation{}, reason, now)
}

// ApprovalInvalidation narrows which of a pull request's targets an observed
// change invalidates: OffHead selects targets assessed at an older head,
// CandidatesOnly leaves non-candidates, which the change cannot make worse, and
// Cleared selects completed results held back by any of these now-cleared reasons
// and drops those reasons, so each clearing invalidates once.
type ApprovalInvalidation struct {
	OffHead        string
	CandidatesOnly bool
	Cleared        []string
}

func (g *GormDB) InvalidateMatchingApprovalTargets(user int, owner, repo string, number int, match ApprovalInvalidation, reason string, now time.Time) error {
	return g.approvalTransaction(func(tx *gorm.DB) error {
		q := tx.Where("owner = ? AND repo = ? AND number = ? AND execution_status IN ?", strings.ToLower(owner), strings.ToLower(repo), number, []string{"collecting", "investigating", "validating", "completed"})
		q = q.Where("execution_status <> ? OR id IN (?)", "completed", tx.Model(&approvalProjection{}).Select("target_id"))
		if user > 0 {
			q = q.Where("user_id = ?", user)
		}
		if match.OffHead != "" {
			q = q.Where("LOWER(expected_head_sha) <> ?", strings.ToLower(match.OffHead))
		}
		if match.CandidatesOnly {
			q = q.Where("decision = ?", "candidate")
		}
		if len(match.Cleared) > 0 {
			q = q.Where("execution_status = ?", "completed")
		}
		var targets []ApprovalTarget
		if err := q.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id").Find(&targets).Error; err != nil {
			return err
		}
		for _, t := range targets {
			var reasons []string
			_ = json.Unmarshal([]byte(t.ReasonCodesJSON), &reasons)
			if len(match.Cleared) > 0 {
				kept := slices.DeleteFunc(slices.Clone(reasons), func(r string) bool { return slices.Contains(match.Cleared, r) })
				if len(kept) == len(reasons) {
					continue
				}
				reasons = kept
			}
			found := false
			for _, r := range reasons {
				if r == reason {
					found = true
				}
			}
			if !found {
				reasons = append(reasons, reason)
			}
			body, _ := json.Marshal(reasons)
			if err := tx.Model(&t).Updates(map[string]any{"observed_change_at": now, "freshness": "stale", "valid_until": now, "reason_codes_json": string(body)}).Error; err != nil {
				return err
			}
		}
		return nil
	}, &now)
}
func (g *GormDB) PruneApprovalScans(before time.Time) (int64, error) {
	var count int64
	err := g.approvalTransaction(func(tx *gorm.DB) error {
		var scans []ApprovalScan
		if err := tx.Where("created_at < ? AND status IN ?", before, []string{"completed", "partial", "failed", "cancelled"}).Find(&scans).Error; err != nil {
			return err
		}
		for _, scan := range scans {
			var ids []string
			if err := tx.Model(&ApprovalTarget{}).Where("scan_id = ?", scan.ID).Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) > 0 {
				for _, model := range []any{&approvalProjection{}, &approvalSnapshot{}, &approvalAssessment{}, &ApprovalValidation{}, &approvalUsageReservation{}, &approvalReuseEntry{}} {
					if err := tx.Where("target_id IN ?", ids).Delete(model).Error; err != nil {
						return err
					}
				}
			}
			if err := tx.Where("scan_id = ?", scan.ID).Delete(&ApprovalTarget{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&scan).Error; err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}
