package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// Outbound proxy. With one set (SetOutboundProxy, a SOCKS5 proxy such as tor or Tor Browser) the
// node's own connections leave through it: TCP only, since Tor carries no UDP, and host names are
// handed to the proxy to resolve, so no lookup leaves this computer either. Peers on the local
// network are dialed directly (a proxy cannot reach them), and so is a dial marked WithDirect: a
// call the user chose to make over a direct link.

var outboundProxy atomic.Pointer[string]

// ErrProxyNeedsTCP: the proxy is on but the node has no TCP transport to dial through it with.
var ErrProxyNeedsTCP = errors.New("the proxy needs the TCP transport, and network.tcp_listen_addr is off")

// NormalizeProxyAddr checks a SOCKS5 proxy address ("host:port", optionally with a socks5:// or
// socks5h:// prefix) and returns it as host:port. "" stays "" (no proxy).
func NormalizeProxyAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"socks5h://", "socks5://"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			s = s[len(p):]
			break
		}
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return "", fmt.Errorf("%q is not host:port (for Tor Browser: 127.0.0.1:9150)", s)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("%q is not a port", port)
	}
	return net.JoinHostPort(host, port), nil
}

// SetOutboundProxy sets the SOCKS5 proxy for the node's own connections; "" turns it off.
func SetOutboundProxy(addr string) error {
	a, err := NormalizeProxyAddr(addr)
	if err != nil {
		return err
	}
	if a == "" {
		outboundProxy.Store(nil)
		return nil
	}
	outboundProxy.Store(&a)
	return nil
}

// OutboundProxy is the proxy the node's own connections go through, "" when none.
func OutboundProxy() string {
	if p := outboundProxy.Load(); p != nil {
		return *p
	}
	return ""
}

type directKey struct{}

// WithDirect marks ctx so its dials skip the proxy and the relay-only rules: a direct link the
// user chose (a call set to "direct"). The other side then sees this computer's address.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

// DirectRequested reports whether ctx was marked WithDirect.
func DirectRequested(ctx context.Context) bool {
	v, _ := ctx.Value(directKey{}).(bool)
	return v
}

// Proxied reports whether a dial to addr with ctx goes through the proxy: one is set, the dial
// is not marked WithDirect, and addr is not on this computer or its local network.
func Proxied(ctx context.Context, addr string) bool {
	return OutboundProxy() != "" && !DirectRequested(ctx) && !localNetAddr(addr)
}

// proxyLocalNet sends even local addresses through the proxy: for tests, whose peers are all on
// loopback.
var proxyLocalNet bool

// localNetAddr: a loopback, private or link-local IP address, which a proxy cannot reach.
func localNetAddr(addr string) bool {
	if proxyLocalNet {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host == "localhost"
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// dialTCPOut opens the TCP connection of an outbound dial: through the proxy when Proxied, else
// pinned to the real network (DirectDialer).
func dialTCPOut(ctx context.Context, addr string) (net.Conn, error) {
	if DialRefused(ctx, addr) {
		return nil, ErrDialRefused
	}
	if !Proxied(ctx, addr) {
		return DirectDialer(0).DialContext(ctx, "tcp", addr)
	}
	return dialViaProxy(ctx, OutboundProxy(), addr)
}

func dialViaProxy(ctx context.Context, proxyAddr, addr string) (net.Conn, error) {
	d, err := proxy.SOCKS5("tcp", proxyAddr, nil, DirectDialer(10*time.Second))
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("socks5 dialer cannot take a context")
	}
	c, err := cd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("through the proxy %s: %w", proxyAddr, err)
	}
	return c, nil
}

// CheckProxy connects to target through the SOCKS5 proxy at proxyAddr and closes the connection
// at once: the proxy is up and lets the node out. It sends nothing over the connection.
func CheckProxy(ctx context.Context, proxyAddr, target string) error {
	a, err := NormalizeProxyAddr(proxyAddr)
	if err != nil {
		return err
	}
	if a == "" {
		return errors.New("enter the proxy address")
	}
	c, err := dialViaProxy(ctx, a, target)
	if err != nil {
		return err
	}
	return c.Close()
}

// proxyResolvesName: a proxy is on and addr names its host, so the name is for the proxy to look
// up; resolving it here would show the lookup on this computer's network.
func proxyResolvesName(addr string) bool {
	if OutboundProxy() == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	_, err := netip.ParseAddr(host)
	return err != nil && host != "localhost"
}

// proxyDialMin is the least time a dial gets behind a proxy: a Tor circuit to a new address takes
// seconds of its own before the handshake even starts, and a cold Tor longer still.
const proxyDialMin = 30 * time.Second

// DialBudget is how long a dial that would get d may take: d, or more behind a proxy.
func DialBudget(d time.Duration) time.Duration {
	if OutboundProxy() == "" {
		return d
	}
	return max(d, proxyDialMin)
}
