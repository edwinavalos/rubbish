package sessionstore

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    slot       INTEGER NOT NULL,
    status     TEXT NOT NULL,
    repo_url   TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL DEFAULT '',
    error_msg  TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS favorites (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    repo_url   TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);`

// Row is the persisted representation of a session.
type Row struct {
	ID        string
	Slot      int
	Status    string
	RepoURL   string
	Branch    string
	ErrorMsg  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store wraps a SQLite database for session persistence.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite writer serialisation
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts or updates a session row.
func (s *Store) Upsert(r Row) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, slot, status, repo_url, branch, error_msg, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			slot      = excluded.slot,
			status    = excluded.status,
			repo_url  = excluded.repo_url,
			branch    = excluded.branch,
			error_msg = excluded.error_msg,
			updated_at = excluded.updated_at`,
		r.ID, r.Slot, r.Status, r.RepoURL, r.Branch, r.ErrorMsg,
		r.CreatedAt.UTC().Format(time.RFC3339Nano),
		r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Delete removes a session row. No-ops if the row does not exist.
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// Favorite is a saved launch template (repo + branch + display name).
type Favorite struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	RepoURL   string    `json:"repo_url"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"created_at"`
}

// UpsertFavorite inserts or replaces a favorite row.
func (s *Store) UpsertFavorite(f Favorite) error {
	_, err := s.db.Exec(`
		INSERT INTO favorites (id, name, repo_url, branch, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name       = excluded.name,
			repo_url   = excluded.repo_url,
			branch     = excluded.branch`,
		f.ID, f.Name, f.RepoURL, f.Branch,
		f.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// DeleteFavorite removes a favorite row. No-ops if the row does not exist.
func (s *Store) DeleteFavorite(id string) error {
	_, err := s.db.Exec(`DELETE FROM favorites WHERE id = ?`, id)
	return err
}

// ListFavorites returns all favorites ordered by created_at ascending.
func (s *Store) ListFavorites() ([]Favorite, error) {
	rows, err := s.db.Query(`
		SELECT id, name, repo_url, branch, created_at
		FROM favorites ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Favorite
	for rows.Next() {
		var f Favorite
		var createdAt string
		if err := rows.Scan(&f.ID, &f.Name, &f.RepoURL, &f.Branch, &createdAt); err != nil {
			return nil, err
		}
		f.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, f)
	}
	return out, rows.Err()
}

// List returns all session rows ordered by created_at ascending.
func (s *Store) List() ([]Row, error) {
	rows, err := s.db.Query(`
		SELECT id, slot, status, repo_url, branch, error_msg, created_at, updated_at
		FROM sessions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		var createdAt, updatedAt string
		if err := rows.Scan(&r.ID, &r.Slot, &r.Status, &r.RepoURL, &r.Branch, &r.ErrorMsg, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}
