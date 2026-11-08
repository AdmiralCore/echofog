package node

import (
	"log/slog"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/release"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// EventKind names a typed runtime event. The kinds are the stable contract of the application
// facade: a new kind is added here, its message is operator-facing text.
type EventKind string

const (
	// EventNoCover is D37: the node has no camouflaged listener, so it publishes no public card
	// and serves only peers and invitees. The operator must be told; start still succeeds.
	EventNoCover EventKind = "node.no-cover"
	// EventClockSkew reports that a handshake was refused because the device clock differs from
	// the peer by more than 10 minutes (handshake.md section 7). Message is Russian, neutral form,
	// for the UI; Err is the cause. It is raised at most once per 10 minutes.
	EventClockSkew EventKind = "node.clock-skew"
	// EventClockWarning is the D36 >60 s clock warning.
	EventClockWarning EventKind = "node.clock-warning"
	// EventNetworkChanged reports that the local network changed and live sessions moved to new
	// paths (wire.md §8.2–§8.3). The message never carries addresses.
	EventNetworkChanged EventKind = "node.network-changed"
	// EventMemoryProtection: the process cannot lock secret pages, so secrets live in ordinary
	// memory and are wiped on destroy (platforms.md §1.2, T4.1). Report once to the operator.
	EventMemoryProtection EventKind = "node.memory-protection"
	// EventBackground: the app lifecycle mode changed and sessions were closed accordingly
	// (platforms.md §2).
	EventBackground EventKind = "node.background"
	// EventVisibility: a card reissue changed the node's visibility, so the public card and the
	// D37 condition changed (T3.7a risk 4).
	EventVisibility EventKind = "node.visibility"
	// EventPublicAddr: the node's public IP was decided or changed by OBSERVED_ADDR agreement
	// (wire.md §7.2) and its cards were pointed at it. Event.PublicAddr carries the address:
	// it is the node's own public IP, shown to its operator only.
	EventPublicAddr EventKind = "node.public-addr"
	// EventNoDomesticEntry: the country policy needs an entry node in the own country and there is
	// none (or none verified); Event.Err is ErrNoDomesticEntry or ErrRouteUnsupported (T6.4).
	EventNoDomesticEntry EventKind = "node.no-domestic-entry"
	// EventCountryChanged: the client's own country was decided or changed (user setting, GeoIP of
	// the public address, platform hint); new paths follow the new row, live sessions stay (T6.4c).
	EventCountryChanged EventKind = "node.country-changed"
	// EventNarrowPath reports that the network cannot carry the first packets of noise3 (PMTU 1280
	// or less), so it is marked for 24 h (T3.12). Message is Russian, neutral form, for the UI.
	EventNarrowPath EventKind = "node.narrow-path"
	// EventNetworkFrozen: two flows to one foreign address stopped after 12-20 KiB, so the network
	// is marked frozen for a day and the row of a mobile network towards abroad applies (T6.4d).
	EventNetworkFrozen EventKind = "node.network-frozen"
	// EventPortMapping: the set of ports the router forwards for this node changed and the cards
	// follow it (T6.5b). The message never carries addresses.
	EventPortMapping EventKind = "node.port-mapping"
	// EventUpdateAvailable: an applied release manifest says that this node's own version is below
	// recommended or min_supported; Event.Update has the details (T4.5). Nothing is downloaded.
	EventUpdateAvailable EventKind = "node.update-available"
	// EventReleaseUnavailable: the stored release manifest chain could not be read or verified, so
	// the node refuses manifests until the profile repairs it (T4.5 L2). The node keeps running.
	EventReleaseUnavailable EventKind = "node.release-unavailable"
	// EventListenerStopped: an accept loop stopped on a local error and the node no longer
	// accepts on that listener (T3.7a risk 3).
	EventListenerStopped EventKind = "node.listener-stopped"
)

// Event is one runtime event. Message is operator-facing text; it never contains keys, addresses
// of hidden listeners or peer identities. The typed fields are the payload of their kind and are
// meaningful only for it, so the facade never parses the message.
type Event struct {
	Kind    EventKind
	Message string
	At      time.Time
	Err     error

	// Background and Closed are the payload of EventBackground: the new mode and the number of
	// sessions told to go away.
	Background Background
	Closed     int
	// Visibility is the payload of EventVisibility.
	Visibility cards.Visibility
	// PublicAddr is the payload of EventPublicAddr.
	PublicAddr netip.Addr
	// Update is the payload of EventUpdateAvailable.
	Update *release.Event
	// MemoryLevel is the payload of EventMemoryProtection.
	MemoryLevel secret.Level
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
			At:          time.Now(),
			Err:         st.Err,
			MemoryLevel: st.Level,
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
