package network

import (
	"net"
	"net/netip"
	"sync"
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
	lastActive time.Time
}

func newActiveProbeProxy(targetAddr string, listenConn net.PacketConn) *activeProbeProxy {
	return &activeProbeProxy{
		targetAddr: targetAddr,
		listenConn: listenConn,
		sessions:   make(map[string]*probeSession),
	}
}

func (p *activeProbeProxy) Forward(clientAddr net.Addr, data []byte) {
	if p.targetAddr == "" {
		return
	}
	key := clientAddr.String()
	p.mu.Lock()
	sess, ok := p.sessions[key]
	if !ok {
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
		sess = &probeSession{
			clientAddr: clientAddr,
			upstream:   uConn,
			lastActive: time.Now(),
		}
		p.sessions[key] = sess

		// Background worker to read answers from camouflage site and forward back to stranger
		go func(s *probeSession) {
			defer s.upstream.Close()
			buf := make([]byte, 4096)
			for {
				_ = s.upstream.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := s.upstream.Read(buf)
				if err != nil {
					p.mu.Lock()
					delete(p.sessions, s.clientAddr.String())
					p.mu.Unlock()
					return
				}
				s.lastActive = time.Now()
				_, _ = p.listenConn.WriteTo(buf[:n], s.clientAddr)
			}
		}(sess)
	}
	sess.lastActive = time.Now()
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
	if time.Since(sess.lastActive) > 60*time.Second {
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
	authedDCIDs map[string]bool
}

// NewQuicRealityConn wraps conn with QUIC REALITY functionality.
func NewQuicRealityConn(conn net.PacketConn, qt *QuicTransport) *QuicRealityConn {
	return &QuicRealityConn{
		base:        conn,
		qt:          qt,
		authedAddrs: make(map[string]bool),
		authedDCIDs: make(map[string]bool),
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
	if c.authedAddrs[addr.String()] {
		return true
	}
	if len(dcid) > 0 && c.authedDCIDs[string(dcid)] {
		return true
	}
	return false
}

func (c *QuicRealityConn) markAuthed(addr net.Addr, dcid []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authedAddrs[addr.String()] = true
	if len(dcid) > 0 {
		c.authedDCIDs[string(dcid)] = true
	}
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
			if !c.isAuthed(addr, nil) && proxy != nil && proxy.HasSession(addr) {
				proxy.Forward(addr, p[:n])
				continue
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
