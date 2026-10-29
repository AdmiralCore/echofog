package peerstore

import (
	"sync"
	"testing"
)

// Saves started while the store closes are either waited for or not started: Close used to Wait on
// the WaitGroup while new saves still Added to it, a data race the race detector caught when a node
// shut down as a peer connected.
func TestPeerStore_SaveAsyncDuringClose(t *testing.T) {
	st, cleanup := createTestStorage(t)
	defer cleanup()
	ps := NewPeerStore(st, nil)
	pub, _ := randomPubKey(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ps.SavePeerAsync(pub, "203.0.113.5:4242", 100)
			}
		}()
	}
	_ = ps.Close()
	wg.Wait()
	ps.SavePeerAsync(pub, "203.0.113.5:4242", 100) // after Close: nothing is started
	ps.wg.Wait()
}
