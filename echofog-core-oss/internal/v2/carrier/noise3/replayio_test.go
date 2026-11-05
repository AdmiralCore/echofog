package noise3

import (
	"sync"
	"testing"
	"time"
)

// countKV counts PutTTL calls and the bytes written.
type countKV struct {
	*mapKV
	mu2   sync.Mutex
	puts  int
	bytes int
	byKey map[string]int
}

func newCountKV() *countKV { return &countKV{mapKV: newMapKV(), byKey: map[string]int{}} }

func (k *countKV) PutTTL(key, value []byte, ttl time.Duration) error {
	k.mu2.Lock()
	k.puts++
	k.bytes += len(value)
	k.byKey[string(key)]++
	k.mu2.Unlock()
	return k.mapKV.PutTTL(key, value, ttl)
}

func (k *countKV) reset() {
	k.mu2.Lock()
	k.puts, k.bytes, k.byKey = 0, 0, map[string]int{}
	k.mu2.Unlock()
}

// A-1: ten minutes of idleness on a fake clock write nothing to the KV.
func TestReplayIdleWritesNothing(t *testing.T) {
	kv := newCountKV()
	t0 := time.Unix(90_000, 0)
	c := newReplayCache(kv, t0, nil)
	for s := 1; s <= 600; s++ {
		c.maybeFlush(t0.Add(time.Duration(s) * time.Second))
	}
	if kv.puts != 0 {
		t.Fatalf("%d KV writes (%d bytes) in 10 idle minutes, want 0", kv.puts, kv.bytes)
	}
}

// A-1: under a steady Init stream no more than one generation is written per flush term.
func TestReplayWritesOneGenerationPerTerm(t *testing.T) {
	kv := newCountKV()
	t0 := time.Unix(91_000, 0)
	c := newReplayCache(kv, t0, nil)
	c.seen(selFor("seed"), t0)
	c.maybeFlush(t0.Add(13 * time.Second)) // the first term writes the filled generation
	kv.reset()
	now := t0.Add(13 * time.Second)
	for term := 0; term < 5; term++ {
		before := kv.bytes
		c.seen(selFor("init-"+string(rune('a'+term))), now)
		now = now.Add(13 * time.Second)
		c.maybeFlush(now)
		if got := kv.bytes - before; got > bloomGenBits/8+256 {
			t.Fatalf("term %d wrote %d bytes, more than one generation", term, got)
		}
	}
	if kv.puts == 0 {
		t.Fatal("changed filter was never written")
	}
}

// A-1: the restart restores the filter from the incremental layout.
func TestReplayRestoresAfterIncrementalWrites(t *testing.T) {
	kv := newCountKV()
	t0 := time.Unix(92_000, 0)
	c := newReplayCache(kv, t0, nil)
	c.seen(selFor("a"), t0)
	c.maybeFlush(t0.Add(13 * time.Second))
	later := t0.Add(150 * time.Second) // next generation
	c.seen(selFor("b"), later)
	c.flush(later)
	again := newReplayCache(kv, later, nil)
	if !again.seen(selFor("a"), later) || !again.seen(selFor("b"), later) {
		t.Fatal("selectors lost across a restart")
	}
}
