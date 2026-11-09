package sigbridge

import (
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/call/v1sig"
)

// wrappedBytes is what the remembered wrappers hold, as the bound counts them.
func (r *rig) wrappedBytes() int {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	n := 0
	for _, e := range r.b.wrapper {
		n += len(e.raw)
	}
	return n
}

// bigManifest builds a forwarded signal whose wrapper is close to the 64 KiB SIGNAL limit.
func bigManifest(t *testing.T, msgID uint64, size int) (*pb.CallSignal, []byte) {
	t.Helper()
	m := &pb.CallSignal{
		Action:  pb.Action_ACTION_TREE_MANIFEST,
		CallId:  strings.Repeat("c", size),
		Sender:  idb(pid(2)),
		MsgId:   msgID,
		Payload: &pb.CallSignal_TreeManifest{TreeManifest: &pb.TreeManifest{CallId: "c1", Epoch: 1, Coordinator: idb(pid(2))}},
	}
	raw, err := call.Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, raw
}

// A peer that floods maximum-size forwarded signals must not make the node hold hundreds of
// megabytes: the window of wrappers is bounded by bytes, and what is still needed survives.
func TestWrapperCacheIsBoundedByBytes(t *testing.T) {
	r := newRig(t, false)
	r.b = New(Config{Self: r.self, Service: r.svc, Context: t.Context(), Now: func() time.Time { return r.now },
		Deliver: func([32]byte, []byte) {}}) // the flood only has to fill the wrapper cache
	for i := 0; i < 300; i++ {
		m, raw := bigManifest(t, uint64(i+1), 65000)
		r.b.OnSignal(t.Context(), call.Peer{Device: pid(3)}, m, raw)
	}
	if got := r.wrappedBytes(); got > maxWrapperBytes {
		t.Fatalf("wrapper cache holds %d bytes, limit %d", got, maxWrapperBytes)
	}
	// The newest wrapper is not lost to the flood: it still forwards byte for byte.
	m, raw := bigManifest(t, 1000, 40)
	r.b.OnSignal(t.Context(), call.Peer{Device: pid(3)}, m, raw)
	v1, err := v1sig.ToV1(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.b.SendSignal(pid(4), false, v1); err != nil {
		t.Fatalf("the newest wrapper was lost: %v", err)
	}
	if len(r.svc.fwd) != 1 || string(r.svc.fwd[0]) != string(raw) {
		t.Fatalf("forwarded %d wrappers, want the newest one unchanged", len(r.svc.fwd))
	}
}

// A signal may arrive again while a stalled path is still carrying it; the remembered wrapper must
// live from the last arrival, or the forward that follows is refused as unknown.
func TestWrapperCacheRefreshesWhileTheSignalKeepsArriving(t *testing.T) {
	r := newRig(t, false)
	origin := &pb.CallSignal{
		Action: pb.Action_ACTION_TREE_MANIFEST, CallId: "c1", Sender: idb(pid(2)), MsgId: 77, Epoch: 1, Timestamp: 5,
		Payload: &pb.CallSignal_TreeManifest{TreeManifest: &pb.TreeManifest{CallId: "c1", Epoch: 1, Coordinator: idb(pid(2))}},
	}
	r.inbound(t, pid(3), origin)
	r.now = r.now.Add(29 * time.Second)
	r.inbound(t, pid(3), origin) // the same copy again, still inside the window
	r.now = r.now.Add(2 * time.Second)
	v1, err := v1sig.ToV1(origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.b.SendSignal(pid(4), false, v1); err != nil {
		t.Fatalf("a wrapper that was still arriving expired: %v", err)
	}
}
