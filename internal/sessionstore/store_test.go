package sessionstore_test

import (
	"os"
	"testing"

	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/sessionstore/storetest"
)

func TestJSONStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) sessionstore.Store {
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
	})
}
