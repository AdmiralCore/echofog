package relay

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// Limits are the relay side limits of services.md §6.5. Zero fields take the specification
// values; tests lower them.
type Limits struct {
	PerSession    int           // circuits per session (16)
	PerIP         int           // circuits per source IP (32)
	Total         int           // circuits in total (200)
	Idle          time.Duration // idle circuit (5 min)
	PerCircuit    uint64        // bytes per circuit (4 GiB)
	PerPeerDay    uint64        // bytes per peer per day (16 GiB)
	RateBytes     int64         // bytes per second per peer (50 Mbit/s)
	MaxRendezvous int           // rendezvous IDs per node (256)
	// MemCapMB is the node's mem_cap_mb: a positive value allows at most 16 circuits (T6.1).
	MemCapMB    int
	SignalBytes uint64        // signal circuit size (64 KiB)
	SignalTime  time.Duration // signal circuit lifetime (2 min)
}

func (l *Limits) fill() {
	def := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	def(&l.PerSession, 16)
	def(&l.PerIP, 32)
	def(&l.Total, 200)
	def(&l.MaxRendezvous, MaxRegisterIDs)
	if l.MemCapMB > 0 {
		l.PerSession, l.Total = min(l.PerSession, 16), min(l.Total, 16)
	}
	if l.Idle == 0 {
		l.Idle = 5 * time.Minute
	}
	if l.PerCircuit == 0 {
		l.PerCircuit = 4 << 30
	}
	if l.PerPeerDay == 0 {
		l.PerPeerDay = 16 << 30
	}
	if l.RateBytes == 0 {
		l.RateBytes = 50_000_000 / 8
	}
	if l.SignalBytes == 0 {
		l.SignalBytes = 64 << 10
	}
	if l.SignalTime == 0 {
		l.SignalTime = 2 * time.Minute
	}
}

// Peer is the authenticated session that sent a stream or datagram to R. Everything in it comes
// from R's own session with the peer, never from a frame (K8: v1 trusted an unsigned "source").
type Peer struct {
	Node   [32]byte
	Device [32]byte
	SID    [16]byte
	Remote netip.AddrPort
	// SendDatagram sends a datagram on the session with this peer.
	SendDatagram func(typ uint64, payload []byte) error
}

// Target is a node connected to R that may receive circuits.
type Target interface {
	// OpenStream opens a relay/2 stream on R's session with the node.
	OpenStream(ctx context.Context) (*mux.Stream, error)
	// Listed is false for nodes whose card is unlisted: they are never served by node ID.
	Listed() bool
	// Peer identifies the session.
	Peer() Peer
}

// Directory finds the nodes connected to R.
type Directory interface {
	Lookup(node [32]byte) (Target, bool)
}

// ServerConfig configures a Server.
type ServerConfig struct {
	Directory Directory
	Clock     carrier.Clock
	// Delay draws the uniform(1, 4) s of a refusal; tests inject a short one.
	Delay func() time.Duration
	// ConnectTimeout bounds the wait for CONNECT (8 s) and REGISTER (5 s).
	ConnectTimeout  time.Duration
	RegisterTimeout time.Duration
	Limits          Limits
}

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }
func (sysClock) AfterFunc(d time.Duration, f func()) carrier.Timer {
	return time.AfterFunc(d, f)
}

type end struct {
	peer   Peer
	stream *mux.Stream
	ipKey  netip.Addr
}

type circuit struct {
	cid     uint64
	mode    uint32
	a, b    end
	created time.Time
	bytes   atomic.Uint64
	last    atomic.Int64
	done    chan struct{}
	once    sync.Once
}

func (c *circuit) other(sid [16]byte) (end, bool) {
	switch sid {
	case c.a.peer.SID:
		return c.b, true
	case c.b.peer.SID:
		return c.a, true
	}
	return end{}, false
}

// Server is relay/2 on R.
type Server struct {
	cfg ServerConfig
	clk carrier.Clock

	mu       sync.Mutex
	circuits map[uint64]*circuit
	active   int // reserved or live circuits
	perSess  map[[16]byte]int
	perIP    map[netip.Addr]int
	usedCIDs map[uint64]struct{}
	cidOrder []cidStamp
	day      int64
	dayBytes map[[32]byte]uint64
	buckets  map[[32]byte]*bucket
	// Rendezvous registrations (AD19): their lifetime lives in one index, rdvDays. rdvOf holds
	// each node's IDs with their day and is the only source of a node's count; rdv is the lookup.
	// Only expireLocked and register/forget change them, so each registration is counted once
	// and uncounted once.
	rdv     map[[32]byte][][32]byte // rendezvous ID → registered nodes
	rdvOf   map[[32]byte]map[[32]byte]uint32
	rdvDays map[uint32]map[regKey]struct{} // day → registrations that expire when it ends

	stats Stats
	// tap, set by tests only, sees (and may change) every chunk R copies.
	tap func(cid uint64, sender [32]byte, b []byte)
}

type regKey struct{ node, rid [32]byte }

type cidStamp struct {
	cid uint64
	at  time.Time
}

// Stats counts what the server refused and dropped.
type Stats struct {
	Circuits         atomic.Int64
	Refused          atomic.Uint64
	LimitRefused     atomic.Uint64
	DatagramsDropped atomic.Uint64
	Violations       atomic.Uint64
	Forwarded        atomic.Uint64 // RELAY_DGRAM forwarded
	RegisterRejected atomic.Uint64 // REGISTER with a bad day or over the per-node limit
}

// NewServer returns a relay server.
func NewServer(cfg ServerConfig) *Server {
	cfg.Limits.fill()
	if cfg.Clock == nil {
		cfg.Clock = sysClock{}
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 8 * time.Second
	}
	if cfg.RegisterTimeout == 0 {
		cfg.RegisterTimeout = 5 * time.Second
	}
	if cfg.Delay == nil {
		cfg.Delay = uniformDelay
	}
	return &Server{cfg: cfg, clk: cfg.Clock,
		circuits: map[uint64]*circuit{}, perSess: map[[16]byte]int{}, perIP: map[netip.Addr]int{},
		usedCIDs: map[uint64]struct{}{}, dayBytes: map[[32]byte]uint64{}, buckets: map[[32]byte]*bucket{},
		rdv: map[[32]byte][][32]byte{}, rdvOf: map[[32]byte]map[[32]byte]uint32{},
		rdvDays: map[uint32]map[regKey]struct{}{}}
}

// Stats returns the counters.
func (s *Server) Stats() *Stats { return &s.stats }

// ActiveCircuits returns the number of live circuits.
func (s *Server) ActiveCircuits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.circuits)
}

// uniformDelay is uniform(1, 4) seconds (services.md §6.3).
func uniformDelay() time.Duration {
	n, err := crand.Int(crand.Reader, big.NewInt(3000))
	if err != nil {
		return 4 * time.Second
	}
	return time.Second + time.Duration(n.Int64())*time.Millisecond
}

func limiter() frame.Limiter {
	sp, _ := service.Builtin(service.Relay)
	return sp.Versions[Version].Limiter()
}

func (s *Server) violation(st *mux.Stream, why string) {
	s.stats.Violations.Add(1)
	_ = st.CloseWithCode(frame.CodeProtocolViolation, why)
}

// Handle serves one relay/2 stream opened by an authenticated peer.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	st.SetLimiter(limiter())
	f, err := s.readWithin(st, s.cfg.ConnectTimeout)
	if err != nil {
		st.Reset(frame.CodeProtocolViolation)
		return
	}
	switch f.Type {
	case TypeConnect:
		c, err := ParseConnect(f.Payload)
		if err != nil {
			s.violation(st, "connect")
			return
		}
		s.handleConnect(ctx, p, st, c)
	case TypeRegister:
		r, err := ParseRegister(f.Payload)
		if err != nil {
			s.violation(st, "register")
			return
		}
		s.handleRegister(p, st, r)
	default:
		// Everything else is only valid on a stream R opened itself.
		s.violation(st, "unexpected frame")
	}
}

// readWithin reads the next frame, giving up after d of the server clock. The transport has its
// own (real) deadlines; the injected clock only decides when to ask for one.
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

func (s *Server) wait(ctx context.Context, d time.Duration) bool {
	t := make(chan struct{})
	tm := s.clk.AfterFunc(d, func() { close(t) })
	defer tm.Stop()
	select {
	case <-t:
		return true
	case <-ctx.Done():
		return false
	}
}

// refuse answers REFUSED{unavailable} so that it leaves exactly `at` after the CONNECT, whatever
// the cause (§6.3).
func (s *Server) refuseAt(ctx context.Context, st *mux.Stream, c Connect, start time.Time, d time.Duration) {
	s.stats.Refused.Add(1)
	if rest := d - s.clk.Now().Sub(start); rest > 0 {
		if !s.wait(ctx, rest) {
			st.Reset(frame.CodePathFailed)
			return
		}
	}
	_ = st.WriteFrame(TypeRefused, 0, Refused{Req: c.Req, Target: c.Target, Reason: ReasonUnavailable}.Append(nil))
	_ = st.Close()
}

func (s *Server) refuseLimit(st *mux.Stream, c Connect) {
	s.stats.LimitRefused.Add(1)
	_ = st.WriteFrame(TypeRefused, 0, Refused{Req: c.Req, Target: c.Target, Reason: ReasonLimit}.Append(nil))
	_ = st.Close()
}

func (s *Server) resolve(a Peer, c Connect) (Target, bool) {
	var node [32]byte
	copy(node[:], c.Target)
	switch c.TargetKind {
	case TargetNode:
		t, ok := s.cfg.Directory.Lookup(node)
		if !ok || !t.Listed() {
			return nil, false
		}
		node = t.Peer().Node
		if node == a.Node {
			return nil, false
		}
		return t, true
	case TargetRendezvous:
		s.mu.Lock()
		s.expireLocked()
		nodes := append([][32]byte(nil), s.rdv[node]...)
		s.mu.Unlock()
		for _, n := range nodes {
			if t, ok := s.cfg.Directory.Lookup(n); ok && t.Peer().Node != a.Node {
				return t, true
			}
		}
	}
	return nil, false
}

// reserveRequester counts a circuit against the requester's session, source IP and the relay's
// total, before the target is looked up and whatever the target is. The limit is therefore
// answered the same way for present and absent targets (§6.3: a prober learns nothing).
func (s *Server) reserveRequester(a Peer) bool {
	ip := a.Remote.Addr().Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	lim := s.cfg.Limits
	if s.perSess[a.SID] >= lim.PerSession || s.perIP[ip] >= lim.PerIP || s.active >= lim.Total {
		return false
	}
	s.perSess[a.SID]++
	s.perIP[ip]++
	s.active++
	return true
}

// reserveTarget counts the circuit against the target's session. A full target is not
// distinguishable from an absent one (the caller answers "unavailable").
func (s *Server) reserveTarget(b Peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perSess[b.SID] >= s.cfg.Limits.PerSession {
		return false
	}
	s.perSess[b.SID]++
	return true
}

func (s *Server) release(c *circuit) {
	ip := c.a.peer.Remote.Addr().Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.circuits, c.cid)
	s.active--
	dec := func(sid [16]byte) {
		if s.perSess[sid]--; s.perSess[sid] <= 0 {
			delete(s.perSess, sid)
		}
	}
	dec(c.a.peer.SID)
	dec(c.b.peer.SID)
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

// newCID draws a random 64-bit circuit ID unused for 24 hours (§6.2).
func (s *Server) newCID() (uint64, error) {
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.cidOrder) > 0 && now.Sub(s.cidOrder[0].at) > 24*time.Hour {
		delete(s.usedCIDs, s.cidOrder[0].cid)
		s.cidOrder = s.cidOrder[1:]
	}
	for i := 0; i < 8; i++ {
		var b [8]byte
		if _, err := crand.Read(b[:]); err != nil {
			return 0, err
		}
		cid := binary.BigEndian.Uint64(b[:]) & MaxCID
		if _, used := s.usedCIDs[cid]; cid == 0 || used {
			continue
		}
		s.usedCIDs[cid] = struct{}{}
		s.cidOrder = append(s.cidOrder, cidStamp{cid, now})
		return cid, nil
	}
	return 0, errors.New("relay: no free circuit id")
}

func (s *Server) handleConnect(ctx context.Context, a Peer, st *mux.Stream, c Connect) {
	start := s.clk.Now()
	delay := s.cfg.Delay()
	if c.Mode < ModeStream || c.Mode > ModeSignal || c.TargetKind > TargetRendezvous {
		s.violation(st, "connect mode")
		return
	}
	if !s.reserveRequester(a) {
		s.refuseLimit(st, c)
		return
	}
	t, ok := s.resolve(a, c)
	if !ok {
		s.unreserveRequester(a)
		s.refuseAt(ctx, st, c, start, delay)
		return
	}
	bp := t.Peer()
	if !s.reserveTarget(bp) {
		s.unreserveRequester(a)
		s.refuseAt(ctx, st, c, start, delay)
		return
	}
	cid, err := s.newCID()
	if err != nil {
		s.unreserve(a, bp)
		s.refuseAt(ctx, st, c, start, delay)
		return
	}
	cir := &circuit{cid: cid, mode: c.Mode, created: start, done: make(chan struct{}),
		a: end{peer: a, stream: st}, b: end{peer: bp}}
	cir.last.Store(start.UnixNano())
	s.mu.Lock()
	s.circuits[cid] = cir
	s.mu.Unlock()
	s.stats.Circuits.Add(1)
	defer s.release(cir)

	// The wait for B ends at the refusal deadline: a target that is slow, silent, refusing or
	// unreachable looks the same to the requester as an absent one.
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if rest := delay - s.clk.Now().Sub(start); rest > 0 {
		defer s.clk.AfterFunc(rest, cancel).Stop()
	} else {
		cancel()
	}
	bst, ok := s.invite(bctx, t, cir, a, c)
	if !ok {
		s.refuseAt(ctx, st, c, start, delay)
		return
	}
	cir.b.stream = bst
	if err := st.WriteFrame(TypeConnected, 0, Connected{Req: c.Req, CID: cid, Target: c.Target}.Append(nil)); err != nil {
		bst.Reset(frame.CodePathFailed)
		return
	}
	s.run(ctx, cir)
}

func (s *Server) unreserve(a, b Peer) {
	s.release(&circuit{a: end{peer: a}, b: end{peer: b}})
}

// unreserveRequester returns the slot taken by reserveRequester when no circuit follows.
func (s *Server) unreserveRequester(a Peer) {
	ip := a.Remote.Addr().Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	if s.perSess[a.SID]--; s.perSess[a.SID] <= 0 {
		delete(s.perSess, a.SID)
	}
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

// invite opens the stream to B, sends INCOMING and waits for ACCEPT. INCOMING.source is the
// node R authenticated on its session with A.
func (s *Server) invite(ctx context.Context, t Target, cir *circuit, a Peer, c Connect) (*mux.Stream, bool) {
	bst, err := t.OpenStream(ctx)
	if err != nil {
		return nil, false
	}
	bst.SetLimiter(limiter())
	inc := Incoming{CID: cir.cid, Source: a.Node[:], Mode: c.Mode, Target: c.Target, WantAddr: c.WantAddr}
	if err := bst.WriteFrame(TypeIncoming, 0, inc.Append(nil)); err != nil {
		bst.Reset(frame.CodePathFailed)
		return nil, false
	}
	type result struct {
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		f, err := bst.ReadFrame()
		if err != nil {
			ch <- result{}
			return
		}
		switch f.Type {
		case TypeAccept:
			acc, err := ParseAccept(f.Payload)
			ch <- result{err == nil && acc.CID == cir.cid}
		default: // REFUSE, a wrong CID, or any other frame
			ch <- result{}
		}
	}()
	select {
	case r := <-ch:
		if r.ok {
			return bst, true
		}
	case <-ctx.Done():
		_ = bst.SetReadDeadline(time.Now())
	}
	bst.Reset(frame.CodeProtocolViolation)
	return nil, false
}

// today is the day number of the server clock (wire day = floor(unix / 86400)).
func (s *Server) today() uint32 { return uint32(s.clk.Now().Unix() / 86400) }

// expireLocked drops the registrations of finished days: a registration for day D serves the
// contacts' lookups of day D and ends with it. s.mu is held.
func (s *Server) expireLocked() {
	today := s.today()
	for day, set := range s.rdvDays {
		if day >= today {
			continue
		}
		for k := range set {
			s.dropRegLocked(k.node, k.rid)
		}
		delete(s.rdvDays, day)
	}
}

// dropRegLocked removes one registration from rdvOf, rdv and the day index.
func (s *Server) dropRegLocked(node, rid [32]byte) {
	of := s.rdvOf[node]
	day, ok := of[rid]
	if !ok {
		return
	}
	delete(of, rid)
	if len(of) == 0 {
		delete(s.rdvOf, node)
	}
	if set := s.rdvDays[day]; set != nil {
		delete(set, regKey{node, rid})
		if len(set) == 0 {
			delete(s.rdvDays, day)
		}
	}
	nodes := s.rdv[rid][:0]
	for _, n := range s.rdv[rid] {
		if n != node {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		delete(s.rdv, rid)
	} else {
		s.rdv[rid] = nodes
	}
}

// handleRegister stores a node's rendezvous IDs for the current or the next day. A day outside
// that window, or more IDs than the node may hold, ends the stream with an error code instead of
// a REGISTERED that looks like success; the IDs that fit stay registered.
func (s *Server) handleRegister(p Peer, st *mux.Stream, r Register) {
	// The exchange is short; a stalled peer must not hold the stream longer than RegisterTimeout.
	tm := s.clk.AfterFunc(s.cfg.RegisterTimeout, func() { st.Reset(frame.CodeTimeout) })
	defer tm.Stop()
	s.mu.Lock()
	s.expireLocked()
	today := s.today()
	if r.Day != today && r.Day != today+1 {
		s.mu.Unlock()
		s.stats.RegisterRejected.Add(1)
		st.Reset(frame.CodeRefused)
		return
	}
	of := s.rdvOf[p.Node]
	if of == nil {
		of = map[[32]byte]uint32{}
		s.rdvOf[p.Node] = of
	}
	overflow := false
	for _, id := range r.IDs {
		var rid [32]byte
		copy(rid[:], id)
		k := regKey{p.Node, rid}
		if old, have := of[rid]; have {
			if old < r.Day { // renewed for a later day: move it in the index
				delete(s.rdvDays[old], k)
				of[rid] = r.Day
				s.indexLocked(r.Day, k)
			}
			continue
		}
		if len(of) >= s.cfg.Limits.MaxRendezvous {
			overflow = true
			break
		}
		of[rid] = r.Day
		s.indexLocked(r.Day, k)
		s.rdv[rid] = append(s.rdv[rid], p.Node)
	}
	n := len(of)
	if n == 0 {
		delete(s.rdvOf, p.Node)
	}
	s.mu.Unlock()
	if overflow {
		s.stats.RegisterRejected.Add(1)
		st.Reset(frame.CodeOverloaded)
		return
	}
	_ = st.WriteFrame(TypeRegistered, 0, Registered{Req: r.Req, Count: uint32(n)}.Append(nil))
	_ = st.Close()
}

func (s *Server) indexLocked(day uint32, k regKey) {
	set := s.rdvDays[day]
	if set == nil {
		set = map[regKey]struct{}{}
		s.rdvDays[day] = set
	}
	set[k] = struct{}{}
}

// ForgetNode drops the registrations of a node whose session with R ended.
func (s *Server) ForgetNode(node [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for rid := range s.rdvOf[node] {
		s.dropRegLocked(node, rid)
	}
}

// run carries the circuit until it ends.
func (s *Server) run(ctx context.Context, c *circuit) {
	defer c.once.Do(func() { close(c.done) })
	go s.watch(c)
	if c.mode == ModeDatagram {
		// Datagrams travel on the sessions (HandleDatagram); the two streams only anchor the
		// circuit. Nothing may arrive on them.
		s.anchor(c)
		return
	}
	da, db := c.a.stream.Detach(), c.b.stream.Detach()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.splice(c, c.a.peer.Node, da, db) }()
	go func() { defer wg.Done(); s.splice(c, c.b.peer.Node, db, da) }()
	wg.Wait()
	_ = da.Close()
	_ = db.Close()
}

func (s *Server) anchor(c *circuit) {
	end := make(chan struct{}, 2)
	for _, st := range []*mux.Stream{c.a.stream, c.b.stream} {
		go func(st *mux.Stream) {
			_, _ = st.ReadFrame() // any frame is a violation; EOF or reset ends the circuit
			end <- struct{}{}
		}(st)
	}
	select {
	case <-end:
	case <-c.done:
	}
	c.a.stream.Reset(frame.CodePathFailed)
	c.b.stream.Reset(frame.CodePathFailed)
}

// watch ends an idle circuit, a signal circuit past its time and any circuit past its byte cap.
func (s *Server) watch(c *circuit) {
	lim := s.cfg.Limits
	tick := min(lim.Idle, lim.SignalTime) / 4
	if tick < 5*time.Millisecond {
		tick = 5 * time.Millisecond
	}
	for {
		if !s.sleep(c, tick) {
			return
		}
		now := s.clk.Now()
		idle := now.Sub(time.Unix(0, c.last.Load())) > lim.Idle
		over := c.mode == ModeSignal && now.Sub(c.created) > lim.SignalTime
		if idle || over {
			s.kill(c)
			return
		}
	}
}

func (s *Server) kill(c *circuit) {
	c.once.Do(func() { close(c.done) })
	c.a.stream.Reset(frame.CodePathFailed)
	c.b.stream.Reset(frame.CodePathFailed)
}

// splice copies bytes from src to dst and nothing else, counting them against the circuit, the
// sender's day and rate.
func (s *Server) splice(c *circuit, sender [32]byte, src, dst *mux.Detached) {
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			c.last.Store(s.clk.Now().UnixNano())
			if s.tap != nil {
				s.tap(c.cid, sender, buf[:n])
			}
			v, wait := s.account(c, sender, uint64(n), false)
			if v == verdictKill || (wait > 0 && !s.sleep(c, wait)) {
				s.kill(c)
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				s.kill(c)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = dst.CloseWrite()
			} else {
				s.kill(c)
			}
			return
		}
	}
}

type verdict int

const (
	verdictOK   verdict = iota // forward
	verdictDrop                // datagram only: over the rate or a cap, not forwarded
	verdictKill                // stream only: a cap is exceeded, the circuit ends
)

// account adds n bytes and applies the circuit, day, signal and rate limits. A stream is paced:
// the returned wait is how long the copier must pause. A datagram is never delayed (its caller is
// the read loop of the sender's session): over a limit it is dropped and not counted.
func (s *Server) account(c *circuit, sender [32]byte, n uint64, datagram bool) (verdict, time.Duration) {
	lim := s.cfg.Limits
	over := func() (verdict, time.Duration) {
		if datagram {
			return verdictDrop, 0
		}
		return verdictKill, 0
	}
	total := c.bytes.Add(n)
	if total > lim.PerCircuit || (c.mode == ModeSignal && total > lim.SignalBytes) {
		if datagram {
			c.bytes.Add(^(n - 1))
		}
		return over()
	}
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := now.Unix() / 86400; d != s.day {
		s.day, s.dayBytes = d, map[[32]byte]uint64{}
	}
	if s.dayBytes[sender]+n > lim.PerPeerDay {
		if datagram {
			c.bytes.Add(^(n - 1))
		}
		return over()
	}
	b := s.buckets[sender]
	if b == nil {
		b = &bucket{rate: float64(lim.RateBytes), tokens: float64(lim.RateBytes), at: now}
		s.buckets[sender] = b
	}
	if datagram {
		if !b.tryTake(now, float64(n)) {
			c.bytes.Add(^(n - 1))
			return verdictDrop, 0
		}
		s.dayBytes[sender] += n
		return verdictOK, 0
	}
	s.dayBytes[sender] += n
	return verdictOK, b.take(now, float64(n))
}

type bucket struct {
	rate, tokens float64
	at           time.Time
}

// tryTake removes n tokens only if they are there.
func (b *bucket) tryTake(now time.Time, n float64) bool {
	b.tokens += now.Sub(b.at).Seconds() * b.rate
	if b.tokens > b.rate {
		b.tokens = b.rate
	}
	b.at = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// take removes n tokens and returns how long the caller must wait for them (burst: one second).
func (b *bucket) take(now time.Time, n float64) time.Duration {
	b.tokens += now.Sub(b.at).Seconds() * b.rate
	if b.tokens > b.rate {
		b.tokens = b.rate
	}
	b.at = now
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// HandleDatagram forwards one RELAY_DGRAM received on the session of from: payload is
// `varint cid ‖ sealed` (registries.md §2). R forwards the sealed bytes unchanged to the other
// end, and only if the sender's own session is an end of the circuit: a node that guesses a
// circuit ID gets nothing (route hijack, H1).
func (s *Server) HandleDatagram(from Peer, payload []byte) {
	cid, n, err := frame.ParseVarint(payload)
	if err != nil {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	s.mu.Lock()
	c := s.circuits[cid]
	s.mu.Unlock()
	if c == nil || c.mode != ModeDatagram {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	to, ok := c.other(from.SID)
	if !ok || to.peer.SendDatagram == nil {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	if v, _ := s.account(c, from.Node, uint64(len(payload)-n), true); v != verdictOK {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	c.last.Store(s.clk.Now().UnixNano())
	if err := to.peer.SendDatagram(frame.DgramRelay, payload); err != nil {
		s.stats.DatagramsDropped.Add(1)
		return
	}
	s.stats.Forwarded.Add(1)
}

// sleep waits d for pacing on the server clock and reports false if the circuit ended first.
func (s *Server) sleep(c *circuit, d time.Duration) bool {
	fired := make(chan struct{})
	tm := s.clk.AfterFunc(d, func() { close(fired) })
	defer tm.Stop()
	select {
	case <-fired:
		return true
	case <-c.done:
		return false
	}
}
