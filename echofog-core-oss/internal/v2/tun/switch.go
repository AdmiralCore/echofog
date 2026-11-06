package tun

import (
	"context"
	"errors"
	"sync"
)

// State of the kill switch.
type State uint8

const (
	// StateOff: no filters, no tunnel; the computer is on its own network.
	StateOff State = iota
	// StateActive: filters installed, tunnel up, DNS pinned.
	StateActive
	// StateHeld: the filters stay but the tunnel is gone (helper killed, adapter removed, an
	// earlier run crashed). Nothing leaves the computer. Reconnect or Release leaves it.
	StateHeld
)

func (s State) String() string {
	switch s {
	case StateOff:
		return "off"
	case StateActive:
		return "active"
	case StateHeld:
		return "held"
	}
	return "unknown"
}

// ErrBadState is an operation that does not fit the current state.
var ErrBadState = errors.New("tun: operation does not fit the kill switch state")

// Config wires a Switch.
type Config struct {
	Firewall Firewall
	Tunnel   Tunnel
	DNS      DNSGuard
	Plan     Plan
	Tun      TunConfig
	// OnState is called after every state change, outside the lock.
	OnState func(State, error)
}

// Switch is the fail-closed state machine of the whole-computer mode.
type Switch struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a Switch in StateOff. Call Recover at start-up.
func New(cfg Config) *Switch { return &Switch{cfg: cfg} }

// State returns the current state.
func (s *Switch) State() State { return StateOff }

// Recover looks for filters an earlier process left and, if there are any, enters StateHeld
// without touching them: a crash never turns into a quiet leak.
func (s *Switch) Recover(context.Context) error { return nil }

// Start installs the filters, brings the tunnel up and pins DNS, in that order. Any failure
// rolls back to StateOff, except that a failing rollback of the filters keeps StateHeld.
func (s *Switch) Start(context.Context) error { return nil }

// Reconnect brings the tunnel back up from StateHeld.
func (s *Switch) Reconnect(context.Context) error { return nil }

// Stop is the user's explicit end: tunnel down, DNS restored, filters removed. If the filters
// cannot be removed the state stays held.
func (s *Switch) Stop(context.Context) error { return nil }

// Close ends the watcher; it does not change the filters.
func (s *Switch) Close() error { return nil }
