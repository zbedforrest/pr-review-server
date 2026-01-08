package db

import (
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type PR struct {
	ID             int
	RepoOwner      string
	RepoName       string
	PRNumber       int
	LastCommitSHA  string
	LastReviewedAt *time.Time
	ReviewHTMLPath string
	Status         string // "pending", "generating", "completed", "error"
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

	// Add status column if it doesn't exist (migration for existing DBs)
	_, err := db.conn.Exec(`ALTER TABLE prs ADD COLUMN status TEXT DEFAULT 'pending'`)
	if err != nil && err.Error() != "duplicate column name: status" {
		// Ignore "duplicate column" error
	}

	// Add generating_since column for tracking stale generation attempts
	db.conn.Exec(`ALTER TABLE prs ADD COLUMN generating_since TIMESTAMP`)

	return nil
}

func (db *DB) GetPR(owner, repo string, prNumber int) (*PR, error) {
	pr := &PR{}
	var reviewedAt sql.NullTime
	var htmlPath sql.NullString
	err := db.conn.QueryRow(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending')
		FROM prs WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, owner, repo, prNumber).Scan(
		&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
		&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reviewedAt.Valid {
		pr.LastReviewedAt = &reviewedAt.Time
	}
	if htmlPath.Valid {
		pr.ReviewHTMLPath = htmlPath.String
	}
	return pr, nil
}

func (db *DB) UpsertPR(owner, repo string, prNumber int, commitSHA, htmlPath, status string) error {
	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, status, generating_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, last_reviewed_at = ?, review_html_path = ?, status = ?, generating_since = NULL
	`, owner, repo, prNumber, commitSHA, time.Now(), htmlPath, status,
		commitSHA, time.Now(), htmlPath, status)
	return err
}

func (db *DB) UpdatePRStatus(owner, repo string, prNumber int, status string) error {
	_, err := db.conn.Exec(`
		UPDATE prs SET status = ? WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, status, owner, repo, prNumber)
	return err
}

func (db *DB) SetPRGenerating(owner, repo string, prNumber int, commitSHA string) error {
	now := time.Now()
	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, status, generating_since)
		VALUES (?, ?, ?, ?, 'generating', ?)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, status = 'generating', generating_since = ?
	`, owner, repo, prNumber, commitSHA, now, commitSHA, now)
	return err
}

func (db *DB) GetAllPRs() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending')
		FROM prs
		ORDER BY
			CASE status
				WHEN 'generating' THEN 1
				WHEN 'pending' THEN 2
				WHEN 'completed' THEN 3
				ELSE 4
			END,
			last_reviewed_at DESC
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
		if err := rows.Scan(&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
			&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status); err != nil {
			return nil, err
		}
		if reviewedAt.Valid {
			pr.LastReviewedAt = &reviewedAt.Time
		}
		if htmlPath.Valid {
			pr.ReviewHTMLPath = htmlPath.String
		}
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
	cutoff := time.Now().Add(-time.Duration(timeoutMinutes) * time.Minute)
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

func (db *DB) Close() error {
	return db.conn.Close()
}
