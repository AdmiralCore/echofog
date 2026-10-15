package mimicry

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// A public node mimics a real site. Buying a domain is not required: like REALITY's dest scanning,
// the node can adopt a real neighbour — a site hosted in its own datacenter subnet — as the name it
// presents (SNI) and the address it forwards probes to. Then the SNI matches a site whose IP is in
// the same /24 as ours, and a prober we forward reaches that genuine site and sees its real
// certificate. Only a neighbour whose ServerHello (version, cipher, key-share group) answers exactly
// like ours is accepted, or the passive fingerprint would still differ.
//
// The scan is deliberately gentle: hosters such as Hetzner, OVH and DigitalOcean strike a VPS for
// aggressive outbound port scanning. We probe only a handful of nearby addresses, a few at a time,
// with a pause between launches, and stop at the first match.

// Neighbor is a real site next to our IP that we can mimic.
type Neighbor struct {
	ServerName string `json:"server_name"` // the domain to present as SNI
	Addr       string `json:"addr"`        // host:port to forward probes to (the neighbour's ip:443)
}

const (
	neighborScanSpan        = 20              // probe up to ±this many addresses around our own host octet
	neighborScanConcurrency = 4               // at most this many connects in flight (gentle)
	neighborDialTimeout     = 3 * time.Second // per-candidate TLS dial budget
	neighborPaceMinMS       = 100             // pause between launching connects
	neighborPaceMaxMS       = 200
)

// ScanForNeighbor looks for a mimickable site in ownIP's /24 whose ServerHello matches ourProfile,
// probing gently. It returns the first match, or ok=false if none was found (or ownIP is not a
// public IPv4 address, in which case its neighbours are not real sites and scanning is pointless).
func ScanForNeighbor(ctx context.Context, ownIP netip.Addr, ourProfile ServerProfile, logger *slog.Logger) (Neighbor, bool) {
	ownIP = ownIP.Unmap()
	if !ownIP.Is4() || isPrivateOrReserved(ownIP) {
		return Neighbor{}, false
	}
	cands := nearbyCandidates(ownIP, neighborScanSpan)
	if logger != nil {
		logger.Info("anti-DPI: scanning for a camouflage neighbour", "near", ownIP.String(), "candidates", len(cands))
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan Neighbor, 1)
	sem := make(chan struct{}, neighborScanConcurrency)
	var wg sync.WaitGroup

	for _, ip := range cands {
		if cctx.Err() != nil {
			break // found one, or the caller's deadline passed
		}
		select {
		case sem <- struct{}{}:
		case <-cctx.Done():
		}
		if cctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(ip netip.Addr) {
			defer wg.Done()
			defer func() { <-sem }()
			if nb, ok := probeNeighbor(cctx, net.JoinHostPort(ip.String(), "443"), ourProfile); ok {
				select {
				case result <- nb:
					cancel() // first match wins; stop the rest
				default:
				}
			}
		}(ip)
		pace(cctx)
	}
	wg.Wait()

	select {
	case nb := <-result:
		if logger != nil {
			logger.Info("anti-DPI: adopted a camouflage neighbour", "server_name", nb.ServerName, "forward_to", nb.Addr)
		}
		return nb, true
	default:
		if logger != nil {
			logger.Warn("anti-DPI: no suitable camouflage neighbour found nearby; set anti_dpi.tls_server_name and camouflage_addr manually")
		}
		return Neighbor{}, false
	}
}

// VerifyNeighbor rechecks that a (cached) neighbour is still alive and still answers like us, so a
// node that found one earlier need not scan again while it holds.
func VerifyNeighbor(ctx context.Context, nb Neighbor, ourProfile ServerProfile) bool {
	if nb.ServerName == "" || nb.Addr == "" {
		return false
	}
	got, ok := probeNeighbor(ctx, nb.Addr, ourProfile)
	return ok && got.ServerName == nb.ServerName
}

// probeNeighbor completes one TLS 1.3 handshake to addr with a Chrome-like client, and accepts the
// site only if it negotiates TLS 1.3, its ServerHello matches ourProfile, and its certificate names
// a usable domain (which becomes the SNI we will present).
func probeNeighbor(ctx context.Context, addr string, ourProfile ServerProfile) (Neighbor, bool) {
	dctx, cancel := context.WithTimeout(ctx, neighborDialTimeout)
	defer cancel()
	d := tls.Dialer{Config: chromeLikeClientConfig("")} // no SNI: read the site's default certificate
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return Neighbor{}, false
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
	if cs.Version != tls.VersionTLS13 {
		return Neighbor{}, false
	}
	p := profileFrom(cs)
	if p.Version != ourProfile.Version || p.Cipher != ourProfile.Cipher || p.Group != ourProfile.Group {
		return Neighbor{}, false // a different ServerHello would still be a passive mismatch
	}
	name := pickServerName(cs)
	if name == "" {
		return Neighbor{}, false
	}
	return Neighbor{ServerName: name, Addr: addr}, true
}

// pickServerName reads a concrete hostname from the site's certificate to present as SNI. A plain
// DNS name is preferred; a wildcard such as *.example.com is turned into www.example.com.
func pickServerName(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	var wildcard string
	for _, name := range cs.PeerCertificates[0].DNSNames {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" || strings.Contains(name, " ") {
			continue
		}
		if strings.HasPrefix(name, "*.") {
			if wildcard == "" {
				wildcard = "www." + name[2:]
			}
			continue
		}
		return name
	}
	return wildcard
}

// nearbyCandidates returns host addresses within ownIP's /24, within ±span of its last octet,
// excluding ownIP, in random order.
func nearbyCandidates(ownIP netip.Addr, span int) []netip.Addr {
	b := ownIP.As4()
	host := int(b[3])
	out := make([]netip.Addr, 0, 2*span)
	for d := -span; d <= span; d++ {
		h := host + d
		if d == 0 || h < 1 || h > 254 {
			continue
		}
		c := b
		c[3] = byte(h)
		out = append(out, netip.AddrFrom4(c))
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func pace(ctx context.Context) {
	d := time.Duration(neighborPaceMinMS+rand.Intn(neighborPaceMaxMS-neighborPaceMinMS+1)) * time.Millisecond
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// isPrivateOrReserved reports whether ip is not a public, globally-routable unicast IPv4 address.
// Scanning the /24 of a private, CGNAT, loopback or link-local address finds LAN hosts, not real
// sites, so we skip it.
func isPrivateOrReserved(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	b := ip.As4()
	if net.IP(b[:]).IsPrivate() { // 10/8, 172.16/12, 192.168/16
		return true
	}
	if b[0] == 100 && b[1] >= 64 && b[1] <= 127 { // 100.64/10 CGNAT (RFC 6598)
		return true
	}
	if b[0] == 0 || b[0] >= 240 { // "this network" and reserved/experimental
		return true
	}
	return false
}
