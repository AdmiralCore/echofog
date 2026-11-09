package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/selfcover"
	"github.com/echofog/echofog-core/internal/v2/carrier/ws2"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Reality2Listener is the own-cover listener of reality2-quic and reality2-tcp (T2.6b-2, T2.6e):
// the node serves the operator's real site and the admitted carrier connections with the very same
// certificate and TLS configuration, so an unrecognised client gets the site, never a refusal.
//
// One source serves both transports: TCP and UDP share the certificate and the replay cache, so a
// token is used once across both. A card advertises one address per transport.
type Reality2Listener struct {
	// Domain is the cover site's name and the SNI of the cards. Required.
	Domain string
	// SiteDir is the folder of the operator's site. Required: the own cover never invents one.
	SiteDir string
	// Cert is the operator's certificate (cover_cert). Nil means ACME with ACME.
	Cert *tls.Certificate
	// ACME configures the certificate when Cert is nil.
	ACME selfcover.ACMEConfig
	// CertKV keeps the ACME account and certificate across restarts. Required.
	CertKV store.KV
	// ReplayKV persists the token replay filter across restarts; nil keeps it in memory.
	ReplayKV store.KV

	// TCPAddr and UDPAddr are the bind addresses; empty disables the transport.
	TCPAddr, UDPAddr string
	// HTTPAddr enables the plain-HTTP listener for http-01 (and the redirect a normal site has).
	HTTPAddr string
	// Masks are the live masking keys (at most 8, wire.md §2.1 D10).
	Masks []reality2.MaskKey
	// Addrs are the exact host:port strings this node's cards list.
	Addrs []string
	// Shape is the padding profile of admitted TCP connections; nil is default-tls.
	Shape *shape.Profile
	// LowPower disables idle padding of the profile (platforms.md §2).
	LowPower bool
	// MaxPreToken and FirstRecordTimeout override the reality2-tcp defaults.
	MaxPreToken        int
	FirstRecordTimeout time.Duration
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnWarn receives the non-fatal findings of the cover (an empty site folder).
	OnWarn func(string)
	// OnCert receives every certificate the cover installs (a static one at start, ACME issues
	// and renewals); nil ignores them.
	OnCert func(*x509.Certificate)
	// OnError receives background errors of the cover; nil ignores them.
	OnError func(error)
}

// Listen implements ListenerSource.
func (r Reality2Listener) Listen(ctx context.Context) (carrier.Listener, error) {
	if r.Domain == "" || r.SiteDir == "" {
		return nil, fmt.Errorf("node: reality2 listener: the own cover needs a domain and a site folder")
	}
	if r.CertKV == nil {
		return nil, fmt.Errorf("node: reality2 listener: a certificate store is required")
	}
	if len(r.Masks) == 0 || len(r.Addrs) == 0 {
		return nil, fmt.Errorf("node: reality2 listener: masking keys and card addresses are required")
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	prof := r.Shape
	if prof == nil {
		p, err := shape.Builtin(shape.IDDefaultTLS)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	if r.LowPower {
		cp := *prof
		cp.IdlePad = nil
		prof = &cp
	}
	var replay *reality2.Replay
	if r.ReplayKV != nil {
		replay = reality2.NewReplay(reality2.ReplayConfig{KV: carrierKV{r.ReplayKV}, Now: now, OnError: r.OnError})
	}
	n, err := selfcover.Start(ctx, selfcover.Config{
		Domain: r.Domain, Content: selfcover.Content{Dir: r.SiteDir}, ACME: r.ACME, Cert: r.Cert,
		KV: carrierKV{r.CertKV}, Masks: r.Masks, Addrs: r.Addrs, Replay: replay,
		TCPAddr: r.TCPAddr, UDPAddr: r.UDPAddr, HTTPAddr: r.HTTPAddr,
		MaxPreToken: r.MaxPreToken, FirstRecordTimeout: r.FirstRecordTimeout,
		Now: now, OnWarn: r.OnWarn, OnError: r.OnError, OnCert: r.OnCert,
	})
	if err != nil {
		return nil, fmt.Errorf("node: reality2 listener: %w", err)
	}
	return newCoverCarrierListener(n, r.TCPAddr != "", r.UDPAddr != "", prof), nil
}

// WS2Listener is the server side of ws2 (wire.md §4.4): a plain-HTTP listener behind a CDN, with
// the operator's site on every path but the card's unguessable one. The inner reality2-tcp token
// server runs inside each WebSocket.
type WS2Listener struct {
	// Addr is the plain-HTTP bind address (the CDN terminates the outer TLS).
	Addr string
	// Path is the unguessable upgrade path of the card (it starts with /).
	Path string
	// Domain is the cover site's name used for the inner TLS certificate when Cert is nil.
	Domain string
	// SiteDir is the folder of the operator's site; Site overrides it.
	SiteDir string
	// Site answers every request that is not an upgrade on Path.
	Site http.Handler
	// Cert is the certificate of the inner reality2-tcp;
	// nil generates a self-signed one (the inner TLS is encrypted inside the WebSocket).
	Cert *tls.Certificate
	// Masks are the live masking keys.
	Masks []reality2.MaskKey
	// Addrs are the exact host:port strings ParseURL gives for this node's ws2 card URLs.
	Addrs []string
	// ReplayKV persists the token replay filter; nil keeps it in memory.
	ReplayKV store.KV
	// Shape is the padding profile; nil is default-tls.
	Shape              *shape.Profile
	LowPower           bool
	MaxPreToken        int
	MaxPerAddr         int
	FirstRecordTimeout time.Duration
	// ClientIPHeader names the CDN client-address header (Cloudflare: CF-Connecting-IP).
	ClientIPHeader string
	MaxConns       int
	// Delay is the wait before a refusal; nil is uniform(0.05, 0.5) s.
	Delay func() time.Duration
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnWarn receives the non-fatal findings of the cover (an empty site folder).
	OnWarn func(string)
}

// Listen implements ListenerSource.
func (w WS2Listener) Listen(ctx context.Context) (carrier.Listener, error) {
	if w.Addr == "" || w.Path == "" || w.Path[0] != '/' {
		return nil, fmt.Errorf("node: ws2 listener: an address and a path starting with / are required")
	}
	if len(w.Masks) == 0 || len(w.Addrs) == 0 {
		return nil, fmt.Errorf("node: ws2 listener: masking keys and card addresses are required")
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	var replay *reality2.Replay
	if w.ReplayKV != nil {
		replay = reality2.NewReplay(reality2.ReplayConfig{KV: carrierKV{w.ReplayKV}, Now: now})
	}
	cert := w.Cert
	if cert == nil {
		var err error
		if cert, err = SelfSignedCertificate(w.Domain); err != nil {
			return nil, fmt.Errorf("node: ws2 listener: %w", err)
		}
	}
	site := w.Site
	if site == nil {
		if w.SiteDir == "" {
			return nil, fmt.Errorf("node: ws2 listener: the cover site folder is required")
		}
		h, warnings, err := selfcover.Content{Dir: w.SiteDir}.Handler()
		if err != nil {
			return nil, fmt.Errorf("node: ws2 listener: %w", err)
		}
		if w.OnWarn != nil {
			for _, warning := range warnings {
				w.OnWarn(warning)
			}
		}
		site = h
	}
	prof := w.Shape
	if prof == nil {
		p, err := shape.Builtin(shape.IDDefaultTLS)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	if w.LowPower {
		cp := *prof
		cp.IdlePad = nil
		prof = &cp
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert}}
	ln, err := net.Listen("tcp", w.Addr)
	if err != nil {
		return nil, fmt.Errorf("node: ws2 listener: %w", err)
	}
	l, err := ws2.Listen(ln, ws2.Config{
		Path: w.Path, ServerTLS: tlsCfg, Masks: w.Masks, Addrs: w.Addrs, Replay: replay,
		Shape: prof, MaxPreToken: w.MaxPreToken, MaxPerAddr: w.MaxPerAddr,
		FirstRecordTimeout: w.FirstRecordTimeout, Site: site,
		ClientIPHeader: w.ClientIPHeader, MaxConns: w.MaxConns, Delay: w.Delay, Now: now,
	})
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("node: ws2 listener: %w", err)
	}
	_ = ctx
	return l, nil
}

// carrierKV adapts a store namespace to carrier.KV.
type carrierKV struct{ kv store.KV }

func (k carrierKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	v, err := k.kv.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (k carrierKV) Put(_ context.Context, key, value []byte) error { return k.kv.Put(key, value) }

// coverCarrierListener turns the own-cover node into one carrier.Listener: TCP and QUIC admitted
// connections are handed out through one channel in arrival order.
type coverCarrierListener struct {
	node  *selfcover.Node
	tcp   bool
	quic  bool
	shape *shape.Profile

	ch   chan mux.Carrier
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newCoverCarrierListener(n *selfcover.Node, tcp, quic bool, prof *shape.Profile) *coverCarrierListener {
	l := &coverCarrierListener{node: n, tcp: tcp, quic: quic, shape: prof, ch: make(chan mux.Carrier, 64), done: make(chan struct{})}
	if tcp {
		l.wg.Add(1)
		go l.pumpTCP()
	}
	if quic {
		l.wg.Add(1)
		go l.pumpQUIC()
	}
	return l
}

func (l *coverCarrierListener) pumpTCP() {
	defer l.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-l.done:
		case <-ctx.Done():
		}
		cancel()
	}()
	for {
		nc, err := l.node.AcceptTCP(ctx)
		if err != nil {
			return
		}
		tc, ok := nc.(*tls.Conn)
		if !ok {
			_ = nc.Close()
			continue
		}
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.Handshake(); err != nil {
			_ = tc.Close()
			continue
		}
		_ = tc.SetDeadline(time.Time{})
		mc, err := reality2.WrapTCP(tc, l.shape)
		if err != nil {
			_ = tc.Close()
			continue
		}
		select {
		case l.ch <- mc:
		case <-l.done:
			_ = mc.Close()
			return
		}
	}
}

func (l *coverCarrierListener) pumpQUIC() {
	defer l.wg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-l.done:
		case <-ctx.Done():
		}
		cancel()
	}()
	for {
		mc, err := l.node.AcceptQUIC(ctx)
		if err != nil {
			return
		}
		select {
		case l.ch <- mc:
		case <-l.done:
			_ = mc.Close()
			return
		}
	}
}

func (l *coverCarrierListener) Accept(ctx context.Context) (mux.Carrier, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *coverCarrierListener) Addr() net.Addr {
	if l.tcp {
		return l.node.TCPAddr()
	}
	return l.node.UDPAddr()
}

func (l *coverCarrierListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		_ = l.node.Close()
		l.wg.Wait()
		for {
			select {
			case c := <-l.ch:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

// SelfSignedCertificate makes an ephemeral certificate for the inner TLS of ws2, which is carried
// inside the WebSocket and is not visible to a network observer.
func SelfSignedCertificate(domain string) (*tls.Certificate, error) {
	if domain == "" {
		domain = "localhost"
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		DNSNames:     []string{domain},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
