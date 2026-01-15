package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

type PR struct {
	ID              int
	RepoOwner       string
	RepoName        string
	PRNumber        int
	LastCommitSHA   string
	LastReviewedAt  *time.Time
	ReviewHTMLPath  string
	Status          string // "pending", "generating", "completed", "error"
	GeneratingSince *time.Time
	IsMine          bool      // true if this is my PR (authored by me)
	Title           string    // PR title from GitHub
	Author          string    // PR author from GitHub
	ApprovalCount   int       // Number of current approvals
	MyReviewStatus  string     // "APPROVED", "CHANGES_REQUESTED", "COMMENTED", or ""
	CreatedAt       *time.Time // PR creation timestamp from GitHub
	Draft           bool       // true if PR is in draft mode
	Notes           string     // User notes (max 15 chars)
	CIState         string     // CI status: "success", "failure", "pending", "unknown"
	CIFailedChecks  string     // JSON array of failed check names
}

type DB struct {
	conn *sql.DB
}

func New(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	if err := conn.Ping(); err != nil {
		return nil, err
	}

	db := &DB{conn: conn}
	if err := db.initSchema(); err != nil {
		return nil, err
	}

	return db, nil
}

func (db *DB) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS prs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_owner TEXT NOT NULL,
		repo_name TEXT NOT NULL,
		pr_number INTEGER NOT NULL,
		last_commit_sha TEXT NOT NULL,
		last_reviewed_at TIMESTAMP,
		review_html_path TEXT,
		status TEXT DEFAULT 'pending',
		UNIQUE(repo_owner, repo_name, pr_number)
	);
	`
	if _, err := db.conn.Exec(schema); err != nil {
		return err
	}

	// Run migrations for additional columns (safe to run multiple times)
	// Duplicate column errors are ignored, but other errors will fail fast
	// Wrap all migrations in a transaction for atomicity
	migrations := []string{
		`ALTER TABLE prs ADD COLUMN status TEXT DEFAULT 'pending'`,
		`ALTER TABLE prs ADD COLUMN generating_since TIMESTAMP`,
		`ALTER TABLE prs ADD COLUMN is_mine INTEGER DEFAULT 0`,
		`ALTER TABLE prs ADD COLUMN title TEXT DEFAULT ''`,
		`ALTER TABLE prs ADD COLUMN author TEXT DEFAULT ''`,
		`ALTER TABLE prs ADD COLUMN approval_count INTEGER DEFAULT 0`,
		`ALTER TABLE prs ADD COLUMN my_review_status TEXT DEFAULT ''`,
		`ALTER TABLE prs ADD COLUMN created_at TIMESTAMP`,
		`ALTER TABLE prs ADD COLUMN draft INTEGER DEFAULT 0`,
		`ALTER TABLE prs ADD COLUMN notes TEXT DEFAULT ''`,
		`ALTER TABLE prs ADD COLUMN ci_state TEXT DEFAULT 'unknown'`,
		`ALTER TABLE prs ADD COLUMN ci_failed_checks TEXT DEFAULT '[]'`,
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin migration transaction: %w", err)
	}

	for _, migration := range migrations {
		_, err := tx.Exec(migration)
		if err != nil {
			// Only ignore "duplicate column" errors - these are expected for existing databases
			// Use type assertion to check for sqlite3.Error for more robust error handling
			var sqliteErr sqlite3.Error
			if errors.As(err, &sqliteErr) {
				// Check if it's a duplicate column error by examining the error message
				// SQLite returns "duplicate column" in the error message for ALTER TABLE ADD COLUMN
				if strings.Contains(sqliteErr.Error(), "duplicate column") {
					continue // Expected error, safe to ignore
				}
			}
			// Rollback transaction on any other error
			tx.Rollback()
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, migration)
		}
	}

	// Commit all migrations atomically
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migrations: %w", err)
	}

	return nil
}

// scanPRRow scans a database row into a PR struct, handling nullable fields
func scanPRRow(pr *PR, reviewedAt, generatingSince, createdAt sql.NullTime, htmlPath sql.NullString, isMine, draft int, title, author, myReviewStatus, notes, ciState, ciFailedChecks sql.NullString) {
	if reviewedAt.Valid {
		pr.LastReviewedAt = &reviewedAt.Time
	}
	if htmlPath.Valid {
		pr.ReviewHTMLPath = htmlPath.String
	}
	if generatingSince.Valid {
		pr.GeneratingSince = &generatingSince.Time
	}
	if createdAt.Valid {
		pr.CreatedAt = &createdAt.Time
	}
	pr.IsMine = isMine == 1
	pr.Draft = draft == 1
	if title.Valid {
		pr.Title = title.String
	}
	if author.Valid {
		pr.Author = author.String
	}
	if myReviewStatus.Valid {
		pr.MyReviewStatus = myReviewStatus.String
	}
	if notes.Valid {
		pr.Notes = notes.String
	}
	if ciState.Valid {
		pr.CIState = ciState.String
	}
	if ciFailedChecks.Valid {
		pr.CIFailedChecks = ciFailedChecks.String
	}
}

func (db *DB) GetPR(owner, repo string, prNumber int) (*PR, error) {
	pr := &PR{}
	var reviewedAt sql.NullTime
	var htmlPath sql.NullString
	var generatingSince sql.NullTime
	var createdAt sql.NullTime
	var isMine, draft int
	var title, author, myReviewStatus, notes, ciState, ciFailedChecks sql.NullString
	err := db.conn.QueryRow(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending'), generating_since, COALESCE(is_mine, 0), COALESCE(title, ''), COALESCE(author, ''), COALESCE(approval_count, 0), COALESCE(my_review_status, ''), created_at, COALESCE(draft, 0), COALESCE(notes, ''), COALESCE(ci_state, 'unknown'), COALESCE(ci_failed_checks, '[]')
		FROM prs WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, owner, repo, prNumber).Scan(
		&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
		&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status, &generatingSince, &isMine, &title, &author, &pr.ApprovalCount, &myReviewStatus, &createdAt, &draft, &notes, &ciState, &ciFailedChecks,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	scanPRRow(pr, reviewedAt, generatingSince, createdAt, htmlPath, isMine, draft, title, author, myReviewStatus, notes, ciState, ciFailedChecks)
	return pr, nil
}

func (db *DB) UpsertPR(pr *PR) error {
	isMineInt := 0
	if pr.IsMine {
		isMineInt = 1
	}
	draftInt := 0
	if pr.Draft {
		draftInt = 1
	}

	// Use the provided LastReviewedAt, or NULL if not set
	var lastReviewedAt interface{}
	if pr.LastReviewedAt != nil {
		lastReviewedAt = *pr.LastReviewedAt
	}

	// Use the provided CreatedAt, or NULL if not set
	var createdAt interface{}
	if pr.CreatedAt != nil {
		createdAt = *pr.CreatedAt
	}

	// Use NULL for generating_since in UpsertPR (it's only set via SetPRGenerating)
	var generatingSince interface{}

	// Build UPDATE clause dynamically: only update created_at if provided (not nil)
	updateClause := `
		last_commit_sha = ?,
		last_reviewed_at = COALESCE(?, last_reviewed_at),
		review_html_path = ?,
		status = ?,
		generating_since = NULL,
		is_mine = ?,
		title = ?,
		author = ?,
		approval_count = ?,
		my_review_status = ?,`

	updateParams := []interface{}{
		pr.LastCommitSHA, lastReviewedAt, pr.ReviewHTMLPath, pr.Status, isMineInt, pr.Title, pr.Author, pr.ApprovalCount, pr.MyReviewStatus,
	}

	// Only update created_at if we have a value (not nil), otherwise preserve database value
	if pr.CreatedAt != nil {
		updateClause += `
		created_at = ?,`
		updateParams = append(updateParams, createdAt)
	}

	updateClause += `
		draft = ?,
		notes = ?,
		ci_state = ?,
		ci_failed_checks = ?`
	updateParams = append(updateParams, draftInt, pr.Notes, pr.CIState, pr.CIFailedChecks)

	query := `
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, status, generating_since, is_mine, title, author, approval_count, my_review_status, created_at, draft, notes, ci_state, ci_failed_checks)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET` + updateClause

	insertParams := []interface{}{pr.RepoOwner, pr.RepoName, pr.PRNumber, pr.LastCommitSHA, lastReviewedAt, pr.ReviewHTMLPath, pr.Status, generatingSince, isMineInt, pr.Title, pr.Author, pr.ApprovalCount, pr.MyReviewStatus, createdAt, draftInt, pr.Notes, pr.CIState, pr.CIFailedChecks}
	allParams := append(insertParams, updateParams...)

	_, err := db.conn.Exec(query, allParams...)
	return err
}

func (db *DB) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	_, err := db.conn.Exec(`
		UPDATE prs SET status = ? WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, status, owner, repo, prNumber)
	return err
}

// ResetPRToOutdated resets a PR to pending status with new commit SHA and clears old review data
func (db *DB) ResetPRToOutdated(owner, repo string, prNumber int, newCommitSHA string) error {
	_, err := db.conn.Exec(`
		UPDATE prs
		SET status = 'pending',
		    last_commit_sha = ?,
		    review_html_path = NULL,
		    last_reviewed_at = NULL,
		    generating_since = NULL
		WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, newCommitSHA, owner, repo, prNumber)
	return err
}

func (db *DB) SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, isMine bool, createdAt *time.Time, draft bool) error {
	now := time.Now().UTC()
	isMineInt := 0
	if isMine {
		isMineInt = 1
	}
	draftInt := 0
	if draft {
		draftInt = 1
	}

	// Use the provided CreatedAt, or NULL if not set
	var createdAtVal interface{}
	if createdAt != nil {
		createdAtVal = *createdAt
	}

	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, status, generating_since, is_mine, title, author, review_html_path, created_at, draft)
		VALUES (?, ?, ?, ?, 'generating', ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, status = 'generating', generating_since = ?, is_mine = ?, title = ?, author = ?, review_html_path = NULL, created_at = ?, draft = ?
	`, owner, repo, prNumber, commitSHA, now, isMineInt, title, author, createdAtVal, draftInt, commitSHA, now, isMineInt, title, author, createdAtVal, draftInt)
	return err
}

func (db *DB) GetAllPRs() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending'), generating_since, COALESCE(is_mine, 0), COALESCE(title, ''), COALESCE(author, ''), COALESCE(approval_count, 0), COALESCE(my_review_status, ''), created_at, COALESCE(draft, 0), COALESCE(notes, ''), COALESCE(ci_state, 'unknown'), COALESCE(ci_failed_checks, '[]')
		FROM prs
		ORDER BY
			is_mine ASC,
			created_at DESC NULLS LAST,
			CASE status
				WHEN 'generating' THEN 1
				WHEN 'pending' THEN 2
				WHEN 'completed' THEN 3
				ELSE 4
			END
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PR
	for rows.Next() {
		pr := PR{}
		var reviewedAt sql.NullTime
		var htmlPath sql.NullString
		var generatingSince sql.NullTime
		var createdAt sql.NullTime
		var isMine, draft int
		var title, author, myReviewStatus, notes, ciState, ciFailedChecks sql.NullString
		if err := rows.Scan(&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
			&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status, &generatingSince, &isMine, &title, &author, &pr.ApprovalCount, &myReviewStatus, &createdAt, &draft, &notes, &ciState, &ciFailedChecks); err != nil {
			return nil, err
		}
		scanPRRow(&pr, reviewedAt, generatingSince, createdAt, htmlPath, isMine, draft, title, author, myReviewStatus, notes, ciState, ciFailedChecks)
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (db *DB) DeletePR(owner, repo string, prNumber int) error {
	_, err := db.conn.Exec(`
		DELETE FROM prs WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, owner, repo, prNumber)
	return err
}

func (db *DB) ResetStaleGeneratingPRs(timeoutMinutes int) (int, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(timeoutMinutes) * time.Minute)
	result, err := db.conn.Exec(`
		UPDATE prs
		SET status = 'pending', generating_since = NULL
		WHERE status = 'generating'
		AND (generating_since IS NULL OR generating_since < ?)
	`, cutoff)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

func (db *DB) ResetErrorPRs(maxAgeMinutes int) (int, error) {
	// Reset PRs that have been in error state for more than maxAgeMinutes
	// This allows them to be retried on the next poll
	cutoff := time.Now().UTC().Add(-time.Duration(maxAgeMinutes) * time.Minute)
	result, err := db.conn.Exec(`
		UPDATE prs
		SET status = 'pending'
		WHERE status = 'error'
		AND (last_reviewed_at IS NULL OR last_reviewed_at < ?)
	`, cutoff)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// GetPRsWithMissingMetadata returns PRs that don't have title or author set
func (db *DB) GetPRsWithMissingMetadata() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending'), generating_since, COALESCE(is_mine, 0), COALESCE(title, ''), COALESCE(author, '')
		FROM prs
		WHERE (title IS NULL OR title = '') OR (author IS NULL OR author = '')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PR
	for rows.Next() {
		pr := PR{}
		var reviewedAt sql.NullTime
		var htmlPath sql.NullString
		var generatingSince sql.NullTime
		var isMine int
		var title, author sql.NullString
		if err := rows.Scan(&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
			&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status, &generatingSince, &isMine, &title, &author); err != nil {
			return nil, err
		}
		if reviewedAt.Valid {
			pr.LastReviewedAt = &reviewedAt.Time
		}
		if htmlPath.Valid {
			pr.ReviewHTMLPath = htmlPath.String
		}
		if generatingSince.Valid {
			pr.GeneratingSince = &generatingSince.Time
		}
		pr.IsMine = isMine == 1
		if title.Valid {
			pr.Title = title.String
		}
		if author.Valid {
			pr.Author = author.String
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

// UpdatePRMetadata updates only the title and author for a PR
func (db *DB) UpdatePRMetadata(owner, repo string, prNumber int, title, author string) error {
	_, err := db.conn.Exec(`
		UPDATE prs SET title = ?, author = ? WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, title, author, owner, repo, prNumber)
	return err
}

// UpdatePRNotes updates only the notes field for a PR
func (db *DB) UpdatePRNotes(owner, repo string, prNumber int, notes string) error {
	// Truncate to 15 chars as defensive measure
	if len(notes) > 15 {
		notes = notes[:15]
	}
	_, err := db.conn.Exec(`
		UPDATE prs SET notes = ? WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, notes, owner, repo, prNumber)
	return err
}

// GetPRsWithMissingCreatedAt returns PRs that don't have created_at set
func (db *DB) GetPRsWithMissingCreatedAt() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha
		FROM prs
		WHERE created_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PR
	for rows.Next() {
		pr := PR{}
		if err := rows.Scan(&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber, &pr.LastCommitSHA); err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

// UpdatePRCreatedAt updates only the created_at field for a PR
func (db *DB) UpdatePRCreatedAt(owner, repo string, prNumber int, createdAt time.Time) error {
	_, err := db.conn.Exec(`
		UPDATE prs SET created_at = ? WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, createdAt, owner, repo, prNumber)
	return err
}
