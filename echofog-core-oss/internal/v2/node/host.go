package node

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
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

// Profile is the platform profile and the flags that are local policy (platforms.md §2). T3.7b
// fills the full flag set; the runtime only needs the platform name and the memory cap so far.
type Profile struct {
	Platform   string
	MemCapMB   int
	LowPower   bool
	Metered    bool
	Roles      []string
	Background string
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
	// ExpectedDeviceID sets the rid binding of an XX handshake without a card.
	ExpectedDeviceID *DeviceID
}

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
	Store(ns string) KV
	Clock() Clock
	Country(netip.Addr) (string, bool)
	HTTP() *http.Client
	Profile() Profile
	Events() EventSink
}
