package noise3

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// failingKV is a mapKV whose PutTTL can be switched into a failure.
type failingKV struct {
	*mapKV
	mu      sync.Mutex
	failing bool
}

func (k *failingKV) PutTTL(key, value []byte, ttl time.Duration) error {
	k.mu.Lock()
	failing := k.failing
	k.mu.Unlock()
	if failing {
		return errors.New("kv is down")
	}
	return k.mapKV.PutTTL(key, value, ttl)
}

func (k *failingKV) setFailing(v bool) {
	k.mu.Lock()
	k.failing = v
	k.mu.Unlock()
}

// TestReplayDirtyHeldUntilPutSucceeds reproduces ПА1-1 (Н-2): persistLocked cleared dirty before
// writing and ignored the KV error, so the generation was never written again and a restart
// reopened the replay window. dirty must stay set until PutTTL succeeds.
func TestReplayDirtyHeldUntilPutSucceeds(t *testing.T) {
	kv := &failingKV{mapKV: newMapKV(), failing: true}
	t0 := time.Unix(93_000, 0)
	c := newReplayCache(kv, t0, nil)
	sel := selFor("persist-retry")
	if c.seen(sel, t0) {
		t.Fatal("a fresh selector is reported as a replay")
	}

	c.flush(t0)
	if !c.set.anyDirty() {
		t.Fatal("dirty was cleared although PutTTL failed")
	}
	if !c.seen(sel, t0) {
		t.Fatal("a repeated selector was admitted after a failed persist")
	}

	kv.setFailing(false)
	c.flush(t0)
	if c.set.anyDirty() {
		t.Fatal("dirty was not cleared after a successful retry")
	}
	again := newReplayCache(kv, t0, nil)
	if !again.seen(sel, t0) {
		t.Fatal("selector lost across a restart after the retry")
	}
}

// gatedKV blocks the first PutTTL until the test releases it, which opens the window between
// the snapshot in persistLocked and the write to KV.
type gatedKV struct {
	*mapKV
	gate    chan struct{}
	blocked chan struct{}
	once    sync.Once
}

func newGatedKV() *gatedKV {
	return &gatedKV{mapKV: newMapKV(), gate: make(chan struct{}), blocked: make(chan struct{})}
}

func (k *gatedKV) PutTTL(key, value []byte, ttl time.Duration) error {
	k.once.Do(func() {
		close(k.blocked)
		<-k.gate
	})
	return k.mapKV.PutTTL(key, value, ttl)
}

func (k *gatedKV) release() { close(k.gate) }

// TestReplayDirtyResetIsVersioned reproduces F1 of the PA-FIX-2b acceptance: a concurrent seen
// during the KV write must not be swallowed by clearing dirty after PutTTL returns. The
// generation is dirtied in the window, so it stays dirty and the next flush writes it; the
// selector then survives a restart.
func TestReplayDirtyResetIsVersioned(t *testing.T) {
	kv := newGatedKV()
	t0 := time.Unix(95_000, 0)
	c := newReplayCache(kv, t0, nil)
	first := selFor("persist-window-first")
	if c.seen(first, t0) {
		t.Fatal("a fresh selector is reported as a replay")
	}

	done := make(chan struct{})
	go func() {
		c.flush(t0) // blocks inside the first PutTTL, with r.mu released
		close(done)
	}()
	<-kv.blocked

	// A concurrent seen (readLoop) in the window touches the same generation.
	second := selFor("persist-window-second")
	if c.seen(second, t0) {
		t.Fatal("a fresh selector is reported as a replay")
	}

	kv.release()
	<-done

	if !c.set.anyDirty() {
		t.Fatal("dirty was cleared although the generation changed during PutTTL")
	}
	c.flush(t0)
	if c.set.anyDirty() {
		t.Fatal("dirty was not cleared after the retry flush")
	}

	restarted := newReplayCache(kv.mapKV, t0, nil)
	if !restarted.seen(second, t0) {
		t.Fatal("selector added during PutTTL was lost across a restart")
	}
}

// TestReplayPersistErrorIsReported checks that a failed write reaches the journal hook.
func TestReplayPersistErrorIsReported(t *testing.T) {
	kv := &failingKV{mapKV: newMapKV(), failing: true}
	t0 := time.Unix(94_000, 0)
	c := newReplayCache(kv, t0, nil)
	var got error
	c.onError = func(err error) { got = err }
	c.seen(selFor("report"), t0)
	c.flush(t0)
	if got == nil {
		t.Fatal("a persistence error was not reported")
	}
}
