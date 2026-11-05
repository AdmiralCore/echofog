package mailbox

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// These tests pin AD19: every lifetime (queue, item, binding) lives only in the logical index E/,
// and the counters change only when that index is processed, exactly once.

func readQueueRecord(t *testing.T, kv store.KV, queue []byte) queueRecord {
	t.Helper()
	raw, err := kv.Get(queueKey(queue))
	if err != nil {
		t.Fatalf("queue record: %v", err)
	}
	rec, err := parseQueueRecord(raw)
	if err != nil {
		t.Fatalf("parse queue record: %v", err)
	}
	return rec
}

func readTotals(t *testing.T, s *Server, public bool) counter {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.totalLocked(public)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	return c
}

func indexEntries(t *testing.T, kv store.KV) int {
	t.Helper()
	entries, err := kv.Scan([]byte(expiryIndexPrefix), 10000)
	if err != nil {
		t.Fatalf("scan index: %v", err)
	}
	return len(entries)
}

// (a) A queue shorter than its items must release the items' share of the relay totals when the
// store's own TTL would already have dropped the queue record.
func TestShortQueueTTLReleasesItsItemsOnRealTime(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Now = time.Now
		c.Limits = &Limits{QueueTTL: 2 * time.Second, ItemTTL: 10 * time.Second, TotalItems: 1}
	})
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	if st, _ := f.deposit(t, p, []byte("held"), 0); st != StatusStored {
		t.Fatalf("first deposit: status %d", st)
	}
	time.Sleep(3500 * time.Millisecond)

	other := bytes.Repeat([]byte{0x91}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey()}); err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if st := depositStatus(t, f.srv, p, other, []byte("new")); st != StatusStored {
		t.Fatalf("an expired queue kept its item in the relay totals: status %d", st)
	}
	if c := readTotals(t, f.srv, false); c.Items != 1 || c.Bytes != 3 {
		t.Fatalf("totals after the first queue expired: %+v, want 1 item / 3 bytes", c)
	}
}

// (b) Exact Count/Bytes after expiry, and the index is processed once however often it is walked.
func TestExpiryAccountingIsExactAndOnce(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, false, 0)
	f.deposit(t, p, []byte("aaaa"), 0) // 4 bytes, ends at day 7
	f.clock.Advance(4 * 24 * time.Hour)
	f.deposit(t, p, []byte("bb"), 0)
	f.deposit(t, p, []byte("ccc"), 0)
	f.clock.Advance(4 * 24 * time.Hour) // the first item is past 7 days

	for i := 0; i < 3; i++ { // the index must not be applied twice
		items, _ := f.fetch(t, p, nil, 10, 0)
		if len(items) != 2 {
			t.Fatalf("round %d: %d items, want 2", i, len(items))
		}
		rec := readQueueRecord(t, f.kv, f.queue)
		if rec.Count != 2 || rec.Bytes != 5 {
			t.Fatalf("round %d: queue Count/Bytes = %d/%d, want 2/5", i, rec.Count, rec.Bytes)
		}
		if c := readTotals(t, f.srv, false); c.Items != 2 || c.Bytes != 5 {
			t.Fatalf("round %d: totals %+v, want 2/5", i, c)
		}
	}

	// A whole queue leaving through the index: its items go with it, once.
	other := bytes.Repeat([]byte{0x92}, QueueSize)
	if _, err := f.srv.CreateQueue(p, QueueCreate{Req: 1, Queue: other, AuthKey: f.authKey(), TTL: 1}); err != nil {
		t.Fatalf("short queue: %v", err)
	}
	for _, blob := range []string{"xx", "yyy"} {
		if st := depositStatus(t, f.srv, p, other, []byte(blob)); st != StatusStored {
			t.Fatalf("deposit into the short queue: %d", st)
		}
	}
	if c := readTotals(t, f.srv, false); c.Items != 4 || c.Bytes != 10 {
		t.Fatalf("totals with the short queue: %+v, want 4/10", c)
	}
	f.clock.Advance(25 * time.Hour)
	for i := 0; i < 3; i++ {
		f.fetch(t, p, nil, 10, 0)
		if c := readTotals(t, f.srv, false); c.Items != 2 || c.Bytes != 5 {
			t.Fatalf("round %d: totals after the short queue expired %+v, want 2/5", i, c)
		}
	}
	if _, err := f.kv.Get(queueKey(other)); err == nil {
		t.Fatal("the expired queue record is still stored")
	}
	if n := indexEntries(t, f.kv); n != 3 { // two items and the long queue
		t.Fatalf("index holds %d entries, want 3", n)
	}
}

// (c) A queue record written before the fix round (raw auth key, 95 bytes) is still read, served
// and rewritten in the sealed layout.
func TestLegacyQueueRecordIsReadAndMigrated(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)

	now := f.clock.Now()
	legacy := make([]byte, queueRecordLegacySize)
	legacy[0] = queueRecordVersionLegacy
	copy(legacy[3:35], f.authKey())
	putU64 := func(off int, v uint64) {
		for i := 0; i < 8; i++ {
			legacy[off+i] = byte(v >> (56 - 8*i))
		}
	}
	putU64(35, uint64(now.Unix()))
	putU64(43, uint64(now.Add(24*time.Hour).Unix()))
	legacy[54] = 1 // count = 1
	putU64(55, 3)  // bytes = 3
	copy(legacy[63:95], p.Node[:])
	if err := f.kv.Put(queueKey(f.queue), legacy); err != nil {
		t.Fatal(err)
	}
	id := bytes.Repeat([]byte{0x07}, ItemSize)
	stored := now.UnixNano()
	if err := f.kv.Put(itemKey(f.queue, id), encodeItem(stored, []byte("old"))); err != nil {
		t.Fatal(err)
	}
	if err := f.kv.Put(expiryKey(f.queue, id, f.srv.itemExpiry(stored), false), expiryIndexValue(3)); err != nil {
		t.Fatal(err)
	}

	items, _ := f.fetch(t, p, nil, 10, 0)
	if len(items) != 1 || string(items[0].Blob) != "old" {
		t.Fatalf("FETCH on a v1 record: %+v", items)
	}
	err := f.srv.Ack(p, Ack{Req: 3, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 3)})
	if err != nil {
		t.Fatalf("ACK on a v1 record: %v", err)
	}
	raw, err := f.kv.Get(queueKey(f.queue))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != queueRecordSize || raw[0] != queueRecordVersion {
		t.Fatalf("record not migrated: %d bytes, version %d", len(raw), raw[0])
	}
	if bytes.Contains(raw, f.authKey()) {
		t.Fatal("the migrated record still carries the raw auth key")
	}
	rec := readQueueRecord(t, f.kv, f.queue)
	if rec.Count != 0 || rec.Bytes != 0 {
		t.Fatalf("Count/Bytes after ACK: %d/%d", rec.Count, rec.Bytes)
	}
	if _, _, err := f.srv.Fetch(context.Background(), p, Fetch{Req: 4, Queue: f.queue, Max: 5,
		Auth: signFor(t, f.signer, OpFetch, p.SID, f.queue, 4)}); err != nil {
		t.Fatalf("FETCH on the migrated record: %v", err)
	}
}
