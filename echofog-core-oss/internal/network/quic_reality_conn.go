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
}

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
	} else {
		c.probeProxy = nil
	}
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
				valid, dcid := mimicry.VerifyQUICInitialToken(p[:n], authKey)
				isAuthed := c.isAuthed(addr, dcid)

				if !valid && !isAuthed {
					// Stranger / scanner / active probe: forward to camouflage site
					if proxy != nil {
						proxy.Forward(addr, p[:n])
					}
					continue
				}

				c.markAuthed(addr, dcid)
				if c.qt != nil && c.qt.obfs != nil {
					if ua, ok := addr.(*net.UDPAddr); ok {
						c.qt.obfs.SetRaw(ua.AddrPort(), true)
					} else if ap, err2 := netip.ParseAddrPort(addr.String()); err2 == nil {
						c.qt.obfs.SetRaw(ap, true)
					}
				}

				if clean, ok := mimicry.StripInitialToken(p[:n]); ok {
					copy(p, clean)
					return len(clean), addr, nil
				}
				return n, addr, nil
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
	// Client dialing camouflage node: inject stealth token into QUIC Initial packet
	if mimicry.IsQUICInitial(p) && c.qt != nil {
		addrStr := addr.String()
		sni, hasSNI := c.qt.book.getSNI(addrStr)
		expectedID, haveExpected := c.qt.book.get(addrStr)
		if !haveExpected {
			if ua, ok := addr.(*net.UDPAddr); ok {
				expectedID, haveExpected = c.qt.book.get(ua.AddrPort().String())
				if !hasSNI {
					sni, hasSNI = c.qt.book.getSNI(ua.AddrPort().String())
				}
			}
		}
		if !hasSNI && haveExpected {
			sni, hasSNI = c.qt.book.getSNIByID(expectedID)
		}

		if hasSNI && sni != "" && haveExpected {
			authKey := mimicry.NodeStealthKey(c.qt.obfsSecret, expectedID)
			if modified, dcid, ok := mimicry.InjectInitialToken(p, authKey); ok {
				c.markAuthed(addr, dcid)
				p = modified
			}
		}
	}

	c.followRebinding(p, addr)
	return c.base.WriteTo(p, addr)
}

func (c *QuicRealityConn) Close() error {
	c.mu.Lock()
	if c.probeProxy != nil {
		c.probeProxy.Close()
	}
	c.mu.Unlock()
	return c.base.Close()
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
