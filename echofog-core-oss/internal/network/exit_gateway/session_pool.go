package exit_gateway

import (
	"context"
	crand "crypto/rand"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func cryptoRandInt64(min, max int64) int64 {
	if min >= max {
		return min
	}
	diff := max - min + 1
	n, err := crand.Int(crand.Reader, big.NewInt(diff))
	if err != nil {
		return min
	}
	return min + n.Int64()
}

// countingStream wraps a FramedStream to meter byte counts and active stream counts.
type countingStream struct {
	FramedStream
	sess      *ManagedSession
	closeOnce sync.Once
	onClose   func()
}

func (s *countingStream) Read(p []byte) (int, error) {
	n, err := s.FramedStream.Read(p)
	if n > 0 {
		s.sess.bytesMoved.Add(int64(n))
	}
	return n, err
}

func (s *countingStream) Write(p []byte) (int, error) {
	n, err := s.FramedStream.Write(p)
	if n > 0 {
		s.sess.bytesMoved.Add(int64(n))
	}
	return n, err
}

func (s *countingStream) WriteFrame(msgType net_types.MessageType, data []byte) error {
	err := s.FramedStream.WriteFrame(msgType, data)
	if err == nil && len(data) > 0 {
		s.sess.bytesMoved.Add(int64(len(data)))
	}
	return err
}

func (s *countingStream) ReadFrame() (net_types.MessageType, []byte, error) {
	msgType, data, err := s.FramedStream.ReadFrame()
	if err == nil && len(data) > 0 {
		s.sess.bytesMoved.Add(int64(len(data)))
	}
	return msgType, data, err
}

func (s *countingStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.sess.activeStreams.Add(-1)
		if s.onClose != nil {
			s.onClose()
		}
		err = s.FramedStream.Close()
	})
	return err
}

func (s *countingStream) CloseWrite() error {
	if cw, ok := s.FramedStream.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (s *countingStream) SetDeadline(t time.Time) error {
	if d, ok := s.FramedStream.(interface{ SetDeadline(time.Time) error }); ok {
		return d.SetDeadline(t)
	}
	return nil
}

func (s *countingStream) SetReadDeadline(t time.Time) error {
	if d, ok := s.FramedStream.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

func (s *countingStream) SetWriteDeadline(t time.Time) error {
	if d, ok := s.FramedStream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}

func (s *countingStream) udpLink(toExit bool) udpLink {
	if ds, ok := s.FramedStream.(datagramStream); ok {
		return ds.udpLink(toExit)
	}
	return nil
}

// ManagedSession wraps a stream opening session with lifecycle, byte, and concurrency tracking.
type ManagedSession struct {
	id          int64
	opener      StreamOpener
	createdAt   time.Time
	maxLifetime time.Duration
	maxBytes    int64
	maxStreams  int

	bytesMoved    atomic.Int64
	activeStreams atomic.Int32
	totalStreams  atomic.Int64
	draining      atomic.Bool

	closeFn   func() error
	drainCh   chan struct{}
	closeOnce sync.Once
}

// NewManagedSession creates a new ManagedSession with randomized bounds based on cfg.
func NewManagedSession(id int64, opener StreamOpener, cfg config.FlowShapingConfig, closeFn func() error) *ManagedSession {
	cfg.Normalize()
	minLife := int64(cfg.MaxConnLifetimeMin)
	maxLife := int64(cfg.MaxConnLifetimeMax)
	lifetime := time.Duration(cryptoRandInt64(minLife, maxLife))
	maxBytes := cryptoRandInt64(cfg.MaxBytesMin, cfg.MaxBytesMax)

	return &ManagedSession{
		id:          id,
		opener:      opener,
		createdAt:   time.Now(),
		maxLifetime: lifetime,
		maxBytes:    maxBytes,
		maxStreams:  cfg.MaxStreamsPerConn,
		closeFn:     closeFn,
		drainCh:     make(chan struct{}, 1),
	}
}

// ShouldRotate reports whether this session should be rotated.
func (s *ManagedSession) ShouldRotate() bool {
	if s.draining.Load() {
		return true
	}
	if s.maxBytes > 0 && s.bytesMoved.Load() >= s.maxBytes {
		return true
	}
	if s.maxLifetime > 0 && time.Since(s.createdAt) >= s.maxLifetime {
		return true
	}
	if s.maxStreams > 0 && s.totalStreams.Load() >= int64(s.maxStreams) {
		return true
	}
	return false
}

// OpenStream opens a new stream through this session.
func (s *ManagedSession) OpenStream(ctx context.Context) (FramedStream, error) {
	s.totalStreams.Add(1)
	s.activeStreams.Add(1)

	st, err := s.opener(ctx)
	if err != nil {
		s.activeStreams.Add(-1)
		return nil, err
	}

	cs := &countingStream{
		FramedStream: st,
		sess:         s,
		onClose: func() {
			if s.activeStreams.Load() <= 0 && s.draining.Load() {
				select {
				case s.drainCh <- struct{}{}:
				default:
				}
			}
		},
	}
	return cs, nil
}

// Close marks the session as draining and closes any underlying resources.
func (s *ManagedSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.draining.Store(true)
		if s.closeFn != nil {
			err = s.closeFn()
		}
	})
	return err
}

func (s *ManagedSession) ID() int64                   { return s.id }
func (s *ManagedSession) BytesMoved() int64           { return s.bytesMoved.Load() }
func (s *ManagedSession) ActiveStreams() int32        { return s.activeStreams.Load() }
func (s *ManagedSession) TotalStreams() int64         { return s.totalStreams.Load() }
func (s *ManagedSession) IsDraining() bool            { return s.draining.Load() }
func (s *ManagedSession) CreatedAt() time.Time        { return s.createdAt }
func (s *ManagedSession) MaxLifetime() time.Duration  { return s.maxLifetime }
func (s *ManagedSession) MaxBytes() int64             { return s.maxBytes }
func (s *ManagedSession) MaxStreams() int             { return s.maxStreams }
func (s *ManagedSession) SetMaxBytes(b int64)         { s.maxBytes = b }
func (s *ManagedSession) SetMaxLifetime(d time.Duration) { s.maxLifetime = d }
func (s *ManagedSession) SetCreatedAt(t time.Time)    { s.createdAt = t }

// PeerSessionPool manages active and draining sessions for a target peer ID.
type PeerSessionPool struct {
	peerID        types.PeerID
	cfg           config.FlowShapingConfig
	opener        StreamOpener
	sessionDialer func(ctx context.Context) (StreamOpener, func() error, error)

	mu            sync.Mutex
	dialMu        sync.Mutex
	active        []*ManagedSession
	draining      []*ManagedSession
	lastDialTime  time.Time
	closed        bool
	stopCh        chan struct{}
	drainTimeout  time.Duration
	nextSessionID atomic.Int64
}

// NewPeerSessionPool creates a new pool using a StreamOpener.
func NewPeerSessionPool(peerID types.PeerID, cfg config.FlowShapingConfig, opener StreamOpener) *PeerSessionPool {
	cfg.Normalize()
	return &PeerSessionPool{
		peerID:       peerID,
		cfg:          cfg,
		opener:       opener,
		drainTimeout: 15 * time.Second,
		stopCh:       make(chan struct{}),
	}
}

// NewPeerSessionPoolWithDialer creates a new pool with a custom dialer function for creating sessions.
func NewPeerSessionPoolWithDialer(peerID types.PeerID, cfg config.FlowShapingConfig, dialer func(ctx context.Context) (StreamOpener, func() error, error)) *PeerSessionPool {
	cfg.Normalize()
	return &PeerSessionPool{
		peerID:        peerID,
		cfg:           cfg,
		sessionDialer: dialer,
		drainTimeout:  15 * time.Second,
		stopCh:        make(chan struct{}),
	}
}

// SetDrainTimeout sets the timeout for waiting on active streams to finish during session drain.
func (p *PeerSessionPool) SetDrainTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drainTimeout = d
}

// IsClosed reports whether the pool is closed.
func (p *PeerSessionPool) IsClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// ActiveSessions returns a snapshot of active sessions.
func (p *PeerSessionPool) ActiveSessions() []*ManagedSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*ManagedSession(nil), p.active...)
}

// DrainingSessions returns a snapshot of draining sessions.
func (p *PeerSessionPool) DrainingSessions() []*ManagedSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*ManagedSession(nil), p.draining...)
}

// TotalSessions returns the number of sessions created by the pool.
func (p *PeerSessionPool) TotalSessions() int64 {
	return p.nextSessionID.Load()
}

// OpenStream selects an active session or creates a new one and opens a stream.
func (p *PeerSessionPool) OpenStream(ctx context.Context) (FramedStream, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		sess, err := p.getOrDialSession(ctx)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess.OpenStream(ctx)
		}
	}
}

func (p *PeerSessionPool) getOrDialSession(ctx context.Context) (*ManagedSession, error) {
	// 1. Fast path: check for an active session with capacity that does not need rotation.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("session pool closed")
	}

	var best *ManagedSession
	for _, s := range p.active {
		if s.draining.Load() || s.ShouldRotate() {
			continue
		}
		if s.activeStreams.Load() >= int32(p.cfg.MaxConcurrency) {
			continue
		}
		if best == nil || s.activeStreams.Load() < best.activeStreams.Load() {
			best = s
		}
	}
	if best != nil {
		p.mu.Unlock()
		return best, nil
	}
	p.mu.Unlock()

	// 2. Slow path: Acquire dialMu to serialize dials and protect against thundering herd.
	p.dialMu.Lock()
	defer p.dialMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("session pool closed")
	}

	// Double check if another goroutine dialed a suitable session while we waited for dialMu.
	best = nil
	for _, s := range p.active {
		if s.draining.Load() || s.ShouldRotate() {
			continue
		}
		if s.activeStreams.Load() >= int32(p.cfg.MaxConcurrency) {
			continue
		}
		if best == nil || s.activeStreams.Load() < best.activeStreams.Load() {
			best = s
		}
	}
	if best != nil {
		p.mu.Unlock()
		return best, nil
	}

	now := time.Now()
	var elapsed time.Duration
	if !p.lastDialTime.IsZero() {
		elapsed = now.Sub(p.lastDialTime)
	} else {
		elapsed = p.cfg.MinDialInterval
	}
	pacingWait := p.cfg.MinDialInterval - elapsed

	// Dial pacing check:
	// If bytes exceeded threshold before minDialInterval (e.g. on gigabit line),
	// continue on the current session rather than blocking traffic until minDialInterval passes.
	if pacingWait > 0 && len(p.active) > 0 {
		var fallback *ManagedSession
		for _, s := range p.active {
			if s.draining.Load() {
				continue
			}
			if fallback == nil || s.activeStreams.Load() < fallback.activeStreams.Load() {
				fallback = s
			}
		}
		if fallback != nil {
			p.mu.Unlock()
			return fallback, nil
		}
	}

	if pacingWait > 0 && len(p.active) == 0 {
		p.mu.Unlock()
		select {
		case <-time.After(pacingWait):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.stopCh:
			return nil, errors.New("session pool closed")
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("session pool closed")
		}
	}

	// 3. Create the new session outside p.mu.
	p.mu.Unlock()

	var op StreamOpener
	var closeFn func() error
	var err error

	if p.sessionDialer != nil {
		op, closeFn, err = p.sessionDialer(ctx)
		if err != nil {
			return nil, err
		}
	} else if p.opener != nil {
		op = p.opener
	} else {
		return nil, errors.New("no stream opener configured in session pool")
	}

	id := p.nextSessionID.Add(1)
	newSess := NewManagedSession(id, op, p.cfg, closeFn)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = newSess.Close()
		return nil, errors.New("session pool closed")
	}

	p.lastDialTime = time.Now()

	// Rotate old sessions that should rotate.
	var keep []*ManagedSession
	for _, s := range p.active {
		if s.ShouldRotate() {
			s.draining.Store(true)
			p.draining = append(p.draining, s)
			go p.drainSession(s)
		} else {
			keep = append(keep, s)
		}
	}

	// Respect MaxConnections
	for len(keep) >= p.cfg.MaxConnections && len(keep) > 0 {
		oldest := keep[0]
		keep = keep[1:]
		oldest.draining.Store(true)
		p.draining = append(p.draining, oldest)
		go p.drainSession(oldest)
	}

	keep = append(keep, newSess)
	p.active = keep
	p.mu.Unlock()

	return newSess, nil
}

func (p *PeerSessionPool) drainSession(sess *ManagedSession) {
	p.mu.Lock()
	timeout := p.drainTimeout
	p.mu.Unlock()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		if sess.activeStreams.Load() <= 0 {
			break
		}
		select {
		case <-sess.drainCh:
			if sess.activeStreams.Load() <= 0 {
				goto done
			}
		case <-timer.C:
			goto done
		case <-p.stopCh:
			goto done
		}
	}

done:
	_ = sess.Close()
	p.mu.Lock()
	for i, s := range p.draining {
		if s == sess {
			p.draining = append(p.draining[:i], p.draining[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
}

// Close closes the pool and all active and draining sessions.
func (p *PeerSessionPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.stopCh)

	all := append([]*ManagedSession(nil), p.active...)
	all = append(all, p.draining...)
	p.active = nil
	p.draining = nil
	p.mu.Unlock()

	for _, s := range all {
		_ = s.Close()
	}
	return nil
}
