package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// sealReset makes a fresh envelope from from to to that carries an authenticated Reset.
func sealReset(t *testing.T, from, to *peer) []byte {
	t.Helper()
	reset := &chatpb.PersonalMessageProto{Control: &chatpb.Control{Kind: &chatpb.Control_Reset_{Reset_: &chatpb.Reset{}}}}
	plain, err := chatpb.PadPersonal(reset)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := from.cr.Encrypt(to.self.Device, plain)
	if err != nil {
		t.Fatal(err)
	}
	return craft(t, to, from, from.self.Account, from.self.DeviceKey, msg, false)
}

// Tick must read the envelope of an outgoing item under the same lock that a received Reset takes to
// drop it: reading it.Env bare was a race on the slice header (ЧМ-6). Run with -race.
func TestIncomingResetAndTickShareTheOutboxSafely(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a) // b has a session with a

	// b queues messages to a while a is unreachable: the items keep their envelopes and stay in the
	// outbox, so every Tick attempt reads them
	w.mu.Lock()
	w.down[a.self.Device] = true
	w.mu.Unlock()
	for i := 0; i < 8; i++ {
		w.send(b, a, "queued")
	}
	w.tick(b)

	// build the resets before the concurrent part so only the service shares its crypto state
	envs := make([][]byte, 80)
	for i := range envs {
		envs[i] = sealReset(t, a, b)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = b.svc.Tick(context.Background())
			}
		}
	}()
	for _, env := range envs {
		w.clock.Add(6 * time.Minute) // make the mailbox retry due again
		_ = b.svc.Receive(context.Background(), env)
	}
	close(done)
	wg.Wait()
}
