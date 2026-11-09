package mailbox

import (
	"fmt"
	"testing"
	"time"
)

// dedupeKeyCount is how many direct-DEPOSIT memories (p/) the relay keeps.
func dedupeKeyCount(t *testing.T, f *fixture) int {
	t.Helper()
	entries, err := f.kv.ScanFrom([]byte("p/"), nil, 1000)
	if err != nil {
		t.Fatalf("scan p/: %v", err)
	}
	return len(entries)
}

// depositAndAck stores one public blob and acknowledges it, so the queue stays empty and only the
// dedupe memory accumulates.
func depositAndAck(t *testing.T, f *fixture, p Peer, blob []byte) {
	t.Helper()
	st, id, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: goodPow(t, f.queue, blob, 1)})
	if err != nil || st != StatusStored {
		t.Fatalf("deposit %q: status %d err %v", blob, st, err)
	}
	if err := f.srv.Ack(p, Ack{
		Req: 2, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 2),
	}); err != nil {
		t.Fatalf("ack %q: %v", blob, err)
	}
}

// The dedupe memory of a direct DEPOSIT is not part of the queue counters and survives ACK, so the
// relay must bound it on its own: without a cap, a sender that acknowledges and refills leaks one
// key per deposit for the item's lifetime (services.md 5.2, review PA-FIX-2a finding 3).
func TestDirectDepositMemoryHasAHardCap(t *testing.T) {
	const cap = 5
	f := newFixture(t, func(c *Config) { c.Limits = &Limits{PublicItems: 100, DedupeItems: cap} })
	p := f.peer(1)
	f.create(t, p, 0, true, 1)

	for i := 0; i < 20; i++ {
		depositAndAck(t, f, p, []byte(fmt.Sprintf("blob-%d", i)))
		if n := dedupeKeyCount(t, f); n > cap {
			t.Fatalf("%d dedupe keys after %d deposits, cap %d", n, i+1, cap)
		}
	}

	// The memory lives for the item's logical lifetime: past it, the entries are reclaimed instead
	// of growing with the next deposit.
	f.clock.Advance(8 * 24 * time.Hour)
	depositAndAck(t, f, p, []byte("after the lifetime"))
	if n := dedupeKeyCount(t, f); n > cap {
		t.Fatalf("%d dedupe keys after the item lifetime, cap %d", n, cap)
	}
}

// A repeat of an acknowledged deposit is still idempotent: the memory must outlive the item, so the
// sender that did not see the ACK does not create a second entry (decision PA-FIX-2a 1, services.md
// 5.2).
func TestDirectDepositMemorySurvivesAck(t *testing.T) {
	f := newFixture(t, nil)
	p := f.peer(1)
	f.create(t, p, 0, true, 1)

	blob := []byte("same work")
	pow := goodPow(t, f.queue, blob, 1)
	st, id, err := f.srv.Deposit(p, Deposit{Req: 1, Queue: f.queue, Blob: blob, Pow: pow})
	if err != nil || st != StatusStored {
		t.Fatalf("first deposit: status %d err %v", st, err)
	}
	if err := f.srv.Ack(p, Ack{
		Req: 2, Queue: f.queue, IDs: [][]byte{id},
		Auth: signFor(t, f.signer, OpAck, p.SID, f.queue, 2),
	}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	if st, _, err := f.srv.Deposit(p, Deposit{Req: 3, Queue: f.queue, Blob: blob, Pow: pow}); err != nil || st != StatusStored {
		t.Fatalf("repeat after ack: status %d err %v, want stored", st, err)
	}
	items, _ := f.fetch(t, p, nil, 10, 0)
	if len(items) != 0 {
		t.Fatalf("the queue holds %d items after a repeat of the acknowledged blob, want 0", len(items))
	}
	if n := dedupeKeyCount(t, f); n != 1 {
		t.Fatalf("%d dedupe keys for one blob, want 1", n)
	}
}
