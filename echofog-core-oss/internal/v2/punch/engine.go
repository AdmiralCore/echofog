package punch

import (
	"context"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Timings of services.md §6.4.
const (
	// AttemptTime is how long one attempt (and the wait for the peer's offer) lasts.
	AttemptTime = 5 * time.Second
	// MaxAttempts is how many attempts one round makes.
	MaxAttempts = 2
	// OfferInterval is the least time between two PUNCH_OFFERs of a session, in both directions.
	OfferInterval = 30 * time.Second
	// RetryMin and RetryMax bound the pause before the next round of a session that stays relayed.
	RetryMin = 90 * time.Second
	RetryMax = 150 * time.Second
	// MaxInitDatagrams is the most Init datagrams the initiator sends in one attempt.
	MaxInitDatagrams = 24
	// JunkMin..JunkMax datagrams of JunkSizeMin..JunkSizeMax bytes, JunkGapMin..JunkGapMax apart,
	// go to each target of the responder.
	JunkMin, JunkMax         = 3, 6
	JunkSizeMin, JunkSizeMax = 32, 256
	JunkGapMin, JunkGapMax   = 10 * time.Millisecond, 40 * time.Millisecond
)

// Role is the end of the session the engine runs on. The initiator of the end-to-end session is
// also the noise3 initiator (services.md §6.4 step 4).
type Role uint8

const (
	Initiator Role = iota + 1
	Responder
)

// Link is the relayed session the punch is coordinated on.
type Link interface {
	SendControl(typ uint64, flags byte, payload []byte) error
	Ping(ctx context.Context) (time.Duration, error)
}

// Net is the datagram side of the node.
type Net interface {
	// OwnAddrs are the addresses to offer (at most 4 are used): the external address of the socket
	// that Dial and the responder's listener use.
	OwnAddrs() []netip.AddrPort
	// SendJunk sends one datagram from the noise3 listener's socket to to.
	SendJunk(to netip.AddrPort, payload []byte) error
	// Dial sends fresh noise3 Init datagrams (at most budget in all) to the targets and returns the
	// first flow whose inner QUIC handshake completes, as a carrier.
	Dial(ctx context.Context, targets []netip.AddrPort, budget int) (mux.Carrier, error)
}

// Config wires an Engine to one session.
type Config struct {
	Role  Role
	Link  Link
	Net   Net
	Clock carrier.Clock
	// Rand returns a uniform value in [0, n); nil uses math/rand/v2.
	Rand func(n int64) int64
	// Rules yields the target rules (PEER_ADDR IPs and the peer card's IPs) at the time of use.
	Rules func() Rules
	// Allowed reports whether punching is allowed now: same country (D21), feature path-migration
	// negotiated, profile and policy. A false answer makes the engine silent.
	Allowed func() bool
	// HasDirect reports that the session already has a direct path.
	HasDirect func() bool
	// Bind attaches the punched carrier to the session (PATH_BIND).
	Bind func(ctx context.Context, c mux.Carrier) error
}

// Engine coordinates hole punching on one relayed session.
type Engine struct{ cfg Config }

// New returns an engine.
func New(cfg Config) *Engine { return &Engine{cfg: cfg} }

// Run is the initiator's loop: rounds of offer, sync and Init attempts until a direct path is bound,
// the session ends or ctx is cancelled. It returns nil after a successful bind.
func (e *Engine) Run(ctx context.Context) error { return nil }

// HandleControl feeds a PUNCH_OFFER or PUNCH_SYNC received on the session.
func (e *Engine) HandleControl(f frame.Frame) {}
