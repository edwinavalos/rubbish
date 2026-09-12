// Package sessionstore persists VM sessions and launch favorites.
package sessionstore

import "time"

// Row is the persisted representation of a session.
type Row struct {
	ID        string
	Slot      int
	Status    string
	RepoURL   string
	Branch    string
	ErrorMsg  string
	DevMode   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// Role is the session type: "interactive" | "research" | "plan" | "implement".
	// Defaults to "interactive" when empty.
	Role   string
	Prompt string // claude -p prompt for non-interactive sessions
	Result string // captured stdout, set when session reaches Stopped
}

// Favorite is a saved launch template (repo + branch + display name).
type Favorite struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	RepoURL   string    `json:"repo_url"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists sessions and favorites. Implementations: JSONStore (flat
// file, used for tests/local dev) and pgstore.Store (Postgres, production).
type Store interface {
	Upsert(r Row) error
	Get(id string) (Row, bool, error)
	List() ([]Row, error)
	Delete(id string) error

	UpsertFavorite(f Favorite) error
	DeleteFavorite(id string) error
	ListFavorites() ([]Favorite, error)

	Close() error
}
