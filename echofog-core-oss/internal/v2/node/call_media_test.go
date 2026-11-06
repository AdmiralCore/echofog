package node_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// observer reads the path kind of every CALL_VIDEO datagram a node receives, next to the call
// transport, which gets the same datagrams.
type kindLog struct {
	mu    sync.Mutex
	kinds []mux.PathKind
}

func (k *kindLog) count(kind mux.PathKind) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, x := range k.kinds {
		if x == kind {
			n++
		}
	}
	return n
}

func watchVideoKinds(ctx context.Context, w *relayWorld, n *testNode) *kindLog {
	log := &kindLog{}
	port := n.rt.Datagrams(frame.DgramCallVideo)
	go func() {
		for {
			d, err := port.Receive(ctx)
			if err != nil {
				return
			}
			log.mu.Lock()
			log.kinds = append(log.kinds, d.PathKind)
			log.mu.Unlock()
		}
	}()
	return log
}

// T9.3 scenario at the node level: A and B hold a relayed session, then punch a direct path, so
// the session has both. call_route decides which path call media takes; a relay path carries it
// as SEALED datagrams (wire.md 8.4), and call_route=relay never uses the direct path.
func TestCallMediaFollowsCallRouteOnARelayedAndPunchedSession(t *testing.T) {
	w := newRelayWorld(t, true)
	w.openChat(t)
	waitUntil(t, 15*time.Second, "a direct path next to the relay path", func() bool {
		ids, _, ok := w.a.rt.Paths(w.b.id.DeviceID)
		return ok && len(ids) >= 2
	})
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()

	ta, tb := w.a.rt.CallTransport(), w.b.rt.CallTransport()
	if ta == nil || tb == nil {
		t.Fatal("call/2 has no transport on the runtime")
	}
	var got atomic.Int64
	tb.RegisterFastFrameHandler(transport.MediaVideo, func([]byte, types.PeerID) error { got.Add(1); return nil })
	seen := watchVideoKinds(ctx, w, w.b)

	send := func(n int) {
		for i := 0; i < n; i++ {
			if err := ta.SendUnreliableFrame(types.PeerID(w.b.id.DeviceID), transport.MediaVideo, []byte("unit")); err != nil {
				t.Fatalf("send: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitGot := func(want int64) {
		waitUntil(t, 5*time.Second, "media delivered", func() bool { return got.Load() >= want })
	}

	w.a.rt.SetCallRoute(route.Relay)
	w.b.rt.SetCallRoute(route.Relay)
	send(30)
	waitGot(30)
	if r, d := seen.count(mux.PathRelay), seen.count(mux.PathDirect); r != 30 || d != 0 {
		t.Fatalf("call_route=relay: %d datagrams on the relay path, %d on the direct path; want 30 and 0", r, d)
	}

	w.a.rt.SetCallRoute(route.Direct)
	w.b.rt.SetCallRoute(route.Direct)
	send(10)
	waitGot(40)
	if r, d := seen.count(mux.PathRelay), seen.count(mux.PathDirect); r != 30 || d != 10 {
		t.Fatalf("call_route=direct: %d relay, %d direct; want 30 and 10", r, d)
	}

	// B insists on relay: media that A still sends on the direct path is dropped before the call
	// layer sees it.
	w.b.rt.SetCallRoute(route.Relay)
	send(5)
	waitUntil(t, 5*time.Second, "direct media dropped", func() bool { return tb.Stats().DroppedRoute >= 5 })
	if got.Load() != 40 {
		t.Fatalf("the call layer received %d units, want 40", got.Load())
	}
}
