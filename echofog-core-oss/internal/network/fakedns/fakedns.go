// Package fakedns answers DNS queries with addresses from a private pool so that traffic captured
// by the TUN device can be mapped back to the domain the application asked for. The exit node
// then resolves the real domain itself: local DNS never leaves the machine and never sees the
// censored resolver.
package fakedns

import (
	"encoding/binary"
	"net"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

// Pool is 198.18.0.0/15 (RFC 2544 benchmarking range): never routed on the Internet.
var Pool = &net.IPNet{IP: net.IPv4(198, 18, 0, 0).To4(), Mask: net.CIDRMask(15, 32)}

// Reserved addresses inside the pool.
var (
	TunAddr = net.IPv4(198, 18, 0, 1).To4() // address of the TUN interface itself
	DNSAddr = net.IPv4(198, 18, 0, 2).To4() // DNS server the TUN interface advertises
)

const (
	firstHost = 3       // .0 network, .1 TUN, .2 DNS
	poolSize  = 1 << 17 // /15
	answerTTL = 60      // short: mappings are recycled when the pool wraps
)

// Mapper hands out fake IPs for domains and translates them back.
type Mapper struct {
	mu     sync.Mutex
	base   uint32
	next   uint32
	byIP   map[uint32]string
	byName map[string]uint32
}

func New() *Mapper {
	return &Mapper{
		base:   binary.BigEndian.Uint32(Pool.IP.To4()),
		next:   firstHost,
		byIP:   make(map[uint32]string),
		byName: make(map[string]uint32),
	}
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// IPFor returns the fake IP of name, allocating one if needed. The oldest mapping is recycled
// once all ~131k addresses are in use.
func (m *Mapper) IPFor(name string) net.IP {
	name = normalize(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if off, ok := m.byName[name]; ok {
		return m.ip(off)
	}
	off := m.next
	m.next++
	if m.next >= poolSize-1 { // keep the broadcast-looking last address free
		m.next = firstHost
	}
	if old, ok := m.byIP[off]; ok {
		delete(m.byName, old)
	}
	m.byIP[off] = name
	m.byName[name] = off
	return m.ip(off)
}

func (m *Mapper) ip(off uint32) net.IP {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, m.base+off)
	return out
}

// Domain returns the domain a fake IP stands for.
func (m *Mapper) Domain(ip net.IP) (string, bool) {
	v4 := ip.To4()
	if v4 == nil || !Pool.Contains(v4) {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name, ok := m.byIP[binary.BigEndian.Uint32(v4)-m.base]
	return name, ok
}

// IsFake reports whether ip is inside the fake pool, mapped or not.
func IsFake(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && Pool.Contains(v4)
}

// TranslateHostPort replaces a fake IP in "host:port" with its domain. ok is false for a fake
// IP that is no longer mapped (stale cache in the application): such traffic must be dropped.
func (m *Mapper) TranslateHostPort(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, true
	}
	ip := net.ParseIP(host)
	if ip == nil || !IsFake(ip) {
		return addr, true
	}
	name, ok := m.Domain(ip)
	if !ok {
		return "", false
	}
	return net.JoinHostPort(name, port), true
}

const firefoxCanary = "use-application-dns.net"

const (
	typeSVCB  dnsmessage.Type = 64
	typeHTTPS dnsmessage.Type = 65
)

// realAnswers are names whose true address applications check against a fixed value. Windows
// shows "No Internet access" unless dns.msftncsi.com resolves to exactly this, and some apps then
// act offline.
var realAnswers = map[string]net.IP{
	"dns.msftncsi.com": net.IPv4(131, 107, 255, 255),
}

// Answer handles a DNS query locally when it can. A queries for real domain names get a fake IP;
// AAAA queries get an empty answer so applications fall back to IPv4 (which the TUN captures).
// Anything else (PTR, TXT, SRV, MX, HTTPS, single-label names) is left for the caller to forward
// through the exit, and handled is false.
func (m *Mapper) Answer(query []byte) (resp []byte, handled bool) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		return nil, false
	}
	if _, err := p.Question(); err != dnsmessage.ErrSectionDone {
		return nil, false // multi-question queries are not used in practice
	}
	name := normalize(q.Name.String())
	if q.Class != dnsmessage.ClassINET || !strings.Contains(name, ".") || strings.HasSuffix(name, ".local") {
		return nil, false
	}
	rcode := dnsmessage.RCodeSuccess
	switch {
	case name == firefoxCanary:
		// Firefox turns its own DNS-over-HTTPS off when this name does not exist; with DoH on,
		// its lookups would bypass the fake DNS and reach sites by IP the exit cannot name
		rcode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA, q.Type == dnsmessage.TypeAAAA:
	case q.Type == typeSVCB, q.Type == typeHTTPS:
		// no records: forwarded, the real answer would carry ipv4hint/ipv6hint and browsers
		// would connect to those real addresses, past the name the exit needs
	default:
		return nil, false
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		OpCode:             h.OpCode,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, false
	}
	if err := b.Question(q); err != nil {
		return nil, false
	}
	if err := b.StartAnswers(); err != nil {
		return nil, false
	}
	if q.Type == dnsmessage.TypeA && rcode == dnsmessage.RCodeSuccess {
		var a dnsmessage.AResource
		if real, ok := realAnswers[name]; ok {
			copy(a.A[:], real.To4())
		} else {
			copy(a.A[:], m.IPFor(name))
		}
		hdr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: answerTTL}
		if err := b.AResource(hdr, a); err != nil {
			return nil, false
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil, false
	}
	return out, true
}
