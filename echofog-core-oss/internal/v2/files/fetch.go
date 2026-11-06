package files

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Store is where the receiver keeps the chunks it has verified. Have is the resume bitmap.
type Store interface {
	Have(i int) bool
	Count() int
	Put(i int, data []byte) error
	// Commit is called once all chunks are in.
	Commit() error
}

// Gate bounds the transfers a node runs at once (both as sender and receiver).
type Gate struct{}

// NewGate returns a gate for at most n transfers.
func NewGate(n int) *Gate { return &Gate{} }

// Acquire takes a slot; it returns ErrBusy when all are taken. Call the returned function once to
// give the slot back.
func (g *Gate) Acquire() (release func(), err error) { return func() {}, ErrBusy }

// FetchStats are the observations of one fetch (tests and the UI).
type FetchStats struct {
	Requested atomic.Uint64
	Resent    atomic.Uint64
	Rejected  atomic.Uint64
	MaxWindow atomic.Int64
	Losses    atomic.Uint64
}

// FetchOptions tune a fetch.
type FetchOptions struct {
	Clock         carrier.Clock
	MinRTO        time.Duration // default 1.5 s
	InitialWindow int           // default 16
	MaxWindow     int           // default 256
	Gate          *Gate
	Stats         *FetchStats
	OnProgress    func(done, total int)
}

// Fetch downloads the file of offer over st (a files/2 stream opened to the sender) into store.
// It returns nil when every chunk is in and verified.
func Fetch(ctx context.Context, st *mux.Stream, offer Offer, store Store, opt FetchOptions) error {
	return ErrUnavailable
}

// Waiting reports whether a failed fetch can simply be tried again later: the sender is not
// there now, the store keeps what was verified.
func Waiting(err error) bool { return false }
