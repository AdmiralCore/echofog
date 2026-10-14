package cache

import (
	"sync"
	"testing"
	"time"
)

func TestCache_BasicOperations(t *testing.T) {
	c := New[string, int](10, time.Minute)

	c.Set("alice", 100)
	c.Set("bob", 200)

	val, ok := c.Get("alice")
	if !ok || val != 100 {
		t.Fatalf("expected alice=100, got %v, %v", val, ok)
	}

	if c.Len() != 2 {
		t.Fatalf("expected len 2, got %d", c.Len())
	}

	if !c.Delete("alice") {
		t.Fatalf("expected delete alice to return true")
	}

	_, ok = c.Get("alice")
	if ok {
		t.Fatalf("expected alice to be deleted")
	}
}

func TestCache_LRUEviction(t *testing.T) {
	// Capacity 3, no TTL
	c := NewLRU[string, string](3)

	c.Set("k1", "v1")
	c.Set("k2", "v2")
	c.Set("k3", "v3")

	// Access k1 to make it most recently used
	c.Get("k1")

	// Insert k4, should evict k2 (least recently used)
	c.Set("k4", "v4")

	if c.Exists("k2") {
		t.Fatalf("expected k2 to be evicted by LRU")
	}
	if !c.Exists("k1") || !c.Exists("k3") || !c.Exists("k4") {
		t.Fatalf("expected k1, k3, k4 to remain in cache")
	}
}

func TestCache_TTLExpiration(t *testing.T) {
	ttl := 50 * time.Millisecond
	c := NewTTL[string, string](ttl)
	c.SetSlidingTTL(false)

	c.Set("temp", "val")

	val, ok := c.Get("temp")
	if !ok || val != "val" {
		t.Fatalf("expected temp to be valid immediately")
	}

	time.Sleep(70 * time.Millisecond)

	_, ok = c.Get("temp")
	if ok {
		t.Fatalf("expected temp to be expired by TTL")
	}
}

func TestCache_PruneExpired(t *testing.T) {
	ttl := 40 * time.Millisecond
	c := New[int, string](100, ttl)
	c.SetSlidingTTL(false)

	c.Set(1, "one")
	c.Set(2, "two")
	c.Set(3, "three")

	time.Sleep(60 * time.Millisecond)

	pruned := c.PruneExpired()
	if pruned != 3 {
		t.Fatalf("expected 3 pruned, got %d", pruned)
	}
	if c.Len() != 0 {
		t.Fatalf("expected cache to be empty after prune, got len=%d", c.Len())
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	c := New[int, int](100, 500*time.Millisecond)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := j % 20
				c.Set(key, workerID*1000+j)
				c.Get(key)
				if j%5 == 0 {
					c.Delete(key)
				}
			}
		}(i)
	}

	wg.Wait()
}
