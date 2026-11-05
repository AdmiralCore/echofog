package dht

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

func newTable(t *testing.T, kv *memKV, now *time.Time) *Table {
	t.Helper()
	tbl, err := Open(Config{
		Self: testSelf,
		Store: kv,
		Now:   func() time.Time { return *now },
		Rand:  rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(tbl.Stop)
	return tbl
}

// TestThirdPartyCannotChangeAddress pins dht.md §5: an entry's address changes only from a
// higher-seq card or from the authenticated session of that same device. A third party replaying
// an equal or older card changes nothing.
func TestThirdPartyCannotChangeAddress(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	first := buildCard(t, 1, 10, now, objects.ScopePublic, 0, testAddr("10.0.1.1", 443))
	if !tbl.AddFromCard(first) {
		t.Fatal("the first valid card was not added")
	}
	if !tbl.Has(DeviceID(first.DeviceID)) || cardAddr(tbl.Card(DeviceID(first.DeviceID))) != "10.0.1.1" {
		t.Fatal("entry missing after the first card")
	}

	for _, tc := range []struct {
		name string
		card *objects.Card
	}{
		{"equal seq", buildCard(t, 1, 10, now, objects.ScopePublic, 0, testAddr("10.9.9.9", 443))},
		{"older seq", buildCard(t, 1, 9, now, objects.ScopePublic, 0, testAddr("10.8.8.8", 443))},
	} {
		if tbl.AddFromCard(tc.card) {
			t.Fatalf("%s from a third party changed the entry", tc.name)
		}
		if got := cardAddr(tbl.Card(DeviceID(first.DeviceID))); got != "10.0.1.1" {
			t.Fatalf("%s: address is %q, want the original 10.0.1.1", tc.name, got)
		}
	}

	newer := buildCard(t, 1, 11, now, objects.ScopePublic, 0, testAddr("10.0.2.2", 443))
	if !tbl.AddFromCard(newer) {
		t.Fatal("a higher-seq card did not update the entry")
	}
	if got := cardAddr(tbl.Card(DeviceID(first.DeviceID))); got != "10.0.2.2" {
		t.Fatalf("address after a higher seq = %q, want 10.0.2.2", got)
	}

	// An authenticated session is its own source: the card in the handshake is current even if
	// it carries the same seq as the one held.
	sessionCard := buildCard(t, 1, 11, now, objects.ScopePublic, 0, testAddr("10.0.3.3", 443))
	if !tbl.AddFromSession(sessionCard) {
		t.Fatal("the session card did not update the entry")
	}
	if got := cardAddr(tbl.Card(DeviceID(first.DeviceID))); got != "10.0.3.3" {
		t.Fatalf("address after the session = %q, want 10.0.3.3", got)
	}
}

// TestSameDeviceIsNotDuplicated pins that repeated sessions of one device keep one entry.
func TestSameDeviceIsNotDuplicated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	c := buildCard(t, 2, 1, now, objects.ScopePublic, 0, testAddr("10.1.1.1", 443))
	for i := 0; i < 5; i++ {
		if !tbl.AddFromSession(c) {
			t.Fatalf("session add %d was rejected", i)
		}
	}
	if tbl.Len() != 1 {
		t.Fatalf("table has %d entries, want 1", tbl.Len())
	}
	if n := tbl.BucketLen(bucketIndex(testSelf, DeviceID(c.DeviceID))); n != 1 {
		t.Fatalf("bucket holds %d entries, want 1", n)
	}
}

// TestUnlistedIsNeverAdded pins dht.md §5: flags.unlisted cards are never added, whatever the
// source, and an entry that becomes unlisted is dropped.
func TestUnlistedIsNeverAdded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	unlisted := buildCard(t, 3, 5, now, objects.ScopePeer, objects.FlagUnlisted, testAddr("10.2.2.2", 443))
	if tbl.AddFromCard(unlisted) || tbl.AddFromSession(unlisted) {
		t.Fatal("an unlisted card was added")
	}
	if tbl.Len() != 0 {
		t.Fatalf("table has %d entries after an unlisted card, want 0", tbl.Len())
	}

	visible := buildCard(t, 3, 5, now, objects.ScopePeer, 0, testAddr("10.2.2.2", 443))
	if !tbl.AddFromSession(visible) {
		t.Fatal("a visible card was not added")
	}
	hidden := buildCard(t, 3, 6, now, objects.ScopePeer, objects.FlagUnlisted, testAddr("10.2.2.3", 443))
	if tbl.AddFromSession(hidden) {
		t.Fatal("an unlisted update was accepted")
	}
	if tbl.Has(DeviceID(visible.DeviceID)) {
		t.Fatal("an entry that became unlisted is still in the table")
	}
}

// TestRestartRestoresTableFromStore pins dht.md §5 / AD13: the table survives a restart through
// the store and re-verifies every card instead of trusting the disk.
func TestRestartRestoresTableFromStore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	first := newTable(t, kv, &now)

	a := buildCard(t, 10, 1, now, objects.ScopePublic, 0, testAddr("10.3.1.1", 443))
	b := buildCard(t, 11, 1, now, objects.ScopePublic, 0, testAddr("10.3.2.2", 443))
	if !first.AddFromSession(a) || !first.AddFromCard(b) {
		t.Fatal("cards were not added")
	}
	if err := first.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if n := kv.count([]byte{entryPrefix}); n != 2 {
		t.Fatalf("store holds %d entries, want 2", n)
	}

	second := newTable(t, kv, &now)
	if second.Len() != 2 {
		t.Fatalf("restored table has %d entries, want 2", second.Len())
	}
	for _, c := range []*objects.Card{a, b} {
		id := DeviceID(c.DeviceID)
		if !second.Has(id) || second.Card(id) == nil {
			t.Fatalf("device %x was not restored", id)
		}
		if cardAddr(second.Card(id)) != cardAddr(c) {
			t.Fatalf("device %x restored the wrong card", id)
		}
	}

	// A damaged record is skipped, not trusted.
	if err := kv.Put(append([]byte{entryPrefix}, make([]byte, 32)...), []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	third := newTable(t, kv, &now)
	if third.Len() != 2 {
		t.Fatalf("a damaged record changed the table: %d entries", third.Len())
	}
}

// TestBucketKeepsTwentyAndPingsBeforeReplacing pins k = 20 and the replacement rule: a full
// bucket replaces its least recently confirmed entry only when that entry fails a ping.
func TestBucketKeepsTwentyAndPingsBeforeReplacing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	alive := &fakePinger{done: make(chan struct{}, 1)}
	kv := newMemKV()
	tbl, err := Open(Config{
		Self: testSelf, Store: kv,
		Now:    func() time.Time { return now },
		Rand:   rand.New(rand.NewSource(1)),
		Pinger: alive,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tbl.Stop)

	const bucket = 0
	for i := uint64(0); i < K; i++ {
		c := cardInBucket(t, testSelf, bucket, i+1, now, testAddr("10.4.1.1", uint32(1000+i)))
		if !tbl.AddFromSession(c) {
			t.Fatalf("entry %d was rejected", i)
		}
	}
	if tbl.BucketLen(bucket) != K {
		t.Fatalf("bucket holds %d entries, want %d", tbl.BucketLen(bucket), K)
	}

	candidate := cardInBucket(t, testSelf, bucket, K+1, now, testAddr("10.4.2.2", 2000))
	// The candidate does not fit yet: the entry chosen for the check is still in the bucket.
	if tbl.AddFromCard(candidate) {
		t.Fatal("a full bucket accepted a candidate before the ping")
	}
	select {
	case <-alive.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the table did not ping the least recently confirmed entry")
	}
	// The pinged entry answered: nothing is replaced.
	if tbl.Has(DeviceID(candidate.DeviceID)) {
		t.Fatal("a live entry was replaced")
	}
	if tbl.BucketLen(bucket) != K {
		t.Fatalf("bucket holds %d entries after a live ping, want %d", tbl.BucketLen(bucket), K)
	}

	// Now the pinged entry is dead and the next candidate takes its place.
	dead := &fakePinger{err: errors.New("dead"), done: make(chan struct{}, 1)}
	tbl.SetPinger(dead)
	next := cardInBucket(t, testSelf, bucket, K+2, now, testAddr("10.4.3.3", 3000))
	if tbl.AddFromCard(next) {
		t.Fatal("a full bucket accepted a candidate before the ping")
	}
	waitFor(t, 2*time.Second, func() bool {
		return tbl.Has(DeviceID(next.DeviceID)) && tbl.BucketLen(bucket) == K
	})
}

// TestBucketOrderFollowsConfirmedTime pins the order rule: eviction and bucket order follow the
// last authenticated confirmation, not insertion order.
func TestBucketOrderFollowsConfirmedTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)
	const bucket = 0

	a := cardInBucket(t, testSelf, bucket, 1, now, testAddr("10.5.1.1", 443))
	if !tbl.AddFromSession(a) {
		t.Fatal("a was not added")
	}
	now = now.Add(time.Minute)
	b := cardInBucket(t, testSelf, bucket, 2, now, testAddr("10.5.2.2", 443))
	if !tbl.AddFromSession(b) {
		t.Fatal("b was not added")
	}
	now = now.Add(time.Minute)
	if !tbl.AddFromSession(a) {
		t.Fatal("a was not refreshed")
	}

	tbl.mu.Lock()
	order := append([]*entry(nil), tbl.buckets[bucket].entries...)
	tbl.mu.Unlock()
	if len(order) != 2 || order[0].device != DeviceID(b.DeviceID) {
		t.Fatalf("bucket order is not by confirmation time: %x first", order[0].device)
	}
}

// TestDiversityPerBucket pins "at most 2 entries per diversity unit per bucket".
func TestDiversityPerBucket(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	// A /24 is one diversity unit for IPv4.
	first := cardInBucket(t, testSelf, 0, 1, now, testAddr("10.6.6.1", 443))
	second := cardInBucket(t, testSelf, 0, 2, now, testAddr("10.6.6.2", 443))
	third := cardInBucket(t, testSelf, 0, 3, now, testAddr("10.6.6.3", 443))
	if !tbl.AddFromCard(first) || !tbl.AddFromCard(second) {
		t.Fatal("the first two entries of a /24 were rejected")
	}
	if tbl.AddFromCard(third) {
		t.Fatal("a third entry of the same /24 in one bucket was accepted")
	}

	// Another /24 is a different unit.
	other := cardInBucket(t, testSelf, 0, 4, now, testAddr("10.6.7.1", 443))
	if !tbl.AddFromCard(other) {
		t.Fatal("another /24 was rejected")
	}
}

// TestTableWideSixteenMask pins "at most 10 % of the table per /16". The test lowers the cap so
// the rule is visible without 512 cards.
func TestTableWideSixteenMask(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)
	tbl.mu.Lock()
	tbl.maxPer16 = 2
	tbl.mu.Unlock()

	for i, host := range []string{"10.7.1.1", "10.7.2.2", "10.7.3.3"} {
		var c *objects.Card
		for seed := uint64(i * 1000); ; seed++ {
			c = buildCard(t, seed, 1, now, objects.ScopePublic, 0, testAddr(host, 443))
			// A distinct bucket keeps the per-bucket /24 rule out of the way.
			if bucketIndex(testSelf, DeviceID(c.DeviceID)) == i {
				break
			}
		}
		ok := tbl.AddFromCard(c)
		if i < 2 && !ok {
			t.Fatalf("entry %d of the same /16 was rejected before the cap", i)
		}
		if i == 2 && ok {
			t.Fatal("a third entry of the same /16 was accepted above the cap")
		}
	}
}

// TestOneEntryPerAddress pins "at most one entry per address": two devices cannot claim the same
// endpoint.
func TestOneEntryPerAddress(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	a := buildCard(t, 20, 1, now, objects.ScopePublic, 0, testAddr("10.8.8.8", 443))
	b := buildCard(t, 21, 1, now, objects.ScopePublic, 0, testAddr("10.8.8.8", 443))
	if !tbl.AddFromCard(a) {
		t.Fatal("the first card was rejected")
	}
	if tbl.AddFromCard(b) {
		t.Fatal("a second device with the same endpoint was accepted")
	}
	// The same IP on another port is another endpoint.
	c := buildCard(t, 21, 1, now, objects.ScopePublic, 0, testAddr("10.8.8.8", 444))
	if !tbl.AddFromCard(c) {
		t.Fatal("the same IP on another port was rejected")
	}
}

// TestDeadEntriesAreRemoved pins "dead entries are removed after 5 minutes": the table pings an
// entry that has not been confirmed for 5 minutes and drops it on failure.
func TestDeadEntriesAreRemoved(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	alive := &fakePinger{}
	tbl, err := Open(Config{
		Self: testSelf, Now: func() time.Time { return now },
		Rand: rand.New(rand.NewSource(1)), Pinger: alive,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tbl.Stop)

	c := buildCard(t, 30, 1, now, objects.ScopePublic, 0, testAddr("10.9.1.1", 443))
	if !tbl.AddFromSession(c) {
		t.Fatal("card was not added")
	}
	now = now.Add(DeadAfter)
	alive.setErr(errors.New("dead"))
	tbl.Prune(context.Background())
	if tbl.Has(DeviceID(c.DeviceID)) {
		t.Fatal("a dead entry stayed after 5 minutes")
	}

	// A live entry is confirmed again and stays.
	live := buildCard(t, 31, 1, now, objects.ScopePublic, 0, testAddr("10.9.2.2", 443))
	if !tbl.AddFromSession(live) {
		t.Fatal("live card was not added")
	}
	now = now.Add(DeadAfter)
	alive.setErr(nil)
	tbl.Prune(context.Background())
	if !tbl.Has(DeviceID(live.DeviceID)) {
		t.Fatal("a live entry was removed")
	}
}

// TestMemoryCapEvictsLRU pins the mem_cap_mb rule: the in-memory table keeps at most 32 buckets'
// worth of cards and evicts the least recently confirmed one; the store keeps the rest.
func TestMemoryCapEvictsLRU(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	tbl := newTable(t, kv, &now)
	tbl.mu.Lock()
	tbl.memLimit = 3
	tbl.mu.Unlock()

	var cards []*objects.Card
	for i := 0; i < 5; i++ {
		c := buildCard(t, uint64(40+i), 1, now, objects.ScopePublic, 0, testAddr("10.10.10.10", uint32(1000+i)))
		cards = append(cards, c)
		if !tbl.AddFromSession(c) {
			t.Fatalf("card %d was rejected", i)
		}
		now = now.Add(time.Minute)
	}
	if tbl.Len() != 3 {
		t.Fatalf("in-memory table has %d entries, want 3", tbl.Len())
	}
	if tbl.Has(DeviceID(cards[0].DeviceID)) {
		t.Fatal("the least recently confirmed entry was not evicted")
	}
	if n := kv.count([]byte{entryPrefix}); n != 5 {
		t.Fatalf("store holds %d entries, want 5 (evicted entries stay on disk)", n)
	}
}

// TestClosestOrdersByXOR pins the keyspace order used by lookups.
func TestClosestOrdersByXOR(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := newTable(t, nil, &now)

	var cards []*objects.Card
	for i := uint64(0); i < 6; i++ {
		c := buildCard(t, 60+i, 1, now, objects.ScopePublic, 0, testAddr("10.11.1.1", uint32(1000+i)))
		cards = append(cards, c)
		if !tbl.AddFromSession(c) {
			t.Fatalf("card %d was rejected", i)
		}
	}

	target := DeviceID{0xaa}
	got := tbl.Closest([32]byte(target), 3)
	if len(got) != 3 {
		t.Fatalf("Closest returned %d cards, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if distance(DeviceID(got[i-1].DeviceID), target) > distance(DeviceID(got[i].DeviceID), target) {
			t.Fatal("Closest is not sorted by XOR distance")
		}
	}
}

// TestIntervals pins the refresh and save windows of dht.md §5 and platforms.md §2.
func TestIntervals(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		if d := refreshAfter(false, rnd); d < 10*time.Minute || d > 15*time.Minute {
			t.Fatalf("default refresh %v is outside uniform(10,15) minutes", d)
		}
		if d := refreshAfter(true, rnd); d < 45*time.Minute || d > 75*time.Minute {
			t.Fatalf("low-power refresh %v is outside uniform(45,75) minutes", d)
		}
		if d := saveAfter(rnd); d < 4*time.Minute || d > 6*time.Minute {
			t.Fatalf("save interval %v is outside 5 minutes ±20%%", d)
		}
	}
}

// TestStartupSelfLookupAndBucketRefresh pins "a self-lookup runs at start-up" and "each bucket not
// touched for uniform(10,15) minutes is refreshed with a lookup of a random ID in it".
func TestStartupSelfLookupAndBucketRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	var lookups [][32]byte
	tbl, err := Open(Config{
		Self: testSelf,
		Now:  func() time.Time { return now },
		Rand: rand.New(rand.NewSource(3)),
		Lookup: func(_ context.Context, target [32]byte) error {
			mu.Lock()
			lookups = append(lookups, target)
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tbl.Start(ctx)
	defer func() { cancel(); tbl.Stop() }()

	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(lookups) == 1 && lookups[0] == [32]byte(testSelf)
	})

	c := cardInBucket(t, testSelf, 0, 1, now, testAddr("10.12.1.1", 443))
	if !tbl.AddFromSession(c) {
		t.Fatal("card was not added")
	}
	now = now.Add(20 * time.Minute)
	tbl.Refresh(ctx)
	mu.Lock()
	got := append([][32]byte(nil), lookups...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("bucket lookup did not run: %d lookups", len(got))
	}
	if idx := bucketIndex(testSelf, DeviceID(got[1])); idx != 0 {
		t.Fatalf("refresh looked up bucket %d, want 0", idx)
	}
}
