package selfcover

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	quic "github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Config is the `cover: self` mode of a reality2 listener (T2.6b-2, wire.md §3.4 with the cover
// being the operator's own site). One process serves the website and the admitted connections
// with the same certificate and the same TLS configuration, so a stranger, a prober and a
// client see one server: the profile of the cover matches by construction, with no padding.
type Config struct {
	// Domain is the site's name: the operator's own domain or a subdomain of a DDNS provider.
	// It must resolve to this node; it is the SNI of the cards.
	Domain string
	// Content is the website. There is no default site.
	Content Content
	// ACME configures the certificate.
	ACME ACMEConfig
	// Cert is the operator's own certificate for Domain (cover_cert): when set, ACME is not
	// used and the certificate is renewed by the operator, not by the node.
	Cert *tls.Certificate
	// KV stores the certificate, the ACME account and the measured profiles (T4.3).
	KV carrier.KV

	// Group is the key exchange group the website selects: zero means X25519MLKEM768 (the Go
	// default), anything else restricts the site to that group and the paths run sealed (§8.5).
	Group tls.CurveID

	// Masks, Addrs and Replay are those of reality2.ListenerConfig. Replay is shared by the QUIC
	// gate and the TCP front, so a token is used once across both.
	Masks  []reality2.MaskKey
	Addrs  []string
	Replay *reality2.Replay

	// TCPAddr and UDPAddr are the listening addresses (443 recommended); empty disables the
	// transport. HTTPAddr is the plain-HTTP listener for http-01 (and the redirect a normal site
	// has); empty disables it, which leaves tls-alpn-01 only.
	TCPAddr, UDPAddr, HTTPAddr string
	// MaxPreToken overrides DefaultMaxPreToken.
	MaxPreToken int
	// FirstRecordTimeout overrides the 10 s wait for the first TLS record.
	FirstRecordTimeout time.Duration

	// Samples is the number of connections the own check measures the website with; zero means 20.
	// The selected side always uses 5, as the rule of T2.6b-1 says.
	Samples int
	// Settle is the quiet time of a check sample; zero means 100 ms.
	Settle time.Duration

	// Now and Sleep are the injectable clock of the renewal and re-measurement loops.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Interval is the time to the next re-measurement; nil means uniform(5, 7) hours.
	Interval func() time.Duration

	// OnWarn receives non-fatal findings, for example an empty website folder.
	OnWarn func(string)
	// OnReport receives every re-measurement. A report that is not OK means the own server
	// stopped matching its website: the suite must not stay enabled on this card.
	OnReport func(*Report)
	// OnError receives background errors; nil ignores them.
	OnError func(error)
}

// Node is a running own-cover listener.
type Node struct {
	cfg    Config
	certs  *Certs
	group  tls.CurveID
	minter *minter

	tcpTLS, quicTLS *tls.Config

	front      *front
	siteQ      *connQueue
	siteSrv    *http.Server
	httpSrv    *http.Server
	h3         *http3.Server
	h3Conn     net.PacketConn
	ql         *reality2.Listener
	tcpAddr    net.Addr
	udpAddr    net.Addr
	warnings   []string
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once
	changed    chan struct{}
	done       chan struct{}
	ownMu      sync.Mutex
	own        map[string]struct{}
	quicCh     chan *quic.Conn
	tcpCh      chan net.Conn
	remeasures sync.Mutex
}

// quietLog keeps the website servers from printing handshake errors of strangers: a node's
// output is not a place for other people's garbage.
var quietLog = log.New(io.Discard, "", 0)

// ErrNoTransport reports a Config with neither TCP nor UDP address.
var ErrNoTransport = errors.New("selfcover: neither a TCP nor a UDP address is configured")

// Start brings the listener up and waits for the first certificate. The CA connects to the
// node during that wait, so Domain must already point at it and the validation port must be
// reachable. A node without a certificate does not start: it would answer strangers with an error
// a real site never gives.
func Start(ctx context.Context, cfg Config) (*Node, error) {
	if cfg.TCPAddr == "" && cfg.UDPAddr == "" {
		return nil, ErrNoTransport
	}
	handler, warnings, err := cfg.Content.handler()
	if err != nil {
		return nil, err
	}
	if len(cfg.Masks) == 0 || len(cfg.Addrs) == 0 {
		return nil, errors.New("selfcover: masking keys and card addresses are required")
	}
	if cfg.KV == nil {
		return nil, errors.New("selfcover: a key-value store is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Samples <= 0 {
		cfg.Samples = 20
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 100 * time.Millisecond
	}
	if cfg.Interval == nil {
		cfg.Interval = uniformInterval
	}
	if cfg.Replay == nil {
		cfg.Replay = reality2.NewReplay(reality2.ReplayConfig{KV: cfg.KV, Now: cfg.Now, OnError: cfg.OnError})
	}
	n := &Node{cfg: cfg, warnings: warnings, changed: make(chan struct{}, 1), done: make(chan struct{}), own: map[string]struct{}{}, quicCh: make(chan *quic.Conn, 64), tcpCh: make(chan net.Conn, 64)}
	for _, w := range warnings {
		if cfg.OnWarn != nil {
			cfg.OnWarn(w)
		}
	}
	n.certs, err = NewCerts(CertsConfig{
		Domain: cfg.Domain, ACME: cfg.ACME, Static: cfg.Cert, KV: cfg.KV, Now: cfg.Now, Sleep: cfg.Sleep, OnError: cfg.OnError,
		OnChange: func(*x509.Certificate) {
			select {
			case n.changed <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		return nil, err
	}
	n.group = cfg.Group
	if n.group == 0 {
		n.group = tls.X25519MLKEM768
	}
	var curves []tls.CurveID
	if cfg.Group != 0 && cfg.Group != tls.X25519MLKEM768 {
		curves = []tls.CurveID{cfg.Group}
	}
	n.tcpTLS = &tls.Config{
		GetCertificate:   n.certs.GetCertificate,
		MinVersion:       tls.VersionTLS12,
		CurvePreferences: curves,
		NextProtos:       []string{"h2", "http/1.1", "acme-tls/1"},
	}
	n.quicTLS = &tls.Config{
		GetCertificate:   n.certs.GetCertificate,
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: curves,
		NextProtos:       []string{"h3"},
	}
	n.minter = &minter{maskPub: cfg.Masks[0].Private.PublicKey().Bytes(), addr: cfg.Addrs[0], now: cfg.Now}

	runCtx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	site := withAltSvc(handler, cfg.UDPAddr != "", cfg.Addrs[0])

	if err := n.startTCP(site); err != nil {
		n.Close()
		return nil, err
	}
	if err := n.startHTTP(); err != nil {
		n.Close()
		return nil, err
	}
	if err := n.startQUIC(site); err != nil {
		n.Close()
		return nil, err
	}
	if err := n.certs.Ensure(ctx); err != nil {
		n.Close()
		return nil, fmt.Errorf("selfcover: obtaining the certificate for %s: %w", n.certs.Domain(), err)
	}
	n.goRun(runCtx, n.certs.Run)
	n.goRun(runCtx, n.remeasureLoop)
	return n, nil
}

func (n *Node) goRun(ctx context.Context, f func(context.Context)) {
	n.wg.Add(1)
	go func() { defer n.wg.Done(); f(ctx) }()
}

func (n *Node) report(err error) {
	if n.cfg.OnError != nil && err != nil {
		n.cfg.OnError(err)
	}
}

// withAltSvc adds the Alt-Svc header a site with HTTP/3 sends on its TCP responses.
func withAltSvc(h http.Handler, h3 bool, cardAddr string) http.Handler {
	if !h3 {
		return h
	}
	_, port, err := net.SplitHostPort(cardAddr)
	if err != nil {
		return h
	}
	v := `h3=":` + port + `"; ma=86400`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", v)
		h.ServeHTTP(w, r)
	})
}

func (n *Node) startTCP(site http.Handler) error {
	if n.cfg.TCPAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", n.cfg.TCPAddr)
	if err != nil {
		return fmt.Errorf("selfcover: TCP listen: %w", err)
	}
	n.tcpAddr = ln.Addr()
	v, err := reality2.NewVerifier(n.cfg.Masks, n.cfg.Addrs, n.cfg.Replay, n.cfg.Now)
	if err != nil {
		_ = ln.Close()
		return err
	}
	n.siteQ = newConnQueue(ln.Addr())
	n.siteSrv = &http.Server{Handler: site, TLSConfig: n.tcpTLS, ReadHeaderTimeout: 30 * time.Second, ErrorLog: quietLog}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = n.siteSrv.ServeTLS(n.siteQ, "", "")
	}()
	n.front = newFront(ln, v, n.tcpTLS, n.siteQ, n.cfg.MaxPreToken, n.cfg.FirstRecordTimeout)
	n.wg.Add(1)
	go n.dispatchTCP()
	return nil
}

func (n *Node) startHTTP() error {
	if n.cfg.HTTPAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", n.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("selfcover: HTTP listen: %w", err)
	}
	domain := n.certs.Domain()
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	n.httpSrv = &http.Server{Handler: n.certs.HTTPHandler(redirect), ReadHeaderTimeout: 30 * time.Second, ErrorLog: quietLog}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); _ = n.httpSrv.Serve(ln) }()
	return nil
}

// quicConfig is the one QUIC configuration of the HTTP/3 website and of the admitted connections:
// the transport parameters in the server's handshake are the same, so their sizes are too.
func quicConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, MaxIncomingStreams: 256}
}

func (n *Node) startQUIC(site http.Handler) error {
	if n.cfg.UDPAddr == "" {
		return nil
	}
	// The website's HTTP/3 server listens on loopback; the gate forwards strangers to it.
	h3Conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return fmt.Errorf("selfcover: HTTP/3 site socket: %w", err)
	}
	n.h3Conn = h3Conn
	n.h3 = &http3.Server{Handler: site, TLSConfig: n.quicTLS, QUICConfig: quicConfig()}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); _ = n.h3.Serve(h3Conn) }()

	udp, err := net.ListenPacket("udp", n.cfg.UDPAddr)
	if err != nil {
		return fmt.Errorf("selfcover: UDP listen: %w", err)
	}
	n.udpAddr = udp.LocalAddr()
	n.ql, err = reality2.Listen(udp, reality2.ListenerConfig{
		Cover:      reality2.CoverProfile{Group: n.group},
		ServerTLS:  n.quicTLS,
		QUICConfig: quicConfig(),
		Masks:      n.cfg.Masks,
		Addrs:      n.cfg.Addrs,
		Replay:     n.cfg.Replay,
		CoverAddr:  h3Conn.LocalAddr().(*net.UDPAddr),
		Now:        n.cfg.Now,
		OnError:    n.cfg.OnError,
	})
	if err != nil {
		_ = udp.Close()
		return err
	}
	n.wg.Add(1)
	go n.acceptQUIC()
	return nil
}

func (n *Node) acceptQUIC() {
	defer n.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-n.done; cancel() }()
	for {
		c, err := n.ql.AcceptConn(ctx)
		if err != nil {
			return
		}
		if n.isOwnCheck(c.RemoteAddr()) {
			// The node's own check: hold the connection until the check ends.
			go func() { <-c.Context().Done() }()
			continue
		}
		select {
		case n.quicCh <- c:
		case <-ctx.Done():
			return
		}
	}
}

// isOwnCheck reports a connection whose source is a socket of the node's own re-measurement:
// those connections are the node's probes and are not handed to the caller.
func (n *Node) isOwnCheck(a net.Addr) bool {
	n.ownMu.Lock()
	defer n.ownMu.Unlock()
	_, ok := n.own[a.String()]
	return ok
}

// registerOwn records the source address of a check connection.
func (n *Node) registerOwn(a net.Addr) {
	n.ownMu.Lock()
	n.own[a.String()] = struct{}{}
	n.ownMu.Unlock()
}

func (n *Node) clearOwn() {
	n.ownMu.Lock()
	clear(n.own)
	n.ownMu.Unlock()
}

// dispatchTCP sorts the admitted TCP connections: the node's own check connections are served
// here, so a check does not depend on the caller's accept loop, and the rest go to AcceptTCP.
func (n *Node) dispatchTCP() {
	defer n.wg.Done()
	for {
		c, err := n.front.accept(n.done)
		if err != nil {
			return
		}
		if n.isOwnCheck(c.RemoteAddr()) {
			go drain(c)
			continue
		}
		select {
		case n.tcpCh <- c:
		case <-n.done:
			_ = c.Close()
			return
		}
	}
}

// AcceptTCP returns the next admitted TCP connection: a tls.Conn of the website's configuration
// whose handshake has not run yet.
func (n *Node) AcceptTCP(ctx context.Context) (net.Conn, error) {
	if n.front == nil {
		return nil, errors.New("selfcover: TCP is not enabled")
	}
	select {
	case c := <-n.tcpCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.done:
		return nil, net.ErrClosed
	}
}

func drain(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*tls.Conn); ok && tc.Handshake() != nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		if _, err := c.Read(buf); err != nil {
			return
		}
	}
}

// AcceptQUIC returns the next admitted QUIC connection as a carrier.
func (n *Node) AcceptQUIC(ctx context.Context) (mux.Carrier, error) {
	if n.ql == nil {
		return nil, errors.New("selfcover: QUIC is not enabled")
	}
	select {
	case c := <-n.quicCh:
		return reality2.Wrap(c), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.done:
		return nil, net.ErrClosed
	}
}

// TCPAddr and UDPAddr are the bound listening addresses (nil when disabled).
func (n *Node) TCPAddr() net.Addr { return n.tcpAddr }
func (n *Node) UDPAddr() net.Addr { return n.udpAddr }

// Group is the key exchange group of the website. A classical group means sealed paths.
func (n *Node) Group() tls.CurveID { return n.group }

// Sealed reports that paths on this node run in sealed mode (wire.md §8.5).
func (n *Node) Sealed() bool { return n.group != tls.X25519MLKEM768 }

// Warnings lists the non-fatal findings of Start.
func (n *Node) Warnings() []string { return append([]string(nil), n.warnings...) }

// Certs is the certificate manager, for status screens and tests.
func (n *Node) Certs() *Certs { return n.certs }

// Close stops every listener and goroutine. It is idempotent.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		close(n.done)
		if n.cancel != nil {
			n.cancel()
		}
		if n.front != nil {
			n.front.close()
		}
		if n.siteSrv != nil {
			_ = n.siteSrv.Close()
		}
		if n.httpSrv != nil {
			_ = n.httpSrv.Close()
		}
		if n.ql != nil {
			_ = n.ql.Close()
		}
		if n.h3 != nil {
			_ = n.h3.Close()
		}
		if n.h3Conn != nil {
			_ = n.h3Conn.Close()
		}
		n.wg.Wait()
	})
	return nil
}

// loopbackAddr turns a listening address into one a local check can dial.
func loopbackAddr(a net.Addr) string {
	_, port, _ := net.SplitHostPort(a.String())
	if p, err := strconv.Atoi(port); err == nil {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	}
	return a.String()
}
