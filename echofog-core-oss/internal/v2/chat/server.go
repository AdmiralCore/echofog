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
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
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

// Server answers BUNDLE_GET.
type Server struct{}

// New builds a Server.
func New(cfg Config) *Server { return &Server{} }

// Handle serves one chat/2 stream. It owns the stream and closes it.
func (s *Server) Handle(ctx context.Context, p Peer, st Stream) {}
