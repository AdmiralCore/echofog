package swarm

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// answerTransport answers every dial as the peer got.
type answerTransport struct {
	got   types.PeerID
	dials atomic.Int32
}

func (a *answerTransport) Addr() net.Addr                        { return nil }
func (a *answerTransport) ListenEarly(context.Context) error     { return nil }
func (a *answerTransport) ConnChan() <-chan network.NewConnEvent { return nil }
func (a *answerTransport) Close() error                          { return nil }
func (a *answerTransport) GetSTUNConn() net.PacketConn           { return nil }
func (a *answerTransport) DialEarly(context.Context, string) error {
	a.dials.Add(1)
	return &network.PeerMismatchError{Got: a.got}
}

// A stale record of an old ID at this node's own address leads the node to itself once; after
// that the address is not dialed again. Another peer at a stale address is only a mismatch.
func TestSwarmStopsDialingItsOwnAddress(t *testing.T) {
	self, other := types.PeerID{1}, types.PeerID{2}
	tr := &answerTransport{got: self}
	s := &Swarm{selfID: self, netTransport: tr}

	for i := 0; i < 3; i++ {
		if err := s.dialEarly(context.Background(), "203.0.113.5:443"); !errors.Is(err, ErrSelfAddr) {
			t.Fatalf("dial %d: got %v, want ErrSelfAddr", i, err)
		}
	}
	if n := tr.dials.Load(); n != 1 {
		t.Fatalf("own address dialed %d times, want 1", n)
	}

	tr.got = other
	err := s.dialEarly(context.Background(), "203.0.113.6:443")
	if !errors.Is(err, network.ErrPeerMismatch) || errors.Is(err, ErrSelfAddr) {
		t.Fatalf("someone else's stale address: got %v, want a plain mismatch", err)
	}
	if err := s.dialEarly(context.Background(), "203.0.113.6:443"); !errors.Is(err, network.ErrPeerMismatch) {
		t.Fatalf("someone else's address is dialed again: %v", err)
	}
	if n := tr.dials.Load(); n != 3 {
		t.Fatalf("dials: %d, want 3", n)
	}
}
