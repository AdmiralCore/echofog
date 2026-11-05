package dht

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

func fixTable(t *testing.T, kv store.KV, now *time.Time, memLimit int, p Pinger) *Table {
	t.Helper()
	tbl, err := Open(Config{
		Self:        testSelf,
		Store:       kv,
		Now:         func() time.Time { return *now },
		Rand:        rand.New(rand.NewSource(11)),
		MemoryLimit: memLimit,
		Pinger:      p,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(tbl.Stop)
	return tbl
}

// TestColdEntryRejectsOlderCard is F1: after an entry leaves memory for the disk, an older card
// replayed by a third party still loses on seq and cannot roll the stored address back.
func TestColdEntryRejectsOlderCard(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	tbl := fixTable(t, kv, &now, 1, nil)

	a10 := buildCard(t, 500, 10, now, objects.ScopePublic, 0, testAddr("10.40.1.1", 443))
	a9 := buildCard(t, 500, 9, now, objects.ScopePublic, 0, testAddr("10.40.9.9", 443))
	a11 := buildCard(t, 500, 11, now, objects.ScopePublic, 0, testAddr("10.40.2.2", 443))
	if !tbl.AddFromSession(a10) {
		t.Fatal("preparing the table failed")
	}
	now = now.Add(time.Minute)
	other := buildCard(t, 501, 1, now, objects.ScopePublic, 0, testAddr("10.40.3.3", 443))
	if !tbl.AddFromSession(other) {
		t.Fatal("preparing the table failed")
	}
	id := DeviceID(a10.DeviceID)
	if tbl.Card(id) != nil || !tbl.Has(id) {
		t.Fatal("the older entry was not evicted to the disk index")
	}

	if tbl.AddFromCard(a9) {
		t.Fatal("an older card of a cold entry was accepted")
	}
	if got := storedSeq(t, kv, id); got != 10 {
		t.Fatalf("stored seq after the replay = %d, want 10", got)
	}

	if !tbl.AddFromCard(a11) {
		t.Fatal("a newer card of a cold entry was rejected")
	}
	if got := storedSeq(t, kv, id); got != 11 {
		t.Fatalf("stored seq after the update = %d, want 11", got)
	}

	// A restart must restore the newer card, not the replayed one.
	restarted := fixTable(t, kv, &now, 0, nil)
	if got := cardAddr(restarted.Card(id)); got != "10.40.2.2" {
		t.Fatalf("restarted address = %q, want the newest 10.40.2.2", got)
	}
}

// TestColdEntriesKeepLimits is F1: endpoint uniqueness, the /24 cap and the /16 cap count cards
// that are on disk, not only the cards in memory.
func TestColdEntriesKeepLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	tbl := fixTable(t, kv, &now, 2, nil)

	// Two entries of one /24 in one bucket are the limit. They are then evicted by newer cards
	// from other buckets.
	a1 := cardInBucket(t, testSelf, 0, 20, now, testAddr("10.41.0.1", 443))
	a2 := cardInBucket(t, testSelf, 0, 21, now, testAddr("10.41.0.2", 443))
	if !tbl.AddFromSession(a1) || !tbl.AddFromSession(a2) {
		t.Fatal("preparing the table failed")
	}
	now = now.Add(time.Minute)
	b1 := cardInBucket(t, testSelf, 1, 22, now, testAddr("10.42.1.1", 443))
	if !tbl.AddFromSession(b1) {
		t.Fatal("preparing the table failed")
	}
	now = now.Add(time.Minute)
	b2 := cardInBucket(t, testSelf, 2, 23, now, testAddr("10.43.1.1", 443))
	if !tbl.AddFromSession(b2) {
		t.Fatal("preparing the table failed")
	}
	if tbl.Card(DeviceID(a1.DeviceID)) != nil || !tbl.Has(DeviceID(a1.DeviceID)) {
		t.Fatal("a1 was not evicted to the disk index")
	}
	if tbl.Card(DeviceID(a2.DeviceID)) != nil {
		t.Fatal("a2 was not evicted to the disk index")
	}

	// A third card of the same /24 in the same bucket must lose to the cold entries.
	third := cardInBucket(t, testSelf, 0, 24, now.Add(3*time.Minute), testAddr("10.41.0.3", 443))
	if tbl.AddFromCard(third) {
		t.Fatal("a third /24 entry was accepted although two cold entries already use it")
	}

	// The endpoint of a cold entry is still taken.
	squatter := cardInBucket(t, testSelf, 1, 25, now.Add(3*time.Minute), testAddr("10.41.0.1", 443))
	if tbl.AddFromCard(squatter) {
		t.Fatal("a cold entry's endpoint was given to another device")
	}

	// The /16 cap counts cold entries too.
	tbl.mu.Lock()
	tbl.maxPer16 = 2
	tbl.mu.Unlock()
	e := buildCard(t, 530, 1, now.Add(4*time.Minute), objects.ScopePublic, 0, testAddr("10.44.1.1", 443))
	f := buildCard(t, 531, 1, now.Add(4*time.Minute), objects.ScopePublic, 0, testAddr("10.44.2.2", 443))
	g := buildCard(t, 532, 1, now.Add(4*time.Minute), objects.ScopePublic, 0, testAddr("10.44.3.3", 443))
	if !tbl.AddFromCard(e) || !tbl.AddFromCard(f) {
		t.Fatal("the first two entries of a /16 were rejected")
	}
	if tbl.AddFromCard(g) {
		t.Fatal("a third entry of the /16 was accepted above the cap")
	}
}

// TestEvictedUnlistedDeletesStore is F1: an unlisted card removes the record even when the visible
// card was already evicted to disk, and the record does not come back after a restart.
func TestEvictedUnlistedDeletesStore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	tbl := fixTable(t, kv, &now, 1, nil)

	visible := buildCard(t, 600, 5, now, objects.ScopePeer, 0, testAddr("10.45.1.1", 443))
	hidden := buildCard(t, 600, 6, now, objects.ScopePeer, objects.FlagUnlisted, testAddr("10.45.3.3", 443))
	if !tbl.AddFromSession(visible) {
		t.Fatal("preparing the table failed")
	}
	now = now.Add(time.Minute)
	other := buildCard(t, 601, 1, now, objects.ScopePublic, 0, testAddr("10.45.2.2", 443))
	if !tbl.AddFromSession(other) {
		t.Fatal("preparing the table failed")
	}
	id := DeviceID(visible.DeviceID)
	if tbl.Card(id) != nil {
		t.Fatal("the visible card was not evicted")
	}

	if tbl.AddFromSession(hidden) {
		t.Fatal("the unlisted card was accepted")
	}
	if tbl.Has(id) {
		t.Fatal("the unlisted device is still known")
	}
	if got := storedSeq(t, kv, id); got != 0 {
		t.Fatalf("the unlisted device still has a stored record (seq %d)", got)
	}
	restarted := fixTable(t, kv, &now, 1, nil)
	if restarted.Has(id) {
		t.Fatal("the unlisted device came back after a restart")
	}
}

// TestStopSavesConfirmedTime is F2: a successful Prune ping updates the confirmation time in
// memory; Stop must write it, or a restart would treat the entry as older than it is.
func TestStopSavesConfirmedTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	p := &fakePinger{}
	tbl, err := Open(Config{
		Self: testSelf, Store: kv, Now: func() time.Time { return now },
		Rand:   rand.New(rand.NewSource(1)),
		Pinger: p,
	})
	if err != nil {
		t.Fatal(err)
	}

	c := buildCard(t, 700, 1, now, objects.ScopePublic, 0, testAddr("10.46.1.1", 443))
	if !tbl.AddFromSession(c) {
		t.Fatal("card was not added")
	}
	id := DeviceID(c.DeviceID)
	if got := storedConfirmed(t, kv, id); !got.Equal(now) {
		t.Fatalf("stored confirmation = %v, want the add time %v", got, now)
	}

	now = now.Add(6 * time.Minute)
	tbl.Prune(context.Background())

	tbl.Stop()
	if got := storedConfirmed(t, kv, id); !got.Equal(now) {
		t.Fatalf("stored confirmation after Stop = %v, want %v", got, now)
	}
}

// TestWritesKeepMutationOrder is F3: a slow first write must not be overtaken by a second update
// of the same device, or the store would end up with the older card.
func TestWritesKeepMutationOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newGatedKV()
	gate := make(chan struct{})
	kv.setGate(gate)

	tbl := fixTable(t, kv, &now, 0, nil)
	a11 := buildCard(t, 710, 11, now, objects.ScopePublic, 0, testAddr("10.47.1.1", 443))
	a12 := buildCard(t, 710, 12, now, objects.ScopePublic, 0, testAddr("10.47.2.2", 443))
	id := DeviceID(a11.DeviceID)

	done1 := make(chan bool, 1)
	go func() { done1 <- tbl.AddFromCard(a11) }()
	select {
	case <-kv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first write did not start")
	}

	done2 := make(chan bool, 1)
	go func() { done2 <- tbl.AddFromCard(a12) }()
	select {
	case <-kv.started:
		t.Fatal("the second write reached the store while the first was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate)
	if !<-done1 || !<-done2 {
		t.Fatal("an update was rejected")
	}
	if got := storedSeq(t, kv.memKV, id); got != 12 {
		t.Fatalf("stored seq = %d, want the newest 12", got)
	}
}

// TestSaveCannotResurrectDeletedEntry is F3: a Save snapshot taken before an unlisted delete must
// not write the entry back after the delete.
func TestSaveCannotResurrectDeletedEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newGatedKV()
	tbl := fixTable(t, kv, &now, 0, nil)

	visible := buildCard(t, 720, 5, now, objects.ScopePeer, 0, testAddr("10.48.1.1", 443))
	hidden := buildCard(t, 720, 6, now, objects.ScopePeer, objects.FlagUnlisted, testAddr("10.48.2.2", 443))
	if !tbl.AddFromSession(visible) {
		t.Fatal("card was not added")
	}
	id := DeviceID(visible.DeviceID)

	kv.drainStarted()
	gate := make(chan struct{})
	kv.setGate(gate)
	doneSave := make(chan error, 1)
	go func() { doneSave <- tbl.Save() }()
	select {
	case <-kv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Save did not reach the store")
	}

	doneDelete := make(chan bool, 1)
	go func() { doneDelete <- tbl.AddFromSession(hidden) }()
	select {
	case <-doneDelete:
		t.Fatal("the delete completed while Save was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate)
	if err := <-doneSave; err != nil {
		t.Fatalf("Save: %v", err)
	}
	if <-doneDelete {
		t.Fatal("the unlisted card was accepted")
	}
	if got := storedSeq(t, kv.memKV, id); got != 0 {
		t.Fatalf("Save resurrected the deleted record (seq %d)", got)
	}
}

// TestColdEntryUpdateStaysOnDisk is F1: when a bucket is full, an update of a cold entry must
// still be written to the store even though the card stays out of memory.
func TestColdEntryUpdateStaysOnDisk(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kv := newMemKV()
	const bucket = 0

	var targetSeed uint64
	var targetID DeviceID
	for i := 0; i < K+1; i++ {
		seed := seedInBucket(t, testSelf, bucket, uint64(1000+i*100000), now)
		c := buildCard(t, seed, 1, now, objects.ScopePublic, 0, testAddr(fmt.Sprintf("10.51.%d.1", i+1), 443))
		confirmed := now.Add(time.Duration(i) * time.Second)
		if i == 0 {
			targetSeed, targetID = seed, DeviceID(c.DeviceID)
		}
		e := newEntry(c, confirmed)
		if err := kv.Put(entryKey(DeviceID(c.DeviceID)), encodeEntry(e.state())); err != nil {
			t.Fatal(err)
		}
	}

	tbl := fixTable(t, kv, &now, 0, nil)
	if tbl.Card(targetID) != nil || !tbl.Has(targetID) {
		t.Fatal("the oldest entry of a full bucket was not kept on disk only")
	}

	newer := buildCard(t, targetSeed, 2, now, objects.ScopePublic, 0, testAddr("10.52.1.1", 443))
	if !tbl.AddFromCard(newer) {
		t.Fatal("a newer card of a cold entry was rejected")
	}
	if got := storedSeq(t, kv, targetID); got != 2 {
		t.Fatalf("stored seq = %d, want 2", got)
	}
	if tbl.Card(targetID) != nil || !tbl.Has(targetID) {
		t.Fatal("the updated card left the disk index")
	}
}

// TestSessionDuringCheckKeepsNode is the other half of the overlap rule: a node that is seen
// again while its liveness check is in flight must not be removed when that (stale) check fails.
func TestSessionDuringCheckKeepsNode(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := newBlockingPinger()
	p.err = errors.New("dead")
	tbl := fixTable(t, nil, &now, 0, p)

	const bucket = 0
	var seeds []uint64
	for i := uint64(0); i < K; i++ {
		seed := seedInBucket(t, testSelf, bucket, i*7919+900, now)
		seeds = append(seeds, seed)
		c := buildCard(t, seed, 1, now, objects.ScopePublic, 0, testAddr(fmt.Sprintf("10.53.%d.1", i+1), 443))
		if !tbl.AddFromSession(c) {
			t.Fatalf("entry %d was rejected", i)
		}
	}

	candidate := cardInBucket(t, testSelf, bucket, 400, now, testAddr("10.54.1.1", 443))
	added := make(chan bool, 1)
	go func() { added <- tbl.AddFromCard(candidate) }()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the replacement check did not start")
	}

	tbl.mu.Lock()
	victim := tbl.buckets[bucket].entries[0]
	victimID := victim.device
	tbl.mu.Unlock()

	var victimSeed uint64
	found := false
	for _, seed := range seeds {
		if DeviceID(buildCard(t, seed, 1, now, objects.ScopePublic, 0).DeviceID) == victimID {
			victimSeed = seed
			found = true
			break
		}
	}
	if !found {
		t.Fatal("could not find the victim seed")
	}
	seenAgain := buildCard(t, victimSeed, 2, now, objects.ScopePublic, 0, testAddr("10.55.1.1", 443))
	if !tbl.AddFromSession(seenAgain) {
		t.Fatal("the session update of the victim was rejected")
	}

	close(p.release)
	if <-added {
		t.Fatal("the candidate was accepted")
	}
	if !tbl.Has(victimID) {
		t.Fatal("a node seen again during the check was removed by the failed ping")
	}
}

// TestPruneUsesBucketCheck is F4: Prune and a replacement check of one bucket cannot overlap, so
// two pings cannot race to remove a live entry.
func TestPruneUsesBucketCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := newBlockingPinger()
	tbl := fixTable(t, nil, &now, 0, p)

	const bucket = 0
	for i := uint64(0); i < K; i++ {
		c := cardInBucket(t, testSelf, bucket, i+30, now, testAddr(fmt.Sprintf("10.49.%d.1", i+1), 443))
		if !tbl.AddFromSession(c) {
			t.Fatalf("entry %d was rejected", i)
		}
	}
	now = now.Add(6 * time.Minute)

	done := make(chan struct{})
	go func() {
		tbl.Prune(context.Background())
		close(done)
	}()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Prune did not start a check")
	}

	// A second Prune must skip the busy bucket instead of starting another check.
	tbl.Prune(context.Background())
	if got := p.count(); got != 1 {
		t.Fatalf("Prune started %d checks of one bucket, want 1", got)
	}

	// A replacement check of the same bucket must also wait.
	candidate := cardInBucket(t, testSelf, bucket, 300, now, testAddr("10.50.1.1", 443))
	if tbl.AddFromCard(candidate) {
		t.Fatal("a full bucket accepted a candidate before the ping")
	}
	if got := p.count(); got != 1 {
		t.Fatalf("replacement started a second check of the bucket (%d)", got)
	}

	close(p.release)
	<-done
	if tbl.Has(DeviceID(candidate.DeviceID)) {
		t.Fatal("the live entry was replaced")
	}
	if got := tbl.BucketLen(bucket); got != K {
		t.Fatalf("bucket holds %d entries, want %d", got, K)
	}
}
