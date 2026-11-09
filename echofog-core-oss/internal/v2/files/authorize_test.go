package files

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// A file posted in a group is served by the rule of that group's log, and the rule is asked at every
// WANT: a member gets the bytes, a stranger and one the group no longer holds get one and the same
// CANCEL (services.md §8, AD20).
func TestAuthorizeIsAskedAtEveryWant(t *testing.T) {
	l := newLab(t, ServerConfig{})
	data := content(3*ChunkSize, 21)

	var mu sync.Mutex
	allowed := map[[32]byte]bool{nodeB.node(): true}
	sh, err := PrepareShare(bytes.NewReader(data), int64(len(data)), "в группе.bin", "application/octet-stream", nodeA.node(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sh.Authorize = func(n [32]byte) bool {
		mu.Lock()
		defer mu.Unlock()
		return allowed[n]
	}
	if err := l.srv.Add(sh); err != nil {
		t.Fatal(err)
	}
	o := sh.Offer

	kB, kC := l.dial(nodeB), l.dial(nodeC)
	// the member the rule lets in is served the whole file
	st := newMemStore(o)
	if err := l.fetch(kB, o, st, fast()); err != nil {
		t.Fatalf("a member: %v", err)
	}
	if !bytes.Equal(st.bytes(), data) || !st.done {
		t.Fatal("a member got other bytes")
	}
	sent := l.srv.Stats().ChunksSent.Load()

	// a stranger and a file nobody has give the same answer: CANCEL unavailable, the same size
	refused := askRaw(t, l, kC, o.FID)
	unknown := fid(202)
	refusedUnknown := askRaw(t, l, kC, unknown)
	for name, f := range map[string]frame.Frame{"known file, not a member": refused, "unknown file": refusedUnknown} {
		if f.Type != TypeCancel {
			t.Fatalf("%s: frame %#x", name, f.Type)
		}
		c, err := ParseCancel(f.Payload)
		if err != nil || c.Reason != ReasonUnavailable {
			t.Fatalf("%s: %+v %v", name, c, err)
		}
	}
	if len(refused.Payload) != len(refusedUnknown.Payload) {
		t.Fatal("the two refusals differ in size: an oracle for the file's existence")
	}
	if err := l.fetch(kC, o, newMemStore(o), fast()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a stranger fetched: %v", err)
	}

	// the member is taken out of the group: the next WANT is refused, with the same answer
	mu.Lock()
	delete(allowed, nodeB.node())
	mu.Unlock()
	if err := l.fetch(kB, o, newMemStore(o), fast()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a removed member fetched: %v", err)
	}
	refusedRemoved := askRaw(t, l, kB, o.FID)
	if refusedRemoved.Type != TypeCancel || len(refusedRemoved.Payload) != len(refusedUnknown.Payload) {
		t.Fatalf("a removed member: frame %#x, %d bytes", refusedRemoved.Type, len(refusedRemoved.Payload))
	}
	if c, err := ParseCancel(refusedRemoved.Payload); err != nil || c.Reason != ReasonUnavailable {
		t.Fatalf("a removed member: %+v %v", c, err)
	}
	if l.srv.Stats().ChunksSent.Load() != sent {
		t.Fatal("a refused WANT got chunks")
	}
}
