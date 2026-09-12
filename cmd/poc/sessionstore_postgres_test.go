package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/sessionstore/pgstore"
)

// TestSessionManager_SurvivesRestart_Postgres exercises the property
// RecoverInProgress/loadFromDB relies on: a session persisted through a
// Postgres-backed sessionstore.Store is recovered by a brand-new
// SessionManager (standing in for a daemon restart) pointed at the same
// database. Requires RUBBISH_TEST_POSTGRES_DSN; skipped otherwise.
//
// Uses a snapshot failure to reach a stable terminal (Failed) row rather
// than Stop(), which deletes the session's row entirely once cleanup
// finishes — there'd be nothing left to recover.
func TestSessionManager_SurvivesRestart_Postgres(t *testing.T) {
	dsn := os.Getenv("RUBBISH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RUBBISH_TEST_POSTGRES_DSN not set; skipping Postgres session-manager restart test")
	}

	store, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("pgstore.Open: %v", err)
	}
	t.Cleanup(func() {
		store.Close() //nolint:errcheck
	})

	// First "boot" of the daemon: launch a session that fails during boot
	// (using fakes for the VM layer), persisting to Postgres via the
	// OnTransition persist hook.
	snap := &fakeSnapshotter{createErr: errors.New("disk full")}
	launcher := &fakeLauncher{handle: &fakeVMHandle{}}
	mgr1 := newSessionManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}}, nil, "", nil, store, 4, "", nil)

	sess, err := mgr1.Create(context.Background(), "", "", "", false, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitState(t, mgr1, sess.ID, session.StateFailed, 3*time.Second)
	t.Cleanup(func() {
		store.Delete(sess.ID) //nolint:errcheck
	})

	// Confirm the row actually landed in Postgres, independent of in-memory
	// state — this is what the migration to pgstore is meant to prove.
	row, ok, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !ok {
		t.Fatal("session row not found in Postgres after boot failure")
	}
	if row.Status != string(session.StateFailed) {
		t.Errorf("persisted status = %q, want %q", row.Status, session.StateFailed)
	}

	// "Restart": a fresh SessionManager, fresh in-memory state, same Postgres
	// store — loadFromDB must recover the session from the DB row alone.
	mgr2 := newSessionManager(&fakeSnapshotter{}, &fakeLauncher{}, &fakeBridgeFactory{&fakeSetupRunner{}}, nil, "", nil, store, 4, "", nil)
	mgr2.loadFromDB(context.Background())

	recovered, ok := mgr2.Get(sess.ID)
	if !ok {
		t.Fatal("session not recovered after simulated restart")
	}
	if recovered.SessionStatus() != session.StateFailed {
		t.Errorf("recovered status = %s, want %s", recovered.SessionStatus(), session.StateFailed)
	}
	if recovered.ID != sess.ID {
		t.Errorf("recovered ID = %q, want %q", recovered.ID, sess.ID)
	}
}
