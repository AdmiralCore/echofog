package network

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/echofog/echofog-core/internal/network/mimicry"
)

var _ net.PacketConn = (*QuicRealityConn)(nil)
var _ dpi.UDPLikePacketConn = (*QuicRealityConn)(nil)

type activeProbeProxy struct {
	targetAddr string
	listenConn net.PacketConn
	mu         sync.Mutex
	sessions   map[string]*probeSession
}

type probeSession struct {
	clientAddr net.Addr
	upstream   *net.UDPConn
	lastActive atomic.Int64 // unix nanoseconds; the reader goroutine touches it without p.mu
}

func (s *probeSession) touch()              { s.lastActive.Store(time.Now().UnixNano()) }
func (s *probeSession) idle() time.Duration { return time.Since(time.Unix(0, s.lastActive.Load())) }

func rawPacketConn(pc net.PacketConn) net.PacketConn {
	type unwrapper interface {
		Unwrap() net.PacketConn
	}
	if u, ok := pc.(unwrapper); ok {
		return u.Unwrap()
	}
	return pc
}

func newActiveProbeProxy(targetAddr string, listenConn net.PacketConn) *activeProbeProxy {
	return &activeProbeProxy{
		targetAddr: targetAddr,
		listenConn: rawPacketConn(listenConn),
		sessions:   make(map[string]*probeSession),
	}
}

const maxStrangerSessions = 48

func (p *activeProbeProxy) Forward(clientAddr net.Addr, data []byte) {
	if p.targetAddr == "" {
		return
	}
	key := clientAddr.String()
	p.mu.Lock()
	sess, ok := p.sessions[key]
	if !ok {
		if len(p.sessions) >= maxStrangerSessions {
			var oldestKey string
			var oldest int64
			for k, s := range p.sessions {
				if t := s.lastActive.Load(); oldestKey == "" || t < oldest {
					oldestKey, oldest = k, t
				}
			}
			if s, exists := p.sessions[oldestKey]; exists {
				_ = s.upstream.Close()
				delete(p.sessions, oldestKey)
			}
		}
		uAddr, err := net.ResolveUDPAddr("udp", p.targetAddr)
		if err != nil {
			p.mu.Unlock()
			return
		}
		uConn, err := net.DialUDP("udp", nil, uAddr)
		if err != nil {
			p.mu.Unlock()
			return
		}
		sess = &probeSession{clientAddr: clientAddr, upstream: uConn}
		sess.touch()
		p.sessions[key] = sess

		// Background worker to read answers from camouflage site and forward back to stranger
		go func(s *probeSession) {
			defer s.upstream.Close()
			buf := make([]byte, 4096)
			for {
				_ = s.upstream.SetReadDeadline(time.Now().Add(10 * time.Second))
				n, err := s.upstream.Read(buf)
				if err != nil {
					p.mu.Lock()
					delete(p.sessions, s.clientAddr.String())
					p.mu.Unlock()
					return
				}
				s.touch()
				_, _ = p.listenConn.WriteTo(buf[:n], s.clientAddr)
			}
		}(sess)
	}
	sess.touch()
	p.mu.Unlock()

	_, _ = sess.upstream.Write(data)
}

func (p *activeProbeProxy) HasSession(clientAddr net.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	sess, ok := p.sessions[clientAddr.String()]
	if !ok {
		return false
	}
	if sess.idle() > 60*time.Second {
		_ = sess.upstream.Close()
		delete(p.sessions, clientAddr.String())
		return false
	}
	return true
}

func (p *activeProbeProxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, s := range p.sessions {
		_ = s.upstream.Close()
		delete(p.sessions, k)
	}
}

// QuicRealityConn wraps a net.PacketConn for QUIC REALITY stealth authentication.
type QuicRealityConn struct {
	base net.PacketConn
	qt   *QuicTransport

	mu             sync.Mutex
	serverMode     bool
	serverAuthKey  []byte
	camouflageAddr string
	probeProxy     *activeProbeProxy

	authedAddrs map[string]bool
	authedDCIDs map[string]string // dcid -> client addr string

	// A QUIC-REALITY client's NAT may give it a new outside port mid-connection. quic-go follows
	// the client to the new address, which must get raw QUIC too: sealed as noise, the answers
	// would never open and the connection would die. Connection IDs cannot tell which connection
	// a packet belongs to (clients move to IDs the node issued in encrypted frames), so the
	// decision follows quic-go: an address that sent a packet that was neither noise nor an
	// Initial becomes a candidate, and turns raw only when quic-go, having opened that packet,
	// sends there, and the address is not a noise peer's. Nobody can switch a live noise peer to
	// raw from outside: its address keeps being heard as noise.
	rebindMu sync.Mutex
	cands    map[netip.AddrPort]time.Time
	followed []netip.AddrPort // addresses made raw this way, oldest first, bounded
	nCands   atomic.Int32     // len(cands), read without the lock on every write

	// Buffered gated REALITY reassembly. The stealth token is bound to the ClientHello's key_share,
	// which (with ML-KEM) spans both of a client's Initials and whose CRYPTO quic-go scatters across
	// them, so the token cannot be verified from the first Initial alone. Client Initials from an
	// unauthenticated source are held in pending until the ClientHello reassembles; the flight is then
	// verified once and released to quic-go (drain) if valid, or to the cover site if a stranger or
	// forged. Nothing unverified ever reaches quic-go, so the node never shows its certificate to a
	// prober or to a censor replaying a stolen random on its own key_share. reaper evicts flights that
	// never complete (to the cover site), bounded by maxPendingHellos and maxPendingBytesPerSrc.
	reassMu    sync.Mutex
	pending    map[string]*pendingHello
	drain      []drainPkt
	reaperStop chan struct{}
	reaperOnce sync.Once
}

type pendingHello struct {
	addr  net.Addr
	frags map[int][]byte
	raws  [][]byte
	dcid  []byte
	bytes int
	start time.Time
}

type drainPkt struct {
	b    []byte
	addr net.Addr
}

const (
	// maxPendingHellos bounds concurrent half-received ClientHellos, like stranger sessions; on
	// overflow the oldest is evicted to the cover site.
	maxPendingHellos = maxStrangerSessions
	// maxPendingBytesPerSrc caps a source's buffered Initials. A real ML-KEM ClientHello is two ~1250 B
	// Initials; this leaves room for a few retransmits before giving up (to the cover site).
	maxPendingBytesPerSrc = 8 << 10
	// pendingReassembleTTL is how long a partial ClientHello is held. A client's two Initials arrive in
	// one burst, so this only matters for abandoned or replayed partial flights; it is kept well under
	// the replay test's 1 s window so such flights still reach the cover site in time.
	pendingReassembleTTL = 500 * time.Millisecond
	reaperInterval       = 100 * time.Millisecond
)

const (
	maxRebindCands = 1024
	// maxFollowed bounds the addresses made raw after a rebinding; the oldest goes back to noise.
	// Its connection is long gone by then, or has moved again.
	maxFollowed = 4096
)

func addrPortOfNet(a net.Addr) (netip.AddrPort, bool) {
	if ua, ok := a.(*net.UDPAddr); ok {
		ap := ua.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
	}
	ap, err := netip.ParseAddrPort(a.String())
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), err == nil
}

// noteCandidate: a packet from addr that is neither noise nor an Initial went to quic-go.
func (c *QuicRealityConn) noteCandidate(addr net.Addr) {
	ap, ok := addrPortOfNet(addr)
	if !ok {
		return
	}
	c.rebindMu.Lock()
	defer c.rebindMu.Unlock()
	if c.cands == nil || len(c.cands) >= maxRebindCands {
		c.cands = make(map[netip.AddrPort]time.Time) // stale or flooded: start over
	}
	c.cands[ap] = time.Now()
	c.nCands.Store(int32(len(c.cands)))
}

// followRebinding turns a candidate address raw once quic-go sends it a short-header packet:
// quic-go opened what came from there, so a connection really moved to it.
func (c *QuicRealityConn) followRebinding(p []byte, addr net.Addr) {
	if c.nCands.Load() == 0 || len(p) == 0 || p[0]&0x80 != 0 || c.qt == nil || c.qt.obfs == nil {
		return
	}
	ap, ok := addrPortOfNet(addr)
	if !ok {
		return
	}
	c.rebindMu.Lock()
	at, ok := c.cands[ap]
	if !ok {
		c.rebindMu.Unlock()
		return
	}
	delete(c.cands, ap)
	c.nCands.Store(int32(len(c.cands)))
	if time.Since(at) > 30*time.Second || c.qt.obfs.IsNoiseHeard(addr) {
		c.rebindMu.Unlock()
		return
	}
	var expired netip.AddrPort
	if len(c.followed) >= maxFollowed {
		expired = c.followed[0]
		c.followed = c.followed[1:]
	}
	c.followed = append(c.followed, ap)
	c.rebindMu.Unlock()
	if expired.IsValid() {
		c.qt.obfs.SetRaw(expired, false)
	}
	c.markAuthed(addr, nil)
	c.qt.obfs.SetRaw(ap, true)
}

// NewQuicRealityConn wraps conn with QUIC REALITY functionality.
func NewQuicRealityConn(conn net.PacketConn, qt *QuicTransport) *QuicRealityConn {
	return &QuicRealityConn{
		base:        conn,
		qt:          qt,
		authedAddrs: make(map[string]bool),
		authedDCIDs: make(map[string]string),
		pending:     make(map[string]*pendingHello),
	}
}

// SetCamouflage configures the server-side active probe forwarding and stealth authentication.
func (c *QuicRealityConn) SetCamouflage(authKey []byte, camouflageAddr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serverMode = true
	c.serverAuthKey = authKey
	c.camouflageAddr = camouflageAddr
	if c.probeProxy != nil {
		c.probeProxy.Close()
	}
	if camouflageAddr != "" {
		c.probeProxy = newActiveProbeProxy(camouflageAddr, c.base)
		c.startReaper()
	} else {
		c.probeProxy = nil
	}
}

// startReaper runs a background sweep that evicts partial ClientHellos older than
// pendingReassembleTTL to the cover site, so an abandoned or replayed partial flight does not linger
// and still ends up where a stranger's does. Started once in server mode, stopped in Close.
func (c *QuicRealityConn) startReaper() {
	c.reaperOnce.Do(func() {
		c.reaperStop = make(chan struct{})
		go func() {
			t := time.NewTicker(reaperInterval)
			defer t.Stop()
			for {
				select {
				case <-c.reaperStop:
					return
				case <-t.C:
					c.reapStalePending()
				}
			}
		}()
	})
}

func (c *QuicRealityConn) isAuthed(addr net.Addr, dcid []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if addr != nil && c.authedAddrs[addr.String()] {
		return true
	}
	if len(dcid) > 0 {
		if authedAddr, ok := c.authedDCIDs[string(dcid)]; ok {
			if addr != nil && authedAddr == addr.String() {
				return true
			}
		}
	}
	return false
}

func (c *QuicRealityConn) isAddrAuthed(addr net.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authedAddrs[addr.String()]
}

func (c *QuicRealityConn) markAuthed(addr net.Addr, dcid []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authedAddrs[addr.String()] = true
	if len(dcid) > 0 {
		c.authedDCIDs[string(dcid)] = addr.String()
	}
}

// UnmarkConnection cleans up authenticated state when a peer connection closes.
func (c *QuicRealityConn) UnmarkConnection(addr net.Addr) {
	if addr == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	addrStr := addr.String()
	delete(c.authedAddrs, addrStr)
	for d, a := range c.authedDCIDs {
		if a == addrStr {
			delete(c.authedDCIDs, d)
		}
	}
	if c.qt != nil && c.qt.obfs != nil {
		if ap, err := netip.ParseAddrPort(addrStr); err == nil {
			c.qt.obfs.SetRaw(ap, false)
		}
	}
	c.rebindMu.Lock()
	for i, a := range c.followed {
		if a.String() == addrStr {
			c.followed = append(c.followed[:i], c.followed[i+1:]...)
			break
		}
	}
	c.rebindMu.Unlock()
}

func (c *QuicRealityConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		// Hand back any buffered datagrams of an already-verified flight before reading the socket.
		if qb, qaddr, ok := c.popDrain(); ok {
			return copy(p, qb), qaddr, nil
		}

		n, addr, err := c.base.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}

		c.mu.Lock()
		serverMode := c.serverMode
		authKey := c.serverAuthKey
		proxy := c.probeProxy
		c.mu.Unlock()

		if serverMode && authKey != nil {
			isNoise := false
			if c.qt != nil && c.qt.obfs != nil {
				isNoise = c.qt.obfs.IsNoiseHeard(addr)
			}
			if isNoise {
				c.markAuthed(addr, nil)
				return n, addr, nil
			}

			if mimicry.IsQUICInitial(p[:n]) {
				dcid, _ := mimicry.ExtractInitialDCID(p[:n])
				if c.isAuthed(addr, dcid) {
					// An already-authenticated source: a retransmitted Initial, or one spoofed from a
					// connected client's address. The stealth token rides in the ClientHello random, a
					// genuine part of the handshake, so the Initial goes to quic-go unchanged.
					c.markAuthed(addr, dcid)
					c.setAddrRaw(addr)
					return n, addr, nil
				}
				// A source already being proxied to the cover site: keep its later Initials going there
				// rather than re-buffering them.
				if proxy != nil && proxy.HasSession(addr) {
					proxy.Forward(addr, p[:n])
					continue
				}
				// Unknown source: buffer until the ClientHello reassembles, then authenticate the whole
				// flight at once. The token is bound to the key_share, which spans both Initials, so
				// nothing unverified reaches quic-go and nothing is decided from a single Initial. A
				// verified flight is queued to drain (returned above); a stranger or forged hello is
				// forwarded to the cover site inside accumulateInitial.
				c.accumulateInitial(addr, append([]byte(nil), p[:n]...), authKey, proxy)
				continue
			}

			// Non-Initial packet
			if !c.isAddrAuthed(addr) {
				if proxy != nil && proxy.HasSession(addr) {
					proxy.Forward(addr, p[:n])
					continue
				}
				c.noteCandidate(addr)
			}
		}

		return n, addr, nil
	}
}

func (c *QuicRealityConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// A REALITY client's stealth token travels in the ClientHello random, set on the dial's TLS
	// randomness source (see QuicTransport.dial / mimicry.QUICInitialRand), not injected into the
	// Initial here: the first Initial then has an empty Token field, as a real Chrome's does.
	c.followRebinding(p, addr)
	return c.base.WriteTo(p, addr)
}

func (c *QuicRealityConn) Close() error {
	c.mu.Lock()
	if c.probeProxy != nil {
		c.probeProxy.Close()
	}
	stop := c.reaperStop
	c.mu.Unlock()
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	return c.base.Close()
}

// popDrain returns the next buffered datagram of a verified flight, to hand to quic-go in place of a
// live read. Verified Initials are replayed this way (one per ReadFrom) because they were consumed
// from the socket while being buffered for reassembly.
func (c *QuicRealityConn) popDrain() ([]byte, net.Addr, bool) {
	c.reassMu.Lock()
	defer c.reassMu.Unlock()
	if len(c.drain) == 0 {
		return nil, nil, false
	}
	d := c.drain[0]
	c.drain = c.drain[1:]
	return d.b, d.addr, true
}

// setAddrRaw marks addr as a raw-QUIC (REALITY) peer so the obfuscation layer stops expecting noise
// from it.
func (c *QuicRealityConn) setAddrRaw(addr net.Addr) {
	if c.qt == nil || c.qt.obfs == nil {
		return
	}
	if ua, ok := addr.(*net.UDPAddr); ok {
		c.qt.obfs.SetRaw(ua.AddrPort(), true)
	} else if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
		c.qt.obfs.SetRaw(ap, true)
	}
}

// accumulateInitial buffers a client Initial from an unauthenticated source and, once its ClientHello
// reassembles, authenticates the whole flight at once: a valid flight is queued to quic-go (drain), a
// stranger's or a forged one (e.g. a stolen random carried on a different key_share) goes to the cover
// site. The token is bound to the key_share, which spans both Initials, so the decision cannot be made
// from a single Initial — which is why the flight is held rather than forwarded eagerly. Caller must
// not hold c.reassMu.
func (c *QuicRealityConn) accumulateInitial(addr net.Addr, raw []byte, authKey []byte, proxy *activeProbeProxy) {
	dcid, frags, okDec := mimicry.QUICInitialCrypto(raw)
	key := addr.String()

	type coverPkt struct {
		addr net.Addr
		b    []byte
	}
	var toCover []coverPkt
	var verified net.Addr // non-nil once the flight is authed and queued to drain
	var verifiedDCID []byte

	c.reassMu.Lock()
	ph := c.pending[key]
	if ph == nil {
		if len(c.pending) >= maxPendingHellos {
			if ok, oldKey, old := c.oldestPendingLocked(); ok {
				for _, r := range old.raws {
					toCover = append(toCover, coverPkt{old.addr, r})
				}
				delete(c.pending, oldKey)
			}
		}
		ph = &pendingHello{addr: addr, frags: make(map[int][]byte), start: time.Now()}
		c.pending[key] = ph
	}
	ph.raws = append(ph.raws, raw)
	ph.bytes += len(raw)
	if okDec {
		if ph.dcid == nil && len(dcid) > 0 {
			ph.dcid = dcid
		}
		ph.frags = mimicry.MergeCryptoFrags(ph.frags, frags)
	}

	hello := mimicry.AssembleClientHello(ph.frags)
	switch {
	case mimicry.QUICClientHelloComplete(hello):
		if mimicry.VerifyQUICClientHelloToken(hello, authKey) {
			for _, r := range ph.raws {
				c.drain = append(c.drain, drainPkt{b: r, addr: addr})
			}
			verified, verifiedDCID = addr, ph.dcid
		} else {
			for _, r := range ph.raws {
				toCover = append(toCover, coverPkt{addr, r})
			}
		}
		delete(c.pending, key)
	case ph.bytes > maxPendingBytesPerSrc:
		for _, r := range ph.raws {
			toCover = append(toCover, coverPkt{addr, r})
		}
		delete(c.pending, key)
	default:
		// still partial: keep buffering until the next Initial or the reaper's TTL
	}
	c.reassMu.Unlock()

	if verified != nil {
		c.markAuthed(verified, verifiedDCID)
		c.setAddrRaw(verified)
	}
	if proxy != nil {
		for _, cp := range toCover {
			proxy.Forward(cp.addr, cp.b)
		}
	}
}

// oldestPendingLocked returns the pending flight with the earliest start. c.reassMu must be held.
func (c *QuicRealityConn) oldestPendingLocked() (bool, string, *pendingHello) {
	var oldestKey string
	var oldest *pendingHello
	for k, p := range c.pending {
		if oldest == nil || p.start.Before(oldest.start) {
			oldestKey, oldest = k, p
		}
	}
	return oldest != nil, oldestKey, oldest
}

// reapStalePending evicts partial ClientHellos older than the TTL, sending their datagrams to the
// cover site so an abandoned or replayed partial flight ends where a stranger's does.
func (c *QuicRealityConn) reapStalePending() {
	c.mu.Lock()
	proxy := c.probeProxy
	c.mu.Unlock()

	type coverPkt struct {
		addr net.Addr
		b    []byte
	}
	var toCover []coverPkt
	now := time.Now()
	c.reassMu.Lock()
	for k, ph := range c.pending {
		if now.Sub(ph.start) > pendingReassembleTTL {
			for _, r := range ph.raws {
				toCover = append(toCover, coverPkt{ph.addr, r})
			}
			delete(c.pending, k)
		}
	}
	c.reassMu.Unlock()

	if proxy != nil {
		for _, cp := range toCover {
			proxy.Forward(cp.addr, cp.b)
		}
	}
}

func (c *QuicRealityConn) LocalAddr() net.Addr                { return c.base.LocalAddr() }
func (c *QuicRealityConn) SetDeadline(t time.Time) error      { return c.base.SetDeadline(t) }
func (c *QuicRealityConn) SetReadDeadline(t time.Time) error  { return c.base.SetReadDeadline(t) }
func (c *QuicRealityConn) SetWriteDeadline(t time.Time) error { return c.base.SetWriteDeadline(t) }

func (c *QuicRealityConn) SetReadBuffer(bytes int) error {
	if u, ok := c.base.(dpi.UDPLikePacketConn); ok {
		return u.SetReadBuffer(bytes)
	}
	return nil
}

func (c *QuicRealityConn) SetWriteBuffer(bytes int) error {
	if u, ok := c.base.(dpi.UDPLikePacketConn); ok {
		return u.SetWriteBuffer(bytes)
	}
	return nil
}

func (c *QuicRealityConn) SyscallConn() (syscall.RawConn, error) {
	if u, ok := c.base.(dpi.UDPLikePacketConn); ok {
		return u.SyscallConn()
	}
	return nil, nil
}
