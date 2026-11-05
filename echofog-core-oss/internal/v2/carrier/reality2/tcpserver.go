package reality2

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// Limits of wire.md §4.2 and the defaults of this listener.
// coverMaxLife bounds one proxied connection to the cover.
var coverMaxLife = 10 * time.Minute

const (
	// FirstRecordTimeout is how long the server waits for the first TLS record of a connection.
	FirstRecordTimeout = 10 * time.Second
	// MaxFirstRecord is the largest first record the server reads (16 KiB).
	MaxFirstRecord = 16 << 10
	// DefaultMaxPreToken is the number of connections that may wait for their first record.
	DefaultMaxPreToken = 256
	// DefaultMaxPerAddr is the number of admitted connections one source address may hold. It
	// counts only connections that presented a valid token (wire.md §4.2).
	DefaultMaxPerAddr = 64

	handshakeTimeout = 10 * time.Second
	coverDialTimeout = 5 * time.Second
	coverIdleTimeout = 2 * time.Minute
	defaultMaxCover  = 1024
	acceptBacklog    = 64
	proxyBufSize     = 32 << 10
)

// TCPCover is the cover profile of reality2-tcp, measured over TCP (wire.md §3.4, §4.2): the
// group and the ALPN the cover selects, and the certificate re-issued with the cover's length.
type TCPCover struct {
	// Group is the key exchange group the cover selects.
	Group tls.CurveID
	// ALPN is the application protocol the cover selects ("h2" or "http/1.1"); empty selects none.
	ALPN string
	// Cert is the cover's certificate as the node reproduces it.
	Cert *tls.Certificate
	// NoTickets turns session tickets off when the cover sends none.
	NoTickets bool
}

// TCPServerTLS is the TLS config of the admitted connections of a cover.
func TCPServerTLS(c TCPCover) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, SessionTicketsDisabled: c.NoTickets}
	if c.Group != 0 {
		cfg.CurvePreferences = []tls.CurveID{c.Group}
	}
	if c.ALPN != "" {
		cfg.NextProtos = []string{c.ALPN}
	}
	if c.Cert != nil {
		cfg.Certificates = []tls.Certificate{*c.Cert}
	}
	return cfg
}

// TCPListenerConfig configures a TCPListener.
type TCPListenerConfig struct {
	// Cover is the cover profile; required unless ServerTLS is set.
	Cover TCPCover
	// ServerTLS, when set, is the TLS config of the admitted connections instead of one built from
	// Cover (the own-cover mode passes the config its website uses).
	ServerTLS *tls.Config
	// Masks are the live masking keys (at most 8, wire.md §2.1 D10).
	Masks []MaskKey
	// Addrs are the exact host:port strings this node's cards list for this suite.
	Addrs []string
	// Replay is the replay cache shared by every listener with the same masking key; nil makes a
	// local one, which is then not shared.
	Replay *Replay
	// CoverAddr is the cover site's TCP address (host:port); strangers are proxied there.
	CoverAddr string
	// DialCover opens the connection to the cover; nil dials TCP directly.
	DialCover func(ctx context.Context, network, addr string) (net.Conn, error)
	// Shape is the padding profile of the admitted connections; nil is the built-in default-tls
	// profile (wire.md §9).
	Shape *shape.Profile
	// FirstRecordTimeout, MaxPreToken, MaxPerAddr and MaxCover override the defaults above;
	// MaxCover bounds the proxied connections to the cover. A negative MaxPerAddr means no limit.
	FirstRecordTimeout time.Duration
	MaxPreToken        int
	MaxPerAddr         int
	MaxCover           int
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnError receives replay persistence errors; nil ignores them.
	OnError func(error)
}

// TCPListener accepts reality2-tcp connections (wire.md §4.2): it reads the first TLS record of
// every connection, completes TLS itself for a valid token and proxies everything else to the
// cover, bytes already read included.
type TCPListener struct {
	ln       net.Listener
	cfg      TCPListenerConfig
	verifier *Verifier
	replay   *Replay
	tlsCfg   *tls.Config
	shape    *shape.Profile

	slots chan struct{} // pre-token slots
	cover chan struct{} // proxied connections to the cover
	ready chan mux.Carrier
	done  chan struct{}

	mu      sync.Mutex
	pending map[net.Conn]struct{} // connections not yet decided or not yet handshaken
	perAddr map[netip.Addr]int
	wg      sync.WaitGroup
	once    sync.Once
	err     error

	verifyHook func() // test hook: runs just before the token check
}

var _ carrier.Listener = (*TCPListener)(nil)

// ErrTCPCover reports a TCP listener without a cover to proxy strangers to.
var ErrTCPCover = errors.New("reality2: the TCP cover address and profile are required")

// ListenTCP serves reality2-tcp on ln. The listener owns ln and closes it.
func ListenTCP(ln net.Listener, cfg TCPListenerConfig) (*TCPListener, error) {
	if cfg.CoverAddr == "" || (cfg.ServerTLS == nil && (cfg.Cover.Group == 0 || cfg.Cover.Cert == nil)) {
		return nil, ErrTCPCover
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FirstRecordTimeout <= 0 {
		cfg.FirstRecordTimeout = FirstRecordTimeout
	}
	if cfg.MaxPreToken <= 0 {
		cfg.MaxPreToken = DefaultMaxPreToken
	}
	if cfg.MaxPerAddr == 0 {
		cfg.MaxPerAddr = DefaultMaxPerAddr
	}
	if cfg.MaxCover <= 0 {
		cfg.MaxCover = defaultMaxCover
	}
	if cfg.DialCover == nil {
		cfg.DialCover = (&net.Dialer{Timeout: coverDialTimeout}).DialContext
	}
	prof := cfg.Shape
	if prof == nil {
		p, err := shape.Builtin(shape.IDDefaultTLS)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	replay := cfg.Replay
	if replay == nil {
		replay = NewReplay(ReplayConfig{Now: cfg.Now, OnError: cfg.OnError})
	}
	v, err := NewVerifier(cfg.Masks, cfg.Addrs, replay, cfg.Now)
	if err != nil {
		return nil, err
	}
	replay.acquire()
	tlsCfg := cfg.ServerTLS
	if tlsCfg == nil {
		tlsCfg = TCPServerTLS(cfg.Cover)
	}
	l := &TCPListener{
		ln: ln, cfg: cfg, verifier: v, replay: replay, tlsCfg: tlsCfg, shape: prof,
		slots:   make(chan struct{}, cfg.MaxPreToken),
		cover:   make(chan struct{}, cfg.MaxCover),
		ready:   make(chan mux.Carrier, acceptBacklog),
		done:    make(chan struct{}),
		pending: map[net.Conn]struct{}{},
		perAddr: map[netip.Addr]int{},
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

// Accept returns the next admitted connection as a carrier.
func (l *TCPListener) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Addr is the address the listener bound.
func (l *TCPListener) Addr() net.Addr { return l.ln.Addr() }

// Close stops the listener, every undecided connection and every proxy. Carriers already handed
// out stay open. It is idempotent.
func (l *TCPListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.err = l.ln.Close()
		l.mu.Lock()
		for c := range l.pending {
			_ = c.Close()
		}
		l.mu.Unlock()
		l.wg.Wait()
		l.replay.release()
		for {
			select {
			case c := <-l.ready:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return l.err
}

// track registers a connection that Close must end; false means the listener is closed.
func (l *TCPListener) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.done:
		return false
	default:
	}
	l.pending[c] = struct{}{}
	return true
}

func (l *TCPListener) untrack(c net.Conn) {
	l.mu.Lock()
	delete(l.pending, c)
	l.mu.Unlock()
}

func (l *TCPListener) acceptLoop() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient accept error (descriptor exhaustion, a timeout): back off and go on.
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-l.done:
				return
			}
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(c)
		}()
	}
}

// handle decides one connection. The pre-token slot is held only while the first record is read.
func (l *TCPListener) handle(c net.Conn) {
	if !l.track(c) {
		_ = c.Close()
		return
	}
	defer l.untrack(c)
	select {
	case l.slots <- struct{}{}:
	default:
		// Over the limit: straight to the cover, unread, never reset (wire.md §4.2).
		l.toCover(c, nil)
		return
	}
	buf, hello := readFirstRecord(c, l.cfg.FirstRecordTimeout)
	<-l.slots // released as soon as the token is checked, whatever the answer
	if hello != nil && l.verifier.Verify(hello) {
		l.admit(&prefixConn{Conn: c, pre: buf})
		return
	}
	l.toCover(c, buf)
}

// admit completes TLS 1.3 on a connection with a valid token and hands out its carrier. The
// per-address limit applies here and only here (wire.md §4.2): it counts the carrier's whole life.
func (l *TCPListener) admit(c net.Conn) {
	release := l.reserve(c)
	if release == nil {
		_ = c.Close()
		return
	}
	tc := tls.Server(c, l.tlsCfg)
	stop := settle(c, handshakeTimeout)
	if err := tc.Handshake(); err != nil {
		release()
		_ = c.Close()
		return
	}
	stop()
	st := tc.ConnectionState()
	cb, err := channelBinding(st.ExportKeyingMaterial)
	if err != nil || st.Version != tls.VersionTLS13 {
		release()
		_ = c.Close()
		return
	}
	mc := newTCPCarrier(tc, false, cb, !isPostQuantumGroup(uint16(st.CurveID)), l.shape)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		select {
		case <-mc.Done():
		case <-l.done:
		}
		release()
	}()
	select {
	case l.ready <- mc:
	default:
		_ = mc.Close() // nobody accepts: refuse rather than queue without bound
	}
}

// reserve counts one admitted connection of c's source address (an IPv6 /64 is one subscriber).
// It returns the function that gives the slot back, or nil when the address is at its limit.
func (l *TCPListener) reserve(c net.Conn) func() {
	max := l.cfg.MaxPerAddr
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if max < 0 || !ok {
		return func() {}
	}
	ip := ta.AddrPort().Addr().Unmap()
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perAddr[ip] >= max {
		return nil
	}
	l.perAddr[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.perAddr[ip]--; l.perAddr[ip] <= 0 {
				delete(l.perAddr, ip)
			}
			l.mu.Unlock()
		})
	}
}

// toCover proxies c to the cover, with the bytes already read in front. It never resets a
// stranger: a cover that cannot be reached closes the connection like a site that is down.
func (l *TCPListener) toCover(c net.Conn, pre []byte) {
	select {
	case l.cover <- struct{}{}:
		defer func() { <-l.cover }()
	default:
		_ = c.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), coverDialTimeout)
	defer cancel()
	up, err := l.cfg.DialCover(ctx, "tcp", l.cfg.CoverAddr)
	if err != nil {
		_ = c.Close()
		return
	}
	if len(pre) > 0 {
		if _, err := up.Write(pre); err != nil {
			_ = up.Close()
			_ = c.Close()
			return
		}
	}
	proxy(c, up)
}

// proxy copies both ways until either side ends; each read has an idle limit so that a stalled
// stranger cannot hold a cover connection for ever.
func proxy(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, proxyBufSize)
		for {
			_ = src.SetReadDeadline(time.Now().Add(coverIdleTimeout))
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					err = werr
				}
			}
			if err != nil {
				if cw, ok := dst.(interface{ CloseWrite() error }); ok && errors.Is(err, io.EOF) {
					_ = cw.CloseWrite()
				} else {
					_ = dst.Close()
					_ = src.Close()
				}
				return
			}
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// readFirstRecord reads up to the first TLS record and returns every byte read plus the
// ClientHello when the record holds a complete one. Any shortfall (timeout, EOF, a record that is
// not a handshake) returns what was read and a nil hello: the connection is a stranger's.
func readFirstRecord(c net.Conn, timeout time.Duration) (buf []byte, hello *Hello) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})
	var hdr [5]byte
	n, err := io.ReadFull(c, hdr[:])
	buf = append(buf, hdr[:n]...)
	if err != nil || hdr[0] != 0x16 {
		return buf, nil
	}
	l := int(hdr[3])<<8 | int(hdr[4])
	if l == 0 || l > MaxFirstRecord {
		return buf, nil
	}
	body := make([]byte, l)
	n, err = io.ReadFull(c, body)
	buf = append(buf, body[:n]...)
	if err != nil || len(body) < 4 || body[0] != 1 {
		return buf, nil
	}
	msgLen := 4 + (int(body[1])<<16 | int(body[2])<<8 | int(body[3]))
	if msgLen > len(body) {
		return buf, nil // the hello spans records: not a hello of ours
	}
	h, err := ParseHelloTCP(body[:msgLen])
	if err != nil {
		return buf, nil
	}
	return buf, h
}

// prefixConn replays bytes already read before the rest of the connection.
type prefixConn struct {
	net.Conn
	pre []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(p, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// WrapTCP makes a carrier of a server-side TLS connection that has completed its handshake, for a
// caller that runs its own front (the own-cover mode of selfcover). prof nil is default-tls.
func WrapTCP(tc *tls.Conn, prof *shape.Profile) (mux.Carrier, error) {
	st := tc.ConnectionState()
	if !st.HandshakeComplete || st.Version != tls.VersionTLS13 {
		return nil, errors.New("reality2: TLS 1.3 handshake is not complete")
	}
	cb, err := channelBinding(st.ExportKeyingMaterial)
	if err != nil {
		return nil, err
	}
	if prof == nil {
		p, err := shape.Builtin(shape.IDDefaultTLS)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	return newTCPCarrier(tc, false, cb, !isPostQuantumGroup(uint16(st.CurveID)), prof), nil
}
