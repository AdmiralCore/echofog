package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Egress (T11.6a): all of the node's own traffic through a SOCKS5 proxy, a plain one or Tor. The mode
// is a closed door, not a preference: the node opens no listener, announces no address, punches no
// hole, maps no port, uses no UDP carrier (QUIC and noise3 are refused) and asks no STUN server;
// names are resolved by the proxy; DHT, PEX and relay circuits ride the same sessions as everything
// else, so they go through the proxy too; Host.HTTP() is a proxied client. When the proxy does not
// hold, nothing connects: there is no fallback to a direct socket.
//
// Calls go through a relay in this mode: a direct media path is UDP to the peer and would show the
// address, so call_route direct and auto both mean relay.
type Egress struct {
	// Proxy is the SOCKS5 proxy, host:port (a socks5:// prefix is accepted).
	Proxy string
	// Tor says the proxy is Tor (anonymous = tor); it only changes what the status reports.
	Tor bool
	// RootCAs, when set, replaces the system roots for Host.HTTP() (tests).
	RootCAs *x509.CertPool
}

// EgressStatus is what runs.
type EgressStatus struct {
	Active bool
	Tor    bool
	Proxy  string
}

var (
	// ErrEgressConfig reports a configuration that would show the address behind a proxy.
	ErrEgressConfig = errors.New("node: this configuration shows the address behind a proxy")
	// ErrEgressNetwork reports a request for a non-TCP socket behind a proxy.
	ErrEgressNetwork = errors.New("node: only TCP goes through a proxy")
)

// proxyAddr normalizes the proxy address to host:port.
func proxyAddr(s string) (string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "socks5h://"), "socks5://"))
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return "", fmt.Errorf("%w: proxy %q is not host:port", ErrEgressConfig, s)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%w: proxy port %q", ErrEgressConfig, port)
	}
	return net.JoinHostPort(host, port), nil
}

// check validates the node configuration against the mode.
func (e *Egress) check(cfg *Config) error {
	addr, err := proxyAddr(e.Proxy)
	if err != nil {
		return err
	}
	e.Proxy = addr
	switch {
	case len(cfg.Listeners)+len(cfg.ListenerSources)+len(cfg.ContactListeners)+len(cfg.ContactListenerSources) > 0:
		return fmt.Errorf("%w: a listener", ErrEgressConfig)
	case cfg.PunchNet != nil:
		return fmt.Errorf("%w: hole punching", ErrEgressConfig)
	case cfg.DialContext != nil:
		return fmt.Errorf("%w: the node has its own socket opener", ErrEgressConfig)
	}
	return nil
}

// dialer returns the opener of every TCP socket of the node: SOCKS5 over one TCP connection to the
// proxy, names sent unresolved. Anything but TCP is refused.
func (e *Egress) dialer() (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	d, err := proxy.SOCKS5("tcp", e.Proxy, nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEgressConfig, err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("%w: the SOCKS5 dialer has no context", ErrEgressConfig)
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(network, "tcp") {
			return nil, ErrEgressNetwork
		}
		return cd.DialContext(ctx, "tcp", addr)
	}, nil
}

// httpClient is Host.HTTP() behind the proxy: the TLS of a browser (uTLS, http/1.1 only), the
// certificate checked, no environment proxy, names resolved by the proxy.
func (e *Egress) httpClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{
		Proxy:       nil,
		DialContext: dial,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
			if err != nil {
				return nil, err
			}
			for _, ext := range spec.Extensions {
				if alpn, ok := ext.(*utls.ALPNExtension); ok {
					alpn.AlpnProtocols = []string{"http/1.1"}
				}
			}
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
			defer stop()
			uc := utls.UClient(raw, &utls.Config{ServerName: host, RootCAs: e.RootCAs}, utls.HelloCustom)
			if err := uc.ApplyPreset(&spec); err != nil {
				_ = raw.Close()
				return nil, err
			}
			if err := uc.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return uc, nil
		},
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 30 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// tcpOnly keeps the attempts of the TCP suites: QUIC and noise3 are UDP sockets the proxy cannot
// carry. With nothing left the dial fails.
func tcpOnly(atts []carrier.Attempt) []carrier.Attempt {
	out := atts[:0:0]
	for _, a := range atts {
		if a.Kind == carrier.AttemptCarrier && a.Suite != objects.SuiteReality2TCP && a.Suite != objects.SuiteWS2 {
			continue
		}
		out = append(out, a)
	}
	return out
}

// EgressStatus reports the proxy mode in effect.
func (r *Runtime) EgressStatus() EgressStatus {
	if r.egress == nil {
		return EgressStatus{}
	}
	return EgressStatus{Active: true, Tor: r.egress.Tor, Proxy: r.egress.Proxy}
}
