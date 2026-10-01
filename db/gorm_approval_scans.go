package db

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"path"
	"runtime"
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
		if err == nil || attempt >= 12 || (!strings.Contains(err.Error(), "database is locked") && !strings.Contains(err.Error(), "database table is locked")) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
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
		for _, input := range req.Targets {
			t := ApprovalTarget{ID: input.ID, ScanID: result.ID, UserID: result.UserID, Owner: strings.ToLower(input.Owner), Repo: strings.ToLower(input.Repo), Number: input.Number, RepositoryID: input.RepositoryID, AccessPartition: input.AccessPartition, ExpectedHeadSHA: input.ExpectedHeadSHA, CreatedAt: req.Now, ExecutionStatus: "queued", Freshness: "expired", ReasonCodesJSON: "[]"}
			if input.ExecutionStatus == "completed" && input.Decision == "excluded" {
				t.ExecutionStatus = "completed"
				t.Decision = "excluded"
				t.ReasonCodesJSON = input.ReasonCodesJSON
				t.Summary = input.Summary
				t.CompletedAt = &req.Now
			}
			var p approvalProjection
			err := tx.Where("user_id = ? AND owner = ? AND repo = ? AND number = ?", t.UserID, t.Owner, t.Repo, t.Number).First(&p).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			t.Generation = p.Generation + 1
			if err := tx.Create(&t).Error; err != nil {
				return err
			}
			p = approvalProjection{UserID: t.UserID, Owner: t.Owner, Repo: t.Repo, Number: t.Number, TargetID: t.ID, Generation: t.Generation}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "owner"}, {Name: "repo"}, {Name: "number"}}, DoUpdates: clause.AssignmentColumns([]string{"target_id", "generation"})}).Create(&p).Error; err != nil {
				return err
			}
		}
		return nil
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
	now := time.Now()
	return g.approvalTransaction(func(tx *gorm.DB) error {
		var target ApprovalTarget
		if err := tx.Where("id = ? AND lease_token = ? AND execution_status IN ?", id, token, []string{"completed", "failed", "timed_out", "cancelled"}).First(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := tx.Model(&target).Updates(map[string]any{"lease_token": "", "lease_until": nil}).Error; err != nil {
			return err
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
func (g *GormDB) ListApprovalTargets(user int, scan string, limit int, cursor string) ([]ApprovalTarget, error) {
	if _, err := g.GetApprovalScan(user, scan); err != nil {
		return nil, err
	}
	q := g.db.Where("scan_id = ? AND user_id = ?", scan, user)
	if cursor != "" {
		q = q.Where("id > ?", cursor)
	}
	var out []ApprovalTarget
	err := q.Order("id ASC").Limit(approvalLimit(limit)).Find(&out).Error
	return out, err
}
func (g *GormDB) ListCurrentApprovalTargets(user int, limit int, cursor string) ([]ApprovalTarget, error) {
	q := g.db.Model(&ApprovalTarget{}).Select("approval_targets.*").Joins("JOIN approval_user_targets p ON p.target_id = approval_targets.id AND p.generation = approval_targets.generation").Where("p.user_id = ?", user)
	if cursor != "" {
		q = q.Where("approval_targets.id > ?", cursor)
	}
	var out []ApprovalTarget
	err := q.Order("approval_targets.id ASC").Limit(approvalLimit(limit)).Find(&out).Error
	return out, err
}

func approvalLease(tx *gorm.DB, id, token string, now time.Time) (*ApprovalTarget, error) {
	var t ApprovalTarget
	if err := tx.Where("id = ? AND lease_token = ? AND lease_until > ?", id, token, now).First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrApprovalLeaseLost
		}
		return nil, err
	}
	if approvalTerminal(t.ExecutionStatus) || t.Deadline == nil || !t.Deadline.After(now) {
		return nil, ErrApprovalLeaseLost
	}
	var s ApprovalScan
	if err := tx.First(&s, "id = ?", t.ScanID).Error; err != nil {
		return nil, err
	}
	if s.CancelRequested {
		return nil, ErrApprovalLeaseLost
	}
	return &t, nil
}

func approvalReleaseBudget(tx *gorm.DB, t *ApprovalTarget) error {
	if t.BudgetDay == "" {
		return nil
	}
	if err := approvalAdjustDailyBudget(tx, t.BudgetDay, t.InputTokens-t.ReservedInput, t.OutputTokens-t.ReservedOutput); err != nil {
		return err
	}
	t.ReservedInput = t.InputTokens
	t.ReservedOutput = t.OutputTokens
	return nil
}

func approvalAdjustDailyBudget(tx *gorm.DB, day string, inputDelta, outputDelta int64) error {
	var budget approvalDailyBudget
	if err := tx.First(&budget, "day = ?", day).Error; err != nil {
		return err
	}
	adjust := func(current, delta int64) (int64, error) {
		if current < 0 || delta > 0 && current > math.MaxInt64-delta || delta < 0 && current < -delta {
			return 0, ErrApprovalBudget
		}
		return current + delta, nil
	}
	var err error
	budget.InputTokens, err = adjust(budget.InputTokens, inputDelta)
	if err != nil {
		return err
	}
	budget.OutputTokens, err = adjust(budget.OutputTokens, outputDelta)
	if err != nil {
		return err
	}
	return tx.Save(&budget).Error
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
func approvalTerminate(tx *gorm.DB, t *ApprovalTarget, now time.Time, status, reason string) error {
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
	if err := approvalReleaseBudget(tx, t); err != nil {
		return err
	}
	return tx.Save(t).Error
}

func (g *GormDB) ClaimApprovalTarget(req ApprovalClaim) (*ApprovalTarget, error) {
	if req.Worker == "" || req.MaxSlots <= 0 || req.LeaseDuration <= 0 || req.TargetDuration <= 0 {
		return nil, fmt.Errorf("invalid approval claim")
	}
	var claimed *ApprovalTarget
	err := g.approvalTransaction(func(tx *gorm.DB) error {
		var cancelling []ApprovalScan
		if err := tx.Where("status = ?", "cancelling").Find(&cancelling).Error; err != nil {
			return err
		}
		for _, scan := range cancelling {
			if err := approvalSettleParent(tx, scan.ID, req.Now); err != nil {
				return err
			}
		}
		var targets []ApprovalTarget
		if err := tx.Where("execution_status IN ?", []string{"queued", "collecting", "investigating", "validating"}).Order("created_at ASC, id ASC").Find(&targets).Error; err != nil {
			return err
		}
		perUser := req.MaxPerUser
		if perUser <= 0 {
			perUser = 1
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
		for i := range targets {
			t := &targets[i]
			scan, ok := scans[t.ScanID]
			if !ok {
				return gorm.ErrRecordNotFound
			}
			reason, status := "", "timed_out"
			switch {
			case scan.CancelRequested:
				reason = "cancelled"
				status = "cancelled"
			case t.StartedAt == nil && !scan.Deadline.After(req.Now):
				reason = "queue_deadline"
			case t.Deadline != nil && !t.Deadline.After(req.Now):
				reason = "target_deadline"
			case t.LeaseUntil != nil && !t.LeaseUntil.After(req.Now) && t.Attempts >= 2:
				reason = "recovery_exhausted"
				status = "failed"
			}
			if reason != "" {
				if err := approvalTerminate(tx, t, req.Now, status, reason); err != nil {
					return err
				}
				if err := approvalSettleParent(tx, t.ScanID, req.Now); err != nil {
					return err
				}
				continue
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
		if slots >= req.MaxSlots {
			return nil
		}
		for i := range targets {
			t := &targets[i]
			if approvalTerminal(t.ExecutionStatus) || activeUsers[t.UserID] >= perUser || (t.LeaseUntil != nil && t.LeaseUntil.After(req.Now)) {
				continue
			}
			if t.StartedAt == nil {
				started := req.Now
				deadline := req.Now.Add(req.TargetDuration)
				t.StartedAt = &started
				t.Deadline = &deadline
			}
			until := req.Now.Add(req.LeaseDuration)
			if until.After(*t.Deadline) {
				until = *t.Deadline
			}
			t.Attempts++
			t.LeaseToken = req.Worker + ":" + approvalToken()
			t.LeaseUntil = &until
			t.ExecutionStatus = "collecting"
			t.PendingCallID = ""
			t.PendingInput = 0
			t.PendingOutput = 0
			if err := tx.Save(t).Error; err != nil {
				return err
			}
			if err := approvalSettleParent(tx, t.ScanID, req.Now); err != nil {
				return err
			}
			copy := *t
			claimed = &copy
			return nil
		}
		return nil
	}, &req.Now)
	return claimed, err
}

func (g *GormDB) HeartbeatApprovalTarget(id, token string, now time.Time, duration time.Duration) error {
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
		until := now.Add(duration)
		if until.After(*t.Deadline) {
			until = *t.Deadline
		}
		return tx.Model(t).Update("lease_until", until).Error
	}, &now)
}
func (g *GormDB) SetApprovalTargetStage(id, token, stage string, now time.Time) error {
	if stage != "collecting" && stage != "investigating" && stage != "validating" {
		return fmt.Errorf("invalid approval stage")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
		return tx.Model(t).Update("execution_status", stage).Error
	}, &now)
}

func (g *GormDB) SetApprovalTargetProgress(id, token, summary string, now time.Time) error {
	if len(summary) > 160 || len(strings.Fields(summary)) > 10 {
		return fmt.Errorf("invalid approval progress summary")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
		return tx.Model(t).Update("summary", summary).Error
	}, &now)
}

func (g *GormDB) SaveApprovalSnapshot(id, token, body string, now time.Time) error {
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("invalid snapshot JSON")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		if _, err := approvalLease(tx, id, token, now); err != nil {
			return err
		}
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
	}, &now)
}

func (g *GormDB) ReserveApprovalBudget(id, token string, now time.Time, dailyInput, dailyOutput, targetInput, targetOutput int64) error {
	if targetInput <= 0 || targetOutput <= 0 {
		return ErrApprovalBudget
	}
	capped := dailyInput > 0 && dailyOutput > 0
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
		if t.BudgetDay != "" {
			return nil
		}
		day := now.UTC().Format("2006-01-02")
		var b approvalDailyBudget
		err = tx.First(&b, "day = ?", day).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			b.Day = day
		} else if err != nil {
			return err
		}
		if capped && (b.InputTokens+targetInput > dailyInput || b.OutputTokens+targetOutput > dailyOutput) {
			return ErrApprovalBudget
		}
		b.InputTokens += targetInput
		b.OutputTokens += targetOutput
		if err := tx.Save(&b).Error; err != nil {
			return err
		}
		t.BudgetDay = day
		t.ReservedInput = targetInput
		t.ReservedOutput = targetOutput
		return tx.Save(t).Error
	}, &now)
}
func (g *GormDB) ReserveApprovalCall(id, token string, now time.Time, r ApprovalCallReservation) error {
	if r.CallID == "" || r.InputTokens < 0 || r.OutputTokens < 0 || r.ToolCalls < 0 {
		return fmt.Errorf("invalid call reservation")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
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
	}, &now)
}
func (g *GormDB) CompleteApprovalCall(id, token, callID string, now time.Time, input, output int64) error {
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
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
	}, &now)
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
	return g.approvalTransaction(func(tx *gorm.DB) error {
		if f.ExecutionStatus == "timed_out" {
			var expired ApprovalTarget
			if err := tx.Where("id = ? AND lease_token = ?", id, token).First(&expired).Error; err != nil {
				return ErrApprovalLeaseLost
			}
			if !approvalTerminal(expired.ExecutionStatus) && expired.Deadline != nil && !expired.Deadline.After(now) {
				if err := approvalTerminate(tx, &expired, now, "timed_out", "deadline_exceeded"); err != nil {
					return err
				}
				return approvalSettleParent(tx, expired.ScanID, now)
			}
		}
		t, err := approvalLease(tx, id, token, now)
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
		}
		if f.Decision == "candidate" {
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
		t.ExecutionStatus = f.ExecutionStatus
		t.Decision = f.Decision
		if f.ExecutionStatus != "completed" {
			t.Decision = ""
		}
		t.Freshness = f.Freshness
		if f.Freshness == "current" && t.ObservedChangeAt != nil && t.StartedAt != nil && !t.ObservedChangeAt.Before(*t.StartedAt) {
			t.Freshness = "stale"
			f.ValidUntil = &now
			f.ReasonCodesJSON = approvalReason("observed_pr_change")
		}
		t.ReasonCodesJSON = f.ReasonCodesJSON
		t.Summary = f.Summary
		t.ValidatedAt = f.ValidatedAt
		t.ValidUntil = f.ValidUntil
		t.CompletedAt = &now
		t.LeaseToken = ""
		t.LeaseUntil = nil
		t.PendingCallID = ""
		t.PendingInput = 0
		t.PendingOutput = 0
		if err := approvalReleaseBudget(tx, t); err != nil {
			return err
		}
		if err := tx.Save(t).Error; err != nil {
			return err
		}
		return approvalSettleParent(tx, t.ScanID, now)
	}, &now)
}

func (g *GormDB) ReserveApprovalUsage(id, token string, now time.Time, r ApprovalUsageReservation) error {
	if r.ID == "" || r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.ToolBytes < 0 || r.Usage.Rounds < 0 || r.Usage.ToolCalls < 0 {
		return fmt.Errorf("invalid usage reservation")
	}
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
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
	}, &now)
}

func (g *GormDB) SettleApprovalUsage(id, token, reservationID string, now time.Time, actual ApprovalUsage) error {
	return g.approvalTransaction(func(tx *gorm.DB) error {
		t, err := approvalLease(tx, id, token, now)
		if err != nil {
			return err
		}
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
		if inputOver > 0 || outputOver > 0 {
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
	}, &now)
}

func approvalCancel(tx *gorm.DB, user int, id string, now time.Time) error {
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
		if !approvalTerminal(targets[i].ExecutionStatus) {
			if err := approvalTerminate(tx, &targets[i], now, "cancelled", "cancelled"); err != nil {
				return err
			}
		}
	}
	return approvalSettleParent(tx, id, now)
}
func (g *GormDB) CancelApprovalScan(user int, id string, now time.Time) error {
	return g.approvalTransaction(func(tx *gorm.DB) error { return approvalCancel(tx, user, id, now) }, &now)
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
		for _, s := range scans {
			if err := approvalCancel(tx, s.UserID, s.ID, now); err != nil {
				return err
			}
		}
		return nil
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
	return g.approvalTransaction(func(tx *gorm.DB) error {
		q := tx.Where("owner = ? AND repo = ? AND number = ? AND execution_status IN ?", strings.ToLower(owner), strings.ToLower(repo), number, []string{"collecting", "investigating", "validating", "completed"})
		q = q.Where("execution_status <> ? OR id IN (?)", "completed", tx.Model(&approvalProjection{}).Select("target_id"))
		if user > 0 {
			q = q.Where("user_id = ?", user)
		}
		var targets []ApprovalTarget
		if err := q.Find(&targets).Error; err != nil {
			return err
		}
		for _, t := range targets {
			var reasons []string
			_ = json.Unmarshal([]byte(t.ReasonCodesJSON), &reasons)
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
