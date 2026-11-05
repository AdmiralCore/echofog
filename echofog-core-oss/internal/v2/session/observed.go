package session

import (
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Own-address discovery (wire.md §7.2, D20). The node never sends STUN from a carrier socket.
// Authenticated peers tell it how they see it (HsResp.observed, OBSERVED_ADDR), and the Observer
// turns those reports into two decisions:
//
//   - the node's public IP: at least 2 peers in different /16 networks (IPv6: /32) agree, and
//     the leading address is strictly ahead of every other one. A tie decides nothing and is
//     never broken by an ordering. A report that differs from the decision is suspicious, not
//     evidence (one liar changes nothing), and peers of one /16 vote once;
//   - the NAT class of one local socket (for T6.2): 3 observations from distinct networks on
//     the same socket with the agreed IP. A different IP is not evidence of a symmetric NAT.

// Observer limits.
const (
	// ObservationTTL is how long a report counts. NAT mappings and uplinks change; an old report
	// must not outvote the present.
	ObservationTTL = 30 * time.Minute
	// MaxObservations bounds memory: the oldest report is dropped first (peers cost a handshake
	// each, so the cap is rarely reached by honest traffic).
	MaxObservations = 256
	// MinPublicGroups is the number of networks that must agree on the public IP (wire.md §7.2).
	MinPublicGroups = 2
	// MinNATObservations is the number of observations NAT classification needs (wire.md §7.2).
	MinNATObservations = 3
)

// NATClass is the NAT behaviour as far as OBSERVED_ADDR can tell.
type NATClass uint8

const (
	// NATUnknown means there is not enough trustworthy evidence.
	NATUnknown NATClass = iota
	// NATCone is endpoint-independent mapping: every peer sees the same external port for one
	// local socket, so a peer can reach the node at that port. Filtering (full, restricted or
	// port-restricted) is not visible from observations and stays unknown.
	NATCone
	// NATSymmetric is endpoint-dependent mapping: each peer sees a different external port.
	NATSymmetric
)

func (c NATClass) String() string {
	switch c {
	case NATCone:
		return "cone"
	case NATSymmetric:
		return "symmetric"
	}
	return "unknown"
}

// Verdict is what the Observer made of one report.
type Verdict uint8

const (
	// ObsIgnored is a report that cannot count: no usable peer or reported address.
	ObsIgnored Verdict = iota
	// ObsAccepted is a stored report.
	ObsAccepted
	// ObsSuspicious is a stored report whose IP differs from the decided public IP.
	ObsSuspicious
)

// Observation is one authenticated report: Peer said it sees the node at Reported.
type Observation struct {
	// Peer is the authenticated device ID of the reporter.
	Peer [32]byte
	// PeerAddr is the reporter's own network address as seen by this node. It places the peer
	// in a /16 (IPv6: /32). A peer behind a relay circuit has none and is ignored: its source
	// address says nothing about its network.
	PeerAddr netip.Addr
	// Reported is the address the peer sees the node at.
	Reported netip.AddrPort
	// Socket names the local socket the session runs on. NAT classification compares only
	// reports of one socket (a cone NAT gives a different port to each socket); an empty name
	// takes part in the public-IP decision only.
	Socket string
}

// PublicAddrChange is the event of a changed public IP.
type PublicAddrChange struct {
	// Old is the previous decision; invalid for the first one.
	Old netip.Addr
	New netip.Addr
}

// ObserverConfig configures an Observer.
type ObserverConfig struct {
	// Now is the time source (AD13); nil means time.Now.
	Now func() time.Time
	// OnChange is called after the public IP decision changed, serialized, without the
	// Observer's lock held. It must not call back into the Observer.
	OnChange func(PublicAddrChange)
	// OnSuspicious is called for a report that contradicts the decided public IP. Optional;
	// the same rules as OnChange.
	OnSuspicious func(peer [32]byte)
}

type obsKey struct {
	peer   [32]byte
	socket string
}

type obsRec struct {
	ip    netip.Addr
	port  uint16
	group netip.Prefix
	at    time.Time
}

// Observer collects OBSERVED_ADDR reports and decides the node's public address and NAT class.
// It is safe for concurrent use.
type Observer struct {
	cfg ObserverConfig

	cbMu sync.Mutex // serializes callbacks in decision order; taken before mu is released

	mu      sync.Mutex
	recs    map[obsKey]obsRec
	decided netip.Addr
}

// NewObserver returns an empty Observer.
func NewObserver(cfg ObserverConfig) *Observer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Observer{cfg: cfg, recs: make(map[obsKey]obsRec)}
}

// ParseObserved decodes the HsResp.observed value (family ‖ address ‖ port).
func ParseObserved(b []byte) (netip.AddrPort, bool) {
	ap, err := frame.ParseObservedAddr(b)
	if err != nil || !ap.IsValid() {
		return netip.AddrPort{}, false
	}
	return ap, true
}

// groupOf is the network a peer address belongs to: /16 for IPv4, /32 for IPv6.
func groupOf(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	bits := 16
	if a.Is6() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// usableReported reports whether an address can be this node's public address.
func usableReported(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() &&
		!a.IsLinkLocalUnicast() && !a.IsUnspecified() && !a.IsMulticast()
}

// usablePeer reports whether an address can place a peer in a network.
func usablePeer(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && !a.IsLoopback() && !a.IsUnspecified() && !a.IsMulticast() && !a.IsLinkLocalUnicast()
}

// Observe stores one report and re-evaluates the decision.
func (o *Observer) Observe(ob Observation) Verdict {
	rep := ob.Reported
	if !rep.IsValid() || rep.Port() == 0 || !usableReported(rep.Addr()) || !usablePeer(ob.PeerAddr) {
		return ObsIgnored
	}
	ip := rep.Addr().Unmap()
	now := o.cfg.Now()

	o.cbMu.Lock()
	defer o.cbMu.Unlock()
	o.mu.Lock()

	o.expireLocked(now)
	// The newest word of a peer about the IP replaces what it said before.
	for k, r := range o.recs {
		if k.peer == ob.Peer && r.ip != ip {
			delete(o.recs, k)
		}
	}
	o.recs[obsKey{ob.Peer, ob.Socket}] = obsRec{ip: ip, port: rep.Port(), group: groupOf(ob.PeerAddr), at: now}
	for len(o.recs) > MaxObservations {
		o.evictOldestLocked()
	}

	verdict := ObsAccepted
	if o.decided.IsValid() && o.decided != ip {
		verdict = ObsSuspicious
	}
	change, changed := o.decideLocked()
	o.mu.Unlock()

	if verdict == ObsSuspicious && o.cfg.OnSuspicious != nil {
		o.cfg.OnSuspicious(ob.Peer)
	}
	if changed && o.cfg.OnChange != nil {
		o.cfg.OnChange(change)
	}
	return verdict
}

func (o *Observer) expireLocked(now time.Time) {
	for k, r := range o.recs {
		if now.Sub(r.at) > ObservationTTL {
			delete(o.recs, k)
		}
	}
}

func (o *Observer) evictOldestLocked() {
	var oldest obsKey
	var at time.Time
	first := true
	for k, r := range o.recs {
		if first || r.at.Before(at) {
			oldest, at, first = k, r.at, false
		}
	}
	if !first {
		delete(o.recs, oldest)
	}
}

// votesLocked counts, per IP, the networks that vote for it. A network votes for an IP only if
// all of its live reports name that IP: peers of one /16 are one voice, and a conflicting
// network abstains.
func (o *Observer) votesLocked() map[netip.Addr]int {
	perGroup := make(map[netip.Prefix]netip.Addr)
	conflict := make(map[netip.Prefix]bool)
	for _, r := range o.recs {
		if !r.group.IsValid() {
			continue
		}
		if ip, ok := perGroup[r.group]; ok && ip != r.ip {
			conflict[r.group] = true
			continue
		}
		perGroup[r.group] = r.ip
	}
	votes := make(map[netip.Addr]int)
	for g, ip := range perGroup {
		if !conflict[g] {
			votes[ip]++
		}
	}
	return votes
}

// decideLocked applies the §7.2 rule. A winner needs MinPublicGroups networks and must be
// strictly ahead of every other address; otherwise the previous decision stands.
func (o *Observer) decideLocked() (PublicAddrChange, bool) {
	votes := o.votesLocked()
	var top netip.Addr
	topN, second := 0, 0
	for ip, n := range votes {
		switch {
		case n > topN:
			second = topN
			top, topN = ip, n
		case n > second:
			second = n
		}
	}
	if topN < MinPublicGroups || topN == second || top == o.decided {
		return PublicAddrChange{}, false
	}
	c := PublicAddrChange{Old: o.decided, New: top}
	o.decided = top
	return c, true
}

// PublicAddr returns the decided public IP of the node.
func (o *Observer) PublicAddr() (netip.Addr, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(o.cfg.Now())
	return o.decided, o.decided.IsValid()
}

// Len is the number of live reports.
func (o *Observer) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(o.cfg.Now())
	return len(o.recs)
}

// Reset forgets every report and the decision. The runtime calls it when the local network
// changed: what peers saw before says nothing about the new uplink. No change event is emitted
// (the address is unknown, not different).
func (o *Observer) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.recs = make(map[obsKey]obsRec)
	o.decided = netip.Addr{}
}

// NAT classifies the NAT as seen by one local socket. It needs the public IP to be decided and
// MinNATObservations reports on that socket from distinct networks naming that IP. Reports that
// name another IP are ignored here: a divergent IP is a suspicious report, not evidence of a
// symmetric NAT.
//
// Cone needs one port named by at least 2/3 of the reports (and at least 3), so a single lying
// peer cannot turn a cone NAT into a symmetric one; symmetric needs every port to differ.
func (o *Observer) NAT(socket string) NATClass {
	if socket == "" {
		return NATUnknown
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(o.cfg.Now())
	_, c := o.natCountLocked(socket)
	return c
}

// natCountLocked returns the number of counted reports and the class.
func (o *Observer) natCountLocked(socket string) (int, NATClass) {
	if !o.decided.IsValid() {
		return 0, NATUnknown
	}
	ports := make(map[netip.Prefix]uint16)
	conflict := make(map[netip.Prefix]bool)
	for k, r := range o.recs {
		if k.socket != socket || r.ip != o.decided || !r.group.IsValid() {
			continue
		}
		if p, ok := ports[r.group]; ok && p != r.port {
			conflict[r.group] = true
			continue
		}
		ports[r.group] = r.port
	}
	count := make(map[uint16]int)
	n := 0
	for g, p := range ports {
		if conflict[g] {
			continue
		}
		count[p]++
		n++
	}
	if n < MinNATObservations {
		return n, NATUnknown
	}
	top := 0
	for _, c := range count {
		if c > top {
			top = c
		}
	}
	switch {
	case top == 1:
		return n, NATSymmetric
	case top >= MinNATObservations && top*3 >= 2*n:
		return n, NATCone
	}
	return n, NATUnknown
}

// NATClass classifies the node's NAT for path planning: the class of the socket with the most
// counted reports. When two sockets share the most and disagree, the answer is unknown (no
// ordering decides).
func (o *Observer) NATClass() NATClass {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(o.cfg.Now())
	sockets := make(map[string]struct{})
	for k := range o.recs {
		if k.socket != "" {
			sockets[k.socket] = struct{}{}
		}
	}
	best, class, tie := 0, NATUnknown, false
	for s := range sockets {
		n, c := o.natCountLocked(s)
		if c == NATUnknown {
			continue
		}
		switch {
		case n > best:
			best, class, tie = n, c, false
		case n == best && c != class:
			tie = true
		}
	}
	if tie {
		return NATUnknown
	}
	return class
}
