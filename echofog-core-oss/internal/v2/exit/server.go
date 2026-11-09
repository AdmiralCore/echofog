package exit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/pkg/api/types"
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

// Timeouts of services.md §7.
const (
	tcpConnectTimeout = 10 * time.Second
	udpOpenTimeout    = 5 * time.Second
	accountTimeout    = 5 * time.Second
	voucherTimeout    = 20 * time.Second
)

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
	// Vouchers is the v1 voucher book, when the exit takes them (T10.3 owns the payment flow).
	Vouchers *core.VoucherBook
	// VaultCodes is the allow-list of vault code hashes (hex). Empty accepts any code the book
	// knows; a public exit must set it.
	VaultCodes map[string]bool
	Limits     Limits
}

type sysClock struct{}

func (sysClock) Now() time.Time                                    { return time.Now() }
func (sysClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

// Stats counts what the exit served and dropped.
type Stats struct {
	Accepted             atomic.Uint64
	Refused              atomic.Uint64
	RateLimited          atomic.Uint64
	UDPAccepted          atomic.Uint64
	DatagramsDropped     atomic.Uint64
	FragmentsReassembled atomic.Uint64
	Violations           atomic.Uint64
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

	accessMu sync.RWMutex
	admit    func(Peer) bool
	refused  func(Peer)
	denied   atomic.Uint64
	liveMu   sync.Mutex
	live     map[*liveStream]struct{}

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
	// The v2 limit is per client identity and normative (services.md §7); the v1 default for UDP
	// sessions is 32, so raise it to the v2 value while keeping the rest of the operator's limits.
	coreLimits := cfg.Core.CurrentLimits()
	coreLimits.UDPPerPeer = cfg.Limits.UDPSessions
	cfg.Core.SetLimits(coreLimits)
	cfg.Core.AllowPrivateTargets(cfg.AllowPrivateTargets)
	if cfg.SelfIPs != nil {
		cfg.Core.SetSelfIPs(cfg.SelfIPs)
	}
	if cfg.Vouchers != nil {
		cfg.Core.SetVouchers(cfg.Vouchers)
	}
	if err := cfg.Core.SetDNS(cfg.DNS, false); err != nil {
		return nil, fmt.Errorf("exit DNS: %w", err)
	}
	// A test resolver goes after SetDNS: the production path is DoH only.
	if cfg.Resolver != nil {
		cfg.Core.SetResolver(cfg.Resolver)
	}
	return &Server{cfg: cfg, clk: cfg.Clock, core: cfg.Core,
		tcp:     newTCPLimiter(cfg.Limits.TCPPerSecond, cfg.Limits.TCPTargetsPerMin),
		packet:  newPacketLimiter(cfg.Limits.UDPPacketsPerSec),
		udpByID: map[udpKey]*udpAdapter{}, udpCount: map[[32]byte]int{}, live: map[*liveStream]struct{}{}}, nil
}

// SetAccess limits the exit to the clients f admits (by node); nil admits every client. A client it
// does not admit is refused as if the exit were off, which the client reads as a policy refusal.
func (s *Server) SetAccess(admit func(Peer) bool, refused func(Peer)) {
	s.accessMu.Lock()
	s.admit, s.refused = admit, refused
	s.accessMu.Unlock()
}

// admitted says whether the client may be served now, without telling anyone.
func (s *Server) admitted(p Peer) bool {
	if !s.core.Enabled() {
		return false
	}
	s.accessMu.RLock()
	f := s.admit
	s.accessMu.RUnlock()
	return f == nil || f(p)
}

type liveStream struct {
	p      Peer
	st     *mux.Stream
	cancel context.CancelFunc
}

func (l *liveStream) end() {
	l.cancel()
	_ = l.st.CloseWithCode(CodePolicy, "exit access ended")
}

// track registers an open stream so that Recheck and SetEnabled can end it.
func (s *Server) track(ctx context.Context, p Peer, st *mux.Stream) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	l := &liveStream{p: p, st: st, cancel: cancel}
	s.liveMu.Lock()
	s.live[l] = struct{}{}
	s.liveMu.Unlock()
	return ctx, func() {
		s.liveMu.Lock()
		delete(s.live, l)
		s.liveMu.Unlock()
		cancel()
	}
}

// Recheck closes the open streams of the clients that are no longer admitted.
func (s *Server) Recheck() {
	s.liveMu.Lock()
	var gone []*liveStream
	for l := range s.live {
		if !s.admitted(l.p) {
			gone = append(gone, l)
		}
	}
	s.liveMu.Unlock()
	for _, l := range gone {
		l.end()
	}
}

// deny answers a first frame of a client that is not served, the way an exit that is off does: a
// policy refusal that says nothing about why.
func (s *Server) deny(st *mux.Stream, f frame.Frame) {
	switch f.Type {
	case TypeTCPConnect:
		writeResult(st, ResultDenied)
		_ = st.Close()
	case TypeUDPOpen:
		if u, err := ParseUDPOpen(f.Payload); err == nil {
			_ = st.WriteFrame(TypeUDPOK, 0, UDPOK{Req: u.Req, Code: ResultDenied}.Append(nil))
		}
		_ = st.Close()
	case TypeAccountGet, TypeVoucher:
		_ = st.CloseWithCode(CodePolicy, "denied")
	default:
		s.violation(st, "unexpected frame")
	}
}

// SetEnabled turns the exit on or off. Turning it off refuses new streams and closes the live ones.
func (s *Server) SetEnabled(on bool) {
	s.core.SetEnabled(on)
	if on {
		return
	}
	s.liveMu.Lock()
	all := make([]*liveStream, 0, len(s.live))
	for l := range s.live {
		all = append(all, l)
	}
	s.liveMu.Unlock()
	for _, l := range all {
		l.end()
	}
}

// LiveClient is a client with streams open right now.
type LiveClient struct {
	Node    [32]byte
	Streams int
}

// Live lists the clients with streams open, by node.
func (s *Server) Live() []LiveClient {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	by := map[[32]byte]int{}
	for l := range s.live {
		by[l.p.Node]++
	}
	out := make([]LiveClient, 0, len(by))
	for n, c := range by {
		out = append(out, LiveClient{Node: n, Streams: c})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].Node[:], out[j].Node[:]) < 0 })
	return out
}

// Totals returns the tunnels served, the tunnels refused and the bytes relayed.
func (s *Server) Totals() (accepted, refused, bytes uint64) {
	a, r, b := s.core.Stats()
	return a, r + s.denied.Load(), b
}

// RefusedBy counts the refusals by reason.
func (s *Server) RefusedBy() map[string]uint64 {
	out := s.core.RefusedByReason()
	if out == nil {
		out = map[string]uint64{}
	}
	if n := s.denied.Load(); n > 0 {
		out["policy"] += n
	}
	return out
}

// Stats returns the counters.
func (s *Server) Stats() *Stats { return &s.stats }

// Handle serves one exit/2 stream opened by an authenticated peer.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	st.SetLimiter(limiter())
	f, err := s.readWithin(st, tcpConnectTimeout)
	if err != nil {
		s.violation(st, "first frame")
		return
	}
	if !s.admitted(p) {
		s.denied.Add(1)
		s.accessMu.RLock()
		refused := s.refused
		s.accessMu.RUnlock()
		if refused != nil && s.core.Enabled() {
			refused(p)
		}
		s.deny(st, f)
		return
	}
	ctx, done := s.track(ctx, p, st)
	defer done()
	switch f.Type {
	case TypeTCPConnect:
		s.handleTCP(ctx, p, st, f.Payload)
	case TypeUDPOpen:
		s.handleUDP(ctx, p, st, f.Payload)
	case TypeAccountGet:
		s.handleAccount(ctx, p, st, f.Payload)
	case TypeVoucher:
		s.handleVoucher(ctx, p, st, f.Payload)
	default:
		// Every other frame is valid only on a stream the exit opened.
		s.violation(st, "unexpected frame")
	}
}

// readWithin reads the next frame, giving up after d of the injected clock. The transport has its
// own (real) deadline; the clock only decides when to ask for one.
func (s *Server) readWithin(st *mux.Stream, d time.Duration) (frame.Frame, error) {
	var mu sync.Mutex
	finished := false
	tm := s.clk.AfterFunc(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_ = st.SetReadDeadline(time.Now())
		}
	})
	f, err := st.ReadFrame()
	tm.Stop()
	mu.Lock()
	finished = true
	if err == nil {
		_ = st.SetReadDeadline(time.Time{})
	}
	mu.Unlock()
	return f, err
}

func (s *Server) violation(st *mux.Stream, why string) {
	s.stats.Violations.Add(1)
	_ = st.CloseWithCode(frame.CodeProtocolViolation, "exit: "+why)
}

func limiter() frame.Limiter {
	sp, ok := service.Builtin(service.Exit)
	if !ok {
		return nil
	}
	return sp.Versions[2].Limiter()
}

// handleTCP opens one TCP tunnel. The v1 core validates and resolves the target, applies the
// quotas and accounting, and splices the bytes; the adapter translates frames.
func (s *Server) handleTCP(ctx context.Context, p Peer, st *mux.Stream, payload []byte) {
	c, err := ParseTCPConnect(payload)
	if err != nil {
		s.violation(st, "tcp_connect")
		return
	}
	target := net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port)))
	if !s.tcp.allow(s.clk.Now(), p.Node, target) {
		s.stats.RateLimited.Add(1)
		writeResult(st, ResultRateLimited)
		_ = st.Close()
		return
	}
	a := newTCPAdapter(st, s.cfg.Shape, s.cfg.Sleep, s.clk)
	a.arm(tcpConnectTimeout)
	s.stats.Accepted.Add(1)
	s.core.HandleTunnelFrom(ctx, a, []byte(target), types.PeerID(p.Node))
}

// handleUDP opens one UDP tunnel. Data travels as EXIT_UDP datagrams when the session supports
// them, and as UDP_PKT frames on the stream otherwise.
func (s *Server) handleUDP(ctx context.Context, p Peer, st *mux.Stream, payload []byte) {
	u, err := ParseUDPOpen(payload)
	if err != nil {
		s.violation(st, "udp_open")
		return
	}
	if !s.acquireUDP(p.Node) {
		s.stats.Refused.Add(1)
		_ = st.WriteFrame(TypeUDPOK, 0, UDPOK{Req: u.Req, Code: ResultBusy}.Append(nil))
		_ = st.Close()
		return
	}
	release := func() { s.releaseUDP(p.Node) }
	a, sessionID, ok := s.addUDP(p.Node, st, p, u.Req)
	if !ok {
		release()
		_ = st.WriteFrame(TypeUDPOK, 0, UDPOK{Req: u.Req, Code: ResultBusy}.Append(nil))
		_ = st.Close()
		return
	}
	defer s.forgetUDP(p.Node, sessionID)
	defer release()
	a.arm(udpOpenTimeout)
	s.stats.UDPAccepted.Add(1)
	s.core.HandleUDP(ctx, a, types.PeerID(p.Node), nil)
}

func (s *Server) handleAccount(_ context.Context, p Peer, st *mux.Stream, payload []byte) {
	if len(payload) != 0 {
		s.violation(st, "account_get")
		return
	}
	peer := types.PeerID(p.Node)
	if !s.core.AllowQuery(peer) {
		s.stats.Refused.Add(1)
		_ = st.CloseWithCode(CodePolicy, "account rate limit")
		return
	}
	a := accountFrom(s.core.AccountOf(peer))
	_ = st.WriteFrame(TypeAccount, 0, a.Append(nil))
	_ = st.Close()
}

func accountFrom(info core.AccountInfo) Account {
	a := Account{
		Credit:     uint64(max(0, info.Credit)),
		FreeLeft:   uint64(max(0, info.FreeLeft)),
		PayAddress: info.PayAddress,
		PayTag:     info.Tag,
		MinPay:     uint64(max(0, info.MinPayMicro)),
	}
	if info.PricePerMB > 0 {
		a.Price = uint64(info.PricePerMB)
	}
	for _, asset := range info.Assets {
		switch asset {
		case "USDT":
			a.Assets = append(a.Assets, 1)
		case "TON":
			a.Assets = append(a.Assets, 2)
		}
	}
	return a
}

func (s *Server) handleVoucher(ctx context.Context, p Peer, st *mux.Stream, payload []byte) {
	v, err := ParseVoucher(payload)
	if err != nil {
		s.violation(st, "voucher")
		return
	}
	res := VoucherResult{Req: v.Req}
	defer func() {
		_ = st.WriteFrame(TypeVoucherResult, 0, res.Append(nil))
		_ = st.Close()
	}()
	if s.cfg.Vouchers == nil {
		res.Error = 1
		return
	}
	if len(s.cfg.VaultCodes) > 0 && !s.cfg.VaultCodes[hex.EncodeToString(v.CodeHash)] {
		res.Error = 1
		return
	}
	owner, ok := rawOwnerAddress(v.Owner)
	if !ok || len(v.PubKey) != 32 || len(v.Sig) != 64 || len(v.Cumulative) == 0 || len(v.Cumulative) > 16 {
		res.Error = 1
		return
	}
	offer := core.VoucherOffer{
		Version:    1,
		Owner:      owner,
		PubKey:     hex.EncodeToString(v.PubKey),
		Cumulative: new(big.Int).SetBytes(v.Cumulative).String(),
		Sig:        hex.EncodeToString(v.Sig),
	}
	cctx, cancel := context.WithCancel(ctx)
	tm := s.clk.AfterFunc(voucherTimeout, cancel)
	reply := s.cfg.Vouchers.Accept(cctx, s.core.AccountOf(types.PeerID(p.Node)).Tag, offer)
	tm.Stop()
	cancel()
	res.OK = reply.OK
	res.Credit = uint64(max(0, reply.Credit))
	if cum, ok := new(big.Int).SetString(reply.Cumulative, 10); ok && cum.Sign() > 0 {
		res.Cumulative = cum.Bytes()
	}
	if !reply.OK {
		res.Error = 1
	}
}

// rawOwnerAddress converts the 32-byte vault owner or the 36-byte raw address (workchain ‖ hash)
// to the text form the v1 voucher book parses.
func rawOwnerAddress(b []byte) (string, bool) {
	switch len(b) {
	case 32:
		return "0:" + hex.EncodeToString(b), true
	case 36:
		wc := int32(binary.BigEndian.Uint32(b[:4]))
		return strconv.Itoa(int(wc)) + ":" + hex.EncodeToString(b[4:]), true
	}
	return "", false
}

// acquireUDP counts one active UDP session of p.Node against the §7 limit.
func (s *Server) acquireUDP(node [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udpCount[node] >= s.cfg.Limits.UDPSessions {
		return false
	}
	s.udpCount[node]++
	return true
}

func (s *Server) releaseUDP(node [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udpCount[node]--; s.udpCount[node] <= 0 {
		delete(s.udpCount, node)
	}
}

// addUDP draws an unused session id and registers the adapter. The id is part of every EXIT_UDP
// datagram of this tunnel, and the client can only reach its own sessions (keyed by Node).
func (s *Server) addUDP(node [32]byte, st *mux.Stream, p Peer, req uint64) (*udpAdapter, uint32, bool) {
	for i := 0; i < 8; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, 0, false
		}
		id := binary.BigEndian.Uint32(b[:])
		if id == 0 {
			continue
		}
		a := newUDPAdapter(st, p, req, id, s.cfg, s.clk)
		s.mu.Lock()
		k := udpKey{node: node, id: id}
		if _, used := s.udpByID[k]; used {
			s.mu.Unlock()
			continue
		}
		s.udpByID[k] = a
		s.mu.Unlock()
		return a, id, true
	}
	return nil, 0, false
}

func (s *Server) lookupUDP(node [32]byte, id uint32) *udpAdapter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.udpByID[udpKey{node: node, id: id}]
}

func (s *Server) forgetUDP(node [32]byte, id uint32) {
	s.mu.Lock()
	delete(s.udpByID, udpKey{node: node, id: id})
	s.mu.Unlock()
}

// HandleDatagram serves one EXIT_UDP datagram received on the session of from.
func (s *Server) HandleDatagram(from Peer, payload []byte) {
	if len(payload) == 0 {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	if !s.packet.allow(s.clk.Now(), from.Node) {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	f, err := ParseFragment(payload)
	if err != nil || f.Session > math.MaxUint32 {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	a := s.lookupUDP(from.Node, uint32(f.Session))
	if a == nil {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	addr, data, ok := a.frags.feed(f)
	if !ok {
		return
	}
	s.stats.FragmentsReassembled.Add(1)
	a.deliver(addr, data)
}

// writeResult writes a RESULT frame, ignoring the error: the stream is about to end anyway.
func writeResult(st *mux.Stream, code uint32) {
	_ = st.WriteFrame(TypeResult, 0, Result{Code: code}.Append(nil))
}
