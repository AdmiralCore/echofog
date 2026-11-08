package chat

import (
	"context"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// An envelope that did not open is not attributed to the device it names: anybody with the public
// prekeys of the receiver can seal one. Such refusals must not start a reset or replace a session
// (services.md 4.4, ЧМ-1).
func TestUnauthenticatedSenderStartsNoRecovery(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "hello")
	w.tick(a)
	if len(b.texts()) != 1 {
		t.Fatalf("setup: b got %v", b.texts())
	}
	// A message of the honest session that is still on the way when the attack starts.
	kept := craft(t, b, a, a.self.Account, a.self.DeviceKey, mustEnc(t, a, b, "kept"), false)

	// An outsider seals envelopes to b with b's public prekeys and names a's account and device.
	// Every one of them fails to open on b's real session with a.
	for i := 0; i < 6; i++ {
		st := b.svc.Receive(context.Background(), bentFrom(t, a, b, a.self.Account, a.self.DeviceKey))
		if st != chatpb.StatusRefused {
			t.Fatalf("attacker envelope %d answered %d", i, st)
		}
		w.clock.Add(time.Second)
	}
	w.tick(b) // anything b queued would leave here
	for _, c := range a.ctrl {
		if c.GetReset_() != nil {
			t.Fatalf("a reset was started by envelopes that did not open: %v", a.ctrl)
		}
	}

	// The honest session was neither reset nor replaced: its pending message still opens...
	if st := b.svc.Receive(context.Background(), kept); st != chatpb.StatusOK {
		t.Fatalf("a message of the honest session after the attack: %d", st)
	}
	if got := b.texts(); len(got) != 2 || got[1] != "kept" {
		t.Fatalf("b got %v", got)
	}
	// ...and the session keeps carrying messages.
	w.send(a, b, "still works")
	w.tick(a)
	if got := b.texts(); got[len(got)-1] != "still works" {
		t.Fatalf("the session was replaced: %v", got)
	}
}

// Two replacements within seven days must not evict the session before them (services.md 4.4:
// every replaced session stays seven days for messages still on the way).
func TestSecondReplacementKeepsTheOlderSessionForSevenDays(t *testing.T) {
	w := newWorld(t)
	a, b := w.newPeer(), w.newPeer()
	w.send(a, b, "first")
	w.tick(a)
	// A message of the first session, not delivered before the replacements.
	kept := craft(t, b, a, a.self.Account, a.self.DeviceKey, mustEnc(t, a, b, "kept"), false)

	// a loses its sessions twice; each fresh init replaces b's current session once.
	for i := 0; i < 2; i++ {
		a.dir = t.TempDir()
		a.restart()
		w.send(a, b, "fresh")
		w.tick(a)
		w.clock.Add(time.Hour)
	}
	if st := b.svc.Receive(context.Background(), kept); st != chatpb.StatusOK {
		t.Fatalf("a message of the session replaced twice within seven days: %d", st)
	}
	if got := b.texts(); got[len(got)-1] != "kept" {
		t.Fatalf("b got %v", got)
	}
}
