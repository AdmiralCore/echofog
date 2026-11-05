package node

import (
	"testing"
	"time"
)

// F4: a relay that refuses REGISTER is retried with a growing pause, not every 3 s for ever.
func TestRegisterBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for fails := 0; fails < 12; fails++ {
		d := registerBackoff(fails)
		if d < prev {
			t.Fatalf("pause shrank at %d failures: %v < %v", fails, d, prev)
		}
		prev = d
	}
	if registerBackoff(0) != registerRetry || prev != registerPoll*10 {
		t.Fatalf("first pause %v, cap %v; want %v and %v", registerBackoff(0), prev, registerRetry, registerPoll*10)
	}
}
