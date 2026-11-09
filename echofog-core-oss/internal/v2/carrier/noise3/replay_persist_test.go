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
