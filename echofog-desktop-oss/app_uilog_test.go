package main

import (
	"testing"
	"time"
)

// A message repeating in a loop gets a short burst through, then one line per interval that
// says how many were held back.
func TestUILogLimitsRepeats(t *testing.T) {
	k := uiLogKey{"call", "test-limiter " + t.Name()}
	now := time.Now()
	for i := 0; i < uiLogBurst; i++ {
		if ok, _ := uiLogAllow(k, now); !ok {
			t.Fatalf("line %d of the burst was held back", i)
		}
	}
	for i := 0; i < 7; i++ {
		if ok, _ := uiLogAllow(k, now.Add(time.Second)); ok {
			t.Fatal("past the burst, lines must wait for the interval")
		}
	}
	ok, skipped := uiLogAllow(k, now.Add(uiLogEvery))
	if !ok || skipped != 7 {
		t.Fatalf("after the interval: ok=%v skipped=%d, want true 7", ok, skipped)
	}
}
