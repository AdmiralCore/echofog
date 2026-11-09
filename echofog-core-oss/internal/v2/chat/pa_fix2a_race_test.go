package chat

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Tick reads the device and the envelope of an outgoing item in attempt; a received init drops those
// envelopes in invalidate under s.mu. Reading them bare raced on the slice header (ЧМ-6). The stress
// loop keeps both sides running so -race sees the unsynchronised access; the fix reads them under
// s.mu.
func TestOutboxEnvelopeReadIsSynchronisedWithIncomingWrites(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a) // b has a session with a

	// b has an outgoing item with a built envelope; a is unreachable, so the item stays active
	w.mu.Lock()
	w.down[a.self.Device] = true
	w.mu.Unlock()
	w.send(b, a, "queued")
	w.tick(b)

	var it *item
	b.svc.mu.Lock()
	for _, x := range b.svc.items {
		if x.To == a.self.Node {
			it = x
		}
	}
	b.svc.mu.Unlock()
	if it == nil {
		t.Fatal("no outgoing item")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.svc.attempt(ctx, it, map[[32]byte]lookup{}) // what Tick does for a due item
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.svc.invalidate(a.self.Node) // what Receive does when an init opens
			}
		}
	}()
	<-time.After(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}
