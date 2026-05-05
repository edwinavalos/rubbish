package sessionstore_test

import (
	"os"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/sessionstore"
)

func openTmp(t *testing.T) *sessionstore.Store {
	t.Helper()
	f, err := os.CreateTemp("", "rubbish-sessions-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := sessionstore.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndList(t *testing.T) {
	s := openTmp(t)

	now := time.Now().UTC().Truncate(time.Second)
	r := sessionstore.Row{
		ID:        "abc-123",
		Slot:      1,
		Status:    "ready",
		RepoURL:   "https://github.com/user/repo",
		Branch:    "main",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Upsert(r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	rows, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.ID != r.ID || got.Status != r.Status || got.RepoURL != r.RepoURL {
		t.Errorf("row mismatch: got %+v", got)
	}
}

func TestUpsertUpdates(t *testing.T) {
	s := openTmp(t)
	now := time.Now().UTC()

	s.Upsert(sessionstore.Row{ID: "x", Slot: 0, Status: "provisioning", CreatedAt: now, UpdatedAt: now}) //nolint:errcheck

	updated := now.Add(time.Second)
	if err := s.Upsert(sessionstore.Row{ID: "x", Slot: 0, Status: "ready", CreatedAt: now, UpdatedAt: updated}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Status != "ready" {
		t.Errorf("status = %q, want ready", rows[0].Status)
	}
}

func TestDelete(t *testing.T) {
	s := openTmp(t)
	now := time.Now().UTC()

	s.Upsert(sessionstore.Row{ID: "y", Slot: 0, Status: "stopped", CreatedAt: now, UpdatedAt: now}) //nolint:errcheck

	if err := s.Delete("y"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows after delete, got %d", len(rows))
	}
}

func TestDeleteNoOp(t *testing.T) {
	s := openTmp(t)
	if err := s.Delete("nonexistent"); err != nil {
		t.Errorf("Delete of missing row: %v", err)
	}
}

func TestListOrdering(t *testing.T) {
	s := openTmp(t)
	base := time.Now().UTC()

	for i, id := range []string{"c", "a", "b"} {
		s.Upsert(sessionstore.Row{ //nolint:errcheck
			ID:        id,
			Slot:      i,
			Status:    "stopped",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base,
		})
	}

	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// Should be ordered by created_at: c(+0s), a(+1s), b(+2s)
	want := []string{"c", "a", "b"}
	for i, r := range rows {
		if r.ID != want[i] {
			t.Errorf("row[%d].ID = %q, want %q", i, r.ID, want[i])
		}
	}
}
