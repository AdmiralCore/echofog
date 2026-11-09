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
//     evidence (one liar changes nothing), and peers of one /16 vote once. Only routable
//     addresses count; a change is applied at most once per MinChangeInterval;
//   - the NAT class of one local socket (for T6.2): 3 observations from distinct networks on
//     the same socket with the agreed IP. A different IP is not evidence of a symmetric NAT.
//
// How a network votes. The peers of one /16 are one voice. Inside the network the address named
// by most distinct peers wins; on a tie the address that has been reported the longest wins
// (a liar who joins later cannot overturn an honest peer, and cannot silence the network
// either). A tie with equal age abstains. A liar that sits in an honest peer's /16 therefore
// neither blocks the decision nor changes it.

// Observer limits.
const (
	// ObservationTTL is how long a report counts. NAT mappings and uplinks change; an old report
	// must not outvote the present. A decision that has lost all its reports expires with them.
	ObservationTTL = 30 * time.Minute
	// MaxObservations bounds memory. When it is exceeded the oldest report of the network that
	// holds the most reports is dropped, so a crowd from one /16 evicts itself first.
	MaxObservations = 256
	// MaxPerNetwork bounds the reports of one /16 (IPv6: /32); the oldest of that network goes.
	MaxPerNetwork = 16
	// MaxSocketsPerPeer bounds the reports of one peer (one per local socket).
	MaxSocketsPerPeer = 4
	// MinPublicGroups is the number of networks that must agree on the public IP (wire.md §7.2).
	MinPublicGroups = 2
	// MinNATObservations is the number of observations NAT classification needs (wire.md §7.2).
	MinNATObservations = 3
	// MinChangeInterval is the shortest time between two changes of the public IP. A card
	// reissue is visible to everyone, so a flapping or Sybil-driven majority must not cause
	// reissues faster than this. The first decision, and one after a Reset or after the old
	// decision expired, is not a change.
	MinChangeInterval = 10 * time.Minute
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
	at    time.Time // last report
	// ipSince and portSince are when this peer began naming this IP and this port. They order
	// reports by age inside a network.
	ipSince   time.Time
	portSince time.Time
}

// Observer collects OBSERVED_ADDR reports and decides the node's public address and NAT class.
// It is safe for concurrent use.
type Observer struct {
	cfg ObserverConfig

	cbMu sync.Mutex // serializes callbacks in decision order; taken before mu is released

	mu         sync.Mutex
	recs       map[obsKey]obsRec
	decided    netip.Addr
	lastChange time.Time
	buckets    map[[32]byte]obsBucket
}

// ObservePeerBurst is how many reports one peer may land at once; the bucket refills one report
// per second (T3.4 inheritance: a peer cannot flood Observe).
const ObservePeerBurst = 8

// maxObsBuckets bounds the per-peer buckets.
const maxObsBuckets = 4096

type obsBucket struct {
	tokens float64
	at     time.Time
}

// takeTokenLocked spends one report of the peer's bucket.
func (o *Observer) takeTokenLocked(peer [32]byte, now time.Time) bool {
	b, ok := o.buckets[peer]
	if !ok {
		if len(o.buckets) >= maxObsBuckets {
			for p, x := range o.buckets {
				if now.Sub(x.at).Seconds()+x.tokens >= ObservePeerBurst {
					delete(o.buckets, p)
				}
			}
			for p := range o.buckets {
				if len(o.buckets) < maxObsBuckets {
					break
				}
				delete(o.buckets, p)
			}
		}
		b = obsBucket{tokens: ObservePeerBurst, at: now}
	} else if d := now.Sub(b.at).Seconds(); d > 0 {
		b.tokens = min(ObservePeerBurst, b.tokens+d)
		b.at = now
	}
	if b.tokens < 1 {
		o.buckets[peer] = b
		return false
	}
	b.tokens--
	o.buckets[peer] = b
	return true
}

// NewObserver returns an empty Observer.
func NewObserver(cfg ObserverConfig) *Observer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Observer{cfg: cfg, recs: make(map[obsKey]obsRec), buckets: make(map[[32]byte]obsBucket)}
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

// nonPublicV4 lists the IPv4 ranges that are never a node's public address: "this" network,
// private use, CGNAT, loopback, link local, IETF protocol assignments, the three documentation
// ranges, 6to4 relay anycast, benchmarking, multicast and the reserved class E block (which
// includes the limited broadcast address).
var nonPublicV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

// nonPublicV6 lists the ranges inside 2000::/3 that are not a node's public address: IETF
// protocol assignments (Teredo, benchmarking, ORCHIDv2 and others), documentation and 6to4.
// Everything outside 2000::/3 (unspecified, loopback, link local, unique local, multicast,
// NAT64, discard, deprecated site local) is not global unicast and is refused by the check
// for 2000::/3 itself.
var nonPublicV6 = mustPrefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")

var globalUnicastV6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// usableReported reports whether an address can be this node's public address: routable on the
// public internet, not reserved.
func usableReported(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return false
	}
	ranges := nonPublicV4
	if a.Is6() {
		if !globalUnicastV6.Contains(a) {
			return false
		}
		ranges = nonPublicV6
	}
	for _, p := range ranges {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// PublicAddr reports whether an address is a public unicast address: not private, loopback, link-local,
// shared, documentation, benchmarking, multicast or reserved, and not an IPv6 form that carries an IPv4
// address (NAT64, 6to4, Teredo; the IPv4-mapped form is judged as the IPv4 address it holds).
func PublicAddr(a netip.Addr) bool { return usableReported(a) }

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
	group := groupOf(ob.PeerAddr)

	o.cbMu.Lock()
	defer o.cbMu.Unlock()
	o.mu.Lock()

	if !o.takeTokenLocked(ob.Peer, now) {
		o.mu.Unlock()
		return ObsIgnored
	}
	o.expireLocked(now)
	// The newest word of a peer about the IP replaces what it said before.
	for k, r := range o.recs {
		if k.peer == ob.Peer && r.ip != ip {
			delete(o.recs, k)
		}
	}
	key := obsKey{ob.Peer, ob.Socket}
	rec := obsRec{ip: ip, port: rep.Port(), group: group, at: now, ipSince: now, portSince: now}
	if old, ok := o.recs[key]; ok && old.ip == ip {
		rec.ipSince = old.ipSince
		if old.port == rep.Port() {
			rec.portSince = old.portSince
		}
	}
	o.recs[key] = rec
	o.trimLocked(key)

	verdict := ObsAccepted
	if o.decided.IsValid() && o.decided != ip {
		verdict = ObsSuspicious
	}
	change, changed := o.decideLocked(now)
	o.mu.Unlock()

	if verdict == ObsSuspicious && o.cfg.OnSuspicious != nil {
		o.cfg.OnSuspicious(ob.Peer)
	}
	if changed && o.cfg.OnChange != nil {
		o.cfg.OnChange(change)
	}
	return verdict
}

// expireLocked drops reports older than ObservationTTL. A decision that lost every report that
// supported it is forgotten with them: nothing says any more that it is still true.
func (o *Observer) expireLocked(now time.Time) {
	removed := 0
	for k, r := range o.recs {
		if now.Sub(r.at) > ObservationTTL {
			delete(o.recs, k)
			removed++
		}
	}
	if removed > 0 && o.decided.IsValid() && o.votesLocked()[o.decided] == 0 {
		o.decided = netip.Addr{}
		o.lastChange = time.Time{}
	}
}

func oldestKey(recs map[obsKey]obsRec, keep func(obsKey, obsRec) bool) (obsKey, bool) {
	var best obsKey
	var at time.Time
	found := false
	for k, r := range recs {
		if !keep(k, r) {
			continue
		}
		if !found || r.at.Before(at) {
			best, at, found = k, r.at, true
		}
	}
	return best, found
}

// trimLocked enforces the bounds after report fresh was stored: per peer, per network and in
// total. The new report is never the one dropped.
func (o *Observer) trimLocked(fresh obsKey) {
	for {
		n := 0
		for k := range o.recs {
			if k.peer == fresh.peer {
				n++
			}
		}
		if n <= MaxSocketsPerPeer {
			break
		}
		k, ok := oldestKey(o.recs, func(k obsKey, _ obsRec) bool { return k.peer == fresh.peer && k != fresh })
		if !ok {
			break
		}
		delete(o.recs, k)
	}
	g := o.recs[fresh].group
	for {
		n := 0
		for _, r := range o.recs {
			if r.group == g {
				n++
			}
		}
		if n <= MaxPerNetwork {
			break
		}
		k, ok := oldestKey(o.recs, func(k obsKey, r obsRec) bool { return r.group == g && k != fresh })
		if !ok {
			break
		}
		delete(o.recs, k)
	}
	for len(o.recs) > MaxObservations {
		sizes := make(map[netip.Prefix]int)
		for _, r := range o.recs {
			sizes[r.group]++
		}
		var big netip.Prefix
		bigN := 0
		for p, n := range sizes {
			if n > bigN || (n == bigN && p.Addr().Less(big.Addr())) {
				big, bigN = p, n
			}
		}
		k, ok := oldestKey(o.recs, func(k obsKey, r obsRec) bool { return r.group == big && k != fresh })
		if !ok {
			break
		}
		delete(o.recs, k)
	}
}

// voteItem is one peer's claim inside a network.
type voteItem[K comparable] struct {
	peer  [32]byte
	value K
	since time.Time
}

// networkVote is the value a network votes for: the one named by most distinct peers; on a tie
// the one reported the longest; a tie in age abstains.
func networkVote[K comparable](items []voteItem[K]) (K, bool) {
	var zero K
	type agg struct {
		peers map[[32]byte]struct{}
		since time.Time
	}
	by := make(map[K]*agg)
	for _, it := range items {
		a := by[it.value]
		if a == nil {
			a = &agg{peers: make(map[[32]byte]struct{}), since: it.since}
			by[it.value] = a
		}
		a.peers[it.peer] = struct{}{}
		if it.since.Before(a.since) {
			a.since = it.since
		}
	}
	best, bestN, tied := zero, 0, false
	var bestSince time.Time
	for v, a := range by {
		n := len(a.peers)
		switch {
		case n > bestN:
			best, bestN, bestSince, tied = v, n, a.since, false
		case n == bestN:
			switch {
			case a.since.Before(bestSince):
				best, bestSince, tied = v, a.since, false
			case a.since.Equal(bestSince):
				tied = true
			}
		}
	}
	if bestN == 0 || tied {
		return zero, false
	}
	return best, true
}

// votesLocked counts, per IP, the networks that vote for it.
func (o *Observer) votesLocked() map[netip.Addr]int {
	perGroup := make(map[netip.Prefix][]voteItem[netip.Addr])
	for k, r := range o.recs {
		if r.group.IsValid() {
			perGroup[r.group] = append(perGroup[r.group], voteItem[netip.Addr]{k.peer, r.ip, r.ipSince})
		}
	}
	votes := make(map[netip.Addr]int)
	for _, items := range perGroup {
		if ip, ok := networkVote(items); ok {
			votes[ip]++
		}
	}
	return votes
}

// decideLocked applies the §7.2 rule. A winner needs MinPublicGroups networks and must be
// strictly ahead of every other address; otherwise the previous decision stands. Replacing an
// existing decision is allowed once per MinChangeInterval.
func (o *Observer) decideLocked(now time.Time) (PublicAddrChange, bool) {
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
	if o.decided.IsValid() && now.Sub(o.lastChange) < MinChangeInterval {
		return PublicAddrChange{}, false
	}
	c := PublicAddrChange{Old: o.decided, New: top}
	o.decided = top
	o.lastChange = now
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
	o.lastChange = time.Time{}
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

// natCountLocked returns the number of counted networks and the class.
func (o *Observer) natCountLocked(socket string) (int, NATClass) {
	if !o.decided.IsValid() {
		return 0, NATUnknown
	}
	perGroup := make(map[netip.Prefix][]voteItem[uint16])
	for k, r := range o.recs {
		if k.socket != socket || r.ip != o.decided || !r.group.IsValid() {
			continue
		}
		perGroup[r.group] = append(perGroup[r.group], voteItem[uint16]{k.peer, r.port, r.portSince})
	}
	count := make(map[uint16]int)
	n := 0
	for _, items := range perGroup {
		if p, ok := networkVote(items); ok {
			count[p]++
			n++
		}
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

// NATDetail is the NAT class together with the external port range that the reports named for the
// classified socket (T6.7: the range a spray aims at).
type NATDetail struct {
	Class NATClass
	// Lo and Hi are the lowest and highest voted external port; zero when the class is not known.
	Lo, Hi uint16
	// Samples is how many networks voted a port.
	Samples int
}

// NATDetail classifies like NATClass and adds the port range of the winning socket.
func (o *Observer) NATDetail() NATDetail {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expireLocked(o.cfg.Now())
	sockets := make(map[string]struct{})
	for k := range o.recs {
		if k.socket != "" {
			sockets[k.socket] = struct{}{}
		}
	}
	best, bestSock, class, tie := 0, "", NATUnknown, false
	for s := range sockets {
		n, c := o.natCountLocked(s)
		if c == NATUnknown {
			continue
		}
		switch {
		case n > best:
			best, bestSock, class, tie = n, s, c, false
		case n == best && c != class:
			tie = true
		}
	}
	if tie || class == NATUnknown {
		return NATDetail{}
	}
	d := NATDetail{Class: class}
	if class != NATSymmetric {
		return d
	}
	perGroup := make(map[netip.Prefix][]voteItem[uint16])
	for k, r := range o.recs {
		if k.socket != bestSock || r.ip != o.decided || !r.group.IsValid() {
			continue
		}
		perGroup[r.group] = append(perGroup[r.group], voteItem[uint16]{k.peer, r.port, r.portSince})
	}
	for _, items := range perGroup {
		p, ok := networkVote(items)
		if !ok {
			continue
		}
		if d.Samples == 0 || p < d.Lo {
			d.Lo = p
		}
		if p > d.Hi {
			d.Hi = p
		}
		d.Samples++
	}
	return d
}
