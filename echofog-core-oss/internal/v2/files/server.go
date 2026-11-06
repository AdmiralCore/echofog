package files

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Peer is the authenticated session that opened a stream. It comes from the session, never from
// a frame (services.md §8: the sender serves only recipients named by the offer).
type Peer struct {
	Node   [32]byte
	Device [32]byte
}

// Limits are the holder's limits. Zero fields take the defaults.
type Limits struct {
	// PerPeer and Total bound the transfers (streams that asked for chunks) running at once.
	PerPeer int
	Total   int
	// Files and TreeBytes bound what the holder keeps shared; MaxFileSize bounds one file.
	Files       int
	TreeBytes   int64
	MaxFileSize int64
	// WantTimeout is how long a receiver may take to accept the chunks of one WANT before its
	// stream is reset; IdleTimeout is how long a stream may sit without a frame.
	WantTimeout time.Duration
	IdleTimeout time.Duration
}

// ServerConfig configures a Server.
type ServerConfig struct {
	// Clock is the injected clock (AD13). Nil means the system clock.
	Clock  carrier.Clock
	Limits Limits
	// OnDone is called when a recipient confirmed it got the whole file.
	OnDone func(fid [16]byte, node [32]byte)
}

// Share is a file the holder offers: where the bytes are, the tree over them, and who may fetch it.
type Share struct {
	Offer      Offer
	Source     io.ReaderAt
	Tree       *filexfer.Tree
	Recipients [][32]byte
}

// Stats counts what the server did.
type Stats struct {
	ChunksSent atomic.Uint64
	Refused    atomic.Uint64
	Violations atomic.Uint64
	Done       atomic.Uint64
	Active     atomic.Int64
}

// Server is the sender side of files/2 on one node.
type Server struct{}

// NewServer returns a server.
func NewServer(cfg ServerConfig) *Server { return &Server{} }

// PrepareShare hashes size bytes of src and builds the offer and the share for the recipients.
func PrepareShare(src io.ReaderAt, size int64, name, mime string, sender [32]byte, recipients [][32]byte) (Share, error) {
	return Share{}, ErrOffer
}

// Add starts sharing a file. It refuses a share that breaks the holder's quotas (ErrQuota).
func (s *Server) Add(sh Share) error { return ErrOffer }

// Remove stops sharing a file; streams serving it end.
func (s *Server) Remove(fid [16]byte) {}

// Stats returns the counters.
func (s *Server) Stats() *Stats { return &Stats{} }

// Handle serves one files/2 stream opened by an authenticated peer.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {}

// SetChunkFilter installs a test hook that drops chunks the server was about to send (loss).
func (s *Server) SetChunkFilter(f func(fid [16]byte, index uint64) bool) {}
