package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// TestRuntimeStatusReportsSessions: the status snapshot of a running node lists the live
// sessions with the suite, the path kind, the path set and stream counts (T0.3 `efv2 status`).
func TestRuntimeStatusReportsSessions(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peer, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.Chat, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()

	st := a.rt.Status()
	if st.Node != a.id.NodeID || st.Device != a.id.DeviceID {
		t.Fatalf("status identity %x/%x, want %x/%x", st.Node, st.Device, a.id.NodeID, a.id.DeviceID)
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("status has %d sessions, want 1: %+v", len(st.Sessions), st.Sessions)
	}
	s := st.Sessions[0]
	if s.Device != peer.Device || s.Node != peer.Node {
		t.Fatalf("session peer %x/%x, want %x/%x", s.Node, s.Device, peer.Node, peer.Device)
	}
	if s.Suite == 0 {
		t.Fatal("session has no handshake suite")
	}
	if s.Path != mux.PathDirect {
		t.Fatalf("path %v, want direct", s.Path)
	}
	if len(s.Paths) == 0 || s.ActivePath != 0 {
		t.Fatalf("paths %v active %d, want the initial path 0", s.Paths, s.ActivePath)
	}
	if s.Standby != 0 {
		t.Fatalf("standby %d, want 0", s.Standby)
	}
	if s.StreamsLocal < 1 {
		t.Fatalf("local streams %d, want at least the open chat stream", s.StreamsLocal)
	}
	if s.SID != peer.SID {
		t.Fatalf("session id %x, want %x", s.SID, peer.SID)
	}
	if len(st.Abuse) != 0 {
		t.Fatalf("abuse counters %+v, want none", st.Abuse)
	}
}

// TestRuntimeStatusBeforeStart is the snapshot of a node that has not started: no sessions and
// no panic.
func TestRuntimeStatusBeforeStart(t *testing.T) {
	n := newTestNode(t, cards.Hidden, false, nil, nil)
	st := n.rt.Status()
	if len(st.Sessions) != 0 || len(st.Abuse) != 0 {
		t.Fatalf("status of a stopped node: %+v", st)
	}
	if st.Node != n.id.NodeID || st.Device != n.id.DeviceID {
		t.Fatalf("status identity %x/%x", st.Node, st.Device)
	}
}
