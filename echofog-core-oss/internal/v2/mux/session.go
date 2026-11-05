package mux

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// PathKind is a direct carrier or a relay circuit (wire.md §8).
type PathKind uint8

const (
	PathDirect PathKind = iota
	PathRelay
)

// StreamPolicy tells the session which (service, version) pairs were selected in the handshake
// and the frame limits of each. The service package implements it.
type StreamPolicy interface {
	// Limiter returns the frame limiter of streams of (service, version) and whether the pair
	// was selected for this session.
	Limiter(service, version uint64) (frame.Limiter, bool)
}

// Abuse reports a malformed or misplaced frame of a known type. Unknown types, unknown
// fields and UNSUPPORTED never produce one (versioning.md §2 rule 2).
type Abuse struct {
	Path   uint64
	Stream uint64
	Type   uint64
	Err    error
}

// Timers of the session (wire.md §7, §8.3, §10). Zero values take the spec defaults.
type Timers struct {
	KeepaliveMin time.Duration // 12 s
	KeepaliveMax time.Duration // 24 s
	// PongMin: an active path whose PING has no PONG within max(PongMin, 3 × RTT) is failing.
	PongMin time.Duration // 1 s
	// FailSilence: an active path with data or PINGs outstanding and nothing received for this
	// long is failing (wire.md §8.3).
	FailSilence time.Duration // 10 s
	PathDead    time.Duration // 60 s: no authenticated traffic
	GoAwayDrain time.Duration // 15 s
	BindTimeout time.Duration // 5 s: wait for PATH_BIND_OK (the handshake per-message wait)
	NoPathGrace time.Duration // 60 s: a session without any path is closed after this
	Tick        time.Duration // timer resolution, 500 ms
}

// DefaultTimers returns the timer set with every zero value replaced by the specification
// default (wire.md §7.1, §7.3, §8.3, §10).
func DefaultTimers() Timers {
	var t Timers
	t.fill()
	return t
}

func (t *Timers) fill() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&t.KeepaliveMin, 12*time.Second)
	def(&t.KeepaliveMax, 24*time.Second)
	def(&t.PongMin, time.Second)
	def(&t.FailSilence, 10*time.Second)
	def(&t.PathDead, 60*time.Second)
	def(&t.GoAwayDrain, 15*time.Second)
	def(&t.BindTimeout, 5*time.Second)
	def(&t.NoPathGrace, 60*time.Second)
	def(&t.Tick, 500*time.Millisecond)
}

// Config configures a session.
type Config struct {
	Keys Keys
	// Features is the negotiated feature set (the intersection, GREASE removed).
	Features []uint16
	Policy   StreamPolicy
	// MaxStreams is the number of streams each side may have open (256, wire.md §10).
	MaxStreams    int
	DatagramQueue int
	// MaxStandby is the number of standby paths kept besides the active one (default 2,
	// wire.md §8.3). With LowPower there are none and the keepalive is uniform(40, 55) s
	// (platforms.md §2).
	MaxStandby int
	LowPower   bool
	Timers     Timers
	OnAbuse    func(Abuse)
	// OnControl receives control-stream frames for other layers: OBSERVED_ADDR, CARD_UPDATE,
	// PUNCH_OFFER, PUNCH_SYNC (each only when its feature was negotiated).
	OnControl func(pathID uint64, f frame.Frame)
	// IdlePad draws the gap until the next PADDING-only frame and its payload size when the
	// idle-padding feature was negotiated (wire.md §6.4, §9). A nil function, a false result or
	// a non-positive gap disables idle padding.
	IdlePad func() (time.Duration, int, bool)
	// OnPathFailed reports a path that just died. It is called without session locks held and
	// must not block; the path policy uses it to release a carrier slot or start a backoff.
	OnPathFailed func(PathFailure)
}

// PathFailure describes a path that died (wire.md §8.3): a broken carrier, an abandoned path or
// a failed liveness check.
type PathFailure struct {
	SID    [16]byte
	PathID uint64
	Kind   PathKind
	Cause  error
}

// Datagram is a received datagram (wire.md §6.5).
type Datagram struct {
	Type    uint64
	Payload []byte
	Path    uint64
}

// SessionStats counts dropped input.
type SessionStats struct {
	DatagramsUnknown uint64
	DatagramsBad     uint64
	DatagramsDropped uint64
}

// RejectError: the peer refused the session after the handshake (REJECT on the control stream).
type RejectError struct{ frame.Reject }

func (e *RejectError) Error() string {
	return fmt.Sprintf("mux: session rejected: reason %d", e.Reason)
}

// GoAwayError: the session ended after GOAWAY.
type GoAwayError struct {
	frame.GoAway
	Remote bool
}

func (e *GoAwayError) Error() string {
	return fmt.Sprintf("mux: goaway code %d %q (remote %v)", e.Code, e.Reason, e.Remote)
}

type pingWait struct {
	ch   chan struct{}
	sent time.Time
}

type path struct {
	id      uint64
	kind    PathKind
	carrier Carrier
	guard   *Guard
	sealed  bool // sealed mode (wire.md §8.5)
	// pipeSeal: each stream is its own record pipe (carriers with native streams).
	pipeSeal bool
	ctrl     *Stream
	// seq is the attachment order in this session: the standby limit drops the path attached
	// first. A wall-clock timestamp is not enough: time.Now may return equal values for two
	// paths bound in the same tick, and then "oldest" would depend on map order.
	seq uint64

	lastSend atomic.Int64
	lastRecv atomic.Int64
	dead     atomic.Bool

	// guarded by Session.mu
	streams   map[*Stream]struct{}
	rtt       time.Duration
	keepalive time.Duration
	pings     map[[8]byte]*pingWait
	// failing: the path was the active path and missed a PONG or went silent with traffic
	// outstanding; it stays a standby path until it recovers or dies (wire.md §8.3).
	failing      bool
	failingSince time.Time
}

func (p *path) isDead() bool { return p.dead.Load() }

// sealedDatagrams: datagrams of the path travel as SEALED datagrams (sealed mode and relay paths,
// wire.md §8.4).
func (p *path) sealedDatagrams() bool { return p.sealed || p.kind == PathRelay }
func (p *path) touchSend()            { p.lastSend.Store(time.Now().UnixNano()) }
func (p *path) touchRecv()            { p.lastRecv.Store(time.Now().UnixNano()) }

func (p *path) lastReceive() time.Time {
	t := time.Unix(0, p.lastRecv.Load())
	if c := p.carrier.LastReceive(); c.After(t) {
		t = c
	}
	return t
}

// Session is one EchoFog v2 session (handshake.md §9) carried by one or more paths.
type Session struct {
	cfg      Config
	keys     Keys
	sid      [16]byte
	sealer   *Sealer
	kPath    []byte
	features map[uint16]bool

	mu           sync.Mutex
	paths        map[uint64]*path
	active       *path
	nextPathID   uint64
	nextPathSeq  uint64
	usedPeerIDs  map[uint64]bool
	localStreams int
	peerStreams  int
	goAwayAt     time.Time
	goingAway    bool
	noPathSince  time.Time
	// idle padding of the active path (wire.md §6.4, §9): the gap drawn from the shape policy
	// and the instant the next padding-only frame is due.
	idlePadGap time.Duration
	idlePadAt  time.Time
	closed     bool
	closeErr   error
	stats      SessionStats

	accept  chan *Stream
	dgrams  chan Datagram
	done    chan struct{}
	onClose []func()
}

// NewSession starts a session after a handshake that ran on stream 0 of the guarded carrier.
// control is that stream 0, positioned after the handshake frames; it becomes the control stream
// of path 0. If the carrier is in sealed mode, stream 0 continues as sealed records.
func NewSession(cfg Config, g *Guard, control RawStream, kind PathKind) (*Session, error) {
	if cfg.Keys == nil {
		return nil, errors.New("mux: no session keys")
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = DefaultMaxStreams
	}
	if cfg.DatagramQueue == 0 {
		cfg.DatagramQueue = DefaultDatagramQueue
	}
	if cfg.LowPower {
		cfg.MaxStandby = 0
		if cfg.Timers.KeepaliveMin == 0 && cfg.Timers.KeepaliveMax == 0 {
			cfg.Timers.KeepaliveMin, cfg.Timers.KeepaliveMax = 40*time.Second, 55*time.Second
		}
	} else if cfg.MaxStandby == 0 {
		cfg.MaxStandby = 2
	}
	cfg.Timers.fill()
	sealer, err := NewSealer(cfg.Keys)
	if err != nil {
		return nil, err
	}
	s := &Session{
		cfg:         cfg,
		keys:        cfg.Keys,
		sid:         cfg.Keys.SID(),
		sealer:      sealer,
		kPath:       PathBindKey(cfg.Keys),
		features:    map[uint16]bool{},
		paths:       map[uint64]*path{},
		usedPeerIDs: map[uint64]bool{0: true},
		accept:      make(chan *Stream, cfg.MaxStreams),
		dgrams:      make(chan Datagram, cfg.DatagramQueue),
		done:        make(chan struct{}),
	}
	for _, f := range cfg.Features {
		s.features[f] = true
	}
	if cfg.Keys.Initiator() {
		s.nextPathID = 2
	} else {
		s.nextPathID = 1
	}
	s.attachPath(0, kind, g, control, nil, false)
	go s.tickLoop()
	return s, nil
}

// SID returns the session ID.
func (s *Session) SID() [16]byte { return s.sid }

// HasFeature reports whether a feature was negotiated.
func (s *Session) HasFeature(id uint16) bool { return s.features[id] }

// GoingAway reports whether GOAWAY was sent or received: no new streams may be opened, the
// session is draining (wire.md §7.3).
func (s *Session) GoingAway() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goingAway
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// Stats returns a snapshot of the counters.
// LastActivity returns the most recent send or receive on any path. A session with no
// authenticated traffic for the path-dead time dies on its own (wire.md §8.3); the session
// table uses this value for its own idle policy.
func (s *Session) LastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last time.Time
	for _, p := range s.paths {
		for _, ns := range [2]int64{p.lastSend.Load(), p.lastRecv.Load()} {
			if ns == 0 {
				continue
			}
			if t := time.Unix(0, ns); t.After(last) {
				last = t
			}
		}
	}
	return last
}

// StreamCounts returns the number of open non-control streams opened by this side and by the
// peer. GOAWAY waits for both (wire.md §7.3).
func (s *Session) StreamCounts() (local, peer int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localStreams, s.peerStreams
}

func (s *Session) Stats() SessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Paths returns the IDs of the live paths and the active one.
func (s *Session) Paths() (ids []uint64, active uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.paths {
		ids = append(ids, id)
	}
	if s.active != nil {
		return ids, s.active.id, true
	}
	return ids, 0, false
}

// attachPath registers a path whose stream 0 is control. pre, if set, is written in plaintext
// on stream 0 before anything else (PATH_BIND_OK), after the guard hands streams to the session.
// forceActive makes the new path active before the standby trim: a network change must keep the
// freshly bound path even on a platform with no standby slots (low_power).
func (s *Session) attachPath(id uint64, kind PathKind, g *Guard, control RawStream, pre []byte, forceActive bool) (*path, error) {
	p := &path{
		id: id, kind: kind, carrier: g.c, guard: g, sealed: g.c.Sealed() || kind == PathRelay,
		streams: map[*Stream]struct{}{}, pings: map[[8]byte]*pingWait{},
	}
	// Sealed mode: a byte-stream carrier seals the whole connection as one pipe; a carrier with
	// native streams (QUIC) seals each stream as its own pipe (wire.md §8.5).
	rc, carrierSeals := g.c.(interface{ StartSealing(*Sealer, uint64) })
	p.pipeSeal = p.sealed && !carrierSeals
	p.keepalive = s.drawKeepalive()
	p.touchRecv()
	p.ctrl = s.newStream(p, control, true, true)
	p.ctrl.wmu.Lock()
	s.mu.Lock()
	p.seq = s.nextPathSeq
	s.nextPathSeq++
	s.paths[id] = p
	activate := forceActive
	if !activate && s.active != nil && s.cfg.MaxStandby == 0 {
		// No standby slots: a newly bound carrier must not be trimmed away. A direct path still
		// wins over a relay one; otherwise the newest path is the new carrier/network.
		activate = kind != PathRelay || s.active.kind != PathDirect
	}
	if s.active == nil || activate || better(p, s.active) {
		s.active = p
	}
	s.noPathSince = time.Time{}
	s.mu.Unlock()
	g.bind(s, p)
	if pre != nil {
		if _, err := control.Write(pre); err != nil {
			p.ctrl.wmu.Unlock()
			s.failPath(p, err)
			return nil, err
		}
	}
	if p.sealed && carrierSeals {
		rc.StartSealing(s.sealer, id) // after PATH_BIND_OK, which leaves in plaintext
	}
	p.ctrl.wmu.Unlock()
	defer s.trimStandby()
	go s.controlLoop(p)
	go s.datagramLoop(p)
	go func() {
		select {
		case <-p.carrier.Done():
			s.failPath(p, ErrPathFailed)
		case <-s.done:
		}
	}()
	return p, nil
}

// trimStandby abandons the oldest standby paths beyond MaxStandby (wire.md §8.3).
func (s *Session) trimStandby() {
	for {
		s.mu.Lock()
		var oldest *path
		n := 0
		for _, q := range s.paths {
			if q == s.active || q.isDead() {
				continue
			}
			n++
			if oldest == nil || q.seq < oldest.seq {
				oldest = q
			}
		}
		s.mu.Unlock()
		if n <= s.cfg.MaxStandby || oldest == nil {
			return
		}
		if s.AbandonPath(oldest.id, frame.CodeNoError) != nil {
			return
		}
	}
}

func (s *Session) drawKeepalive() time.Duration {
	t := s.cfg.Timers
	if t.KeepaliveMax <= t.KeepaliveMin {
		return t.KeepaliveMin
	}
	return t.KeepaliveMin + time.Duration(mrand.Int64N(int64(t.KeepaliveMax-t.KeepaliveMin)))
}

// ---- streams

// OpenStream opens a stream for (service, version) on the active path and sends OPEN.
func (s *Session) OpenStream(ctx context.Context, service, version uint64) (*Stream, error) {
	return s.OpenStreamFlags(ctx, service, version, 0)
}

// OpenStreamFlags is OpenStream with OPEN flags.
func (s *Session) OpenStreamFlags(ctx context.Context, service, version uint64, flags byte) (*Stream, error) {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, ErrSessionClosed
	case s.goingAway:
		s.mu.Unlock()
		return nil, ErrGoingAway
	case s.localStreams >= s.cfg.MaxStreams:
		s.mu.Unlock()
		return nil, ErrTooManyStreams
	case s.active == nil:
		s.mu.Unlock()
		return nil, ErrNoPath
	}
	p := s.active
	s.localStreams++
	s.mu.Unlock()
	raw, err := p.carrier.OpenStream(ctx)
	if err != nil {
		s.mu.Lock()
		s.localStreams--
		s.mu.Unlock()
		return nil, err
	}
	st := s.newStream(p, raw, true, false)
	st.open = frame.Open{Service: service, Version: version, Flags: flags}
	if s.cfg.Policy != nil {
		st.rd.Lim, st.selected = s.cfg.Policy.Limiter(service, version)
	}
	if !s.addStream(p, st) {
		return nil, ErrPathFailed
	}
	if err := st.WriteFrame(frame.TypeOpen, 0, st.open.Append(nil)); err != nil {
		st.Reset(frame.CodeInternal)
		return nil, err
	}
	return st, nil
}

func (s *Session) addStream(p *path, st *Stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.isDead() {
		if st.local {
			s.localStreams--
		} else {
			s.peerStreams--
		}
		st.released = true
		return false
	}
	p.streams[st] = struct{}{}
	return true
}

func (s *Session) releaseStream(st *Stream) {
	if st.control {
		return
	}
	s.mu.Lock()
	if _, ok := st.path.streams[st]; ok {
		delete(st.path.streams, st)
		if st.local {
			s.localStreams--
		} else {
			s.peerStreams--
		}
	}
	s.mu.Unlock()
}

// AcceptStream returns the next stream the peer opened, after its OPEN. The stream may carry a
// pair that was not selected (Selected() false): the service layer answers UNSUPPORTED.
func (s *Session) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, s.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handleIncoming reads the OPEN of a stream the peer opened on path p.
func (s *Session) handleIncoming(p *path, raw RawStream) {
	st := s.newStream(p, raw, false, false)
	s.mu.Lock()
	refuse := s.peerStreams >= s.cfg.MaxStreams || s.goingAway || s.closed
	s.peerStreams++
	s.mu.Unlock()
	if !s.addStream(p, st) {
		raw.Reset(frame.CodePathFailed)
		return
	}
	f, err := st.rd.ReadFrame()
	for err == nil && f.Type == frame.TypePadding {
		f, err = st.rd.ReadFrame()
	}
	if err != nil {
		st.readErr(err)
		return
	}
	if f.Type != frame.TypeOpen {
		err := fmt.Errorf("%w: first frame %s is not OPEN", ErrProtocolViolation, frame.TypeName(f.Type))
		st.abuse(f.Type, err)
		st.CloseWithCode(frame.CodeProtocolViolation, "")
		return
	}
	o, err := frame.ParseOpen(f.Payload)
	if err != nil {
		st.abuse(f.Type, err)
		st.CloseWithCode(frame.CodeProtocolViolation, "")
		return
	}
	st.open = o
	if refuse {
		st.CloseWithCode(frame.CodeRefused, "")
		return
	}
	if s.cfg.Policy != nil {
		st.rd.Lim, st.selected = s.cfg.Policy.Limiter(o.Service, o.Version)
	}
	select {
	case s.accept <- st:
	default:
		st.CloseWithCode(frame.CodeOverloaded, "")
	}
}

// ---- control stream

func (s *Session) controlLoop(p *path) {
	for {
		f, err := p.ctrl.rd.ReadFrame()
		if err != nil {
			var tl *frame.TooLargeError
			switch {
			case frame.IsUnknown(err):
				s.goAwayClose(frame.CodeUnknownFrame, err)
			case errors.As(err, &tl):
				if tl.Known {
					p.ctrl.abuse(tl.Type, err)
				}
				s.goAwayClose(frame.CodeFrameTooLarge, err)
			case err == frame.ErrNonMinimal:
				s.goAwayClose(frame.CodeProtocolViolation, err)
			default:
				s.failPath(p, fmt.Errorf("%w: control stream: %v", ErrPathFailed, err))
			}
			return
		}
		p.touchRecv()
		if !s.handleControl(p, f) {
			return
		}
	}
}

func (s *Session) handleControl(p *path, f frame.Frame) bool {
	bad := func(err error) bool {
		p.ctrl.abuse(f.Type, err)
		s.goAwayClose(frame.CodeProtocolViolation, err)
		return false
	}
	switch f.Type {
	case frame.TypePadding:
	case frame.TypePing:
		if len(f.Payload) != 8 {
			return bad(frame.ErrMalformed)
		}
		p.ctrl.WriteFrame(frame.TypePong, 0, f.Payload)
	case frame.TypePong:
		v, err := frame.ParsePing(f.Payload)
		if err != nil {
			return bad(err)
		}
		s.mu.Lock()
		w := p.pings[v]
		delete(p.pings, v)
		s.mu.Unlock()
		if w != nil {
			close(w.ch)
		}
	case frame.TypeGoAway:
		g, err := frame.ParseGoAway(f.Payload)
		if err != nil {
			return bad(err)
		}
		s.mu.Lock()
		if !s.goingAway {
			s.goingAway = true
			s.goAwayAt = time.Now()
		}
		if s.closeErr == nil {
			s.closeErr = &GoAwayError{GoAway: g, Remote: true}
		}
		s.mu.Unlock()
		s.maybeDrained()
	case frame.TypePathAbandon:
		pa, err := frame.ParsePathAbandon(f.Payload)
		if err != nil {
			return bad(err)
		}
		s.mu.Lock()
		q := s.paths[pa.PathID]
		s.mu.Unlock()
		if q != nil {
			s.failPath(q, fmt.Errorf("%w (peer code %d)", ErrPathAbandoned, pa.Code))
			if q == p {
				return false
			}
		}
	case frame.TypeReject:
		r, err := frame.ParseReject(f.Payload)
		if err != nil {
			return bad(err)
		}
		s.close(&RejectError{r})
		return false
	case frame.TypeClose:
		c, _ := frame.ParseClose(f.Payload)
		s.failPath(p, &CloseError{Code: c.Code, Reason: c.Reason, Remote: true})
		return false
	case frame.TypeObservedAddr, frame.TypeCardUpdate, frame.TypePunchOffer, frame.TypePunchSync:
		info, _ := frame.SessionType(f.Type)
		if !s.HasFeature(info.Feature) {
			// A frame of a feature that was not negotiated is an unknown frame here.
			if f.Ignorable() {
				return true
			}
			s.goAwayClose(frame.CodeUnknownFrame, fmt.Errorf("mux: %s without feature %#04x", info.Name, info.Feature))
			return false
		}
		switch f.Type {
		case frame.TypeObservedAddr:
			if _, err := frame.ParseObservedAddr(f.Payload); err != nil {
				return bad(err)
			}
		case frame.TypePunchSync:
			if _, err := frame.ParsePing(f.Payload); err != nil {
				return bad(err)
			}
		}
		if s.cfg.OnControl != nil {
			s.cfg.OnControl(p.id, f)
		}
	default:
		// HS_*, PATH_BIND*, OPEN, UNSUPPORTED, MUX_* and service-local types are not control
		// frames after the handshake.
		if f.Type >= frame.ServiceLocalMin {
			return true // unreachable: the control reader has no limiter, these are unknown
		}
		return bad(fmt.Errorf("%w: %s on the control stream", ErrProtocolViolation, frame.TypeName(f.Type)))
	}
	return true
}

// SendControl writes a control frame on the active path. Frames that a feature introduces are
// refused unless that feature was negotiated (versioning.md §3.3).
func (s *Session) SendControl(typ uint64, flags byte, payload []byte) error {
	if info, ok := frame.SessionType(typ); ok && info.Feature != 0 && !s.HasFeature(info.Feature) {
		return ErrFeatureNotSelected
	}
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return ErrNoPath
	}
	return p.ctrl.WriteFrame(typ, flags, payload)
}

// SendGrease writes a GREASE frame on the active path's control stream (D18).
func (s *Session) SendGrease() error {
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return ErrNoPath
	}
	return p.ctrl.WriteGrease()
}

// Ping measures the RTT of the active path.
func (s *Session) Ping(ctx context.Context) (time.Duration, error) {
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return 0, ErrNoPath
	}
	return s.ping(ctx, p)
}

func (s *Session) ping(ctx context.Context, p *path) (time.Duration, error) {
	var v [8]byte
	rand.Read(v[:])
	start := time.Now()
	w := &pingWait{ch: make(chan struct{}), sent: start}
	s.mu.Lock()
	p.pings[v] = w
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(p.pings, v)
		s.mu.Unlock()
	}()
	if err := p.ctrl.WriteFrame(frame.TypePing, 0, v[:]); err != nil {
		return 0, err
	}
	select {
	case <-w.ch:
		rtt := time.Since(start)
		s.mu.Lock()
		p.rtt = rtt
		s.mu.Unlock()
		return rtt, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.done:
		return 0, ErrSessionClosed
	}
}

// GoAway announces the end of the session: no new streams are opened, and the session closes
// when its streams end or after the drain time (wire.md §7.3).
func (s *Session) GoAway(code uint64, reason string, retryAfter uint32) error {
	g := frame.GoAway{Code: code, Reason: reason, RetryAfter: retryAfter}
	s.mu.Lock()
	if !s.goingAway {
		s.goingAway = true
		s.goAwayAt = time.Now()
	}
	if s.closeErr == nil {
		s.closeErr = &GoAwayError{GoAway: g}
	}
	p := s.active
	s.mu.Unlock()
	var err error
	if p != nil {
		err = p.ctrl.WriteFrame(frame.TypeGoAway, 0, g.Append(nil))
	}
	s.maybeDrained()
	return err
}

func (s *Session) goAwayClose(code uint64, cause error) {
	g := frame.GoAway{Code: code}
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p != nil {
		p.ctrl.WriteFrame(frame.TypeGoAway, 0, g.Append(nil))
	}
	s.close(fmt.Errorf("mux: session ended with GOAWAY{%d}: %w", code, cause))
}

func (s *Session) maybeDrained() {
	s.mu.Lock()
	drained := s.goingAway && (s.localStreams+s.peerStreams == 0 || time.Since(s.goAwayAt) >= s.cfg.Timers.GoAwayDrain)
	s.mu.Unlock()
	if drained {
		s.close(nil)
	}
}

// ---- paths

// BindPath binds a new carrier, opened by this side, to the session with PATH_BIND (wire.md
// §8.2). The carrier's stream 0 becomes a control stream of the session. It requires the
// feature path-migration. The active path is chosen by the usual preference rules.
func (s *Session) BindPath(ctx context.Context, g *Guard, kind PathKind) (uint64, error) {
	return s.bindPath(ctx, g, kind, false)
}

// BindPathActive is BindPath and makes the new path active before the standby trim. A network
// change uses it: with MaxStandby = 0 (low_power) the freshly bound path must not be the one
// trimmed away (wire.md §8.3, platforms.md §2).
func (s *Session) BindPathActive(ctx context.Context, g *Guard, kind PathKind) (uint64, error) {
	return s.bindPath(ctx, g, kind, true)
}

func (s *Session) bindPath(ctx context.Context, g *Guard, kind PathKind, activate bool) (uint64, error) {
	if !s.HasFeature(frame.FeaturePathMigration) {
		return 0, ErrFeatureNotSelected
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrSessionClosed
	}
	id := s.nextPathID
	s.nextPathID += 2
	s.mu.Unlock()
	c := g.c
	raw, err := c.OpenStream(ctx)
	if err != nil {
		return 0, err
	}
	mac := PathMAC(s.kPath, c.ChannelBinding(), s.sid, id, roleOf(s.keys.Initiator()))
	b := frame.AppendFrame(nil, frame.Frame{Type: frame.TypePathBind,
		Payload: frame.PathBind{SID: s.sid, PathID: id, MAC: mac}.Append(nil)})
	if _, err := raw.Write(b); err != nil {
		c.Close()
		return 0, err
	}
	dl := time.Now().Add(s.cfg.Timers.BindTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	raw.SetReadDeadline(dl)
	f, err := frame.NewReader(raw, nil).ReadFrame()
	if err != nil {
		c.Close()
		return 0, fmt.Errorf("mux: PATH_BIND refused: %w", err)
	}
	ok, perr := frame.ParsePathBindOK(f.Payload)
	if f.Type != frame.TypePathBindOK || perr != nil || ok != id {
		c.Close()
		return 0, fmt.Errorf("mux: PATH_BIND: unexpected answer %s", frame.TypeName(f.Type))
	}
	raw.SetReadDeadline(time.Time{})
	if _, err := s.attachPath(id, kind, g, raw, nil, activate); err != nil {
		return 0, err
	}
	return id, nil
}

// acceptBind verifies a PATH_BIND received on stream 0 of a new carrier (wire.md §8.2): MAC over
// the new carrier's channel binding (constant time), path_id unused and of the sender's parity.
func (s *Session) acceptBind(pb frame.PathBind, cb [32]byte) bool {
	if !s.HasFeature(frame.FeaturePathMigration) {
		return false
	}
	senderInitiator := !s.keys.Initiator()
	if pb.PathID == 0 || (pb.PathID%2 == 0) != senderInitiator {
		return false
	}
	want := PathMAC(s.kPath, cb, s.sid, pb.PathID, roleOf(senderInitiator))
	if !hmacEqual(want[:], pb.MAC[:]) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.usedPeerIDs[pb.PathID] {
		return false
	}
	s.usedPeerIDs[pb.PathID] = true
	return true
}

// SetActivePath makes a live path the one this side sends on.
func (s *Session) SetActivePath(id uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.paths[id]
	if p == nil {
		return ErrNoPath
	}
	s.active = p
	return nil
}

// AbandonPath tells the peer a path will no longer be used (PATH_ABANDON on another live path)
// and closes it.
func (s *Session) AbandonPath(id, code uint64) error {
	s.mu.Lock()
	p := s.paths[id]
	var other *path
	for _, q := range s.paths {
		if q != p && !q.isDead() {
			other = q
			break
		}
	}
	s.mu.Unlock()
	if p == nil {
		return ErrNoPath
	}
	if other == nil {
		return errors.New("mux: cannot abandon the only path")
	}
	other.ctrl.WriteFrame(frame.TypePathAbandon, 0, frame.PathAbandon{PathID: id, Code: code}.Append(nil))
	s.failPath(p, ErrPathAbandoned)
	return nil
}

// pickActiveLocked prefers direct paths over relay paths and lower RTT among them.
func (s *Session) pickActiveLocked() *path {
	var best *path
	for _, p := range s.paths {
		if p.isDead() {
			continue
		}
		if best == nil || (best.failing && !p.failing) || (best.failing == p.failing && better(p, best)) {
			best = p
		}
	}
	return best
}

func better(a, b *path) bool {
	if a.kind != b.kind {
		return a.kind == PathDirect
	}
	if a.rtt != b.rtt {
		if a.rtt == 0 {
			return false
		}
		return b.rtt == 0 || a.rtt < b.rtt
	}
	return a.id < b.id
}

// failPath ends a path: its streams are reset with path-failed and services reopen them on
// another path. The session, its keys and its peer authentication survive (wire.md §8).
func (s *Session) failPath(p *path, cause error) {
	if !p.dead.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	delete(s.paths, p.id)
	streams := make([]*Stream, 0, len(p.streams))
	for st := range p.streams {
		streams = append(streams, st)
	}
	if s.active == p {
		s.active = s.pickActiveLocked()
	}
	if len(s.paths) == 0 {
		s.noPathSince = time.Now()
	}
	s.mu.Unlock()
	for _, st := range streams {
		st.failPath()
	}
	p.carrier.Close()
	if h := s.cfg.OnPathFailed; h != nil {
		h(PathFailure{SID: s.sid, PathID: p.id, Kind: p.kind, Cause: cause})
	}
}

// ---- datagrams

// SendDatagram sends a datagram on the active path; on a sealed path it travels as a SEALED
// datagram whose plaintext is the inner datagram (wire.md §8.4–§8.5).
func (s *Session) SendDatagram(typ uint64, payload []byte) error {
	s.mu.Lock()
	p := s.active
	s.mu.Unlock()
	if p == nil {
		return ErrNoPath
	}
	d := frame.AppendDatagram(nil, typ, payload)
	if p.sealedDatagrams() {
		sealed, err := s.sealer.Seal(nil, d, ClassDatagram, p.id, 0)
		if err != nil {
			return err
		}
		d = frame.AppendDatagram(nil, frame.DgramSealed, sealed)
	}
	if err := p.carrier.SendDatagram(d); err != nil {
		return err
	}
	p.touchSend()
	return nil
}

// ReceiveDatagram returns the next datagram of a known type. Unknown types are dropped and
// counted, never answered (registries.md §2).
func (s *Session) ReceiveDatagram(ctx context.Context) (Datagram, error) {
	select {
	case d := <-s.dgrams:
		return d, nil
	case <-s.done:
		return Datagram{}, s.Err()
	case <-ctx.Done():
		return Datagram{}, ctx.Err()
	}
}

func (s *Session) count(f func(*SessionStats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

func (s *Session) datagramLoop(p *path) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
		case <-p.carrier.Done():
		}
		cancel()
	}()
	for {
		d, err := p.carrier.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		typ, payload, err := frame.ParseDatagram(d)
		if err != nil {
			s.count(func(st *SessionStats) { st.DatagramsBad++ })
			continue
		}
		if p.sealedDatagrams() {
			if typ != frame.DgramSealed {
				s.count(func(st *SessionStats) { st.DatagramsBad++ })
				continue
			}
			inner, err := s.sealer.OpenDatagram(nil, payload, p.id)
			if err != nil {
				s.count(func(st *SessionStats) { st.DatagramsBad++ })
				continue
			}
			if typ, payload, err = frame.ParseDatagram(inner); err != nil || typ == frame.DgramSealed {
				s.count(func(st *SessionStats) { st.DatagramsBad++ })
				continue
			}
		}
		if typ == frame.DgramSealed && !p.sealedDatagrams() {
			s.count(func(st *SessionStats) { st.DatagramsBad++ })
			continue
		}
		p.touchRecv()
		if typ == frame.DgramPadding {
			continue
		}
		if !frame.DatagramKnown(typ) {
			s.count(func(st *SessionStats) { st.DatagramsUnknown++ })
			continue
		}
		select {
		case s.dgrams <- Datagram{Type: typ, Payload: payload, Path: p.id}:
		default:
			s.count(func(st *SessionStats) { st.DatagramsDropped++ })
		}
	}
}

// ---- timers

func (s *Session) tickLoop() {
	t := time.NewTicker(s.cfg.Timers.Tick)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.tick(time.Now())
	}
}

func (s *Session) tick(now time.Time) {
	tm := s.cfg.Timers
	s.mu.Lock()
	paths := make([]*path, 0, len(s.paths))
	for _, p := range s.paths {
		paths = append(paths, p)
	}
	active := s.active
	noPath := !s.noPathSince.IsZero() && now.Sub(s.noPathSince) >= tm.NoPathGrace
	s.mu.Unlock()
	if noPath {
		s.close(ErrNoPath)
		return
	}
	for _, p := range paths {
		last := p.lastReceive()
		silent := now.Sub(last)
		if silent >= tm.PathDead {
			// Dead: abandoned (PATH_ABANDON on another path when there is one).
			if s.AbandonPath(p.id, frame.CodeTimeout) != nil {
				s.failPath(p, fmt.Errorf("%w: no authenticated traffic for %v", ErrPathFailed, tm.PathDead))
			}
			continue
		}
		lastSend := time.Unix(0, p.lastSend.Load())
		s.mu.Lock()
		pongDeadline := max(tm.PongMin, 3*p.rtt)
		overdue, pinging := false, len(p.pings) > 0
		for _, w := range p.pings {
			if now.Sub(w.sent) > pongDeadline {
				overdue = true
			}
		}
		outstanding := pinging || lastSend.After(last)
		probe := false
		switch {
		case p == active && !p.failing && (overdue || (outstanding && silent >= tm.FailSilence)):
			p.failing, p.failingSince = true, now
			if next := s.pickActiveLocked(); next != nil {
				s.active = next
			}
		case p.failing && last.After(p.failingSince):
			p.failing = false // recovered; it stays a standby path
		case p == active && lastSend.After(last) && !pinging && silent >= tm.FailSilence/2:
			// The mux has no acknowledgements: data outstanding is checked with a PING, whose
			// PONG deadline then decides.
			probe = true
		}
		idle := now.Sub(last)
		if ls := now.Sub(lastSend); ls < idle {
			idle = ls
		}
		if idle >= p.keepalive {
			p.keepalive = s.drawKeepalive()
			probe = true
		}
		s.mu.Unlock()
		if probe {
			go func(p *path) {
				ctx, cancel := context.WithTimeout(context.Background(), tm.PathDead)
				defer cancel()
				s.ping(ctx, p)
			}(p)
		}
	}
	_ = active
	if send, p, n := s.idlePaddingDue(now); send {
		_ = p.ctrl.WritePadding(n)
	}
	s.maybeDrained()
}

// idlePaddingDue decides whether the active path owes a PADDING-only frame (wire.md §6.4, §9).
// The gap comes from the caller's shape policy; padding restarts after any traffic on the path,
// so only an idle path is padded. It is called from the tick loop.
func (s *Session) idlePaddingDue(now time.Time) (bool, *path, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.goingAway || s.cfg.IdlePad == nil ||
		!s.features[frame.FeatureIdlePadding] || s.active == nil {
		return false, nil, 0
	}
	p := s.active
	last := time.Unix(0, p.lastSend.Load())
	if r := p.lastReceive(); r.After(last) {
		last = r
	}
	if s.idlePadAt.IsZero() {
		if d, _, ok := s.cfg.IdlePad(); ok && d > 0 {
			s.idlePadGap = d
			s.idlePadAt = now.Add(d)
		}
		return false, nil, 0
	}
	if now.Before(s.idlePadAt) {
		return false, nil, 0
	}
	if now.Sub(last) < s.idlePadGap {
		// Traffic happened inside the gap: the path is not idle, try again later.
		s.idlePadAt = now.Add(s.idlePadGap)
		return false, nil, 0
	}
	d, n, ok := s.cfg.IdlePad()
	if !ok || d <= 0 {
		s.idlePadGap, s.idlePadAt = 0, time.Time{}
		return false, nil, 0
	}
	s.idlePadGap, s.idlePadAt = d, now.Add(d)
	if n < 0 {
		n = 0
	}
	if n > 16384 {
		n = 16384
	}
	return true, p, n
}

// closeCarrier lets queued frames (a final GOAWAY) leave before the carrier closes, where the
// carrier supports it.
func closeCarrier(c Carrier) {
	if f, ok := c.(interface{ CloseAfterFlush(time.Duration) }); ok {
		go f.CloseAfterFlush(time.Second)
		return
	}
	c.Close()
}

// Close ends the session and all its paths.
func (s *Session) Close() error {
	s.close(ErrSessionClosed)
	return nil
}

func (s *Session) close(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.closeErr == nil {
		s.closeErr = err
	}
	if s.closeErr == nil {
		s.closeErr = ErrSessionClosed
	}
	paths := s.paths
	s.paths = map[uint64]*path{}
	s.active = nil
	onClose := s.onClose
	close(s.done)
	s.mu.Unlock()
	for _, p := range paths {
		p.dead.Store(true)
		closeCarrier(p.carrier)
	}
	for _, f := range onClose {
		f()
	}
}

func (s *Session) addOnClose(f func()) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.onClose = append(s.onClose, f)
	}
	s.mu.Unlock()
	if closed {
		f()
	}
}
