package punch

import (
	"context"
	crand "crypto/rand"
	"errors"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
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
	Class  NAT
	Lo, Hi uint16
	// Samples is how many observations the range rests on.
	Samples int
}

// Narrow reports whether the observed port block is tight enough, and seen often enough, for a
// birthday punch to be aimed at it.
func (n NATInfo) Narrow() bool {
	return n.Lo >= 1024 && n.Hi >= n.Lo && n.Samples >= 2 && int(n.Hi-n.Lo) <= NarrowRange
}

// Choose is the mode table of services.md §6.4.1: own mode from the own and the peer's NAT. A peer
// that stated nothing counts as a cone NAT that cannot spray back.
func Choose(own, peer NATInfo) Mode {
	if own.Class == NATSymmetric {
		switch peer.Class {
		case NATCone:
			return ModeDial
		case NATSymmetric:
			if own.Narrow() && peer.Narrow() {
				return ModeMutual
			}
		}
		return ModeNone
	}
	if peer.Class == NATSymmetric {
		return ModeSpray
	}
	return ModeOrdinary
}

// Counterpart reports whether the peer's stated mode matches ours, so that we may run ours.
func Counterpart(mine, peers Mode) bool {
	switch mine {
	case ModeDial:
		return peers == ModeSpray
	case ModeSpray:
		return peers == ModeDial
	case ModeMutual:
		return peers == ModeMutual
	case ModeOrdinary:
		return peers == ModeOrdinary
	}
	return false
}

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
func (g Gates) Open() bool {
	return g.Noise3Allowed && g.Negotiated && g.Flag && !g.Guard && !g.Anonymous && !g.Proxy
}

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
	// Rand returns a uniform value in [0, n); nil uses math/rand/v2.
	Rand func(n int64) int64
	// Gates yields the conditions at the time of use; nil means all closed.
	Gates func() Gates
}

// Birthday runs the three modes and keeps their counters.
type Birthday struct {
	cfg       BirthdayConfig
	mu        sync.Mutex
	active    int
	tried, ok atomic.Int64
}

// NewBirthday returns a Birthday.
func NewBirthday(cfg BirthdayConfig) *Birthday {
	if cfg.Rand == nil {
		cfg.Rand = func(n int64) int64 { return mrand.Int64N(n) }
	}
	return &Birthday{cfg: cfg}
}

// Enabled reports whether the gates are open now.
func (b *Birthday) Enabled() bool { return b.cfg.Gates != nil && b.cfg.Gates().Open() }

// Net is the socket side the modes run on.
func (b *Birthday) Net() SockNet { return b.cfg.Net }

// Stats returns the attempts started and the ones that found a mapping (counts only).
func (b *Birthday) Stats() (tried, ok int64) { return b.tried.Load(), b.ok.Load() }

// begin takes an attempt slot; it fails when punching is off or the node is busy.
func (b *Birthday) begin() error {
	if b.cfg.Gates == nil || !b.cfg.Gates().Open() {
		return ErrDisabled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active >= MaxConcurrent {
		return ErrBusy
	}
	b.active++
	b.tried.Add(1)
	return nil
}

func (b *Birthday) end() {
	b.mu.Lock()
	b.active--
	b.mu.Unlock()
}

func (b *Birthday) junk() []byte {
	buf := make([]byte, JunkSizeMin+int(b.cfg.Rand(int64(JunkSizeMax-JunkSizeMin+1))))
	_, _ = crand.Read(buf)
	return buf
}

const tick = 5 * time.Millisecond

// run is the pacing loop shared by the three modes. It calls send for every datagram that is due
// (the schedule is fixed from the start, so a late wake-up is absorbed by sending the backlog; the
// rate carries a jitter of up to 3 %) and poll for a hit, until a hit, the end of the window or the
// budget, or ctx ends. send reports false when the mode has nothing more to send.
func (b *Birthday) run(ctx context.Context, send func(i int) bool, poll func() (Hit, bool)) (Hit, error) {
	clk := b.cfg.Clock
	start := clk.Now()
	expired := make(chan struct{})
	wt := clk.AfterFunc(PunchWindow, func() { close(expired) })
	defer wt.Stop()
	sent, more := 0, true
	for {
		if h, ok := poll(); ok {
			b.ok.Add(1)
			return h, nil
		}
		select {
		case <-expired:
			return Hit{}, ErrNoHit
		case <-ctx.Done():
			return Hit{}, ctx.Err()
		default:
		}
		if more {
			rate := float64(PunchRate) * (1 + float64(b.cfg.Rand(61)-30)/1000)
			due := int(clk.Now().Sub(start).Seconds()*rate) + 1
			for sent < due && sent < MaxPunchPackets && more {
				more = send(sent)
				sent++
			}
			if sent >= MaxPunchPackets {
				more = false
			}
		}
		t := make(chan struct{})
		tt := clk.AfterFunc(tick, func() { close(t) })
		select {
		case <-t:
		case <-expired:
			tt.Stop()
			if h, ok := poll(); ok {
				b.ok.Add(1)
				return h, nil
			}
			return Hit{}, ErrNoHit
		case <-ctx.Done():
			tt.Stop()
			return Hit{}, ctx.Err()
		}
	}
}

func fromIP(d Datagram, ip netip.Addr) bool { return d.From.Addr().Unmap() == ip.Unmap() }

// Dial is the symmetric side: up to MaxDialSockets sockets send junk to the cone peer; the first
// socket that receives a datagram from the cone's IP is the hit and the others are closed. With
// reply set the hit socket answers with one junk datagram, so that a peer that has to send the Init
// learns the socket's external port.
func (b *Birthday) Dial(ctx context.Context, cone netip.AddrPort, reply bool) (Hit, error) {
	if err := b.begin(); err != nil {
		return Hit{}, err
	}
	defer b.end()
	socks := b.openSocks(MaxDialSockets)
	if len(socks) == 0 {
		return Hit{}, ErrNoSocket
	}
	h, err := b.run(ctx,
		func(i int) bool { _ = socks[i%len(socks)].Send(cone, b.junk()); return true },
		func() (Hit, bool) {
			for _, s := range socks {
				select {
				case d := <-s.Recv():
					if fromIP(d, cone.Addr()) {
						return Hit{Sock: s, Target: d.From}, true
					}
				default:
				}
			}
			return Hit{}, false
		})
	closeExcept(socks, h.Sock)
	if err == nil && reply {
		_ = h.Sock.Send(h.Target, b.junk())
	}
	return h, err
}

// Spray is the cone side: junk from the listener's socket across the ports of the symmetric side's
// IP; the target is the source of the first datagram that comes back from that IP.
func (b *Birthday) Spray(ctx context.Context, ip netip.Addr, lo, hi uint16) (Hit, error) {
	if err := b.begin(); err != nil {
		return Hit{}, err
	}
	defer b.end()
	l := b.cfg.Net.Listener()
	ports := b.sprayPorts(lo, hi, MaxSprayPackets)
	ip = ip.Unmap()
	return b.run(ctx,
		func(i int) bool {
			_ = l.Send(netip.AddrPortFrom(ip, ports[i]), b.junk())
			return i+1 < len(ports)
		},
		func() (Hit, bool) {
			for {
				select {
				case d := <-l.Recv():
					if fromIP(d, ip) {
						return Hit{Sock: l, Target: d.From}, true
					}
				default:
					return Hit{}, false
				}
			}
		})
}

// Mutual is the both-symmetric case: up to MaxMutualSockets sockets, each aimed at exactly one
// guessed port of the peer's range (one destination per socket keeps a symmetric mapping stable).
// The hit is the socket that receives a datagram from exactly the port it aimed at.
func (b *Birthday) Mutual(ctx context.Context, ip netip.Addr, lo, hi uint16) (Hit, error) {
	if err := b.begin(); err != nil {
		return Hit{}, err
	}
	defer b.end()
	ip = ip.Unmap()
	socks := b.openSocks(MaxMutualSockets)
	if len(socks) == 0 {
		return Hit{}, ErrNoSocket
	}
	targets := make([]netip.AddrPort, len(socks))
	span := int64(hi) - int64(lo) + 1
	if span < 1 {
		span = 1
	}
	for i := range targets {
		targets[i] = netip.AddrPortFrom(ip, uint16(int64(lo)+b.cfg.Rand(span)))
	}
	h, err := b.run(ctx,
		func(i int) bool { _ = socks[i%len(socks)].Send(targets[i%len(socks)], b.junk()); return true },
		func() (Hit, bool) {
			for i, s := range socks {
				select {
				case d := <-s.Recv():
					if d.From == targets[i] {
						return Hit{Sock: s, Target: d.From}, true
					}
				default:
				}
			}
			return Hit{}, false
		})
	closeExcept(socks, h.Sock)
	if err == nil {
		// The peer's own socket learns the hit from this datagram: it may have stopped sending.
		_ = h.Sock.Send(h.Target, b.junk())
	}
	return h, err
}

func (b *Birthday) openSocks(limit int) []Sock {
	var socks []Sock
	for len(socks) < limit {
		s, err := b.cfg.Net.OpenSock()
		if err != nil {
			break
		}
		socks = append(socks, s)
	}
	return socks
}

func closeExcept(socks []Sock, keep Sock) {
	for _, s := range socks {
		if s != keep {
			_ = s.Close()
		}
	}
}

// sprayPorts picks the m ports to spray. A narrow observed block is swept, widened by SprayMargin on
// each side and clamped to 1024..65535, in shuffled order; a wide or unknown spread goes random over
// 1024..65535.
func (b *Birthday) sprayPorts(lo, hi uint16, m int) []uint16 {
	out := make([]uint16, 0, m)
	if lo >= 1024 && hi >= lo && int(hi-lo) <= NarrowRange {
		base := max(int(lo)-SprayMargin, 1024)
		top := min(int(hi)+SprayMargin, 65535)
		width := top - base + 1
		offs := make([]int, width)
		for i := range offs {
			offs[i] = i
		}
		for i := width - 1; i > 0; i-- {
			j := int(b.cfg.Rand(int64(i + 1)))
			offs[i], offs[j] = offs[j], offs[i]
		}
		for i := 0; i < m; i++ {
			out = append(out, uint16(base+offs[i%width]))
		}
		return out
	}
	for i := 0; i < m; i++ {
		out = append(out, uint16(1024+b.cfg.Rand(65535-1024+1)))
	}
	return out
}
