// Package chat is the chat/2 service (docs/protocol/v2/services.md 4) as far as T7.1 goes: the
// BUNDLE_GET / BUNDLE exchange that hands a peer a prekeys/2 with one fresh one-time prekey.
// DELIVER and the sealed sender are T7.2.
//
// The package does not depend on the node runtime: it takes the authenticated peer and the stream
// the runtime hands to a service handler, like mailbox/2 does.
package chat

import (
	"context"
	"net/netip"
	"sync"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Frame types (services.md 4.1).
const (
	FrameDeliver    uint64 = 0x40
	FrameDelivered  uint64 = 0x41
	FrameBundleGet  uint64 = 0x42
	FrameBundle     uint64 = 0x43
	BundleGetLimit         = 2           // per session ...
	BundleGetWindow        = time.Minute // ... per minute
	UnitFactor             = 3           // services.md 1: per diversity unit with a factor of 3
)

// Bundler makes the answer to BUNDLE_GET; *chatcrypto.Crypto satisfies it.
type Bundler interface {
	Bundle() ([]byte, error)
}

// Stream is the part of *mux.Stream the handler uses.
type Stream interface {
	ReadFrame() (frame.Frame, error)
	WriteFrame(typ uint64, flags byte, payload []byte) error
	CloseWithCode(code uint64, reason string) error
	Close() error
}

// Peer is the authenticated peer of a stream.
type Peer struct {
	SID    [16]byte
	Path   mux.PathKind
	Remote netip.AddrPort
}

// Config configures a Server.
type Config struct {
	Bundles Bundler
	// Receiver takes the envelopes of DELIVER; nil refuses them.
	Receiver Receiver
	// Now is the node clock (AD13).
	Now func() time.Time
	// After returns a channel that fires after d (AD13: timeouts through an injectable timer); nil
	// means time.After.
	After func(d time.Duration) <-chan time.Time
}

// Receiver handles an incoming envelope and returns the DELIVERED status; *Service satisfies it.
type Receiver interface {
	Receive(ctx context.Context, envelope []byte) uint32
}

// Timeouts (services.md 4.1).
const (
	DeliverTimeout   = 15 * time.Second
	BundleGetTimeout = 5 * time.Second
	// DeliverRate is the most DELIVER frames per second one session may send (services.md 4.4).
	DeliverRate = 20
)

// maxTracked bounds the rate tables: a flood of sessions cannot grow them without limit.
const maxTracked = 4096

// Server answers BUNDLE_GET.
type Server struct {
	bundles  Bundler
	receiver Receiver
	now      func() time.Time
	after    func(d time.Duration) <-chan time.Time

	mu       sync.Mutex
	sessions map[[16]byte][]time.Time
	units    map[netip.Prefix][]time.Time
	delivers map[[16]byte][]time.Time // DELIVER times of the last second, by session
}

// New builds a Server.
func New(cfg Config) *Server {
	after := cfg.After
	if after == nil {
		after = time.After
	}
	return &Server{
		bundles:  cfg.Bundles,
		receiver: cfg.Receiver,
		now:      cfg.Now,
		after:    after,
		sessions: map[[16]byte][]time.Time{},
		units:    map[netip.Prefix][]time.Time{},
		delivers: map[[16]byte][]time.Time{},
	}
}

// allow counts one request against the session and the diversity unit of the peer and reports
// whether both still have room (services.md 4.4: 2 per minute per session; services.md 1: the same
// per network diversity unit with a factor of 3, so new sessions do not reset the quota).
func (s *Server) allow(p Peer) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)

	sess := s.sessions[p.SID]
	if len(sess) >= BundleGetLimit {
		return false
	}
	var unit netip.Prefix
	if p.Remote.IsValid() {
		unit = dht.UnitOf(p.Remote.Addr())
		if len(s.units[unit]) >= BundleGetLimit*UnitFactor {
			return false
		}
	}
	if (len(sess) == 0 && len(s.sessions) >= maxTracked) || (unit.IsValid() && len(s.units[unit]) == 0 && len(s.units) >= maxTracked) {
		return false
	}
	s.sessions[p.SID] = append(sess, now)
	if unit.IsValid() {
		s.units[unit] = append(s.units[unit], now)
	}
	return true
}

// allowDeliver counts one DELIVER against the session: at most DeliverRate in any second.
func (s *Server) allowDeliver(sid [16]byte) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, ts := range s.delivers {
		i := 0
		for i < len(ts) && now.Sub(ts[i]) >= time.Second {
			i++
		}
		if ts = ts[i:]; len(ts) == 0 {
			delete(s.delivers, k)
		} else {
			s.delivers[k] = ts
		}
	}
	ts := s.delivers[sid]
	if len(ts) >= DeliverRate || (len(ts) == 0 && len(s.delivers) >= maxTracked) {
		return false
	}
	s.delivers[sid] = append(ts, now)
	return true
}

// prune drops the entries older than the window. s.mu is held.
func (s *Server) prune(now time.Time) {
	keep := func(ts []time.Time) []time.Time {
		i := 0
		for i < len(ts) && now.Sub(ts[i]) >= BundleGetWindow {
			i++
		}
		return ts[i:]
	}
	for k, ts := range s.sessions {
		if ts = keep(ts); len(ts) == 0 {
			delete(s.sessions, k)
		} else {
			s.sessions[k] = ts
		}
	}
	for k, ts := range s.units {
		if ts = keep(ts); len(ts) == 0 {
			delete(s.units, k)
		} else {
			s.units[k] = ts
		}
	}
}

// Handle serves one chat/2 stream. It owns the stream and closes it.
//
// Every refusal of BUNDLE_GET (a relayed session, a quota, a timeout, a failure to make the prekey)
// is the same CLOSE{refused}: a peer learns nothing about the node from which one it got (AD20).
func (s *Server) Handle(ctx context.Context, p Peer, st Stream) {
	defer st.Close()
	type read struct {
		f   frame.Frame
		err error
	}
	// The reader runs on its own so that the wait for the next frame can be limited: a peer that opens
	// a stream and says nothing is closed after DeliverTimeout (services.md 4.1). It ends when the
	// stream is closed.
	frames := make(chan read)
	done := make(chan struct{})
	defer close(done) // the reader ends when Handle returns, whatever the context of the node
	go func() {
		for {
			f, err := st.ReadFrame()
			select {
			case frames <- read{f, err}:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var r read
		select {
		case r = <-frames:
		case <-s.after(DeliverTimeout):
			_ = st.CloseWithCode(frame.CodeTimeout, "")
			return
		case <-ctx.Done():
			return
		}
		if r.err != nil {
			return
		}
		f := r.f
		if f.Ignorable() {
			continue
		}
		switch f.Type {
		case FrameBundleGet:
			if !s.handleBundleGet(p, st, f) {
				return
			}
		case FrameDeliver:
			if !s.handleDeliver(ctx, p, st, f) {
				return
			}
		default:
			_ = st.CloseWithCode(frame.CodeUnknownFrame, "")
			return
		}
	}
}

func (s *Server) handleBundleGet(p Peer, st Stream, f frame.Frame) bool {
	req, err := chatpb.ParseBundleGet(f.Payload)
	if err != nil {
		_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
		return false
	}
	if p.Path != mux.PathDirect || !s.allow(p) {
		_ = st.CloseWithCode(frame.CodeRefused, "")
		return false
	}
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := s.bundles.Bundle()
		done <- result{raw, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-s.after(BundleGetTimeout):
		r.err = context.DeadlineExceeded
	}
	if r.err != nil {
		_ = st.CloseWithCode(frame.CodeRefused, "")
		return false
	}
	out, err := objects.Marshal(&chatpb.Bundle{Req: req.Req, Prekeys: r.raw})
	return err == nil && st.WriteFrame(FrameBundle, 0, out) == nil
}

func (s *Server) handleDeliver(ctx context.Context, p Peer, st Stream, f frame.Frame) bool {
	req, err := chatpb.ParseDeliver(f.Payload)
	if err != nil {
		_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
		return false
	}
	if !s.allowDeliver(p.SID) {
		_ = st.CloseWithCode(frame.CodeOverloaded, "")
		return false
	}
	status := chatpb.StatusRefused
	if s.receiver != nil {
		// Receive may look an account up in the DHT, chosen by the sender: it has the DELIVER time,
		// and is cancelled when that passes.
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan uint32, 1)
		go func() { done <- s.receiver.Receive(rctx, req.Envelope) }()
		select {
		case status = <-done:
		case <-s.after(DeliverTimeout):
			cancel()
			_ = st.CloseWithCode(frame.CodeTimeout, "")
			return false
		}
	}
	out, err := objects.Marshal(&chatpb.Delivered{Req: req.Req, Status: status})
	return err == nil && st.WriteFrame(FrameDelivered, 0, out) == nil
}
