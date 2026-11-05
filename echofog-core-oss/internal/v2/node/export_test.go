package node

// Test seams: the runtime keeps these internals unexported; the external tests use them to check
// that the platform flags and typed events are actually wired, without widening the public API.

import (
	"github.com/echofog/echofog-core/internal/v2/session"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

var (
	// OpenSessions counts the live (not closed) sessions in the runtime's session table.
	OpenSessions = func(r *Runtime) int {
		n := 0
		for _, est := range r.table.Sessions() {
			select {
			case <-est.Session.Done():
			default:
				n++
			}
		}
		return n
	}
	// LimitsOf returns the pre-authentication limits of the accept path (handshake 11).
	LimitsOf = func(r *Runtime) *session.Limits { return r.limits }
	// ObserverOf returns the runtime's own-address observer (wire.md §7.2).
	ObserverOf = func(r *Runtime) *session.Observer { return r.observer }
	// SetBeforeAdd installs the hook that runs before an accepted session enters the table.
	SetBeforeAdd = func(f func(*session.Established)) { beforeAdd = f }
	// LocalInfoOf returns the carrier-plan view built from the profile.
	LocalInfoOf = func(r *Runtime) carrier.LocalInfo { return r.planInfo() }
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

// HasHandler reports whether the runtime serves the service version (a registered module).
var HasHandler = func(r *Runtime, svc, version uint64) bool { return r.handlerFor(svc, version) != nil }
