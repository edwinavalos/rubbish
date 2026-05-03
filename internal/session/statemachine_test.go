package session_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/session"
)

// ── Happy path ────────────────────────────────────────────────────────────────

func TestHappyPath(t *testing.T) {
	sm := session.New(session.StateProvisioning)

	steps := []session.State{
		session.StateBooting,
		session.StateWaitingSSH,
		session.StateConfiguring,
		session.StateReady,
		session.StateStopping,
		session.StateStopped,
	}

	for _, next := range steps {
		if err := sm.Transition(next); err != nil {
			t.Fatalf("Transition(%s): unexpected error: %v", next, err)
		}
		if got := sm.State(); got != next {
			t.Fatalf("after Transition(%s): State() = %s, want %s", next, got, next)
		}
	}
}

// ── Failure path ─────────────────────────────────────────────────────────────

func TestFailurePath(t *testing.T) {
	// Only pre-Ready states have a failure path; Ready and Stopping are committed
	// to graceful teardown and cannot jump to Failed (per the design diagram).
	nonTerminal := []session.State{
		session.StateProvisioning,
		session.StateBooting,
		session.StateWaitingSSH,
		session.StateConfiguring,
	}

	// progress returns a new SM already advanced to `target` via the happy path.
	progress := func(t *testing.T, target session.State) *session.StateMachine {
		t.Helper()
		sm := session.New(session.StateProvisioning)
		if sm.State() == target {
			return sm
		}
		path := []session.State{
			session.StateBooting,
			session.StateWaitingSSH,
			session.StateConfiguring,
			session.StateReady,
			session.StateStopping,
		}
		for _, s := range path {
			if err := sm.Transition(s); err != nil {
				t.Fatalf("setup Transition(%s): %v", s, err)
			}
			if sm.State() == target {
				return sm
			}
		}
		return sm
	}

	for _, from := range nonTerminal {
		t.Run(string(from), func(t *testing.T) {
			sm := progress(t, from)
			if sm.State() != from {
				t.Fatalf("setup failed: wanted state %s, got %s", from, sm.State())
			}

			if err := sm.Transition(session.StateFailed); err != nil {
				t.Fatalf("Transition(Failed) from %s: %v", from, err)
			}
			if sm.State() != session.StateFailed {
				t.Fatalf("State() = %s, want Failed", sm.State())
			}

			// must be terminal now
			if err := sm.Transition(session.StateReady); err == nil {
				t.Fatal("expected error transitioning out of Failed, got nil")
			}
		})
	}
}

// ── Illegal transitions ───────────────────────────────────────────────────────

func TestIllegalTransitions(t *testing.T) {
	type tc struct {
		from session.State
		to   session.State
	}

	cases := []tc{
		// Provisioning may only go to Booting or Failed
		{session.StateProvisioning, session.StateWaitingSSH},
		{session.StateProvisioning, session.StateConfiguring},
		{session.StateProvisioning, session.StateReady},
		{session.StateProvisioning, session.StateStopping},
		{session.StateProvisioning, session.StateStopped},
		{session.StateProvisioning, session.StateProvisioning},

		// Booting may only go to WaitingSSH or Failed
		{session.StateBooting, session.StateProvisioning},
		{session.StateBooting, session.StateConfiguring},
		{session.StateBooting, session.StateReady},
		{session.StateBooting, session.StateStopping},
		{session.StateBooting, session.StateStopped},
		{session.StateBooting, session.StateBooting},

		// WaitingSSH may only go to Configuring or Failed
		{session.StateWaitingSSH, session.StateProvisioning},
		{session.StateWaitingSSH, session.StateBooting},
		{session.StateWaitingSSH, session.StateReady},
		{session.StateWaitingSSH, session.StateStopping},
		{session.StateWaitingSSH, session.StateStopped},
		{session.StateWaitingSSH, session.StateWaitingSSH},

		// Configuring may only go to Ready or Failed
		{session.StateConfiguring, session.StateProvisioning},
		{session.StateConfiguring, session.StateBooting},
		{session.StateConfiguring, session.StateWaitingSSH},
		{session.StateConfiguring, session.StateStopping},
		{session.StateConfiguring, session.StateStopped},
		{session.StateConfiguring, session.StateConfiguring},

		// Ready may only go to Stopping
		{session.StateReady, session.StateProvisioning},
		{session.StateReady, session.StateBooting},
		{session.StateReady, session.StateWaitingSSH},
		{session.StateReady, session.StateConfiguring},
		{session.StateReady, session.StateStopped},
		{session.StateReady, session.StateFailed},
		{session.StateReady, session.StateReady},

		// Stopping may only go to Stopped
		{session.StateStopping, session.StateProvisioning},
		{session.StateStopping, session.StateBooting},
		{session.StateStopping, session.StateWaitingSSH},
		{session.StateStopping, session.StateConfiguring},
		{session.StateStopping, session.StateReady},
		{session.StateStopping, session.StateFailed},
		{session.StateStopping, session.StateStopping},
	}

	// helper: advance SM to the target state via the standard path.
	advance := func(t *testing.T, target session.State) *session.StateMachine {
		t.Helper()
		sm := session.New(session.StateProvisioning)
		if sm.State() == target {
			return sm
		}
		path := []session.State{
			session.StateBooting,
			session.StateWaitingSSH,
			session.StateConfiguring,
			session.StateReady,
			session.StateStopping,
		}
		for _, s := range path {
			sm.Transition(s) //nolint:errcheck — setup only
			if sm.State() == target {
				return sm
			}
		}
		return sm
	}

	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			sm := advance(t, tc.from)
			if sm.State() != tc.from {
				t.Skipf("could not reach state %s (at %s)", tc.from, sm.State())
			}
			err := sm.Transition(tc.to)
			if err == nil {
				t.Fatalf("expected ErrInvalidTransition, got nil")
			}
			if !errors.Is(err, session.ErrInvalidTransition) {
				t.Fatalf("expected errors.Is(err, ErrInvalidTransition), got: %v", err)
			}
		})
	}
}

// ── Terminal states ───────────────────────────────────────────────────────────

func TestTerminalStates(t *testing.T) {
	allStates := []session.State{
		session.StateProvisioning,
		session.StateBooting,
		session.StateWaitingSSH,
		session.StateConfiguring,
		session.StateReady,
		session.StateStopping,
		session.StateStopped,
		session.StateFailed,
	}

	terminals := map[session.State]func() *session.StateMachine{
		session.StateStopped: func() *session.StateMachine {
			sm := session.New(session.StateProvisioning)
			for _, s := range []session.State{
				session.StateBooting, session.StateWaitingSSH,
				session.StateConfiguring, session.StateReady,
				session.StateStopping, session.StateStopped,
			} {
				sm.Transition(s) //nolint:errcheck
			}
			return sm
		},
		session.StateFailed: func() *session.StateMachine {
			sm := session.New(session.StateProvisioning)
			sm.Transition(session.StateFailed) //nolint:errcheck
			return sm
		},
	}

	for terminal, mkSM := range terminals {
		t.Run(string(terminal), func(t *testing.T) {
			for _, target := range allStates {
				sm := mkSM()
				err := sm.Transition(target)
				if err == nil {
					t.Errorf("Transition(%s) from terminal %s: expected error, got nil", target, terminal)
				} else if !errors.Is(err, session.ErrInvalidTransition) {
					t.Errorf("Transition(%s) from terminal %s: got %v, want ErrInvalidTransition", target, terminal, err)
				}
			}
		})
	}
}

// ── OnTransition hook ────────────────────────────────────────────────────────

func TestOnTransitionHook(t *testing.T) {
	sm := session.New(session.StateProvisioning)

	var (
		gotFrom    session.State
		gotTo      session.State
		gotElapsed time.Duration
		called     bool
	)

	sm.OnTransition(func(from, to session.State, elapsed time.Duration) {
		gotFrom = from
		gotTo = to
		gotElapsed = elapsed
		called = true
	})

	if err := sm.Transition(session.StateBooting); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	if !called {
		t.Fatal("hook was not called")
	}
	if gotFrom != session.StateProvisioning {
		t.Errorf("hook from = %s, want Provisioning", gotFrom)
	}
	if gotTo != session.StateBooting {
		t.Errorf("hook to = %s, want Booting", gotTo)
	}
	if gotElapsed < 0 {
		t.Errorf("hook elapsed = %v, want >= 0", gotElapsed)
	}
}

// ── Multiple hooks ────────────────────────────────────────────────────────────

func TestMultipleHooks(t *testing.T) {
	sm := session.New(session.StateProvisioning)

	count := 0
	for i := 0; i < 3; i++ {
		sm.OnTransition(func(from, to session.State, elapsed time.Duration) {
			count++
		})
	}

	if err := sm.Transition(session.StateBooting); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	if count != 3 {
		t.Errorf("hooks called %d times, want 3", count)
	}
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestConcurrentSafety(t *testing.T) {
	sm := session.New(session.StateProvisioning)

	var wg sync.WaitGroup

	// 50 goroutines continuously reading State()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = sm.State()
			}
		}()
	}

	// One goroutine driving transitions
	wg.Add(1)
	go func() {
		defer wg.Done()
		path := []session.State{
			session.StateBooting,
			session.StateWaitingSSH,
			session.StateConfiguring,
			session.StateReady,
			session.StateStopping,
			session.StateStopped,
		}
		for _, s := range path {
			sm.Transition(s) //nolint:errcheck
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
}
