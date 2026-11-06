package node

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/pex"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Identity is the local account/device pair a runtime acts as.
type Identity struct {
	NodeID   NodeID
	DeviceID DeviceID
	Account  keys.AKey
	Device   *identity.Device
	// Visibility is the card scope policy the builder was configured with (D37).
	Visibility cards.Visibility
}

// Clock is the node's corrected clock: Now already includes the learned offset, Offset is that
// correction (T3.8). *clock.Clock satisfies it.
type Clock interface {
	Now() time.Time
	Offset() time.Duration
}

// KV is one persistent namespace; store.Store(ns) returns it (T4.3).
type KV = store.KV

// CardSource is the node's card view for services: its own cards by scope, verified peer cards
// and the newest device lists of contacts.
type CardSource interface {
	// Own returns the current envelope of one of the node's own cards. A node without a cover
	// has no public card (D37).
	Own(scope objects.Scope) ([]byte, bool)
	// Peer returns the newest verified card of a device, or nil.
	Peer(device DeviceID) *objects.Card
	// Devices returns the newest verified device list of an account, or nil.
	Devices(node NodeID) *objects.DeviceList
}

// DialOpt changes one Session or Open call.
type DialOpt struct {
	// Card is the verified peer card to dial. When nil the runtime takes the newest card of the
	// peer from its card book.
	Card *objects.Card
	// Carrier supplies the carrier directly (relay circuits, tests). It is used once.
	Carrier mux.Carrier
	// CarrierFunc supplies carriers; unlike Carrier it also survives the single suite retry
	// after REJECT{suite} (handshake.md §3.1).
	CarrierFunc func(ctx context.Context) (mux.Carrier, error)
	// Attempts override the carrier plan built from the card (carrier.Plan).
	Attempts []carrier.Attempt
	// Local overrides the main identity (throwaway XX, an identity for one exit).
	Local *session.LocalIdentity
	// AccessSecret is the 16-byte invitation or personal pass secret for XK to a hidden node
	// (handshake.md §5). It is copied for the duration of the handshake.
	AccessSecret []byte
	// ExpectedDeviceID sets the rid binding of an XX handshake without a card.
	ExpectedDeviceID *DeviceID
	// CarrierCard is a shell card that carries only the masking key of a bootstrap list entry. It
	// is set by DialBootstrap; the handshake then runs XX because it gets no card (T5.5).
	CarrierCard *objects.Card
	// PathKind is the kind of the carrier created by this call: direct (default) or relay
	// (wire.md §8). Relay suites are T6.1; tests and relay circuits pass PathRelay here.
	PathKind mux.PathKind
	// ViaRelay skips the direct carrier plan and reaches the peer through a relay the node already
	// holds a session with (T6.2). Without it a relay is the fallback after the direct plan fails.
	ViaRelay bool
	// Probe makes a direct dial that the country policy would forbid, limited to the probe budget of
	// the rule and to TCP suites: a reachability check, never a session to carry traffic (T6.4).
	Probe bool
}

// PeerStore is Host.Peers(): the new/tried candidate table of pex/2 (services.md §3). A candidate
// received in a PEX answer enters the new table; a device reached in a working session moves to
// tried and may become a guard. The tables persist in the store.
type PeerStore = *pex.Store

// Host is what services see (02-architecture §3.1). The final shape is fixed by T3.7a: the DHT,
// PEX and access stores are added by T5.x, T5.4 and T3.6 over the same runtime, not guessed here.
type Host interface {
	Self() Identity
	Cards() CardSource
	Devices(ctx context.Context, node NodeID) ([]DeviceID, error)
	Session(ctx context.Context, peer DeviceID, opt DialOpt) (Peer, *mux.Session, error)
	Open(ctx context.Context, peer DeviceID, service uint64, opt DialOpt) (Peer, *mux.Stream, error)
	Register(spec service.Spec, h Handler)
	Datagrams(types ...uint64) DatagramPort
	Peers() PeerStore
	// DHT is the lookup and publishing client of dht/2 (T5.2).
	DHT() *dht.Client
	Store(ns string) KV
	Clock() Clock
	Country(netip.Addr) (string, bool)
	HTTP() *http.Client
	Profile() Profile
	Events() EventSink
}
