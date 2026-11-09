package mailbox

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// Items the handler does not take must not hide the ones behind them: the poll reads on past them. An
// item left unhandled is never acknowledged: it stays on the relay for a later attempt until its
// 7-day expiry (services.md 5.3.4, K9). holdFor bounds one delivery attempt, not the item lifetime.
func TestReceiverReadsPastItemsItDoesNotTake(t *testing.T) {
	w := newWorld(t)
	var takeBad bool
	var good int
	r, _ := w.receiver(t, func(b []byte) bool {
		if bytes.HasPrefix(b, []byte("bad")) {
			return takeBad
		}
		good++
		return true
	}, nil)
	_ = r.Tick(context.Background())
	q := r.Queues()[0]
	s := &Sender{Dial: w.n}
	for i := 0; i < 300; i++ {
		if st, err := s.Deposit(context.Background(), w.f.device, w.target(q.Queue[:]), []byte(fmt.Sprintf("bad %d", i))); err != nil || st != StatusStored {
			t.Fatalf("bad deposit %d: %d %v", i, st, err)
		}
	}
	for i := 0; i < 5; i++ {
		if st, err := s.Deposit(context.Background(), w.f.device, w.target(q.Queue[:]), []byte(fmt.Sprintf("good %d", i))); err != nil || st != StatusStored {
			t.Fatalf("good deposit %d: %d %v", i, st, err)
		}
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if good != 5 {
		t.Fatalf("%d of 5 good items behind 300 that were not taken", good)
	}
	if n := w.m.fx.srv.PrivateItems(); n != 300 {
		t.Fatalf("M holds %d items, want the 300 not taken", n)
	}
	// an hour later the unhandled items are still there: they were not acknowledged, so a transient
	// failure can still be repaired
	w.clock.Advance(61 * time.Minute)
	_ = r.Tick(context.Background())
	if n := w.m.fx.srv.PrivateItems(); n != 300 {
		t.Fatalf("M holds %d items an hour later, want all 300: unhandled items must not be ACKed", n)
	}
	// a later attempt that succeeds processes and acknowledges them
	takeBad = true
	_ = r.Tick(context.Background())
	if n := w.m.fx.srv.PrivateItems(); n != 0 {
		t.Fatalf("M still holds %d items after they were handled", n)
	}
}

// An unhandled item lives to its 7-day expiry on the relay (services.md 5.3.4): the receiver does not
// cut it short locally.
func TestReceiverKeepsUnhandledItemsForSevenDays(t *testing.T) {
	w := newWorld(t)
	r, _ := w.receiver(t, func([]byte) bool { return false }, nil)
	_ = r.Tick(context.Background())
	q := r.Queues()[0]
	s := &Sender{Dial: w.n}
	for i := 0; i < 10; i++ {
		if st, err := s.Deposit(context.Background(), w.f.device, w.target(q.Queue[:]), []byte(fmt.Sprintf("bad %d", i))); err != nil || st != StatusStored {
			t.Fatalf("deposit %d: %d %v", i, st, err)
		}
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := w.m.fx.srv.PrivateItems(); n != 10 {
		t.Fatalf("M holds %d items, want 10", n)
	}
	w.clock.Advance(8 * 24 * time.Hour) // past the 7-day item lifetime
	_ = r.Tick(context.Background())
	if n := w.m.fx.srv.PrivateItems(); n != 0 {
		t.Fatalf("M still holds %d items after their 7-day expiry", n)
	}
}

// A queue that ended on the relay while the node was off is dropped, not polled for ever.
func TestReceiverDropsAnExpiredQueue(t *testing.T) {
	w := newWorld(t)
	r, rec := w.receiver(t, nil, nil)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := r.Queues()[0]
	w.clock.Advance(12 * 24 * time.Hour) // longer than the queue lives (9 days)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after 12 days: %v", err)
	}
	now := r.Queues()
	if len(now) != 1 || now[0].Queue == first.Queue {
		t.Fatalf("no new queue after the old one ended: %v", now)
	}
	entries, _ := rec.cfgStore.Scan([]byte("q:"), 100)
	if len(entries) != 1 {
		t.Fatalf("%d queue records kept, want only the current one", len(entries))
	}
	w.clock.Advance(72 * 24 * time.Hour)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after 72 more days: %v", err)
	}
	entries, _ = rec.cfgStore.Scan([]byte("q:"), 100)
	if len(entries) != 1 {
		t.Fatalf("%d queue records after a longer break", len(entries))
	}
}
