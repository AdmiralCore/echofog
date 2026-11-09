package sigbridge

import (
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/call/v1sig"
)

// When the wrapper cache is full and evicts, the signal a stalled path kept repeating is the one
// still in use and must survive; the signal that was never repeated is the one to go. A bare
// refresh of the arrival time is not enough: the key must also become the newest in the eviction
// order.
func TestWrapperCacheKeepsTheRepeatedWrapperWhenItEvicts(t *testing.T) {
	r := newRig(t, false)
	r.b = New(Config{Self: r.self, Service: r.svc, Context: t.Context(), Now: func() time.Time { return r.now },
		Deliver: func([32]byte, []byte) {}})

	const size = 65000
	signals := map[uint64]*pb.CallSignal{}
	raws := map[uint64][]byte{}
	add := func(msgID uint64) int {
		m, raw := bigManifest(t, msgID, size)
		signals[msgID] = m
		raws[msgID] = raw
		r.b.OnSignal(t.Context(), call.Peer{Device: pid(3)}, m, raw)
		return len(raw)
	}

	// Fill the cache until one more wrapper would cross the byte limit.
	first := add(1000)
	next := uint64(1001)
	r.b.mu.Lock()
	filled := r.b.bytes
	r.b.mu.Unlock()
	for filled+first <= maxWrapperBytes {
		add(next)
		next++
		r.b.mu.Lock()
		filled = r.b.bytes
		r.b.mu.Unlock()
	}
	if filled <= maxWrapperBytes-first {
		t.Fatalf("cache was not filled: %d bytes, wrapper %d", filled, first)
	}

	// The oldest wrapper keeps arriving on a stalled path: it becomes the newest.
	r.b.OnSignal(t.Context(), call.Peer{Device: pid(3)}, signals[1000], raws[1000])

	// New traffic now forces one eviction.
	add(next)

	// The repeated wrapper is still forwards; the one that never came again is gone.
	v1, err := v1sig.ToV1(signals[1000])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.b.SendSignal(pid(4), false, v1); err != nil {
		t.Fatalf("the wrapper that kept arriving was evicted: %v", err)
	}
	if len(r.svc.fwd) != 1 || string(r.svc.fwd[0]) != string(raws[1000]) {
		t.Fatalf("forwarded %d wrappers, want the repeated one unchanged", len(r.svc.fwd))
	}

	v1, err = v1sig.ToV1(signals[1001])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.b.SendSignal(pid(4), false, v1); !errors.Is(err, ErrNoWrapper) {
		t.Fatalf("the wrapper that never came again is still cached: %v", err)
	}
}
