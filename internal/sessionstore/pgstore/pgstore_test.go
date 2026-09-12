package pgstore

import (
	"os"
	"testing"

	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/sessionstore/storetest"
)

// TestPGStore runs the shared sessionstore.Store contract suite against a
// real Postgres instance. Requires RUBBISH_TEST_POSTGRES_DSN (e.g.
// "postgres://localhost:5432/rubbish_test?sslmode=disable"); skipped when unset
// so `go test ./...` doesn't require Postgres everywhere.
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("RUBBISH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RUBBISH_TEST_POSTGRES_DSN not set; skipping Postgres sessionstore tests")
	}

	storetest.Run(t, func(t *testing.T) sessionstore.Store {
		t.Helper()
		s, err := Open(dsn)
		if err != nil {
			t.Fatalf("pgstore.Open: %v", err)
		}
		t.Cleanup(func() {
			// Truncate rather than drop so goose's version table (and the
			// migration itself) don't need to re-run for the next subtest.
			if _, err := s.db.Exec(`TRUNCATE rubbish.sessions, rubbish.favorites`); err != nil {
				t.Logf("cleanup truncate: %v", err)
			}
			s.Close() //nolint:errcheck
		})
		return s
	})
}
