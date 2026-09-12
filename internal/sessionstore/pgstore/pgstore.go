// Package pgstore is the Postgres-backed implementation of
// sessionstore.Store, targeting the `rubbish` schema of a shared Postgres
// instance (the other schema, `temporal`, belongs to Temporal's own server
// and is never touched here).
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/store"
)

// Store is a Postgres-backed sessionstore.Store. All tables live under the
// `rubbish` schema, kept isolated from Temporal's `temporal` schema in the
// same database.
type Store struct {
	db *sql.DB
}

var _ sessionstore.Store = (*Store)(nil)

// Open connects to Postgres at dsn and applies any pending goose migrations
// under internal/store/migrations against the `rubbish` schema before
// returning. Safe to call on every process start — migrations are
// idempotent/tracked via goose's own version table.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	// goose creates its version table before running any migration, so the
	// schema it lives in must already exist. The first migration also issues
	// this (idempotently) for fresh databases created some other way.
	if _, err := db.Exec(`CREATE SCHEMA IF NOT EXISTS rubbish`); err != nil {
		return fmt.Errorf("create rubbish schema: %w", err)
	}

	migrationsFS, err := fs.Sub(store.MigrationsFS, store.MigrationsDir)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS,
		// Track applied migrations in the rubbish schema, not public, so the
		// whole app footprint stays inside one schema.
		goose.WithTableName("rubbish.goose_db_version"),
	)
	if err != nil {
		return err
	}
	_, err = provider.Up(context.Background())
	return err
}

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts or updates a session row.
func (s *Store) Upsert(r sessionstore.Row) error {
	_, err := s.db.Exec(`
		INSERT INTO rubbish.sessions
			(id, slot, status, repo_url, branch, error_msg, dev_mode, created_at, updated_at, role, prompt, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			slot = EXCLUDED.slot,
			status = EXCLUDED.status,
			repo_url = EXCLUDED.repo_url,
			branch = EXCLUDED.branch,
			error_msg = EXCLUDED.error_msg,
			dev_mode = EXCLUDED.dev_mode,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at,
			role = EXCLUDED.role,
			prompt = EXCLUDED.prompt,
			result = EXCLUDED.result
	`, r.ID, r.Slot, r.Status, r.RepoURL, r.Branch, r.ErrorMsg, r.DevMode, r.CreatedAt, r.UpdatedAt, r.Role, r.Prompt, r.Result)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// Delete removes a session row. No-ops if the row does not exist.
func (s *Store) Delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM rubbish.sessions WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// Get returns a single session row by ID. Returns false if not found.
func (s *Store) Get(id string) (sessionstore.Row, bool, error) {
	row := s.db.QueryRow(`
		SELECT id, slot, status, repo_url, branch, error_msg, dev_mode, created_at, updated_at, role, prompt, result
		FROM rubbish.sessions WHERE id = $1
	`, id)

	var r sessionstore.Row
	err := row.Scan(&r.ID, &r.Slot, &r.Status, &r.RepoURL, &r.Branch, &r.ErrorMsg, &r.DevMode, &r.CreatedAt, &r.UpdatedAt, &r.Role, &r.Prompt, &r.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionstore.Row{}, false, nil
	}
	if err != nil {
		return sessionstore.Row{}, false, fmt.Errorf("get session: %w", err)
	}
	return r, true, nil
}

// List returns all session rows ordered by created_at ascending.
func (s *Store) List() ([]sessionstore.Row, error) {
	rows, err := s.db.Query(`
		SELECT id, slot, status, repo_url, branch, error_msg, dev_mode, created_at, updated_at, role, prompt, result
		FROM rubbish.sessions ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []sessionstore.Row
	for rows.Next() {
		var r sessionstore.Row
		if err := rows.Scan(&r.ID, &r.Slot, &r.Status, &r.RepoURL, &r.Branch, &r.ErrorMsg, &r.DevMode, &r.CreatedAt, &r.UpdatedAt, &r.Role, &r.Prompt, &r.Result); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// UpsertFavorite inserts or replaces a favorite row.
func (s *Store) UpsertFavorite(f sessionstore.Favorite) error {
	_, err := s.db.Exec(`
		INSERT INTO rubbish.favorites (id, name, repo_url, branch, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			repo_url = EXCLUDED.repo_url,
			branch = EXCLUDED.branch,
			created_at = EXCLUDED.created_at
	`, f.ID, f.Name, f.RepoURL, f.Branch, f.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert favorite: %w", err)
	}
	return nil
}

// DeleteFavorite removes a favorite row. No-ops if the row does not exist.
func (s *Store) DeleteFavorite(id string) error {
	if _, err := s.db.Exec(`DELETE FROM rubbish.favorites WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete favorite: %w", err)
	}
	return nil
}

// ListFavorites returns all favorites ordered by created_at ascending.
func (s *Store) ListFavorites() ([]sessionstore.Favorite, error) {
	rows, err := s.db.Query(`
		SELECT id, name, repo_url, branch, created_at
		FROM rubbish.favorites ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	defer rows.Close()

	var out []sessionstore.Favorite
	for rows.Next() {
		var f sessionstore.Favorite
		if err := rows.Scan(&f.ID, &f.Name, &f.RepoURL, &f.Branch, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan favorite: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	return out, nil
}
