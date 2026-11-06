package punch

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Birthday punching through symmetric NATs (services.md §6.4.1, feature `birthday-punch`).

// Limits of services.md §6.4.1.
const (
	// MaxDialSockets is how many sockets the symmetric side of dial opens.
	MaxDialSockets = 256
	// MaxMutualSockets is how many sockets each side of mutual opens.
	MaxMutualSockets = 64
	// MaxSprayPackets is how many datagrams the spraying side sends in one attempt.
	MaxSprayPackets = 900
	// MaxPunchPackets is the most punching datagrams one node sends in one attempt.
	MaxPunchPackets = 960
	// PunchWindow bounds the punching phase of one attempt.
	PunchWindow = 3 * time.Second
	// PunchRate is the average datagrams per second.
	PunchRate = 320
	// MaxConcurrent is how many birthday attempts may run on a node at once.
	MaxConcurrent = 2
	// SprayMargin is how far past the observed range the spray reaches on each side.
	SprayMargin = 4000
	// NarrowRange is the widest observed port block still treated as narrow.
	NarrowRange = 8000
)

// NAT is the class carried in PunchOffer.Nat.
type NAT uint8

const (
	NATUnstated NAT = iota // field absent: counts as cone, never sprayed at
	NATCone
	NATSymmetric
	NATUnknown
)

// Mode is the punching mode carried in PunchOffer.Mode.
type Mode uint8

const (
	ModeOrdinary Mode = iota
	ModeDial
	ModeSpray
	ModeMutual
	ModeNone
)

// NATInfo is what a node knows about its NAT, or what a peer's offer says about it.
type NATInfo struct {
	Class   NAT
	Lo, Hi  uint16
	Samples int
}

// Narrow reports whether the observed port block is tight enough, and seen often enough, for a
// birthday punch to be aimed at it.
func (n NATInfo) Narrow() bool { return false }

// Choose is the mode table of services.md §6.4.1: own mode from the own and the peer's NAT.
func Choose(own, peer NATInfo) Mode { return ModeNone }

// Counterpart reports whether the peer's stated mode matches ours, so that we may run ours.
func Counterpart(mine, peers Mode) bool { return false }

// Gates are the conditions under which birthday punching sends anything (services.md §6.4.1,
// "Where it is off").
type Gates struct {
	Noise3Allowed bool // same country (D21)
	Negotiated    bool // birthday-punch and path-migration
	Guard         bool // guard mode
	Anonymous     bool // anonymous mode
	Proxy         bool // traffic goes through a proxy
	Flag          bool // profile flag `birthday`
}

// Open reports whether birthday punching is allowed.
func (g Gates) Open() bool { return true }

// Datagram is one received UDP datagram.
type Datagram struct {
	From    netip.AddrPort
	Payload []byte
}

// Sock is one UDP socket of the birthday punch.
type Sock interface {
	Send(to netip.AddrPort, payload []byte) error
	Recv() <-chan Datagram
	Close() error
}

// SockNet is the socket side of birthday punching.
type SockNet interface {
	// OpenSock opens a fresh UDP socket on an ephemeral port.
	OpenSock() (Sock, error)
	// Listener is the socket of the noise3 listener (the one OwnAddrs reports).
	Listener() Sock
	// DialFrom sends Init datagrams (at most budget) from sock to target and returns the first flow
	// whose inner QUIC handshake completes, as a carrier.
	DialFrom(ctx context.Context, sock Sock, target netip.AddrPort, budget int) (mux.Carrier, error)
	// AcceptOn runs the noise3 listener on sock until a flow arrives or ctx ends.
	AcceptOn(ctx context.Context, sock Sock) error
}

// Errors of birthday punching.
var (
	ErrDisabled = errors.New("punch: birthday punching is off")
	ErrBusy     = errors.New("punch: too many birthday attempts")
	ErrNoSocket = errors.New("punch: no socket")
	ErrNoHit    = errors.New("punch: no mapping found")
)

// Hit is a found mapping: the socket that carries it and the peer address to send Init to.
type Hit struct {
	Sock   Sock
	Target netip.AddrPort
}

// BirthdayConfig wires a Birthday.
type BirthdayConfig struct {
	Clock carrier.Clock
	Net   SockNet
	Rand  func(n int64) int64
	Gates func() Gates
}

// Birthday runs the three modes and keeps their counters.
type Birthday struct{ cfg BirthdayConfig }

// NewBirthday returns a Birthday.
func NewBirthday(cfg BirthdayConfig) *Birthday { return &Birthday{cfg: cfg} }

// Stats returns the attempts started and the ones that found a mapping (counts only).
func (b *Birthday) Stats() (tried, ok int64) { return 0, 0 }

// Dial is the symmetric side: n sockets, junk to cone, hit on the first datagram from cone's IP.
// With reply set the hit socket answers with one junk datagram, so the peer learns its port.
func (b *Birthday) Dial(ctx context.Context, cone netip.AddrPort, reply bool) (Hit, error) {
	return Hit{}, ErrNoHit
}

// Spray is the cone side: junk from the listener across the symmetric side's ports; the target is
// the source of the first datagram from that IP.
func (b *Birthday) Spray(ctx context.Context, ip netip.Addr, lo, hi uint16) (Hit, error) {
	return Hit{}, ErrNoHit
}

// Mutual is the both-symmetric case: one guessed port per socket.
func (b *Birthday) Mutual(ctx context.Context, ip netip.Addr, lo, hi uint16) (Hit, error) {
	return Hit{}, ErrNoHit
}
