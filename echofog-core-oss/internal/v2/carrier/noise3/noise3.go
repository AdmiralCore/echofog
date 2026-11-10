// Package noise3 is the masked UDP carrier of docs/protocol/v2/wire.md §2. Every datagram of a
// flow looks uniformly random to an observer who does not know the initiator's ephemeral secret or
// the responder's masking private key. It carries an inner QUIC v1 connection (§5).
//
// The package has three parts: carrier/noise3/elligator (the representative of a dirty ephemeral
// key), carrier/noise3/packet (the Init and data datagrams, header protection, packet-number
// reconstruction and the replay window), and this package (Listener, Dialer, masking keys, budgets,
// pending flows and the virtual net.PacketConn the inner QUIC stack runs on).
package noise3

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"golang.org/x/crypto/curve25519"

	quic "github.com/apernet/quic-go"
)

// Clock is the injectable time source of the carrier layer (AD13).
type Clock = carrier.Clock

// Limits of wire.md §2.1 and §10.
const (
	// MaxMaskKeys is the number of live masking keys one UDP port may hold.
	MaxMaskKeys = 8
	// MaxPendingFlows is the total pending-flow limit.
	MaxPendingFlows = 4096
	// MaxPendingPerIP is the pending-flow limit per source IP.
	MaxPendingPerIP = 64

	initTimeWindow  = 300 * time.Second
	pendingInitAge  = 10 * time.Second
	flowIdleTimeout = 120 * time.Second
	// silentRebind is how long a flow must have been silent before it is tried for a datagram
	// from an unknown address, and recentRebindFlows is how many such flows are tried.
	silentRebind      = 1 * time.Second
	recentRebindFlows = 8
	// replyMovePackets is how many authenticated packets with higher numbers a new address
	// needs before the reply address moves.
	replyMovePackets = 2
	// ampFactor is the anti-amplification factor of wire.md §2.5 step 9 and §2.6.
	ampFactor = 3
	// restartMin and restartMax bound the dead-flow restart uniform(12, 18) s.
	restartMin = 12 * time.Second
	restartMax = 18 * time.Second
	// sweepEvery is how often expired flows are removed on packet activity.
	sweepEvery = 5 * time.Second
	// reapQueue bounds the inner datagram queue of one flow.
	reapQueue = 1024
)

// ErrBadConfig reports an unusable Listener or Dialer configuration.
var ErrBadConfig = errors.New("noise3: invalid configuration")

// ErrDNSName means a noise3 address names a host but no resolver/dialer hook was supplied. The
// carrier never resolves peer names with the local system resolver: that would leak the hidden
// node to local DNS (see objects.md §3.1 and wire.md §1).
var ErrDNSName = errors.New("noise3: DNS host requires a resolver")

// MaskKey is one live masking X25519 private key of a listener.
type MaskKey struct {
	Private  [32]byte
	NotAfter time.Time
}

type maskKey struct {
	secret   [32]byte
	public   [32]byte
	notAfter time.Time
}

func newSystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d, f)
}

func randIntN(r *mrand.Rand, n int) int {
	if n <= 1 {
		return 0
	}
	if r != nil {
		return r.IntN(n)
	}
	return mrand.IntN(n)
}

// restartDelay draws the dead-flow restart uniform(12, 18) s.
func restartDelay(r *mrand.Rand) time.Duration {
	span := int64(restartMax - restartMin)
	if r != nil {
		return restartMin + time.Duration(r.Int64N(span+1))
	}
	return restartMin + time.Duration(mrand.Int64N(span+1))
}

// maskPublic derives the public key of a masking private key.
func maskPublic(sk [32]byte) ([32]byte, error) {
	var pub [32]byte
	x, err := curve25519.X25519(sk[:], curve25519.Basepoint)
	if err != nil {
		return pub, err
	}
	copy(pub[:], x)
	return pub, nil
}

// cardMaskKey extracts the masking public key an address points at (1-based mask index).
func cardMaskKey(card *objects.Card, addr *pb.Addr, now time.Time) ([32]byte, error) {
	var out [32]byte
	if card == nil || addr == nil {
		return out, ErrBadConfig
	}
	i := int(addr.GetMask())
	list := card.MaskKeys()
	if i < 1 || i > len(list) {
		return out, fmt.Errorf("%w: mask index %d", ErrBadConfig, i)
	}
	k, err := keys.ParseKind(list[i-1].GetKey(), keys.X25519)
	if err != nil {
		return out, fmt.Errorf("%w: masking key: %v", ErrBadConfig, err)
	}
	if na := list[i-1].GetNotAfter(); na != 0 && uint64(now.Unix()) > na {
		return out, fmt.Errorf("%w: masking key expired", ErrBadConfig)
	}
	copy(out[:], k.Key())
	return out, nil
}

// addressProfile parses the shape profile of a card address.
func addressProfile(addr *pb.Addr) (*shape.Profile, error) {
	sp := objects.SuiteParams(addr)
	return shape.Parse(sp.GetShape())
}

func maxPadding(profile *shape.Profile) int {
	if profile == nil {
		return 0
	}
	n := 0
	if profile.Pad != nil {
		n = int(profile.Pad.Max)
	}
	for _, b := range profile.Buckets {
		if int(b) > n {
			n = int(b)
		}
	}
	return n
}

// innerQUICConfig returns the inner QUIC configuration of wire.md §2.4: Initial datagrams may be
// smaller than 1200 bytes (the fork's AllowSmallInitial), and a client sends its whole handshake
// flight at the drawn flight size (flight, the inner size: the Init total minus 71; zero for a
// listener). maxDatagram is the outer noise3 ceiling; the inner QUIC stack must never emit a
// datagram that does not fit behind the data overhead and the largest padding. A ceiling below
// packet.MinCeiling cannot carry an Init and is rejected.
func innerQUICConfig(base *quic.Config, pad, maxDatagram, flight int) (*quic.Config, error) {
	if maxDatagram <= 0 {
		maxDatagram = packet.DefaultDatagramLen
	}
	if maxDatagram < packet.MinCeiling {
		return nil, fmt.Errorf("%w: %d-byte datagrams are below the %d-byte floor of an Init",
			ErrBadConfig, maxDatagram, packet.MinCeiling)
	}
	if base == nil {
		var err error
		base, err = quicx.Noise3QUICConfig(pad)
		if err != nil {
			return nil, fmt.Errorf("noise3: inner QUIC: %w", err)
		}
	}
	out := *base
	inner := maxDatagram - packet.DataOverhead - pad
	if inner < 800 {
		return nil, fmt.Errorf("%w: a %d-byte ceiling with %d bytes of padding leaves %d-byte inner datagrams",
			ErrBadConfig, maxDatagram, pad, inner)
	}
	out.AllowSmallInitial = true
	out.InitialPacketSize = uint16(min(1200, inner))
	if int(out.MaxUDPPayloadSize) == 0 || int(out.MaxUDPPayloadSize) > inner {
		out.MaxUDPPayloadSize = uint16(inner)
	}
	if flight > 0 {
		out.InitialFlightSize = uint16(flight)
	}
	if out.MaxDatagramFrameSize == 0 || out.MaxDatagramFrameSize > int64(inner) {
		out.MaxDatagramFrameSize = int64(inner)
	}
	return &out, nil
}

// l0MaxDatagram returns the configured outer ceiling or the default (the UDP payload of an IPv4
// path of MTU 1200, wire.md §2.4).
func l0MaxDatagram(v int) int {
	if v <= 0 {
		return packet.DefaultDatagramLen
	}
	if v > packet.MaxDatagramLen {
		return packet.MaxDatagramLen
	}
	return v
}

// flightSize draws the Init total of one flow, uniform over the profile's init_len and clamped to
// the ceiling and to 1200 bytes (wire.md §2.4).
func flightSize(profile *shape.Profile, maxDatagram int, rnd *mrand.Rand) int {
	lo, hi := packet.MinCeiling, packet.MaxFirstLen
	if profile != nil && profile.InitLen != nil {
		lo, hi = int(profile.InitLen.Min), int(profile.InitLen.Max)
	}
	hi = min(hi, maxDatagram, packet.MaxFirstLen)
	lo = min(lo, hi)
	if hi <= lo {
		return hi
	}
	return lo + randIntN(rnd, hi-lo+1)
}

// RegisterDialer registers d under the noise3 suite number.
func (d *Dialer) Register(r *carrier.Registry) { r.RegisterDialer(objects.SuiteNoise3, d) }

// RegisterListener registers l under the noise3 suite number.
func (l *Listener) Register(r *carrier.Registry) { r.RegisterListener(objects.SuiteNoise3, l) }

// randomBytes is unused in production paths; keys come from elligator.Generate.
