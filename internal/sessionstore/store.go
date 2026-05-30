package sessionstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

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

type storeData struct {
	Sessions  map[string]Row      `json:"sessions"`
	Favorites map[string]Favorite `json:"favorites"`
}

// Store persists sessions and favorites as a JSON file protected by a mutex.
type Store struct {
	mu   sync.RWMutex
	path string
	data storeData
}

// Open loads (or creates) the store at path. If the file exists but is not
// valid JSON (e.g. a leftover SQLite file) it starts empty without error.
func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: storeData{
			Sessions:  make(map[string]Row),
			Favorites: make(map[string]Favorite),
		},
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read store: %w", err)
	}
	if len(b) > 0 {
		if jsonErr := json.Unmarshal(b, &s.data); jsonErr != nil {
			// Non-JSON file (e.g. old SQLite DB) — start fresh.
			s.data.Sessions = make(map[string]Row)
			s.data.Favorites = make(map[string]Favorite)
		}
		if s.data.Sessions == nil {
			s.data.Sessions = make(map[string]Row)
		}
		if s.data.Favorites == nil {
			s.data.Favorites = make(map[string]Favorite)
		}
	}
	return s, nil
}

// Close is a no-op; kept for API compatibility.
func (s *Store) Close() error { return nil }

func (s *Store) save() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".store-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp store: %w", err)
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(b)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("write temp store: %w", writeErr)
	}
	if closeErr != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("close temp store: %w", closeErr)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("rename temp store: %w", err)
	}
	return nil
}

// Upsert inserts or updates a session row.
func (s *Store) Upsert(r Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Sessions[r.ID] = r
	return s.save()
}

// Delete removes a session row. No-ops if the row does not exist.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Sessions, id)
	return s.save()
}

// reload re-reads the file into s.data. Caller must hold s.mu for writing.
func (s *Store) reload() {
	b, err := os.ReadFile(s.path)
	if err != nil || len(b) == 0 {
		return
	}
	var d storeData
	if json.Unmarshal(b, &d) != nil {
		return
	}
	if d.Sessions != nil {
		s.data.Sessions = d.Sessions
	}
	if d.Favorites != nil {
		s.data.Favorites = d.Favorites
	}
}

// Get returns a single session row by ID. Returns false if not found.
// It re-reads the backing file so that a second process writing the same file
// (e.g. rubbish-poc writing, rubbish-terminal reading) always sees current state.
func (s *Store) Get(id string) (Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	r, ok := s.data.Sessions[id]
	return r, ok, nil
}

// List returns all session rows ordered by created_at ascending.
func (s *Store) List() ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	rows := make([]Row, 0, len(s.data.Sessions))
	for _, r := range s.data.Sessions {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	return rows, nil
}

// UpsertFavorite inserts or replaces a favorite row.
func (s *Store) UpsertFavorite(f Favorite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Favorites[f.ID] = f
	return s.save()
}

// DeleteFavorite removes a favorite row. No-ops if the row does not exist.
func (s *Store) DeleteFavorite(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Favorites, id)
	return s.save()
}

// ListFavorites returns all favorites ordered by created_at ascending.
func (s *Store) ListFavorites() ([]Favorite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	favs := make([]Favorite, 0, len(s.data.Favorites))
	for _, f := range s.data.Favorites {
		favs = append(favs, f)
	}
	sort.Slice(favs, func(i, j int) bool {
		return favs[i].CreatedAt.Before(favs[j].CreatedAt)
	})
	return favs, nil
}
