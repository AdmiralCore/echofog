package network

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	tcpHandshakeTimeout = 10 * time.Second
	tcpMaxHandshakes    = 64
)

// MimicryOptions configures the TCP/TLS camouflage of the fallback transport.
type MimicryOptions struct {
	// StealthPassword is the network-wide secret gating the camouflage. Empty uses a built-in
	// default key (public in the source; set this for real deployments).
	StealthPassword string
	// ServerName is the SNI the fallback mimics (your own domain when you present a real cert).
	ServerName string
	// CamouflageAddr is the real "host:port" an active probe is forwarded to; empty serves a
	// small static page.
	CamouflageAddr string
}

// MimicryConfigFrom builds the camouflage config from node options. The stealth key gates who gets
// past the camouflage page; peers are authenticated afterwards by the peer handshake, so the TLS
// certificate is not verified (and cannot be: it is self-signed while the SNI names a big website).
func MimicryConfigFrom(o MimicryOptions) *mimicry.MimicryConfig {
	cfg := mimicry.DefaultMimicryConfig()
	cfg.AuthKey = mimicry.StealthKeyFromPassword(o.StealthPassword)
	cfg.InsecureSkipVerify = true
	if o.ServerName != "" {
		cfg.ServerName = o.ServerName
	}
	cfg.FallbackAddr = o.CamouflageAddr
	return cfg
}

// TCPTransport carries peer connections over TCP: TLS 1.3 that looks like a browser visiting
// a big website, then smux for streams and datagrams, then the same peer handshake as QUIC.
// Connections without the stealth token (active probes) are served the camouflage site.
type TCPTransport struct {
	cfg       *mimicry.MimicryConfig
	serverTLS *tls.Config
	auth      PeerAuthenticator

	mu       sync.Mutex
	ln       net.Listener
	connChan chan NewConnEvent
	sem      chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
	logger   *slog.Logger

	advertisedPort atomic.Uint32 // port peers should dial back; announced after authentication

	// per-node stealth keys (see SetNodeKeys); with them off, cfg.AuthKey is used both ways
	perNode bool
	secret  string
	book    peerBook
	guard   atomic.Pointer[handshake.Guard]
}

// SetNodeKeys makes the stealth key per node: this node accepts the key derived from its own ID,
// and a dial to a peer uses the key derived from the peer's ID (see ExpectPeer). secret is an
// optional extra secret for a private network.
func (t *TCPTransport) SetNodeKeys(secret string, own types.PeerID) {
	t.mu.Lock()
	c := *t.cfg
	c.AuthKey = mimicry.NodeStealthKey(secret, own)
	t.cfg = &c
	t.mu.Unlock()
	t.secret = secret
	t.perNode = true
}

// SetCamouflage switches the site this transport mimics: the certificate it presents, the SNI and
// the real address unauthenticated visitors are forwarded to. Connections accepted or dialed
// afterwards use it. A public node that found its camouflage neighbour only after start-up calls
// this before raising its TCP listener (see FallbackTransport.ListenTCP).
func (t *TCPTransport) SetCamouflage(cert tls.Certificate, serverName, fallbackAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := *t.cfg
	if serverName != "" {
		c.ServerName = serverName
	}
	c.FallbackAddr = fallbackAddr
	t.cfg = &c
	t.serverTLS = mimicry.BuildServerTLSConfig(cert)
}

// camouflage returns the current mimicry config and server TLS config, as one consistent snapshot.
func (t *TCPTransport) camouflage() (*mimicry.MimicryConfig, *tls.Config) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg, t.serverTLS
}

// ExpectPeer tells the transport that the node at addr has peer ID id.
func (t *TCPTransport) ExpectPeer(addr string, id types.PeerID) { t.book.set(addr, id) }

// SetCaps sets what this node announces to peers in the handshake.
func (t *TCPTransport) SetCaps(c handshake.Capability) {
	if s, ok := t.auth.(CapsSetter); ok {
		s.SetCaps(c)
	}
}

// SetAccess sets the hidden-node gate and the invitation keyring (see DefaultPeerAuthenticator).
func (t *TCPTransport) SetAccess(g *access.Gate, k *access.Keyring, hide bool) {
	if s, ok := t.auth.(AccessSetter); ok {
		s.SetAccess(g, k)
		s.HideListenPort(hide)
	}
}

func NewTCPTransport(cert tls.Certificate, cfg *mimicry.MimicryConfig, auth PeerAuthenticator) *TCPTransport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &TCPTransport{
		cfg:       cfg,
		serverTLS: mimicry.BuildServerTLSConfig(cert),
		auth:      auth,
		connChan:  make(chan NewConnEvent, 32),
		sem:       make(chan struct{}, tcpMaxHandshakes),
		ctx:       ctx,
		cancel:    cancel,
		logger:    slog.Default().With("component", "tcp_transport"),
	}
	t.guard.Store(handshake.NewGuard(handshake.GuardConfig{}))
	return t
}

func (t *TCPTransport) SetLogger(l *slog.Logger) {
	if l != nil {
		t.logger = l
	}
}

// SetAdvertisedPort sets the port announced to peers we dial (the node's QUIC port: both
// transports listen on the same number).
func (t *TCPTransport) SetAdvertisedPort(p int) { t.advertisedPort.Store(uint32(p)) }

func (t *TCPTransport) ConnChan() <-chan NewConnEvent { return t.connChan }

func (t *TCPTransport) Addr() net.Addr {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln == nil {
		return nil
	}
	return t.ln.Addr()
}

// Listen accepts TCP peers on addr.
func (t *TCPTransport) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", addr, err)
	}
	t.mu.Lock()
	t.ln = ln
	t.mu.Unlock()
	go t.acceptLoop(ln)
	return nil
}

func (t *TCPTransport) acceptLoop(ln net.Listener) {
	for {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		// A global cap on in-flight handshakes bounds our own CPU/goroutine use under a flood. It is
		// deliberately NOT per address: a real web server never singles out one visitor, so neither
		// may we. The per-address Guard (rate limit and ban) is applied later, inside handleInbound,
		// and only to connections that proved knowledge of the stealth token — never to visitors who
		// are merely forwarded to the real site.
		select {
		case t.sem <- struct{}{}:
		default:
			_ = raw.Close() // handshake flood
			continue
		}
		go func() {
			var once sync.Once
			release := func() { once.Do(func() { <-t.sem }) }
			defer release()
			t.handleInbound(raw, release)
		}()
	}
}

// tuneTCP disables Nagle so small media frames go out immediately (TCP already adds
// head-of-line delay on loss; Nagle would stack more on top), and keeps the link warm.
func tuneTCP(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

// handleInbound serves one inbound connection. A visitor that does not present a valid stealth
// token is forwarded to the real site and is NEVER counted, rate-limited or banned: the mimicked
// site does not reset such connections, and a prober opening ordinary HTTPS connections (or any
// NAT/CGNAT behind which many clients share one address) must look exactly like traffic to that
// site, or the node is singled out. The per-address Guard is consulted only once a valid token
// proves the peer knows the stealth secret, and from then on a ban can come only from a failed
// *authenticated* handshake. releaseSlot frees the global handshake-concurrency slot; it is called
// as soon as the expensive TLS handshake is finished so a long-lived forwarded or authenticated
// connection never holds it.
func (t *TCPTransport) handleInbound(raw net.Conn, releaseSlot func()) {
	tuneTCP(raw)
	_ = raw.SetDeadline(time.Now().Add(tcpHandshakeTimeout))
	cfg, serverTLS := t.camouflage()
	conn, isPeer, binding, err := mimicry.ServerHandshake(raw, serverTLS, cfg)
	if err != nil {
		releaseSlot()
		_ = raw.Close()
		return // a TLS error is not attributed to the address: it may be a probe, a middlebox, or a real browser
	}
	if !isPeer {
		releaseSlot()
		forwardToRealSite(conn, cfg) // idle, not absolute, timeout; never touches the Guard
		return
	}

	// A valid stealth token was presented: only now does the per-address Guard apply.
	ip := addrOfNet(raw.RemoteAddr())
	release, allowed := t.guard.Load().Allow(ip)
	if !allowed {
		releaseSlot()
		_ = conn.Close()
		return
	}
	defer release()

	mc, err := NewSmuxConn(t.ctx, conn, binding, false)
	if err != nil {
		releaseSlot()
		_ = conn.Close()
		t.guard.Load().Fail(ip)
		return
	}
	res, err := t.auth.AuthenticateInbound(t.ctx, mc)
	if err != nil {
		releaseSlot()
		_ = mc.CloseWithError(ErrCodeAuthFailed, "auth failed")
		t.logger.Debug("TCP inbound authentication failed", "remote", raw.RemoteAddr(), "err", err)
		t.guard.Load().Fail(ip) // a failed handshake from a holder of a valid token
		return
	}
	t.guard.Load().Success(ip)
	releaseSlot()
	// the source port is ephemeral; the peer announces where it listens (0: it does not)
	addr, dialable := raw.RemoteAddr().String(), false
	actx, cancel := context.WithTimeout(t.ctx, 3*time.Second)
	if port, ok := mc.(*smuxConn).awaitAnnounce(actx); ok {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr, dialable = net.JoinHostPort(host, strconv.Itoa(int(port))), true
		}
	}
	cancel()
	_ = raw.SetDeadline(time.Time{})
	t.emit(NewConnEvent{Conn: mc, IsOut: false, PeerID: res.PeerID, PeerPubKey: res.PubKey, Addr: addr, Dialable: dialable, Caps: res.Caps})
	t.logger.Info("Peer authenticated over TCP", "peer_id", res.PeerID, "direction", "inbound", "remote_addr", raw.RemoteAddr().String())
}

// forwardIdleTimeout bounds the forwarded (camouflage) path by inactivity, reset on every read or
// write, instead of an absolute deadline. A real site keeps an HTTPS connection open for as long as
// it is used; a fixed absolute cut-off would itself be a timing fingerprint that the mimicked site
// does not have.
const forwardIdleTimeout = 2 * time.Minute

// forwardToRealSite hands a visitor to the real site with an idle timeout, so nothing of ours is
// ever shown and the connection lives as long as a genuine one would.
func forwardToRealSite(conn net.Conn, cfg *mimicry.MimicryConfig) {
	mimicry.HandleActiveProbe(&idleConn{Conn: conn, idle: forwardIdleTimeout}, cfg)
}

// idleConn resets a read/write deadline on each operation, turning an absolute deadline into an
// idle one.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

// Dial connects to a peer listening on addr over TCP.
func (t *TCPTransport) Dial(ctx context.Context, addr string) error {
	// pinned to the real network while whole-computer mode is on: through the TUN this
	// connection would try to carry itself
	raw, err := DirectDialer(0).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	tuneTCP(raw)
	deadline := time.Now().Add(tcpHandshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = raw.SetDeadline(deadline)
	cfg, _ := t.camouflage()
	clientCfg := cfg
	expected, haveExpected := t.book.get(addr)
	if t.perNode {
		if !haveExpected {
			_ = raw.Close()
			return ErrPeerUnknown
		}
		c := *cfg
		c.AuthKey = mimicry.NodeStealthKey(t.secret, expected)
		clientCfg = &c
	}
	conn, binding, err := mimicry.ClientHandshake(raw, clientCfg)
	if err != nil {
		_ = raw.Close()
		return err
	}
	mc, err := NewSmuxConn(t.ctx, conn, binding, true)
	if err != nil {
		_ = conn.Close()
		return err
	}
	actx := ctx
	if haveExpected {
		actx = WithExpectedPeer(ctx, expected)
	}
	res, err := t.auth.AuthenticateOutbound(actx, mc)
	if err != nil {
		_ = mc.CloseWithError(ErrCodeAuthFailed, "auth failed")
		return fmt.Errorf("outbound authentication failed: %w", err)
	}
	if haveExpected && res.PeerID != expected {
		_ = mc.CloseWithError(ErrCodeAuthFailed, "unexpected peer")
		return ErrPeerMismatch
	}
	if err := mc.(*smuxConn).announcePort(uint16(t.advertisedPort.Load())); err != nil {
		_ = mc.CloseWithError(ErrCodeNormalClose, "announce failed")
		return err
	}
	_ = raw.SetDeadline(time.Time{})
	t.emit(NewConnEvent{Conn: mc, IsOut: true, PeerID: res.PeerID, PeerPubKey: res.PubKey, Addr: addr, Dialable: true, Caps: res.Caps})
	t.logger.Info("Peer authenticated over TCP", "peer_id", res.PeerID, "direction", "outbound", "remote_addr", addr)
	return nil
}

func (t *TCPTransport) emit(ev NewConnEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = ev.Conn.CloseWithError(ErrCodeNormalClose, "shutting down")
		return
	}
	select {
	case t.connChan <- ev:
	default:
		// nobody is consuming: do not block the handshake goroutine forever
		go func() {
			select {
			case t.connChan <- ev:
			case <-t.ctx.Done():
				_ = ev.Conn.CloseWithError(ErrCodeNormalClose, "shutting down")
			}
		}()
	}
}

func (t *TCPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	ln := t.ln
	t.mu.Unlock()
	t.cancel()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

// ErrNoTCP: the TCP fallback is not available on this node.
var ErrNoTCP = errors.New("TCP transport is not enabled")

// SetGuard replaces the per-address limits of inbound connections (tests, tuning).
func (t *TCPTransport) SetGuard(g *handshake.Guard) {
	if g != nil {
		t.guard.Store(g)
	}
}
