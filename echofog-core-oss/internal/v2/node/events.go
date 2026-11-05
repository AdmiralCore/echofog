package node

import (
	"log/slog"
	"time"
)

// EventKind names a typed runtime event. T3.7b extends this set for the application facade; the
// runtime already needs the two events that describe its own startup and clock state.
type EventKind string

const (
	// EventNoCover is D37: the node has no camouflaged listener, so it publishes no public card
	// and serves only peers and invitees. The operator must be told; start still succeeds.
	EventNoCover EventKind = "node.no-cover"
	// EventClockWarning is the D36 >60 s clock warning.
	EventClockWarning EventKind = "node.clock-warning"
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
