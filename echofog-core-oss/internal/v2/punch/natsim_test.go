package punch

import (
	"context"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// A small simulated internet for the birthday modes: every node sits behind one NAT with
// port-restricted filtering (a packet is delivered only from an address the mapping has sent to).
// A cone NAT keeps one external port per socket; a symmetric NAT picks a fresh random port from its
// block for every (socket, destination) pair.

type simWorld struct {
	mu   sync.Mutex
	nats map[netip.Addr]*simNAT
	clk  *fakeClock
}

func newSimWorld(clk *fakeClock) *simWorld {
	return &simWorld{nats: map[netip.Addr]*simNAT{}, clk: clk}
}

type simMapping struct {
	ext     uint16
	sock    *simSock
	dest    netip.AddrPort // symmetric: the only destination
	allowed map[netip.AddrPort]bool
}

type simNAT struct {
	w          *simWorld
	ip         netip.Addr
	sym        bool
	lo, hi     int // symmetric block
	rnd        *mrand.Rand
	mu         sync.Mutex
	maps       []*simMapping
	used       map[uint16]bool
	nextCone   uint16
	listener   *simSock
	open, peak int
	sent       []simPkt
	dials      []simDial
	accepts    int
	pair       *memcarrier.Pair
}

type simPkt struct {
	to   netip.AddrPort
	size int
	at   time.Time
}

type simDial struct {
	sock   *simSock
	target netip.AddrPort
	budget int
}

func (w *simWorld) addNAT(ip string, sym bool, lo, hi int, seed uint64) *simNAT {
	n := &simNAT{w: w, ip: netip.MustParseAddr(ip), sym: sym, lo: lo, hi: hi,
		rnd: mrand.New(mrand.NewPCG(seed, seed+1)), used: map[uint16]bool{}, nextCone: 50000, pair: memcarrier.NewQUIC()}
	w.mu.Lock()
	w.nats[n.ip] = n
	w.mu.Unlock()
	n.listener = n.newSock(false)
	if !sym {
		n.mu.Lock()
		n.mappingLocked(n.listener, netip.AddrPort{})
		n.mu.Unlock()
	}
	return n
}

// coneAddr is the fixed external address of a cone NAT's listener.
func (n *simNAT) coneAddr() netip.AddrPort { return netip.AddrPortFrom(n.ip, 50000) }

type simSock struct {
	nat    *simNAT
	recv   chan Datagram
	closed bool
	local  bool // the listener: not counted as a birthday socket
}

func (n *simNAT) newSock(count bool) *simSock {
	s := &simSock{nat: n, recv: make(chan Datagram, 4096), local: !count}
	if count {
		n.mu.Lock()
		n.open++
		if n.open > n.peak {
			n.peak = n.open
		}
		n.mu.Unlock()
	}
	return s
}

func (s *simSock) Recv() <-chan Datagram { return s.recv }

func (s *simSock) Close() error {
	n := s.nat
	n.mu.Lock()
	defer n.mu.Unlock()
	if !s.closed && !s.local {
		s.closed = true
		n.open--
	}
	return nil
}

func (s *simSock) Send(to netip.AddrPort, p []byte) error {
	n := s.nat
	n.mu.Lock()
	n.sent = append(n.sent, simPkt{to, len(p), n.w.clk.Now()})
	m := n.mappingLocked(s, to)
	src := netip.AddrPortFrom(n.ip, m.ext)
	m.allowed[to] = true
	n.mu.Unlock()
	n.w.deliver(src, to, p)
	return nil
}

func (n *simNAT) mappingLocked(s *simSock, dest netip.AddrPort) *simMapping {
	for _, m := range n.maps {
		if m.sock != s {
			continue
		}
		if !n.sym || m.dest == dest {
			return m
		}
	}
	m := &simMapping{sock: s, dest: dest, allowed: map[netip.AddrPort]bool{}}
	if n.sym {
		for {
			p := uint16(n.lo + n.rnd.IntN(n.hi-n.lo+1))
			if !n.used[p] {
				n.used[p] = true
				m.ext = p
				break
			}
		}
	} else {
		m.ext = n.nextCone
		n.nextCone++
	}
	n.maps = append(n.maps, m)
	return m
}

// extFor is the external address a socket has toward dest (zero if it never sent there).
func (n *simNAT) extFor(s Sock, dest netip.AddrPort) netip.AddrPort {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.maps {
		if Sock(m.sock) == s && (!n.sym || m.dest == dest) {
			return netip.AddrPortFrom(n.ip, m.ext)
		}
	}
	return netip.AddrPort{}
}

func (w *simWorld) deliver(src, to netip.AddrPort, p []byte) {
	w.mu.Lock()
	dst := w.nats[to.Addr()]
	w.mu.Unlock()
	if dst == nil {
		return
	}
	dst.mu.Lock()
	var sock *simSock
	for _, m := range dst.maps {
		if m.ext == to.Port() && m.allowed[src] {
			sock = m.sock
			break
		}
	}
	dst.mu.Unlock()
	if sock == nil {
		return
	}
	select {
	case sock.recv <- Datagram{From: src, Payload: append([]byte(nil), p...)}:
	default:
	}
}

// --- SockNet over a simNAT ---

type simNet struct{ n *simNAT }

func (s simNet) OpenSock() (Sock, error) { return s.n.newSock(true), nil }
func (s simNet) Listener() Sock          { return s.n.listener }
func (s simNet) DialFrom(_ context.Context, sock Sock, target netip.AddrPort, budget int) (mux.Carrier, error) {
	s.n.mu.Lock()
	s.n.dials = append(s.n.dials, simDial{sock.(*simSock), target, budget})
	s.n.mu.Unlock()
	return s.n.pair.Client, nil
}
func (s simNet) AcceptOn(ctx context.Context, sock Sock) error {
	s.n.mu.Lock()
	s.n.accepts++
	s.n.mu.Unlock()
	return nil
}

// --- driving the fake clock ---

// drive runs f while stepping the fake clock in 5 ms steps; it fails the test if f needs more than
// 30 s of fake time.
func drive(clk *fakeClock, f func()) (elapsed time.Duration) {
	done := make(chan struct{})
	go func() { f(); close(done) }()
	start := clk.Now()
	for {
		select {
		case <-done:
			return clk.Now().Sub(start)
		default:
		}
		if clk.Now().Sub(start) > 30*time.Second {
			panic("birthday attempt ran past 30 s of fake time")
		}
		clk.Advance(5 * time.Millisecond)
		time.Sleep(150 * time.Microsecond)
	}
}

type lockedRand struct {
	mu sync.Mutex
	r  *mrand.Rand
}

func (l *lockedRand) Int63n(n int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Int64N(n)
}

func openGates() func() Gates {
	return func() Gates {
		return Gates{Noise3Allowed: true, Negotiated: true, Flag: true}
	}
}

func newBD(clk *fakeClock, n *simNAT, seed uint64, gates func() Gates) *Birthday {
	lr := &lockedRand{r: mrand.New(mrand.NewPCG(seed, 7))}
	return NewBirthday(BirthdayConfig{Clock: clk, Net: simNet{n}, Rand: lr.Int63n, Gates: gates})
}

func (n *simNAT) packets() []simPkt {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]simPkt(nil), n.sent...)
}

func (n *simNAT) peakSockets() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peak
}
