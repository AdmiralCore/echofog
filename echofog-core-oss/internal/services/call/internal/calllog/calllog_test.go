package calllog

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestLimiterFoldsRepeats(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(10 * time.Second)
	l.now = func() time.Time { return now }

	if ok, n := l.Allow("a"); !ok || n != 0 {
		t.Fatalf("first line: ok=%v skipped=%d", ok, n)
	}
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("a"); ok {
			t.Fatal("repeat inside the interval was let through")
		}
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key must not be held back")
	}
	now = now.Add(10 * time.Second)
	if ok, n := l.Allow("a"); !ok || n != 5 {
		t.Fatalf("after the interval: ok=%v skipped=%d, want true 5", ok, n)
	}
}

func TestShortIDs(t *testing.T) {
	var id types.PeerID
	if Peer(id) != "none" {
		t.Fatal("zero peer should read as none")
	}
	id[0] = 0xab
	if got := Peer(id); got != "ab0000000000" {
		t.Fatalf("Peer = %q", got)
	}
	if got := Call("room_0123456789abcdef01234567"); got != "room_0123456789a" {
		t.Fatalf("Call = %q", got)
	}
}
