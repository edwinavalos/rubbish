// Package storetest is a shared table-driven test suite run against every
// sessionstore.Store implementation (JSONStore, pgstore.Store) so behavior
// stays identical across backends.
package storetest

import (
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/sessionstore"
)

// Run exercises the full sessionstore.Store contract against a fresh store
// returned by newStore for each subtest.
func Run(t *testing.T, newStore func(t *testing.T) sessionstore.Store) {
	t.Run("UpsertAndList", func(t *testing.T) {
		s := newStore(t)
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
	})

	t.Run("UpsertUpdates", func(t *testing.T) {
		s := newStore(t)
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
	})

	t.Run("Delete", func(t *testing.T) {
		s := newStore(t)
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
	})

	t.Run("DeleteNoOp", func(t *testing.T) {
		s := newStore(t)
		if err := s.Delete("nonexistent"); err != nil {
			t.Errorf("Delete of missing row: %v", err)
		}
	})

	t.Run("Get", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		r := sessionstore.Row{
			ID:        "get-test",
			Slot:      2,
			Status:    "ready",
			RepoURL:   "https://github.com/user/repo",
			Branch:    "main",
			DevMode:   true,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		got, ok, err := s.Get("get-test")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !ok {
			t.Fatal("Get: expected row to exist")
		}
		if got.ID != r.ID || got.Status != r.Status || got.DevMode != r.DevMode {
			t.Errorf("Get returned %+v, want %+v", got, r)
		}
	})

	t.Run("Get_Missing", func(t *testing.T) {
		s := newStore(t)

		_, ok, err := s.Get("nonexistent")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if ok {
			t.Error("Get: expected not found for missing ID")
		}
	})

	t.Run("Favorites", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		favs := []sessionstore.Favorite{
			{ID: "fav-1", Name: "My Repo", RepoURL: "https://github.com/user/repo", Branch: "main", CreatedAt: now},
			{ID: "fav-2", Name: "Other", RepoURL: "https://github.com/user/other", Branch: "dev", CreatedAt: now.Add(time.Second)},
		}
		for _, f := range favs {
			if err := s.UpsertFavorite(f); err != nil {
				t.Fatalf("UpsertFavorite %s: %v", f.ID, err)
			}
		}

		list, err := s.ListFavorites()
		if err != nil {
			t.Fatalf("ListFavorites: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("want 2 favorites, got %d", len(list))
		}
		if list[0].ID != "fav-1" || list[1].ID != "fav-2" {
			t.Errorf("ordering wrong: got %v, %v", list[0].ID, list[1].ID)
		}
		if list[0].Name != "My Repo" || list[0].RepoURL != favs[0].RepoURL {
			t.Errorf("fav-1 fields wrong: %+v", list[0])
		}
	})

	t.Run("Favorites_UpsertUpdates", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()

		f := sessionstore.Favorite{ID: "fav-upd", Name: "Original", RepoURL: "https://github.com/user/repo", Branch: "main", CreatedAt: now}
		if err := s.UpsertFavorite(f); err != nil {
			t.Fatalf("UpsertFavorite: %v", err)
		}

		f.Name = "Updated"
		f.Branch = "feature"
		if err := s.UpsertFavorite(f); err != nil {
			t.Fatalf("UpsertFavorite update: %v", err)
		}

		list, err := s.ListFavorites()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("want 1 favorite, got %d", len(list))
		}
		if list[0].Name != "Updated" || list[0].Branch != "feature" {
			t.Errorf("expected updated favorite, got %+v", list[0])
		}
	})

	t.Run("Favorites_Delete", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()

		f := sessionstore.Favorite{ID: "fav-del", Name: "Delete me", RepoURL: "https://github.com/user/repo", Branch: "main", CreatedAt: now}
		if err := s.UpsertFavorite(f); err != nil {
			t.Fatalf("UpsertFavorite: %v", err)
		}

		if err := s.DeleteFavorite("fav-del"); err != nil {
			t.Fatalf("DeleteFavorite: %v", err)
		}

		list, err := s.ListFavorites()
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 0 {
			t.Errorf("want 0 favorites after delete, got %d", len(list))
		}
	})

	t.Run("Favorites_DeleteNoOp", func(t *testing.T) {
		s := newStore(t)
		if err := s.DeleteFavorite("nonexistent"); err != nil {
			t.Errorf("DeleteFavorite of missing row: %v", err)
		}
	})

	t.Run("ListOrdering", func(t *testing.T) {
		s := newStore(t)
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
	})
}
