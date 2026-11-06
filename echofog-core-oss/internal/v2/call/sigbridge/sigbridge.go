// Package sigbridge connects the call layer's signals (v1 envelopes, package v1sig) to the call/2
// signalling service (T9.3b): it sends the call layer's signals as signed CallSignal, forwards
// flooded signals byte for byte, admits joins through the JoinGuard, and hands verified inbound
// signals to the call layer as v1 envelopes.
package sigbridge

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
)

// ErrNotImplemented is returned by the stubs.
var ErrNotImplemented = errors.New("sigbridge: not implemented")

// Service is the part of *call.Service the bridge uses.
type Service interface {
	Send(ctx context.Context, p call.Peer, m *pb.CallSignal) error
	Forward(ctx context.Context, p call.Peer, raw []byte) error
}

// Config wires a Bridge.
type Config struct {
	// Self is the local device ID: the call layer's own peer ID.
	Self    [32]byte
	Service Service
	// Guard admits joins. Nil admits every well-formed join.
	Guard *call.JoinGuard
	// Deliver hands a verified inbound signal, as a v1 envelope, to the call layer; from is the
	// neighbour it came from.
	Deliver func(from [32]byte, v1 []byte)
	Context context.Context
	Now     func() time.Time
}

// Bridge is the signal path between the call layer and call/2.
type Bridge struct{ cfg Config }

// New returns a bridge.
func New(cfg Config) *Bridge { return &Bridge{cfg: cfg} }

// SendSignal sends a v1 envelope the call layer produced to a neighbour. lossy copies are skipped:
// call/2 duplicates lossy signals on CALL_SIGNAL itself when it sends the reliable one.
func (b *Bridge) SendSignal(to [32]byte, lossy bool, payload []byte) error {
	return ErrNotImplemented
}

// OnSignal receives a verified call/2 signal (call.Config.OnSignal).
func (b *Bridge) OnSignal(ctx context.Context, p call.Peer, m *pb.CallSignal, raw []byte) {}
