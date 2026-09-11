package db

import (
	"time"
)

// PollerLeaseModel is a single-row table implementing a leader-election lease.
// Exactly one poller instance holds the lease at a time; only the holder runs
// the automatic poll cycle, so multiple Cloud Run instances (e.g. during a
// deploy overlap) never poll concurrently and race each other on rate limits
// or the via_teams prune.
type PollerLeaseModel struct {
	ID        string    `gorm:"primaryKey;column:id"`
	Holder    string    `gorm:"column:holder;not null"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`
	// Generation is the holder's boot time. A Cloud Run deploy leaves the
	// previous revision's instance alive for up to the request timeout, so
	// leadership is fenced: a newer generation preempts at once and an older
	// one can never take the lease back.
	Generation int64 `gorm:"column:generation;not null;default:0"`
}

func (PollerLeaseModel) TableName() string { return "poller_leases" }

// pollerLeaseID is the fixed primary key of the singleton lease row.
const pollerLeaseID = "poller"

// TryAcquireOrRenewLeadership attempts to acquire the poller lease for holderID,
// or renew it if holderID already holds it. Returns true iff holderID holds a
// valid lease after the call.
//
// One atomic statement: insert the lease if absent, otherwise update it when
// we already hold it, when our boot generation is newer than the row's (a fresh
// deploy preempts the zombie it replaced), or when the lease expired and we are
// at least as new as whoever held it last. A live lease held by a peer of the
// same generation, or any lease ever touched by a newer generation, yields zero
// affected rows.
//
// Times are computed in Go and passed as parameters so the comparison is
// dialect-agnostic (no Postgres now() vs SQLite datetime() divergence).
func (g *GormDB) TryAcquireOrRenewLeadership(holderID string, generation int64, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	expiry := now.Add(ttl)
	res := g.db.Exec(`
		INSERT INTO poller_leases (id, holder, expires_at, generation) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET holder = ?, expires_at = ?, generation = ?
		WHERE poller_leases.holder = ?
		   OR poller_leases.generation < ?
		   OR (poller_leases.expires_at < ? AND poller_leases.generation <= ?)`,
		pollerLeaseID, holderID, expiry, generation,
		holderID, expiry, generation,
		holderID,
		generation,
		now, generation,
	)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
