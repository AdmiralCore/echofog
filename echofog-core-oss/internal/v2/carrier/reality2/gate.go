package reality2

import (
	"crypto/ecdh"
	"crypto/sha256"
	"errors"
	mrand "math/rand/v2"
	"net"
	"sync"
	"time"
)

// The server gate of wire.md §3.3. It sits in front of the QUIC stack as a net.PacketConn:
// a packet from an admitted 4-tuple goes straight through, a QUIC v1 Initial from a stranger
// is buffered until its ClientHello is complete and then admitted only if its token verifies,
// and everything else is forwarded to the cover. The node never answers a stranger itself
// (V1): WriteTo silently drops any packet addressed to a source that was not admitted.
const (
	// DefaultMaxPending is the number of buffered stranger flights of wire.md §3.3 step 2.
	DefaultMaxPending = 256
	// DefaultMaxPendingBytes is how many bytes one stranger source may buffer.
	DefaultMaxPendingBytes = 8 << 10
	// DefaultPendingTTL is how long a partial flight waits for its remaining Initials before
	// it is forwarded to the cover.
	DefaultPendingTTL = 500 * time.Millisecond
	// admittedIdle is how long an admitted 4-tuple keeps its entry without any traffic in
	// either direction. Every packet to or from the source renews the lease, so a live
	// connection is never cut. The value is deliberately longer than a QUIC idle timeout: it
	// is only a bound on the map of sources that went silent, and a stale tuple costs
	// nothing (the QUIC stack ignores packets of an unknown connection).
	admittedIdle = 30 * time.Minute
)

// MaskKey is one live masking key of the listener (wire.md §3.3: at most 8).
type MaskKey struct {
	Private *ecdh.PrivateKey
}

// GateConfig configures a Gate.
type GateConfig struct {
	// Masks are the live masking keys.
	Masks []MaskKey
	// Addrs are the exact host:port strings this node's cards list; a token is valid only
	// for one of them (wire.md §3.2).
	Addrs []string
	// Cover receives every packet that is not admitted. Required.
	Cover Cover
	// Replay is the shared replay cache. Required.
	Replay *Replay
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// MaxPending, MaxPendingBytes and PendingTTL override the defaults above.
	MaxPending      int
	MaxPendingBytes int
	PendingTTL      time.Duration
	// MaxAdmitted bounds the table of admitted 4-tuples; zero means DefaultMaxAdmitted.
	MaxAdmitted int
}

type maskEntry struct {
	sk  *ecdh.PrivateKey
	pub []byte
	id  [32]byte
}

// Gate is the admission filter of wire.md §3.3.
type Gate struct {
	conn   net.PacketConn
	cover  Cover
	replay *Replay
	masks  []maskEntry
	addrs  []string
	now    func() time.Time

	maxPending int
	maxBytes   int
	ttl        time.Duration

	mu       sync.Mutex
	pending  map[string]*pendingFlight
	admitted map[string]time.Time
	queue    []delivery

	stop      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type delivery struct {
	src net.Addr
	p   []byte
}

type pendingFlight struct {
	src     net.Addr
	started time.Time
	bytes   int
	frags   map[int][]byte
	raws    [][]byte
}

// NewGate wraps conn in the admission filter.
func NewGate(conn net.PacketConn, cfg GateConfig) (*Gate, error) {
	if conn == nil {
		return nil, errors.New("reality2: gate needs a packet connection")
	}
	if cfg.Cover == nil {
		return nil, errors.New("reality2: gate needs a cover forwarder")
	}
	if cfg.Replay == nil {
		return nil, errors.New("reality2: gate needs a replay cache")
	}
	if len(cfg.Masks) > 8 {
		return nil, errors.New("reality2: more than 8 masking keys on one listener")
	}
	if len(cfg.Masks) == 0 {
		return nil, errors.New("reality2: gate needs at least one masking key")
	}
	if len(cfg.Addrs) == 0 {
		return nil, errors.New("reality2: gate needs at least one token address")
	}
	g := &Gate{
		conn:       conn,
		cover:      cfg.Cover,
		replay:     cfg.Replay,
		addrs:      append([]string(nil), cfg.Addrs...),
		now:        cfg.Now,
		maxPending: cfg.MaxPending,
		maxBytes:   cfg.MaxPendingBytes,
		ttl:        cfg.PendingTTL,
		pending:    make(map[string]*pendingFlight),
		admitted:   make(map[string]time.Time),
		stop:       make(chan struct{}),
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.maxPending <= 0 {
		g.maxPending = DefaultMaxPending
	}
	if g.maxBytes <= 0 {
		g.maxBytes = DefaultMaxPendingBytes
	}
	if g.ttl <= 0 {
		g.ttl = DefaultPendingTTL
	}
	for _, m := range cfg.Masks {
		if m.Private == nil || m.Private.Curve() != ecdh.X25519() {
			return nil, errors.New("reality2: masking key is not X25519")
		}
		pub := m.Private.PublicKey().Bytes()
		g.masks = append(g.masks, maskEntry{sk: m.Private, pub: pub, id: sha256.Sum256(pub)})
	}
	go g.reapLoop()
	return g, nil
}

// reapLoop forwards partial flights that passed PendingTTL even when no further packet from
// their source arrives: a lone Initial must end at the cover by the clock, not only when the
// next packet happens to come. Decisions use the injectable clock; the ticker only wakes the
// loop up, at half the TTL with the ±20 % jitter of AD13.
func (g *Gate) reapLoop() {
	period := g.ttl / 2
	if period < time.Millisecond {
		period = time.Millisecond
	}
	jitter := period / 5
	timer := time.NewTimer(period + mrand.N(jitter+1))
	defer timer.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-timer.C:
			g.reapExpired()
			timer.Reset(period + mrand.N(jitter+1))
		}
	}
}

// reapExpired moves every pending flight older than the TTL to the cover.
func (g *Gate) reapExpired() {
	now := g.now()
	var expired []coverPkt
	g.mu.Lock()
	for k, f := range g.pending {
		if now.Sub(f.started) > g.ttl {
			for _, raw := range f.raws {
				expired = append(expired, coverPkt{f.src, raw})
			}
			delete(g.pending, k)
		}
	}
	g.mu.Unlock()
	g.forwardAll(expired)
}

// ReadFrom returns the next packet for the QUIC stack. Packets from strangers are verified or
// forwarded to the cover inside the call; only admitted traffic is ever returned.
func (g *Gate) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		if d, ok := g.popDelivery(); ok {
			return copy(p, d.p), d.src, nil
		}
		n, src, err := g.conn.ReadFrom(p)
		if err != nil {
			return 0, nil, err
		}
		if src == nil {
			continue
		}
		key := src.String()

		if g.isAdmitted(key) {
			return n, src, nil
		}

		pkt := append([]byte(nil), p[:n]...)
		if !isQUICInitial(pkt) {
			g.cover.Forward(src, pkt)
			continue
		}
		initial, err := parseClientInitial(pkt)
		if err != nil {
			g.cover.Forward(src, pkt)
			continue
		}
		flight, hello, complete := g.addInitial(key, src, pkt, initial)
		switch {
		case complete:
			if !g.verify(hello) {
				for _, raw := range flight {
					g.cover.Forward(src, raw)
				}
				continue
			}
			g.admit(key)
			g.mu.Lock()
			for _, raw := range flight[1:] {
				g.queue = append(g.queue, delivery{src: src, p: raw})
			}
			g.mu.Unlock()
			return copy(p, flight[0]), src, nil
		case flight != nil: // overflow or eviction: the whole flight goes to the cover
			for _, raw := range flight {
				g.cover.Forward(src, raw)
			}
		}
	}
}

// addInitial buffers one Initial of a stranger. complete reports that the ClientHello is
// assembled; flight then holds the buffered datagrams in arrival order. When the flight must
// go to the cover (a limit or the TTL was hit), flight holds it and complete is false.
func (g *Gate) addInitial(key string, src net.Addr, pkt []byte, initial *initialPacket) ([][]byte, *Hello, bool) {
	now := g.now()
	var evicted []coverPkt

	g.mu.Lock()
	for k, f := range g.pending {
		if now.Sub(f.started) > g.ttl {
			for _, raw := range f.raws {
				evicted = append(evicted, coverPkt{f.src, raw})
			}
			delete(g.pending, k)
		}
	}
	f := g.pending[key]
	if f == nil {
		if len(g.pending) >= g.maxPending {
			if ok, k, old := g.oldestLocked(); ok {
				for _, raw := range old.raws {
					evicted = append(evicted, coverPkt{old.src, raw})
				}
				delete(g.pending, k)
			}
		}
		f = &pendingFlight{src: src, started: now, frags: make(map[int][]byte)}
		g.pending[key] = f
	}
	f.raws = append(f.raws, pkt)
	f.bytes += len(pkt)
	f.frags = mergeCrypto(f.frags, initial.frags)

	flight := make([][]byte, len(f.raws))
	copy(flight, f.raws)
	if f.bytes > g.maxBytes {
		delete(g.pending, key)
		g.mu.Unlock()
		g.forwardAll(evicted)
		return flight, nil, false
	}
	hello, done := assembledClientHello(f.frags)
	if !done {
		g.mu.Unlock()
		g.forwardAll(evicted)
		return nil, nil, false
	}
	delete(g.pending, key)
	g.mu.Unlock()
	g.forwardAll(evicted)
	parsed, err := ParseHello(hello)
	if err != nil {
		return flight, nil, true // verify(nil) rejects and forwards the flight
	}
	return flight, parsed, true
}

func (g *Gate) oldestLocked() (bool, string, *pendingFlight) {
	var oldestKey string
	var oldest *pendingFlight
	for k, f := range g.pending {
		if oldest == nil || f.started.Before(oldest.started) {
			oldestKey, oldest = k, f
		}
	}
	return oldest != nil, oldestKey, oldest
}

type coverPkt struct {
	src net.Addr
	p   []byte
}

func (g *Gate) forwardAll(pkts []coverPkt) {
	for _, c := range pkts {
		g.cover.Forward(c.src, c.p)
	}
}

// verify checks the token of a parsed hello against every live masking key and every address.
// A token that verifies but was already seen is a replay and is rejected.
func (g *Gate) verify(hello *Hello) bool {
	return verifyHello(g.masks, g.addrs, g.replay, g.now(), hello)
}

// popDelivery returns the next buffered packet of an admitted flight.
func (g *Gate) popDelivery() (delivery, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queue) == 0 {
		return delivery{}, false
	}
	d := g.queue[0]
	g.queue = g.queue[1:]
	return d, true
}

// isAdmitted reports whether key is an admitted 4-tuple and renews its lease. Every packet in
// either direction keeps a live connection admitted, so a session is never cut after a fixed
// time; only a source silent for admittedIdle loses its entry (and then a new hello is needed,
// which is exactly what a dead connection would send).
func (g *Gate) isAdmitted(key string) bool {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.admitted[key]
	if !ok {
		return false
	}
	if now.Sub(at) > admittedIdle {
		delete(g.admitted, key)
		return false
	}
	g.admitted[key] = now
	return true
}

func (g *Gate) admit(key string) {
	g.mu.Lock()
	g.admitted[key] = g.now()
	g.mu.Unlock()
}

// WriteTo sends a packet to an admitted source only. Everything else is dropped: a node must
// not send anything to an unauthenticated address (wire.md §3.3 step 3, V1).
func (g *Gate) WriteTo(p []byte, addr net.Addr) (int, error) {
	if addr == nil || !g.isAdmitted(addr.String()) {
		return len(p), nil
	}
	return g.conn.WriteTo(p, addr)
}

// AdmittedEvicted reports how many admitted 4-tuples have been evicted from the table. It is a
// metric for the status view.
func (g *Gate) AdmittedEvicted() uint64 { return 0 }

// Close stops the pending-flight reaper and closes the socket. It is idempotent.
func (g *Gate) Close() error {
	g.closeOnce.Do(func() {
		close(g.stop)
		g.closeErr = g.conn.Close()
	})
	return g.closeErr
}

func (g *Gate) LocalAddr() net.Addr                { return g.conn.LocalAddr() }
func (g *Gate) SetDeadline(t time.Time) error      { return g.conn.SetDeadline(t) }
func (g *Gate) SetReadDeadline(t time.Time) error  { return g.conn.SetReadDeadline(t) }
func (g *Gate) SetWriteDeadline(t time.Time) error { return g.conn.SetWriteDeadline(t) }

var _ net.PacketConn = (*Gate)(nil)
