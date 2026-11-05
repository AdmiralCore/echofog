package exit

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// Peer is the authenticated session that opened a stream or sent a datagram to the exit. It comes
// from the session, never from a frame (§7: limits are per client identity, Peer.Node).
type Peer struct {
	Node         [32]byte
	Device       [32]byte
	SID          [16]byte
	Remote       netip.AddrPort
	SendDatagram func(typ uint64, payload []byte) error
}

// Limits are the per-client-identity limits of services.md §7. Zero fields take the
// specification values; tests lower them.
type Limits struct {
	TCPPerSecond     int
	TCPTargetsPerMin int
	UDPSessions      int
	UDPTargets       int
	UDPPacketsPerSec int
}

func (l Limits) fill() Limits {
	def := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	def(&l.TCPPerSecond, DefaultTCPPerSecond)
	def(&l.TCPTargetsPerMin, DefaultTCPTargetsPerMin)
	def(&l.UDPSessions, DefaultUDPSessions)
	def(&l.UDPTargets, DefaultUDPTargets)
	def(&l.UDPPacketsPerSec, DefaultUDPPacketsPerSec)
	return l
}

// ServerConfig configures an exit server.
type ServerConfig struct {
	// Core is the v1 exit domain. Nil creates a free exit with the default limits.
	Core *core.ExitService
	// Clock is the injected clock (AD13). Nil means the system clock.
	Clock carrier.Clock
	// Sleep is how the H1 jitter waits; nil means time.Sleep. Tests inject a no-op.
	Sleep func(time.Duration)
	// Shape is the address shape profile used to pad the first records of a new tunnel (H1).
	Shape *shape.Profile
	// Resolver replaces the DoH resolver (tests). It must resolve names, never use the host's
	// plaintext resolver in production.
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
	// SelfIPs reports the exit's own public addresses. A client reaching one of them would come
	// back through the operator's router.
	SelfIPs func() []net.IP
	// DNS is the DoH server list (services.md §7). Nil means core.DefaultExitDNS.
	DNS []string
	// AllowPrivateTargets is for tests and closed deployments only.
	AllowPrivateTargets bool
	// PricePerMB and FreeBytes configure the core exit policy. Zero price is a free exit.
	PricePerMB int64
	FreeBytes  int64
	Limits     Limits
}

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }
func (sysClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

// Stats counts what the exit served and dropped.
type Stats struct {
	Accepted          atomic.Uint64
	Refused           atomic.Uint64
	RateLimited       atomic.Uint64
	UDPAccepted       atomic.Uint64
	DatagramsDropped  atomic.Uint64
	FragmentsReassembled atomic.Uint64
	Violations        atomic.Uint64
}

// Server is exit/2 on one node.
type Server struct {
	cfg  ServerConfig
	clk  carrier.Clock
	core *core.ExitService

	tcp    *tcpLimiter
	packet *packetLimiter

	mu       sync.Mutex
	udpByID  map[udpKey]*udpAdapter
	udpCount map[[32]byte]int

	stats Stats
}

type udpKey struct {
	node [32]byte
	id   uint32
}

// NewServer returns an exit server.
func NewServer(cfg ServerConfig) (*Server, error) {
	cfg.Limits = cfg.Limits.fill()
	if cfg.Clock == nil {
		cfg.Clock = sysClock{}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Core == nil {
		cfg.Core = core.NewExitService()
	}
	cfg.Core.SetEnabled(true)
	cfg.Core.SetPolicy(core.Policy{PricePerMB: cfg.PricePerMB, FreeBytes: cfg.FreeBytes})
	cfg.Core.AllowPrivateTargets(cfg.AllowPrivateTargets)
	if cfg.Resolver != nil {
		cfg.Core.SetResolver(cfg.Resolver)
	}
	if cfg.SelfIPs != nil {
		cfg.Core.SetSelfIPs(cfg.SelfIPs)
	}
	return &Server{cfg: cfg, clk: cfg.Clock, core: cfg.Core,
		tcp:    newTCPLimiter(cfg.Limits.TCPPerSecond, cfg.Limits.TCPTargetsPerMin),
		packet: newPacketLimiter(cfg.Limits.UDPPacketsPerSec),
		udpByID: map[udpKey]*udpAdapter{}, udpCount: map[[32]byte]int{}}, nil
}

// Stats returns the counters.
func (s *Server) Stats() *Stats { return &s.stats }

// Handle serves one exit/2 stream opened by an authenticated peer.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {}

// HandleDatagram serves one EXIT_UDP datagram received on the session of from.
func (s *Server) HandleDatagram(from Peer, payload []byte) {}

// limiter returns the frame limiter of exit/2 version 2.
func limiter() frameLimiter { return nil }

type frameLimiter = func(typ uint64) (int, bool)
