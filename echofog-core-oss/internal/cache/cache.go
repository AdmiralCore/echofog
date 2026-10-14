package cache

import (
	"container/list"
	"sync"
	"time"
)

// entry wraps the key-value pair and expiration metadata
type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time
}

// Cache is a thread-safe, high-performance generic LRU + sliding TTL cache.
// Designed to replace raw unbounded maps across P2P subsystems (DHT, Swarm, Storage),
// completely preventing memory leaks and stale state accumulation.
type Cache[K comparable, V any] struct {
	mu         sync.RWMutex
	capacity   int           // <= 0 means unlimited capacity
	ttl        time.Duration // <= 0 means items never expire by TTL
	slidingTTL bool          // if true, reads refresh the expiration deadline
	items      map[K]*list.Element
	evict      *list.List
}

// New creates a new generic Cache bounded by capacity and TTL.
// - capacity: max items retained. When full, least-recently-used item is evicted. Set 0 for no limit.
// - ttl: time before an item is considered stale. Set 0 for no expiration.
func New[K comparable, V any](capacity int, ttl time.Duration) *Cache[K, V] {
	return &Cache[K, V]{
		capacity:   capacity,
		ttl:        ttl,
		slidingTTL: true,
		items:      make(map[K]*list.Element),
		evict:      list.New(),
	}
}

// NewLRU creates a cache with strict capacity limit and no TTL expiration
func NewLRU[K comparable, V any](capacity int) *Cache[K, V] {
	return New[K, V](capacity, 0)
}

// NewTTL creates a cache with TTL expiration and unlimited capacity
func NewTTL[K comparable, V any](ttl time.Duration) *Cache[K, V] {
	return New[K, V](0, ttl)
}

// SetSlidingTTL toggles whether accessing an item refreshes its TTL lifetime
func (c *Cache[K, V]) SetSlidingTTL(sliding bool) *Cache[K, V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slidingTTL = sliding
	return c
}

// Get retrieves an item by key.
// Returns (value, true) if found and not expired.
// If expired, the item is evicted and (zero, false) is returned.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.items[key]
	if !exists {
		var zero V
		return zero, false
	}

	ent := elem.Value.(*entry[K, V])

	// Check TTL expiration
	if c.ttl > 0 && time.Now().After(ent.expiresAt) {
		c.evictElement(elem)
		var zero V
		return zero, false
	}

	// Move to front (Most Recently Used)
	c.evict.MoveToFront(elem)

	// Refresh sliding expiration if enabled
	if c.ttl > 0 && c.slidingTTL {
		ent.expiresAt = time.Now().Add(c.ttl)
	}

	return ent.value, true
}

// Set stores a key-value pair in the cache with sliding TTL.
// If cache exceeds capacity, least-recently-used items are evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	var expiresAt time.Time
	if c.ttl > 0 {
		expiresAt = now.Add(c.ttl)
	}

	// Existing item update
	if elem, exists := c.items[key]; exists {
		c.evict.MoveToFront(elem)
		ent := elem.Value.(*entry[K, V])
		ent.value = value
		ent.expiresAt = expiresAt
		return
	}

	// Capacity enforcement
	if c.capacity > 0 && c.evict.Len() >= c.capacity {
		c.evictOldest()
	}

	// Insert new item at head
	newEntry := &entry[K, V]{
		key:       key,
		value:     value,
		expiresAt: expiresAt,
	}
	elem := c.evict.PushFront(newEntry)
	c.items[key] = elem
}

// Delete removes a key from cache. Returns true if key was present.
func (c *Cache[K, V]) Delete(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.items[key]; exists {
		c.evictElement(elem)
		return true
	}
	return false
}

// Exists checks if key exists and is unexpired without altering LRU order
func (c *Cache[K, V]) Exists(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	elem, exists := c.items[key]
	if !exists {
		return false
	}
	ent := elem.Value.(*entry[K, V])
	if c.ttl > 0 && time.Now().After(ent.expiresAt) {
		return false
	}
	return true
}

// Len returns the current number of cached items
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Clear flushes all entries from the cache
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[K]*list.Element)
	c.evict.Init()
}

// PruneExpired removes all expired items and returns count of removed items.
// Designed to be scheduled periodically by the node TaskScheduler.
func (c *Cache[K, V]) PruneExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl <= 0 {
		return 0
	}

	now := time.Now()
	pruned := 0

	// Walk from back (oldest) to front
	curr := c.evict.Back()
	for curr != nil {
		prev := curr.Prev()
		ent := curr.Value.(*entry[K, V])
		if now.After(ent.expiresAt) {
			c.evictElement(curr)
			pruned++
		}
		curr = prev
	}
	return pruned
}

// Keys returns a snapshot slice of all active unexpired keys
func (c *Cache[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]K, 0, len(c.items))
	now := time.Now()

	for k, elem := range c.items {
		ent := elem.Value.(*entry[K, V])
		if c.ttl <= 0 || !now.After(ent.expiresAt) {
			keys = append(keys, k)
		}
	}
	return keys
}

func (c *Cache[K, V]) evictOldest() {
	elem := c.evict.Back()
	if elem != nil {
		c.evictElement(elem)
	}
}

func (c *Cache[K, V]) evictElement(elem *list.Element) {
	c.evict.Remove(elem)
	ent := elem.Value.(*entry[K, V])
	delete(c.items, ent.key)
}
