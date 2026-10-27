package messenger

import (
	"sync"
	"testing"
	"time"
)

// The states of a message reach the listeners in the order they happened: a confirmation that
// comes a moment after the send must not be overtaken by the "sent" before it.
func TestMessageStatesKeepTheirOrder(t *testing.T) {
	s := &MessageBroker{}
	var mu sync.Mutex
	var got []string
	s.OnMessageState(func(ev MessageState) {
		mu.Lock()
		got = append(got, ev.State)
		mu.Unlock()
	})
	want := []string{}
	for i := 0; i < 200; i++ {
		for _, st := range []string{StateSent, StateDelivered} {
			s.noteState(peerID(1), "m", st)
			want = append(want, st)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("%d of %d states told", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state %d: %s, want %s", i, got[i], want[i])
		}
	}
}
