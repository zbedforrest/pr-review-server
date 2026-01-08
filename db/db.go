package db

import (
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
	IsMine          bool   // true if this is my PR (authored by me)
	Title           string // PR title from GitHub
	Author          string // PR author from GitHub
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

	// Add is_mine column to distinguish my PRs from PRs to review
	db.conn.Exec(`ALTER TABLE prs ADD COLUMN is_mine INTEGER DEFAULT 0`)

	// Add title and author columns for displaying PR metadata
	db.conn.Exec(`ALTER TABLE prs ADD COLUMN title TEXT DEFAULT ''`)
	db.conn.Exec(`ALTER TABLE prs ADD COLUMN author TEXT DEFAULT ''`)

	return nil
}

func (db *DB) GetPR(owner, repo string, prNumber int) (*PR, error) {
	pr := &PR{}
	var reviewedAt sql.NullTime
	var htmlPath sql.NullString
	var generatingSince sql.NullTime
	var isMine int
	var title, author sql.NullString
	err := db.conn.QueryRow(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending'), generating_since, COALESCE(is_mine, 0), COALESCE(title, ''), COALESCE(author, '')
		FROM prs WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, owner, repo, prNumber).Scan(
		&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
		&pr.LastCommitSHA, &reviewedAt, &htmlPath, &pr.Status, &generatingSince, &isMine, &title, &author,
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
	return pr, nil
}

func (db *DB) UpsertPR(owner, repo string, prNumber int, commitSHA, htmlPath, status, title, author string, isMine bool) error {
	now := time.Now().UTC()
	isMineInt := 0
	if isMine {
		isMineInt = 1
	}
	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, status, generating_since, is_mine, title, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, last_reviewed_at = ?, review_html_path = ?, status = ?, generating_since = NULL, is_mine = ?, title = ?, author = ?
	`, owner, repo, prNumber, commitSHA, now, htmlPath, status, isMineInt, title, author,
		commitSHA, now, htmlPath, status, isMineInt, title, author)
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
		    review_html_path = '',
		    last_reviewed_at = NULL,
		    generating_since = NULL
		WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, newCommitSHA, owner, repo, prNumber)
	return err
}

func (db *DB) SetPRGenerating(owner, repo string, prNumber int, commitSHA, title, author string, isMine bool) error {
	now := time.Now().UTC()
	isMineInt := 0
	if isMine {
		isMineInt = 1
	}
	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, status, generating_since, is_mine, title, author, review_html_path)
		VALUES (?, ?, ?, ?, 'generating', ?, ?, ?, ?, '')
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, status = 'generating', generating_since = ?, is_mine = ?, title = ?, author = ?, review_html_path = ''
	`, owner, repo, prNumber, commitSHA, now, isMineInt, title, author, commitSHA, now, isMineInt, title, author)
	return err
}

func (db *DB) GetAllPRs() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path, COALESCE(status, 'pending'), generating_since, COALESCE(is_mine, 0), COALESCE(title, ''), COALESCE(author, '')
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
