package session

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// State represents a session lifecycle state.
type State string

const (
	StateProvisioning State = "provisioning"
	StateBooting      State = "booting"
	StateWaitingSSH   State = "waiting_ssh"
	StateConfiguring  State = "configuring"
	StateReady        State = "ready"
	StateStopping     State = "stopping"
	StateStopped      State = "stopped"
	StateFailed       State = "failed"
)

// ErrInvalidTransition is returned when a transition is not permitted.
var ErrInvalidTransition = errors.New("invalid state transition")

// TransitionHook is called after every successful transition.
type TransitionHook func(from, to State, elapsed time.Duration)

// validTransitions defines all legal (from → to) pairs.
var validTransitions = map[State][]State{
	StateProvisioning: {StateBooting, StateFailed},
	StateBooting:      {StateWaitingSSH, StateFailed},
	StateWaitingSSH:   {StateConfiguring, StateFailed},
	StateConfiguring:  {StateReady, StateFailed},
	StateReady:        {StateStopping},
	StateStopping:     {StateStopped},
	// StateStopped and StateFailed are terminal — no outgoing transitions.
}

// StateMachine is a concurrency-safe session lifecycle state machine.
type StateMachine struct {
	mu        sync.RWMutex
	current   State
	enteredAt time.Time
	hooks     []TransitionHook
}

// New returns a StateMachine initialised to initial.
func New(initial State) *StateMachine {
	return &StateMachine{
		current:   initial,
		enteredAt: time.Now(),
	}
}

// State returns the current state.
func (sm *StateMachine) State() State {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.current
}

// OnTransition registers a hook that fires after every successful transition.
func (sm *StateMachine) OnTransition(fn TransitionHook) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.hooks = append(sm.hooks, fn)
}

// Transition moves to state to if the transition is legal.
// Returns ErrInvalidTransition (wrapped) if not.
func (sm *StateMachine) Transition(to State) error {
	sm.mu.Lock()

	from := sm.current
	allowed := validTransitions[from]
	ok := false
	for _, s := range allowed {
		if s == to {
			ok = true
			break
		}
	}
	if !ok {
		sm.mu.Unlock()
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}

	elapsed := time.Since(sm.enteredAt)
	sm.current = to
	sm.enteredAt = time.Now()
	hooks := sm.hooks // snapshot to call outside the lock
	sm.mu.Unlock()

	for _, h := range hooks {
		h(from, to, elapsed)
	}
	return nil
}
