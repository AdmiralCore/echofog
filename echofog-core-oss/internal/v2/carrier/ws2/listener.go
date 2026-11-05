package ws2

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

const (
	// DefaultMaxConns is the number of WebSockets the listener holds at once; the rest get the
	// ordinary site.
	DefaultMaxConns = 4096

	refuseMin = 50 * time.Millisecond
	refuseMax = 500 * time.Millisecond
)

// Config configures a Listener.
type Config struct {
	// Path is the exact request path that upgrades to a WebSocket: it carries the unguessable
	// component of the card's URL. Required.
	Path string
	// Cover, ServerTLS, Masks, Addrs, Replay, Shape, MaxPreToken, MaxPerAddr and
	// FirstRecordTimeout configure the inner reality2-tcp side (see reality2.TCPListenerConfig).
	// Addrs are the host:port strings ParseURL gives for this node's ws2 card URLs.
	Cover              reality2.TCPCover
	ServerTLS          *tls.Config
	Masks              []reality2.MaskKey
	Addrs              []string
	Replay             *reality2.Replay
	Shape              *shape.Profile
	MaxPreToken        int
	MaxPerAddr         int
	FirstRecordTimeout time.Duration
	// Site answers every request that is not an upgrade on Path; nil is a bare 404 page.
	Site http.Handler
	// ClientIPHeader names the header a CDN puts the client's address in (Cloudflare:
	// CF-Connecting-IP); per-address limits count that address. Set it only when the node is
	// reachable through the CDN alone. Empty uses the connection's own source address.
	ClientIPHeader string
	// MaxConns bounds the open WebSockets; zero is DefaultMaxConns.
	MaxConns int
	// Delay is the wait before a refusal; nil is uniform(0.05, 0.5) s (wire.md §4.4).
	Delay func() time.Duration
	// Now is the injectable clock of the inner token; nil means time.Now.
	Now func() time.Time
}

// Listener is the server side of ws2. It is an http.Handler (mount it behind the node's own web
// server) and a carrier.Listener.
type Listener struct {
	cfg   Config
	inner *reality2.TCPListener
	feed  *feedListener
	site  http.Handler
	up    websocket.Upgrader
	open  atomic.Int64

	srv  *http.Server // set by Listen
	addr net.Addr
	done chan struct{}
	once sync.Once
}

var (
	_ carrier.Listener = (*Listener)(nil)
	_ http.Handler     = (*Listener)(nil)
)

// New makes a Listener that serves through its ServeHTTP.
func New(cfg Config) (*Listener, error) {
	if cfg.Path == "" || cfg.Path[0] != '/' {
		return nil, errors.New("ws2: the path must start with /")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	if cfg.Delay == nil {
		cfg.Delay = uniformDelay
	}
	l := &Listener{cfg: cfg, feed: newFeedListener(), done: make(chan struct{})}
	l.site = cfg.Site
	if l.site == nil {
		l.site = http.NotFoundHandler()
	}
	l.up = websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   MaxMessage,
		WriteBufferSize:  MaxMessage,
		CheckOrigin:      func(*http.Request) bool { return true },
		// A malformed upgrade is answered by the site, not by a WebSocket error page.
		Error: func(w http.ResponseWriter, r *http.Request, _ int, _ error) { l.site.ServeHTTP(w, r) },
	}
	inner, err := reality2.ListenTCP(l.feed, reality2.TCPListenerConfig{
		Cover: cfg.Cover, ServerTLS: cfg.ServerTLS, Masks: cfg.Masks, Addrs: cfg.Addrs, Replay: cfg.Replay,
		Shape: cfg.Shape, MaxPreToken: cfg.MaxPreToken, MaxPerAddr: cfg.MaxPerAddr,
		FirstRecordTimeout: cfg.FirstRecordTimeout, Now: cfg.Now,
		Stranger: l.refuse,
	})
	if err != nil {
		return nil, err
	}
	l.inner = inner
	return l, nil
}

// Listen serves the Listener over plain HTTP on ln (the CDN terminates the outer TLS). The
// listener owns ln.
func Listen(ln net.Listener, cfg Config) (*Listener, error) {
	l, err := New(cfg)
	if err != nil {
		return nil, err
	}
	l.addr = ln.Addr()
	l.srv = &http.Server{Handler: l, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// ServeHTTP upgrades a WebSocket request on the secret path and gives everything else to the site.
func (l *Listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != l.cfg.Path || r.Method != http.MethodGet || !websocket.IsWebSocketUpgrade(r) ||
		l.open.Load() >= int64(l.cfg.MaxConns) {
		l.site.ServeHTTP(w, r)
		return
	}
	ws, err := l.up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already answered
	}
	l.open.Add(1)
	c := newConn(ws, l.clientAddr(r), func() { l.open.Add(-1) })
	if !l.feed.push(c) {
		_ = c.Close()
	}
}

// clientAddr is the address per-address limits count: the CDN's client header when configured and
// valid, else the connection's source.
func (l *Listener) clientAddr(r *http.Request) net.Addr {
	var ip netip.Addr
	if h := l.cfg.ClientIPHeader; h != "" {
		v, _, _ := strings.Cut(r.Header.Get(h), ",") // X-Forwarded-For: client, proxy1, ...
		if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
			ip = a
		}
	}
	if !ip.IsValid() {
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			ip = ap.Addr()
		}
	}
	return &net.TCPAddr{IP: ip.Unmap().AsSlice()}
}

// refuse is the one refusal of every connection without a valid token (wire.md §4.4, AD20): the
// same wait, the same close code, whatever the reason.
func (l *Listener) refuse(nc net.Conn, _ []byte) {
	c, ok := nc.(interface{ refuse() })
	if !ok {
		_ = nc.Close()
		return
	}
	t := time.NewTimer(l.cfg.Delay())
	defer t.Stop()
	select {
	case <-t.C:
	case <-l.done:
	}
	c.refuse()
}

// Accept returns the next admitted connection as a carrier.
func (l *Listener) Accept(ctx context.Context) (mux.Carrier, error) { return l.inner.Accept(ctx) }

// Addr is the address Listen bound, or nil for a Listener mounted with New.
func (l *Listener) Addr() net.Addr { return l.addr }

// Close stops the listener; carriers already handed out stay open. It is idempotent.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		if l.srv != nil {
			err = l.srv.Close()
		}
		l.feed.Close()
		_ = l.inner.Close()
	})
	return err
}

// uniformDelay is uniform(0.05, 0.5) s.
func uniformDelay() time.Duration {
	span := big.NewInt(int64(refuseMax - refuseMin))
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return refuseMax
	}
	return refuseMin + time.Duration(n.Int64())
}

// feedListener is the net.Listener the inner reality2-tcp listener accepts the WebSockets from.
type feedListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newFeedListener() *feedListener {
	return &feedListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

// push hands c to the inner listener; false means the listener is closed.
func (f *feedListener) push(c net.Conn) bool {
	select {
	case f.ch <- c:
		return true
	case <-f.done:
		return false
	}
}

func (f *feedListener) Accept() (net.Conn, error) {
	select {
	case c := <-f.ch:
		return c, nil
	case <-f.done:
		return nil, net.ErrClosed
	}
}

func (f *feedListener) Close() error { f.once.Do(func() { close(f.done) }); return nil }

func (f *feedListener) Addr() net.Addr { return &net.TCPAddr{} }
