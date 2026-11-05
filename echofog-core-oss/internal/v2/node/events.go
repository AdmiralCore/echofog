package node

import (
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/v2/secret"
)

// EventKind names a typed runtime event. The kinds are the stable contract of the application
// facade: a new kind is added here, its message is operator-facing text.
type EventKind string

const (
	// EventNoCover is D37: the node has no camouflaged listener, so it publishes no public card
	// and serves only peers and invitees. The operator must be told; start still succeeds.
	EventNoCover EventKind = "node.no-cover"
	// EventClockWarning is the D36 >60 s clock warning.
	EventClockWarning EventKind = "node.clock-warning"
	// EventMemoryProtection: the process cannot lock secret pages, so secrets live in ordinary
	// memory and are wiped on destroy (platforms.md §1.2, T4.1). Report once to the operator.
	EventMemoryProtection EventKind = "node.memory-protection"
	// EventBackground: the app lifecycle mode changed and sessions were closed accordingly
	// (platforms.md §2).
	EventBackground EventKind = "node.background"
	// EventVisibility: a card reissue changed the node's visibility, so the public card and the
	// D37 condition changed (T3.7a risk 4).
	EventVisibility EventKind = "node.visibility"
	// EventListenerStopped: an accept loop stopped on a local error and the node no longer
	// accepts on that listener (T3.7a risk 3).
	EventListenerStopped EventKind = "node.listener-stopped"
)

// Event is one runtime event. Message is operator-facing text; it never contains keys, addresses
// of hidden listeners or peer identities.
type Event struct {
	Kind    EventKind
	Message string
	At      time.Time
	Err     error
}

// EventSink receives runtime events. Implementations must not block: the runtime calls Emit from
// its own goroutines.
type EventSink interface {
	Emit(Event)
}

// EventFunc adapts a function to EventSink.
type EventFunc func(Event)

// Emit implements EventSink.
func (f EventFunc) Emit(e Event) { f(e) }

// observeSecret is a seam for the memory-protection event test.
var observeSecret = secret.Observe

// watchMemoryProtection reports reduced memory protection to the application once (T4.1). The
// secret package calls the observer only when locking is unavailable, and only once.
func (r *Runtime) watchMemoryProtection() {
	observeSecret(func(st secret.Status) {
		r.events.Emit(Event{
			Kind: EventMemoryProtection,
			Message: "memory protection is reduced: secret pages cannot be locked and are kept " +
				"in ordinary memory, wiped after use",
			At:  time.Now(),
			Err: st.Err,
		})
	})
}

// logSink is the default sink: a structured warning through slog.
type logSink struct{ log *slog.Logger }

func (s logSink) Emit(e Event) {
	if s.log == nil {
		return
	}
	args := []any{"event", string(e.Kind), "at", e.At}
	if e.Err != nil {
		args = append(args, "err", e.Err)
	}
	s.log.Warn(e.Message, args...)
}
