package dht

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// checkInvariantsErr is the T5.1a/T5.1b inheritance (orders №65): after any step
//   - every card handed to persist is in the table or in the store, and nothing in the store is
//     unknown to the table;
//   - no entry is left in a half state (index, buckets, endpoints, cold metadata agree; no check
//     is marked in flight when none runs);
//   - the seq and the diversity metadata of an entry kept only on disk are those of its stored
//     card, so eviction from memory cannot loosen a limit.
func (t *Table) checkInvariantsErr(kv *memKV, inFlight bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	sum := 0
	for i, b := range t.buckets {
		if len(b.entries) > K {
			return fmt.Errorf("bucket %d holds %d entries", i, len(b.entries))
		}
		if b.checking && !inFlight {
			return fmt.Errorf("bucket %d is marked as checking but no check runs", i)
		}
		for _, e := range b.entries {
			sum++
			if t.index[e.device] != e {
				return fmt.Errorf("bucket %d entry %x is not in the index", i, e.device[:4])
			}
			if t.bucketOf(e.device) != i {
				return fmt.Errorf("entry %x sits in bucket %d, belongs to %d", e.device[:4], i, t.bucketOf(e.device))
			}
			if e.card == nil || DeviceID(e.card.DeviceID) != e.device || e.card.Env.Seq != e.seq {
				return fmt.Errorf("hot entry %x has a card that disagrees with it", e.device[:4])
			}
		}
	}
	if sum != t.total || sum != len(t.index) {
		return fmt.Errorf("total %d, buckets %d, index %d", t.total, sum, len(t.index))
	}
	if t.memLimit > 0 && t.total > t.memLimit {
		return fmt.Errorf("%d cards in memory, limit %d", t.total, t.memLimit)
	}
	known := map[DeviceID]*entry{}
	for id, e := range t.index {
		known[id] = e
	}
	for id, e := range t.cold {
		if known[id] != nil {
			return fmt.Errorf("%x is both hot and cold", id[:4])
		}
		if e.card != nil {
			return fmt.Errorf("cold entry %x keeps a card in memory", id[:4])
		}
		known[id] = e
	}
	if _, ok := known[t.self]; ok {
		return errors.New("the table contains the local device")
	}
	nep := 0
	for _, e := range known {
		for _, ep := range e.endpoints {
			nep++
			if t.endpoints[ep] != e.device {
				owner := t.endpoints[ep]
				return fmt.Errorf("endpoint %s of %x is owned by %x", ep, e.device[:4], owner[:4])
			}
		}
	}
	if nep != len(t.endpoints) {
		return fmt.Errorf("%d endpoints in the map, entries say %d", len(t.endpoints), nep)
	}
	perUnit := map[string]int{}
	for _, e := range known {
		for _, u := range e.units {
			k := fmt.Sprintf("%d/%s", t.bucketOf(e.device), u)
			perUnit[k]++
			if perUnit[k] > maxPerUnitPerBucket {
				return fmt.Errorf("unit %s holds %d entries of one bucket", k, perUnit[k])
			}
		}
	}

	if kv == nil {
		return nil
	}
	stored := map[DeviceID]bool{}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for key, value := range kv.m {
		if len(key) != 1+32 || key[0] != entryPrefix {
			continue
		}
		var id DeviceID
		copy(id[:], key[1:])
		raw, _, ok := decodeEntry(value)
		if !ok {
			return fmt.Errorf("undecodable store record %x", id[:4])
		}
		card, err := objects.VerifyCard(raw, t.now())
		if err != nil {
			return fmt.Errorf("store record %x: %v", id[:4], err)
		}
		stored[id] = true
		e := known[id]
		if e == nil {
			return fmt.Errorf("store holds %x, which the table does not know", id[:4])
		}
		if card.Env.Seq != e.seq {
			return fmt.Errorf("store holds seq %d of %x, the entry has %d", card.Env.Seq, id[:4], e.seq)
		}
		fresh := newEntry(card, e.confirmed)
		if fmt.Sprint(fresh.endpoints) != fmt.Sprint(e.endpoints) || fmt.Sprint(fresh.units) != fmt.Sprint(e.units) ||
			fmt.Sprint(fresh.wide) != fmt.Sprint(e.wide) {
			return fmt.Errorf("metadata of %x differs from its stored card", id[:4])
		}
	}
	for id := range known {
		if !stored[id] {
			return fmt.Errorf("entry %x has no store record", id[:4])
		}
	}
	return nil
}

var _ = strings.TrimSpace

// switchPinger answers from a set of dead devices.
type switchPinger struct {
	mu   sync.Mutex
	dead map[DeviceID]bool
}

func (p *switchPinger) Ping(_ context.Context, c *objects.Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead[DeviceID(c.DeviceID)] {
		return errors.New("dead")
	}
	return nil
}

var (
	invOnce sync.Once
	invPool [][]*objects.Card // [device][seq-1]
	invSeed atomic.Int64
)

const invBuckets = 4

func invCards(t *testing.T, now time.Time) [][]*objects.Card {
	invOnce.Do(func() {
		for b := 0; b < invBuckets; b++ {
			for i := 0; i < 24; i++ {
				seed := seedInBucket(t, testSelf, b, uint64(b*100+i+1)<<16, now)
				host := fmt.Sprintf("10.%d.%d.%d", 100+b, i/3, i%3+1) // three devices per /24: the third is refused
				var versions []*objects.Card
				for seq := uint64(1); seq <= 3; seq++ {
					versions = append(versions, buildCard(t, seed, seq, now, objects.ScopePublic, 0, testAddr(host, 4430)))
				}
				invPool = append(invPool, versions)
			}
		}
	})
	return invPool
}

// TestTableInvariantsRandomSteps runs random insertions, session updates, evictions to disk,
// failing pings, pruning, time jumps and restarts, and checks the invariants after every step.
func TestTableInvariantsRandomSteps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pool := invCards(t, now)
	seed := invSeed.Add(1)
	rnd := rand.New(rand.NewSource(seed))
	steps := 1000
	if testing.Short() {
		steps = 200
	}

	kv := newMemKV()
	clock := now
	pinger := &switchPinger{dead: map[DeviceID]bool{}}
	open := func() *Table {
		tbl, err := Open(Config{
			Self: testSelf, Store: kv, Now: func() time.Time { return clock },
			Rand: rand.New(rand.NewSource(seed)), Pinger: pinger, MemoryLimit: 14,
		})
		if err != nil {
			t.Fatalf("seed %d: Open: %v", seed, err)
		}
		return tbl
	}
	tbl := open()
	t.Cleanup(func() { tbl.Stop() })

	snapshot := func() map[DeviceID]uint64 {
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		out := map[DeviceID]uint64{}
		for id, e := range tbl.index {
			out[id] = e.seq
		}
		for id, e := range tbl.cold {
			out[id] = e.seq
		}
		return out
	}

	for step := 0; step < steps; step++ {
		before := snapshot()
		op := rnd.Intn(10)
		sessionStep := false
		dev := pool[rnd.Intn(len(pool))]
		c := dev[rnd.Intn(3)]
		switch {
		case op <= 3:
			tbl.AddFromCard(c)
		case op <= 5:
			tbl.AddFromSession(c)
			sessionStep = true
		case op == 6:
			for i := 0; i < 8; i++ {
				pinger.mu.Lock()
				pinger.dead[DeviceID(pool[rnd.Intn(len(pool))][0].DeviceID)] = rnd.Intn(2) == 0
				pinger.mu.Unlock()
			}
		case op == 7:
			clock = clock.Add(time.Duration(rnd.Intn(90)) * time.Minute)
			tbl.Prune(context.Background())
		case op == 8:
			if err := tbl.Save(); err != nil {
				t.Fatal(err)
			}
		case op == 9:
			tbl.Stop()
			tbl = open()
			after := snapshot()
			// Eviction to disk is not forgetting: a restart knows what it knew.
			if fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("seed %d step %d: restart changed the known set (%d -> %d entries)", seed, step, len(before), len(after))
			}
		}
		if err := tbl.checkInvariantsErr(kv, false); err != nil {
			t.Fatalf("seed %d step %d (op %d): %v", seed, step, op, err)
		}
		if op <= 3 || op == 8 {
			// A card from the DHT or PEX never lowers a seq.
			after := snapshot()
			for id, s := range before {
				if a, ok := after[id]; ok && a < s && !sessionStep {
					t.Fatalf("seed %d step %d: seq of %x fell from %d to %d", seed, step, id[:4], s, a)
				}
			}
		}
	}
}

// TestAsyncCheckDoesNotBlockTheCaller pins the T5.1b-dht200 inheritance: with AsyncChecks the
// liveness check of a full bucket runs in the background, so the caller (the path that accepts a
// session) returns at once; the candidate still replaces a dead entry afterwards, and Stop ends a
// check that is still in flight.
func TestAsyncCheckDoesNotBlockTheCaller(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	bp := newBlockingPinger()
	tbl, err := Open(Config{
		Self: testSelf, Store: newMemKV(), Now: func() time.Time { return now },
		Rand: rand.New(rand.NewSource(1)), Pinger: bp, AsyncChecks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tbl.Stop)
	const bucket = 0
	for i := uint64(0); i < K; i++ {
		if !tbl.AddFromSession(cardInBucket(t, testSelf, bucket, i+1, now, testAddr(fmt.Sprintf("10.4.%d.1", i+1), 443))) {
			t.Fatalf("entry %d was rejected", i)
		}
	}
	cand := cardInBucket(t, testSelf, bucket, K+1, now, testAddr("10.5.1.1", 2000))
	done := make(chan bool, 1)
	go func() { done <- tbl.AddFromSession(cand) }()
	select {
	case accepted := <-done:
		if accepted {
			t.Fatal("a full bucket accepted the candidate before the check")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AddFromSession blocked on the liveness check")
	}
	select {
	case <-bp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the check never started")
	}
	// A second candidate of the same bucket while the check runs starts no second one.
	other := cardInBucket(t, testSelf, bucket, K+2, now, testAddr("10.6.1.1", 2001))
	tbl.AddFromSession(other)
	if bp.count() != 1 {
		t.Fatalf("%d checks in flight for one bucket", bp.count())
	}
	bp.err = errors.New("dead")
	close(bp.release)
	waitFor(t, 2*time.Second, func() bool { return tbl.Has(DeviceID(cand.DeviceID)) })
	if err := tbl.checkInvariantsErr(nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestStopEndsAnInFlightCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	bp := newBlockingPinger()
	tbl, err := Open(Config{
		Self: testSelf, Now: func() time.Time { return now },
		Rand: rand.New(rand.NewSource(1)), Pinger: bp, AsyncChecks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < K; i++ {
		tbl.AddFromSession(cardInBucket(t, testSelf, 0, i+1, now, testAddr(fmt.Sprintf("10.4.%d.1", i+1), 443)))
	}
	go tbl.AddFromSession(cardInBucket(t, testSelf, 0, K+1, now, testAddr("10.5.1.1", 2000)))
	select {
	case <-bp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the check never started")
	}
	stopped := make(chan struct{})
	go func() { tbl.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop waited for a check that nobody will answer")
	}
}
