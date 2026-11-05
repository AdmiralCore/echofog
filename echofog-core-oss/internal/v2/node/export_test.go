package node

// Test seams: the runtime keeps these internals unexported; the external tests use them to check
// that the platform flags and typed events are actually wired, without widening the public API.

import (
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

var (
	// LocalInfoOf returns the carrier-plan view built from the profile.
	LocalInfoOf = func(r *Runtime) carrier.LocalInfo { return r.localInfo }
	// SessionLimitOf returns the limit of the running session table.
	SessionLimitOf = func(r *Runtime) int { return r.table.Max() }
	// NoteVisibility feeds one card visibility to the runtime (the card builder's Update hook).
	NoteVisibility = func(r *Runtime, v cards.Visibility) { r.noteVisibility(v) }
	// NoteListenerStopped reports a stopped accept loop.
	NoteListenerStopped = func(r *Runtime, err error) { r.listenerStopped(err) }
	// SetSecretObserver replaces secret.Observe for the memory-protection event test.
	SetSecretObserver = func(observe func(func(secret.Status))) { observeSecret = observe }
	// GreaseIntervalDefault exposes the default random interval.
	GreaseIntervalDefault func() time.Duration = defaultGreaseInterval
)
