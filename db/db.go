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
		UNIQUE(repo_owner, repo_name, pr_number)
	);
	`
	_, err := db.conn.Exec(schema)
	return err
}

func (db *DB) GetPR(owner, repo string, prNumber int) (*PR, error) {
	pr := &PR{}
	var reviewedAt sql.NullTime
	err := db.conn.QueryRow(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path
		FROM prs WHERE repo_owner = ? AND repo_name = ? AND pr_number = ?
	`, owner, repo, prNumber).Scan(
		&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
		&pr.LastCommitSHA, &reviewedAt, &pr.ReviewHTMLPath,
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
	return pr, nil
}

func (db *DB) UpsertPR(owner, repo string, prNumber int, commitSHA, htmlPath string) error {
	_, err := db.conn.Exec(`
		INSERT INTO prs (repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_owner, repo_name, pr_number)
		DO UPDATE SET last_commit_sha = ?, last_reviewed_at = ?, review_html_path = ?
	`, owner, repo, prNumber, commitSHA, time.Now(), htmlPath,
		commitSHA, time.Now(), htmlPath)
	return err
}

func (db *DB) GetAllPRs() ([]PR, error) {
	rows, err := db.conn.Query(`
		SELECT id, repo_owner, repo_name, pr_number, last_commit_sha, last_reviewed_at, review_html_path
		FROM prs
		ORDER BY last_reviewed_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PR
	for rows.Next() {
		pr := PR{}
		var reviewedAt sql.NullTime
		if err := rows.Scan(&pr.ID, &pr.RepoOwner, &pr.RepoName, &pr.PRNumber,
			&pr.LastCommitSHA, &reviewedAt, &pr.ReviewHTMLPath); err != nil {
			return nil, err
		}
		if reviewedAt.Valid {
			pr.LastReviewedAt = &reviewedAt.Time
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (db *DB) Close() error {
	return db.conn.Close()
}
