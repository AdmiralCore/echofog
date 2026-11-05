package mailbox

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// AD19 (third return): a deduction from the counters happens only together with deleting an index
// entry that the same pass read and found. These tests fail on a double deduction.

// A queue and its items are both past their lifetime in one pass, and a live neighbour sits
// between them in the index; the neighbour's counters and the relay totals must stay exact.
func TestQueueAndItsItemsExpireInOnePassKeepsNeighbourExact(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0) // long queue "B"
	short := bytes.Repeat([]byte{0x93}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: short, AuthKey: f.authKey(), TTL: 1}); err != nil {
		t.Fatalf("short queue: %v", err)
	}
	for _, blob := range []string{"xx", "yyy"} {
		if st := depositStatus(t, f.srv, p, short, []byte(blob)); st != StatusStored {
			t.Fatalf("deposit: %d", st)
		}
	}
	f.clock.Advance(12 * time.Hour)
	if st, _ := f.deposit(t, p, []byte("bbbb"), 0); st != StatusStored {
		t.Fatalf("neighbour deposit: %d", st)
	}
	f.clock.Advance(6*24*time.Hour + 18*time.Hour) // short queue and its items are past 7 days; B's item lives
	f.fetch(t, p, nil, 10, 0)

	if c := readTotals(t, f.srv, false); c.Items != 1 || c.Bytes != 4 {
		t.Fatalf("totals %+v, want 1 item / 4 bytes", c)
	}
	if rec := readQueueRecord(t, f.kv, f.queue); rec.Count != 1 || rec.Bytes != 4 {
		t.Fatalf("neighbour Count/Bytes = %d/%d, want 1/4", rec.Count, rec.Bytes)
	}
	if n := indexEntries(t, f.kv); n != 2 {
		t.Fatalf("index holds %d entries, want 2", n)
	}
}

type oracleQueue struct {
	id     []byte
	public bool
	ttl    uint32
	ids    [][]byte
}

// recount checks the counters against a recount from the live records.
func recount(t *testing.T, f *fixture, qs []*oracleQueue, step int) {
	t.Helper()
	var items, size [2]int64
	for _, q := range qs {
		raw, err := f.kv.Get(queueKey(q.id))
		if err != nil {
			if n := countItems(t, f, q.id); n != 0 {
				t.Fatalf("step %d: queue %x gone but %d items remain", step, q.id[:2], n)
			}
			continue
		}
		rec, err := parseQueueRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := f.kv.Scan(itemPrefix(q.id), 500)
		if err != nil {
			t.Fatal(err)
		}
		var n, b int64
		for _, e := range entries {
			n++
			b += itemBlobLen(e.Value)
		}
		if int64(rec.Count) != n || rec.Bytes != b {
			t.Fatalf("step %d: queue %x Count/Bytes = %d/%d, recount %d/%d", step, q.id[:2], rec.Count, rec.Bytes, n, b)
		}
		i := 0
		if rec.Public {
			i = 1
		}
		items[i] += n
		size[i] += b
	}
	for i, pub := range []bool{false, true} {
		c := readTotals(t, f.srv, pub)
		if int64(c.Items) != items[i] || int64(c.Bytes) != size[i] {
			t.Fatalf("step %d: totals(public=%v) %d/%d, recount %d/%d", step, pub, c.Items, c.Bytes, items[i], size[i])
		}
	}
}

func countItems(t *testing.T, f *fixture, q []byte) int {
	t.Helper()
	entries, err := f.kv.Scan(itemPrefix(q), 500)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestCountersMatchRecountOracle(t *testing.T) {
	seeds := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			f := newFixture(t, nil)
			p := f.peer(1)
			var qs []*oracleQueue
			for i := 0; i < 5; i++ {
				qs = append(qs, &oracleQueue{
					id:     bytes.Repeat([]byte{byte(0xa0 + i)}, QueueSize),
					public: i%2 == 1,
					ttl:    []uint32{1, 2, 0, 3, 0}[i],
				})
			}
			req := uint64(10)
			for step := 0; step < 1200; step++ {
				q := qs[rng.Intn(len(qs))]
				req++
				switch rng.Intn(6) {
				case 0:
					_, _ = f.srv.CreateQueue(p, QueueCreate{Req: req, Queue: q.id, AuthKey: f.authKey(), TTL: q.ttl, Public: q.public})
				case 1, 2:
					blob := bytes.Repeat([]byte{'a'}, 1+rng.Intn(6))
					st, id, err := f.srv.Deposit(p, Deposit{Req: req, Queue: q.id, Blob: blob})
					if err == nil && st == StatusStored {
						q.ids = append(q.ids, id)
					}
				case 3:
					_, _, _ = f.srv.Fetch(t.Context(), p, Fetch{Req: req, Queue: q.id, Max: 10,
						Auth: signFor(t, f.signer, OpFetch, p.SID, q.id, req)})
				case 4:
					if len(q.ids) > 0 {
						k := rng.Intn(len(q.ids))
						_ = f.srv.Ack(p, Ack{Req: req, Queue: q.id, IDs: [][]byte{q.ids[k]},
							Auth: signFor(t, f.signer, OpAck, p.SID, q.id, req)})
						q.ids = append(q.ids[:k], q.ids[k+1:]...)
					}
				case 5:
					f.clock.Advance(time.Duration(rng.Intn(10*24*60)) * time.Minute)
				}
				f.srv.mu.Lock()
				err := f.srv.pruneExpiredLocked(f.clock.Now())
				f.srv.mu.Unlock()
				if err != nil {
					t.Fatalf("step %d: prune: %v", step, err)
				}
				recount(t, f, qs, step)
			}
		})
	}
}

// Subtracting more than is held is clamped to zero, but it is counted: it means an item was
// deducted without being found.
func TestUnderflowIsCounted(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.mu.Lock()
	err := f.srv.addTotalLocked(false, -3, -9)
	f.srv.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if n := f.srv.UnderflowErrors(); n != 1 {
		t.Fatalf("UnderflowErrors = %d, want 1", n)
	}
	f.srv.underflows.Store(0) // provoked on purpose
}
