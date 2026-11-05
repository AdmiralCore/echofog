package ws2

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

const upgradeTimeout = 15 * time.Second

// DialerConfig configures a Dialer.
type DialerConfig struct {
	// Hello is the browser ClientHello of the outer TLS; the zero value is the current stable
	// Chrome. There is no way to use the Go standard TLS stack for the outer connection.
	Hello utls.ClientHelloID
	// RootCAs are the CAs the CDN's certificate is checked against; nil is the system's.
	RootCAs *x509.CertPool
	// UserAgent is sent in the upgrade request; empty is a current Chrome's.
	UserAgent string
	// Now is the injectable clock of the inner token; nil means time.Now.
	Now func() time.Time
}

// Dialer dials ws2 addresses of a verified card (wire.md §4.4).
type Dialer struct {
	cfg   DialerConfig
	inner *reality2.TCPDialer
}

var _ carrier.Dialer = (*Dialer)(nil)

// NewDialer builds the suite's dialer.
func NewDialer(cfg DialerConfig) *Dialer {
	if cfg.Hello == (utls.ClientHelloID{}) {
		cfg.Hello = utls.HelloChrome_Auto
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	}
	return &Dialer{cfg: cfg, inner: reality2.NewTCPDialer(reality2.TCPDialerConfig{Now: cfg.Now})}
}

// Dial opens the outer TLS to the CDN (uTLS), upgrades to a WebSocket on the card's path and runs
// reality2-tcp, token included, inside it. The proxy hook of the request opens the TCP socket.
func (d *Dialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	a := req.Addr
	if a == nil || a.GetSuite() != objects.SuiteWS2 {
		return nil, fmt.Errorf("%w: address is not ws2", reality2.ErrCard)
	}
	if req.Card == nil {
		return nil, fmt.Errorf("%w: no card", reality2.ErrCard)
	}
	hostPort, _, err := ParseURL(a.GetUrl())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", reality2.ErrCard, err)
	}
	u, _ := url.Parse(a.GetUrl()) // ParseURL accepted it
	wd := websocket.Dialer{
		HandshakeTimeout:  upgradeTimeout,
		ReadBufferSize:    MaxMessage,
		WriteBufferSize:   MaxMessage,
		NetDialContext:    d.dialTCP(req),
		NetDialTLSContext: d.dialTLS(req),
	}
	hdr := http.Header{}
	hdr.Set("User-Agent", d.cfg.UserAgent)
	hdr.Set("Origin", "https://"+u.Host)
	hdr.Set("Accept-Language", "en-US,en;q=0.9")
	ws, resp, err := wd.DialContext(ctx, a.GetUrl(), hdr)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("ws2: upgrade at %s: %w", hostPort, err)
	}
	return d.inner.DialOver(ctx, newConn(ws, nil, nil), req.Card, a, hostPort)
}

func (d *Dialer) dialTCP(req carrier.DialRequest) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if req.DialContext != nil {
		return req.DialContext
	}
	var nd net.Dialer
	return nd.DialContext
}

// dialTLS opens the outer HTTPS of the wss:// link the way a browser opens a WebSocket: Chrome's
// ClientHello, offering only http/1.1 (the upgrade needs it anyway); the CDN's certificate is
// checked normally.
func (d *Dialer) dialTLS(req carrier.DialRequest) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		spec, err := utls.UTLSIdToSpec(d.cfg.Hello)
		if err != nil {
			return nil, err
		}
		for _, ext := range spec.Extensions {
			if alpn, ok := ext.(*utls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
			}
		}
		raw, err := d.dialTCP(req)(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		uc := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: d.cfg.RootCAs}, utls.HelloCustom)
		if err := uc.ApplyPreset(&spec); err != nil {
			_ = raw.Close()
			return nil, err
		}
		if err := uc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if !stop() {
			_ = raw.Close()
			return nil, errors.New("ws2: context ended during the outer handshake")
		}
		return uc, nil
	}
}
