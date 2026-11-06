package mailbox

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// Items the handler does not take must not hide the ones behind them: the poll reads on past them, and
// what stays unhandled for an hour is given up (acknowledged), so a flood of such items blocks nothing
// for long.
func TestReceiverReadsPastItemsItDoesNotTake(t *testing.T) {
	w := newWorld(t)
	var good int
	r, _ := w.receiver(t, func(b []byte) bool {
		if bytes.HasPrefix(b, []byte("bad")) {
			return false
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
	// an hour later they are given up
	w.clock.Advance(61 * time.Minute)
	_ = r.Tick(context.Background())
	_ = r.Tick(context.Background())
	if n := w.m.fx.srv.PrivateItems(); n != 0 {
		t.Fatalf("M still holds %d items after the hour", n)
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
