package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type storeData struct {
	Workflows map[string]*Workflow `json:"workflows"`
}

// Store persists workflows as a JSON file protected by a mutex.
type Store struct {
	mu   sync.RWMutex
	path string
	data storeData
}

// Open loads (or creates) the store at path. Returns an error if the file
// exists but is malformed JSON.
func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: storeData{
			Workflows: make(map[string]*Workflow),
		},
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read workflow store: %w", err)
	}
	if len(b) > 0 {
		if jsonErr := json.Unmarshal(b, &s.data); jsonErr != nil {
			return nil, fmt.Errorf("parse workflow store: %w", jsonErr)
		}
		if s.data.Workflows == nil {
			s.data.Workflows = make(map[string]*Workflow)
		}
	}
	return s, nil
}

// Close is a no-op; kept for API compatibility.
func (s *Store) Close() error { return nil }

func (s *Store) save() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("marshal workflow store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".workflow-store-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp workflow store: %w", err)
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(b)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("write temp workflow store: %w", writeErr)
	}
	if closeErr != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("close temp workflow store: %w", closeErr)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return fmt.Errorf("rename temp workflow store: %w", err)
	}
	return nil
}

// Upsert inserts or updates a workflow. UpdatedAt is set to now before saving.
func (s *Store) Upsert(wf *Workflow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wf.UpdatedAt = time.Now()
	// Store a copy so the caller cannot mutate stored state through the pointer.
	cp := *wf
	s.data.Workflows[wf.ID] = &cp
	return s.save()
}

// Get returns a copy of the workflow with the given ID, or false if not found.
func (s *Store) Get(id string) (*Workflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wf, ok := s.data.Workflows[id]
	if !ok {
		return nil, false
	}
	cp := *wf
	return &cp, true
}

// List returns all workflows sorted by CreatedAt descending (newest first).
func (s *Store) List() []*Workflow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Workflow, 0, len(s.data.Workflows))
	for _, wf := range s.data.Workflows {
		cp := *wf
		result = append(result, &cp)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}
