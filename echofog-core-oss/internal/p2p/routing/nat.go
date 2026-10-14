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
	logger   *slog.Logger

	mu         sync.RWMutex
	report     natcheck.Report
	natType    NATType
	publicAddr string
	mapped     netip.AddrPort // external address of a router port mapping, if any
}

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

// SetPortMapping records the router-assigned external address (zero clears it).
func (m *NATManager) SetPortMapping(addr netip.AddrPort) {
	m.mu.Lock()
	m.mapped = addr
	m.mu.Unlock()
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
// address is the same whoever we talk to. Unknown counts as maybe: it is worth trying.
func (m *NATManager) CanBePunched() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.natType != NATSymmetric
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

// Discover re-checks the NAT: STUN servers (when STUN runs on the QUIC socket) and connected
// peers each report the address they see, and the answers are classified together.
func (m *NATManager) Discover() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*stunTimeout)
	defer cancel()

	m.mu.RLock()
	conn, servers, observe := m.stunConn, m.servers, m.observe
	m.mu.RUnlock()

	var obs []natcheck.Observation
	if conn != nil {
		if got, err := natcheck.QuerySTUN(ctx, conn, servers, stunTimeout); err == nil {
			obs = append(obs, got...)
		} else {
			m.logger.Debug("STUN discovery failed", "err", err)
		}
	}
	if observe != nil {
		obs = append(obs, observe(ctx)...)
	}
	if len(obs) == 0 {
		return "", errors.New("no STUN server and no peer reported our address (is UDP blocked, or are we offline?)")
	}

	r := natcheck.Classify(obs, natcheck.LocalAddrs())
	t := NATUnknown
	switch {
	case r.NoNAT:
		t = NATPublic
	case r.Mapping == natcheck.MappingStable:
		t = NATFullCone
	case r.Mapping == natcheck.MappingVaries:
		t = NATSymmetric
	}

	pub := r.Public.String()
	m.mu.Lock()
	prevAddr, prevType := m.publicAddr, m.natType
	m.report, m.natType, m.publicAddr = r, t, pub
	m.mu.Unlock()

	if prevType != t || prevAddr != pub {
		m.logger.Info("NAT status", "public", pub, "type", t.String(), "mapping", r.Mapping.String(), "observers", len(obs))
		if t == NATSymmetric {
			m.logger.Warn("Symmetric NAT: peers behind NATs cannot connect to us directly; we are reachable only through relays or outbound connections")
		}
	}
	return pub, nil
}

// StartKeepAlive refreshes the picture until ctx ends. Every tick also sends a STUN request on the
// QUIC socket, which keeps its NAT mapping open even when no peer is connected.
func (m *NATManager) StartKeepAlive(ctx context.Context) {
	ticker := time.NewTicker(KeepAliveInterval)
	defer ticker.Stop()
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
		case <-ticker.C:
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

// ping keeps the mapping alive with one cheap STUN request.
func (m *NATManager) ping(ctx context.Context) {
	m.mu.RLock()
	conn, servers := m.stunConn, m.servers
	m.mu.RUnlock()
	if conn == nil || len(servers) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, stunTimeout)
	defer cancel()
	if _, err := natcheck.QuerySTUN(pctx, conn, servers[:1], stunTimeout); err != nil {
		m.logger.Debug("NAT keep-alive ping failed", "err", err)
	}
}

func (m *NATManager) GetPublicAddr() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.publicAddr
}

func (m *NATManager) String() string {
	a, t := m.GetNATProfile()
	return fmt.Sprintf("%s (%s)", a, t)
}
