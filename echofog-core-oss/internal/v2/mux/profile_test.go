package mux

import (
	"testing"
)

// TestSessionGreaseSentCounter pins the sender-side GREASE counter that the runtime timer needs
// to prove it fires (D18).
func TestSessionGreaseSentCounter(t *testing.T) {
	pr := sessionPair(t, false, nil)
	if got := pr.a.Stats().GreaseSent; got != 0 {
		t.Fatalf("GREASE sent before the first frame: %d", got)
	}
	if err := pr.a.SendGrease(); err != nil {
		t.Fatalf("SendGrease: %v", err)
	}
	waitFor(t, func() bool { return pr.a.Stats().GreaseSent == 1 })
	if _, err := pr.a.Ping(ctxT(t)); err != nil {
		t.Fatalf("the peer broke on a GREASE frame: %v", err)
	}
}

// TestSessionLowPowerGetter: the platform profile's low_power flag is visible on the session it
// configured (platforms.md §2).
func TestSessionLowPowerGetter(t *testing.T) {
	pr := sessionPair(t, false, func(a, b *Config) { a.LowPower = true })
	if !pr.a.LowPower() {
		t.Fatal("low_power was not applied to the session")
	}
	if pr.b.LowPower() {
		t.Fatal("the peer's session is unexpectedly low power")
	}
}
