package portmap

import (
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"net/netip"
	"time"

	mrand "math/rand/v2"

	"github.com/echofog/echofog-core/internal/v2/carrier"

	"github.com/koron/go-ssdp"
	"github.com/libp2p/go-netroute"
)

var nonPublicV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// isPublic reports whether ip can be reached from the internet: not private, shared (CGNAT),
// loopback, link-local, documentation, multicast or reserved. A router whose "external" address
// is not public sits behind another NAT, and the mapping is useless.
func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return false
	case ip.Is4():
		for _, p := range nonPublicV4 {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	default:
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !netip.MustParsePrefix("2001:db8::/32").Contains(ip)
	}
}

// isLANGateway reports whether ip can be the home router: a private, link-local or CGNAT IPv4
// address. Nothing else is sent port mapping requests, so a wrong route table cannot make the
// node talk to a host on the internet.
func isLANGateway(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func withDefaults(c Config) Config {
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	if c.Gateway == nil {
		c.Gateway = defaultGateway
	}
	if c.Local == nil {
		c.Local = localTowards
	}
	if c.Dial == nil {
		c.Dial = dialUDP
	}
	if c.HTTP == nil {
		c.HTTP = noRedirectClient()
	}
	if c.SSDP == nil {
		c.SSDP = searchSSDP
	}
	if c.Rand == nil {
		c.Rand = mrand.Float64
	}
	if c.Nonce == nil {
		c.Nonce = rand.Reader
	}
	if c.Lifetime <= 0 {
		c.Lifetime = defaultLifetime
	}
	if c.Port == 0 {
		c.Port = defaultPort
	}
	return c
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

func defaultGateway() (netip.Addr, error) {
	r, err := netroute.New()
	if err != nil {
		return netip.Addr{}, err
	}
	_, gw, _, err := r.Route(net.IPv4zero)
	if err != nil {
		return netip.Addr{}, err
	}
	a, ok := netip.AddrFromSlice(gw)
	if !ok {
		return netip.Addr{}, errNoGateway
	}
	return a.Unmap(), nil
}

// localTowards is the address the system would use to reach gw; connecting a UDP socket sends
// nothing.
func localTowards(gw netip.Addr) (netip.Addr, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(gw, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	a, _ := netip.AddrFromSlice(c.LocalAddr().(*net.UDPAddr).IP)
	return a.Unmap(), nil
}

func dialUDP(ctx context.Context, remote netip.AddrPort) (PacketConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", remote.String())
	if err != nil {
		return nil, err
	}
	return c, nil
}

// noRedirectClient never follows a redirect: a router has no reason to send the node elsewhere.
func noRedirectClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the gateway is on the LAN, never behind the node's proxy
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var igdTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// searchSSDP asks the LAN for internet gateway devices. The search library blocks for its wait
// time, so it runs aside and ctx can cut it short.
func searchSSDP(ctx context.Context) ([]string, error) {
	type result struct {
		locs []string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var locs []string
		var firstErr error
		for _, t := range igdTargets {
			found, err := ssdp.Search(t, 2, "")
			if err != nil && firstErr == nil {
				firstErr = err
			}
			for _, s := range found {
				locs = append(locs, s.Location)
			}
		}
		ch <- result{locs, firstErr}
	}()
	select {
	case r := <-ch:
		if len(r.locs) > 0 {
			return r.locs, nil
		}
		return nil, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
