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
	// Now is the node clock (AD13).
	Now func() time.Time
}

// maxTracked bounds the rate tables: a flood of sessions cannot grow them without limit.
const maxTracked = 4096

// Server answers BUNDLE_GET.
type Server struct {
	bundles Bundler
	now     func() time.Time

	mu       sync.Mutex
	sessions map[[16]byte][]time.Time
	units    map[netip.Prefix][]time.Time
}

// New builds a Server.
func New(cfg Config) *Server {
	return &Server{
		bundles:  cfg.Bundles,
		now:      cfg.Now,
		sessions: map[[16]byte][]time.Time{},
		units:    map[netip.Prefix][]time.Time{},
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
// Every refusal of BUNDLE_GET (a relayed session, a quota, a failure to make the prekey) is the same
// CLOSE{refused}: a peer learns nothing about the node from which one it got (AD20).
func (s *Server) Handle(ctx context.Context, p Peer, st Stream) {
	defer st.Close()
	for ctx.Err() == nil {
		f, err := st.ReadFrame()
		if err != nil {
			return
		}
		if f.Ignorable() {
			continue
		}
		if f.Type != FrameBundleGet {
			_ = st.CloseWithCode(frame.CodeUnknownFrame, "")
			return
		}
		req, err := chatpb.ParseBundleGet(f.Payload)
		if err != nil {
			_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
			return
		}
		if p.Path != mux.PathDirect || !s.allow(p) {
			_ = st.CloseWithCode(frame.CodeRefused, "")
			return
		}
		raw, err := s.bundles.Bundle()
		if err != nil {
			_ = st.CloseWithCode(frame.CodeRefused, "")
			return
		}
		out, err := objects.Marshal(&chatpb.Bundle{Req: req.Req, Prekeys: raw})
		if err != nil || st.WriteFrame(FrameBundle, 0, out) != nil {
			return
		}
	}
}
