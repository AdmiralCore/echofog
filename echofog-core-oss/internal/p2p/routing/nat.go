package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/jitter"
	"github.com/echofog/echofog-core/internal/network/natcheck"
)

const (
	// DefaultSTUNServer is kept for callers that name a single server; discovery itself uses the
	// whole natcheck.DefaultSTUNServers list.
	DefaultSTUNServer = "stun.l.google.com:19302"
	KeepAliveInterval = 20 * time.Second
	// classifyEvery: every Nth keep-alive tick re-checks the full picture.
	classifyEvery = 3
	stunTimeout   = 3 * time.Second
)

type NATType int

const (
	NATUnknown   NATType = iota // not enough information yet
	NATPublic                   // the observed address is ours: no NAT in the way
	NATFullCone                 // one public address for every destination: hole punching can work
	NATSymmetric                // a different public address per destination: relay only
)

func (t NATType) String() string {
	switch t {
	case NATPublic:
		return "public"
	case NATFullCone:
		return "cone"
	case NATSymmetric:
		return "symmetric"
	default:
		return "unknown"
	}
}

// Observer returns what connected peers see our address as.
type Observer func(ctx context.Context) []natcheck.Observation

// NATManager keeps an up-to-date picture of our NAT: the public address, whether it is the same for
// every destination, and whether the router mapped a port for us.
type NATManager struct {
	stunConn net.PacketConn // the QUIC socket's STUN side; nil when it is not our QUIC port
	servers  []string
	observe  Observer
	// stunAllowed, when set, says whether public STUN servers may be asked at all (guard mode keeps
	// them out: a STUN exchange with Google from the port the node talks on is a marker).
	stunAllowed func() bool
	logger      *slog.Logger

	mu         sync.RWMutex
	report     natcheck.Report
	natType    NATType
	publicAddr string         // ip:port, empty while no observer of our QUIC socket's port agrees
	publicIP   string         // the IP alone, also from observers on other sockets (country placement)
	mapped     netip.AddrPort // external address of a router port mapping, if any
	// carrierNAT: a router was found but its own WAN address is private or in 100.64/10, so the
	// node sits behind a second NAT (carrier-grade or double NAT) and no port forward can reach it.
	carrierNAT bool
	// peersSeen: the last check heard from connected peers, whose QUIC keep-alives hold the NAT
	// mapping open; no STUN keep-alive is needed then. Only used without keepers.
	peersSeen bool
	// keepers, when set, counts the peers connected now whose keep-alives hold the mapping open
	// (see SetMappingKeepers).
	keepers  func() int
	stunUsed int // checks that asked STUN servers (tests)
}

// How many observers must agree before their answer is taken. Peers can lie about the address
// they see us at (nothing proves it), so a few of them must not decide it alone.
const (
	// minPeerObservers: below this many connected peers with a public IPv4 address, STUN is asked
	// too (where allowed): two views are the least that tell whether the mapping varies. IPv6
	// observers do not count: they tell nothing about the IPv4 NAT, and STUN is what does.
	minPeerObservers = 2
	// safeObservers: STUN is left out only when at least this many independent peers agree on our
	// IP, are the majority, and see one mapping. Fewer, or any doubt, and STUN is asked as well
	// (outside guard mode), which outvotes a liar or two.
	safeObservers = 3
	// guardObservers: where STUN may not be asked (guard mode), the address and with it the home
	// country are taken only from at least this many agreeing peers, the majority of those that
	// answered; otherwise the previous picture stays. One peer must not place the node.
	guardObservers = 2
)

// NewNATManager's conn must be the socket whose NAT mapping matters (the QUIC port), or nil if
// STUN would run from another socket and thus see another mapping; peer observations still work.
func NewNATManager(conn net.PacketConn, logger *slog.Logger) *NATManager {
	return &NATManager{stunConn: conn, servers: natcheck.DefaultSTUNServers, logger: logger}
}

// SetSTUNServers overrides the STUN server list (empty keeps the defaults).
func (m *NATManager) SetSTUNServers(servers []string) {
	if len(servers) == 0 {
		return
	}
	m.mu.Lock()
	m.servers = servers
	m.mu.Unlock()
}

// SetObserver adds peer-observed addresses as a second, STUN-free source.
func (m *NATManager) SetObserver(o Observer) {
	m.mu.Lock()
	m.observe = o
	m.mu.Unlock()
}

// SetSTUNAllowed installs the rule that says whether public STUN servers may be asked (nil: always).
// Connected peers are asked either way.
func (m *NATManager) SetSTUNAllowed(fn func() bool) {
	m.mu.Lock()
	m.stunAllowed = fn
	m.mu.Unlock()
}

// SetMappingKeepers installs the count of connected peers whose QUIC keep-alives hold our NAT
// mapping open (peers at public IPv4 addresses on our QUIC socket). The STUN keep-alive asks it on
// every tick, so it resumes as soon as they are gone, not at the next full check a minute later: a
// carrier NAT that drops idle UDP mappings after 30 s would otherwise move us meanwhile.
func (m *NATManager) SetMappingKeepers(fn func() int) {
	m.mu.Lock()
	m.keepers = fn
	m.mu.Unlock()
}

// SetPortMapping records the router-assigned external address (zero clears it).
func (m *NATManager) SetPortMapping(addr netip.AddrPort) {
	m.mu.Lock()
	m.mapped = addr
	m.mu.Unlock()
}

// SetCarrierNAT records whether the router itself is behind another NAT (carrier-grade/double NAT).
func (m *NATManager) SetCarrierNAT(v bool) {
	m.mu.Lock()
	m.carrierNAT = v
	m.mu.Unlock()
}

// CarrierNAT reports whether the node is behind a carrier-grade or double NAT, so forwarding a
// port on the home router cannot make it reachable.
func (m *NATManager) CarrierNAT() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.carrierNAT
}

// PortMapping is the router-assigned external address, or the zero value when there is none.
func (m *NATManager) PortMapping() netip.AddrPort {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mapped
}

func (m *NATManager) GetNATProfile() (string, NATType) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.publicAddr, m.natType
}

// Report returns the latest classification.
func (m *NATManager) Report() natcheck.Report {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.report
}

// CanBeRouter: others can open connections to us unprompted, because we have a public address or
// the router forwards a port to us. A cone NAT alone is not enough: it lets the world answer us
// but not call us.
func (m *NATManager) CanBeRouter() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.natType == NATPublic || m.mapped.IsValid()
}

// CanBePunched: a direct connection to us can be punched through the NAT, that is, our public
// address is the same whoever we talk to, or the router forwards our port. Unknown counts as maybe:
// it is worth trying.
func (m *NATManager) CanBePunched() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// a port the router forwards is one fixed public address, whatever the NAT does with others:
	// the other side's packets get in there
	return m.natType != NATSymmetric || m.mapped.IsValid()
}

// Candidates are the public addresses another peer can aim at to reach us, best first.
func (m *NATManager) Candidates() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	if m.mapped.IsValid() {
		out = append(out, m.mapped.String())
	}
	if m.publicAddr != "" && (!m.mapped.IsValid() || m.mapped.String() != m.publicAddr) {
		out = append(out, m.publicAddr)
	}
	return out
}

// Discover re-checks the NAT. Connected peers are asked first which address they see us at (our
// own nodes, so nothing goes to a third party); public STUN servers are asked as well unless
// enough peers agree (see safeObservers), and only when STUN runs on the QUIC socket and the node
// allows it. The answers are classified together. An answer no majority backs (a tie), or in guard
// mode one that too few peers back, leaves the previous picture in place.
func (m *NATManager) Discover() (string, error) {
	m.mu.RLock()
	conn, servers, observe, allowed := m.stunConn, m.servers, m.observe, m.stunAllowed
	m.mu.RUnlock()
	stunOK := allowed == nil || allowed()
	local := natcheck.LocalAddrs()

	var obs []natcheck.Observation
	if observe != nil {
		octx, cancel := context.WithTimeout(context.Background(), 2*stunTimeout)
		obs = append(obs, observe(octx)...)
		cancel()
	}
	fromPeers := publicObservers(obs)
	r := natcheck.Classify(obs, local)
	if peersNotEnough(fromPeers, r) && conn != nil && stunOK {
		m.mu.Lock()
		m.stunUsed++
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*stunTimeout)
		defer cancel()
		if got, err := natcheck.QuerySTUN(ctx, conn, servers, stunTimeout); err == nil {
			obs = append(obs, got...)
			r = natcheck.Classify(obs, local)
		} else {
			m.logger.Debug("STUN discovery failed", "err", err)
		}
	}
	m.mu.Lock()
	m.peersSeen = fromPeers > 0
	m.mu.Unlock()
	if len(obs) == 0 {
		return "", errors.New("no peer and no STUN server reported our address (is UDP blocked, or are we offline?)")
	}

	t := NATUnknown
	switch {
	case r.NoNAT:
		t = NATPublic
	case r.Mapping == natcheck.MappingStable:
		t = NATFullCone
	case r.Mapping == natcheck.MappingVaries:
		t = NATSymmetric
	}

	pub := ""
	if r.Public.Port() != 0 {
		pub = r.Public.String()
	} // else only peers on other sockets (REALITY) saw this IP: no port for invitations or punching
	m.mu.Lock()
	prevAddr, prevType := m.publicAddr, m.natType
	if keep := !r.Public.IsValid() || (!stunOK && !r.Agreed(guardObservers)); keep {
		// no majority, or in guard mode too few peers behind it: one or two lying peers must not
		// set our address (it goes into invitations and punch targets, and places the node in a
		// country, which can stand guard mode down)
		m.mu.Unlock()
		m.logger.Debug("NAT check not conclusive, keeping the previous picture", "public", prevAddr,
			"agree", r.Agree, "observers", r.Observers)
		return prevAddr, nil
	}
	m.report, m.natType, m.publicAddr, m.publicIP = r, t, pub, r.Public.Addr().String()
	m.mu.Unlock()

	if prevType != t || prevAddr != pub {
		m.logger.Info("NAT status", "public", pub, "type", t.String(), "mapping", r.Mapping.String(), "observers", len(obs))
		if t == NATSymmetric {
			m.logger.Warn("Symmetric NAT: peers behind NATs cannot connect to us directly; we are reachable only through relays or outbound connections")
		}
	}
	return pub, nil
}

// StartKeepAlive refreshes the picture until ctx ends. While no peer is connected, the ticks in
// between also send a STUN request on the QUIC socket, which keeps its NAT mapping open.
func (m *NATManager) StartKeepAlive(ctx context.Context) {
	// every wait is drawn anew around KeepAliveInterval: a STUN request from the same port every
	// exact 20 s is a clock anyone on the path can read
	timer := time.NewTimer(jitter.Around(KeepAliveInterval, jitter.Default))
	defer timer.Stop()
	m.logger.Info("NAT Keep-Alive started", "interval", KeepAliveInterval)

	// the first answer is wanted quickly, not after a full interval
	if _, err := m.Discover(); err != nil {
		m.logger.Debug("first NAT check failed", "err", err)
	}

	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			m.logger.Info("NAT Keep-Alive stopped")
			return
		case <-timer.C:
			timer.Reset(jitter.Around(KeepAliveInterval, jitter.Default))
			if tick%classifyEvery == 0 {
				if _, err := m.Discover(); err != nil {
					m.logger.Warn("NAT check failed", "err", err)
				}
				continue
			}
			m.ping(ctx)
		}
	}
}

// ping keeps the mapping alive with one cheap STUN request, when nothing else does: with peers
// connected over the QUIC socket their keep-alives hold the mapping open, and no STUN goes out.
func (m *NATManager) ping(ctx context.Context) {
	m.mu.RLock()
	conn, servers, peers, keepers, allowed := m.stunConn, m.servers, m.peersSeen, m.keepers, m.stunAllowed
	m.mu.RUnlock()
	if keepers != nil {
		peers = keepers() > 0
	}
	if conn == nil || len(servers) == 0 || peers || (allowed != nil && !allowed()) {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, stunTimeout)
	defer cancel()
	if _, err := natcheck.QuerySTUN(pctx, conn, servers[:1], stunTimeout); err != nil {
		m.logger.Debug("NAT keep-alive ping failed", "err", err)
	}
}

// peersNotEnough: what the peers said (fromPeers of them with a public address) is too little or
// too doubtful to go without STUN.
func peersNotEnough(fromPeers int, r natcheck.Report) bool {
	return fromPeers < minPeerObservers || !r.Agreed(safeObservers) || r.Mapping != natcheck.MappingStable
}

// publicObservers counts the observations of our QUIC socket with a public IPv4 address, one per
// observer: the ones that tell the IPv4 mapping.
func publicObservers(obs []natcheck.Observation) int {
	seen := map[string]bool{}
	for _, o := range obs {
		a := o.Addr.Addr().Unmap()
		if o.Addr.IsValid() && o.Addr.Port() != 0 && !o.IPOnly && a.Is4() && natcheck.PublicAddr(a) {
			seen[o.From] = true
		}
	}
	return len(seen)
}

// STUNChecks is how many checks asked public STUN servers so far.
func (m *NATManager) STUNChecks() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stunUsed
}

// PortRange is where our NAT was seen to take its public ports from (the lowest and highest port
// observed on our public IPv4 address) and how many different ports were seen. Behind a symmetric
// NAT a peer aims its birthday punch there.
func (m *NATManager) PortRange() (lo, hi uint16, n int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.report.PortLo, m.report.PortHi, m.report.Ports
}

func (m *NATManager) GetPublicAddr() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.publicAddr
}

// PublicIP is our public IP as the observers agree on it, also when only peers connected through
// other sockets (REALITY dials) saw it and GetPublicAddr has no port to go with it. It is what
// places the node in a country. Empty while not known.
func (m *NATManager) PublicIP() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.publicIP
}

func (m *NATManager) String() string {
	a, t := m.GetNATProfile()
	return fmt.Sprintf("%s (%s)", a, t)
}
