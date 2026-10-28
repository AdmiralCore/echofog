package mimicry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
// aggressive outbound port scanning. At start-up we probe only a handful of nearby addresses, a few
// at a time, with a pause between launches, and stop at the first match. When none of them fits,
// a slower wide search (WideCandidates: the whole /24, then the /24s next to it) runs now and then
// in the background.
//
// Only a site with a publicly trusted certificate for a real domain name is taken: a self-signed
// certificate, an internal name (a Kubernetes API server answers as "kubernetes.default.svc") or an
// IP address in place of a name is not a site a browser visits, so imitating it would stand out.

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
	neighborWideBlocks      = 2 // the wide search also covers this many /24s on each side of ours
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
	return scanFirst(ctx, cands, ourProfile, logger)
}

// ScanWide is the slow background search: every address of ownIP's /24, then of the /24s next to
// it (the same hosting block), in that order, at the same gentle pace as ScanForNeighbor. It stops
// at the first match. A full run probes about 1 300 addresses and takes a few minutes.
func ScanWide(ctx context.Context, ownIP netip.Addr, ourProfile ServerProfile, logger *slog.Logger) (Neighbor, bool) {
	ownIP = ownIP.Unmap()
	if !ownIP.Is4() || isPrivateOrReserved(ownIP) {
		return Neighbor{}, false
	}
	cands := WideCandidates(ownIP)
	if logger != nil {
		logger.Info("anti-DPI: searching wider for a camouflage neighbour", "near", ownIP.String(), "candidates", len(cands))
	}
	return scanFirst(ctx, cands, ourProfile, logger)
}

// FindNeighbors probes cands at the gentle pace and returns up to max sites that fit, for an
// operator choosing one by hand (echofog-node camouflage). progress, if set, hears of each probe.
func FindNeighbors(ctx context.Context, cands []netip.Addr, ourProfile ServerProfile, max int, progress func(done, total int)) []Neighbor {
	var mu sync.Mutex
	var found []Neighbor
	done := 0
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, neighborScanConcurrency)
	var wg sync.WaitGroup
	for _, ip := range cands {
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
			nb, ok := probeNeighbor(cctx, net.JoinHostPort(ip.String(), "443"), ourProfile)
			mu.Lock()
			defer mu.Unlock()
			done++
			if ok && len(found) < max {
				found = append(found, nb)
				if len(found) == max {
					cancel()
				}
			}
			if progress != nil {
				progress(done, len(cands))
			}
		}(ip)
		pace(cctx)
	}
	wg.Wait()
	return found
}

// scanFirst probes cands a few at a time and returns the first site that fits.
func scanFirst(ctx context.Context, cands []netip.Addr, ourProfile ServerProfile, logger *slog.Logger) (Neighbor, bool) {
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
	name := pickServerName(cs, time.Now())
	if name == "" {
		return Neighbor{}, false
	}
	return Neighbor{ServerName: name, Addr: addr}, true
}

// pickServerName reads a concrete hostname from the site's certificate to present as SNI: a real
// domain name the certificate is valid for under the system's trusted roots. A plain DNS name is
// preferred; a wildcard such as *.example.com is turned into www.example.com. "" if there is none.
func pickServerName(cs tls.ConnectionState, now time.Time) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	var plain, wild []string
	for _, name := range leaf.DNSNames {
		name = strings.TrimSpace(strings.ToLower(name))
		if strings.HasPrefix(name, "*.") {
			if n := "www." + name[2:]; usableServerName(n) {
				wild = append(wild, n)
			}
		} else if usableServerName(name) {
			plain = append(plain, name)
		}
	}
	for _, name := range append(plain, wild...) {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Intermediates: inter, Roots: neighborRoots, CurrentTime: now}); err == nil {
			return name
		}
	}
	return ""
}

// neighborRoots are the roots a neighbour's certificate must chain to; nil is the system's (tests
// set their own).
var neighborRoots *x509.CertPool

// internalSuffixes are names that never reach a browser: cluster, LAN and reserved test domains.
var internalSuffixes = []string{".local", ".localhost", ".internal", ".svc", ".cluster", ".lan", ".home",
	".corp", ".intranet", ".test", ".example", ".invalid", ".localdomain", ".arpa"}

// usableServerName reports whether name is a public domain name a browser could ask for: at least
// two labels of letters, digits and hyphens, a top-level domain of letters, no internal suffix.
func usableServerName(name string) bool {
	if len(name) < 4 || len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, sfx := range internalSuffixes {
		if strings.HasSuffix(name, sfx) {
			return false
		}
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			if !strings.HasPrefix(tld, "xn--") {
				return false // a TLD is letters (or punycode): this rules out IP addresses too
			}
		}
	}
	return true
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

// WideCandidates are the addresses the wide search probes, in order: the rest of ownIP's /24 in
// random order, then each /24 next to it (nearest first), each in random order.
func WideCandidates(ownIP netip.Addr) []netip.Addr {
	ownIP = ownIP.Unmap()
	b := ownIP.As4()
	block := func(third int) []netip.Addr {
		out := make([]netip.Addr, 0, 254)
		for h := 1; h <= 254; h++ {
			c := b
			c[2], c[3] = byte(third), byte(h)
			if a := netip.AddrFrom4(c); a != ownIP {
				out = append(out, a)
			}
		}
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	out := block(int(b[2]))
	for d := 1; d <= neighborWideBlocks; d++ {
		for _, third := range []int{int(b[2]) - d, int(b[2]) + d} {
			if third >= 0 && third <= 255 {
				out = append(out, block(third)...)
			}
		}
	}
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
