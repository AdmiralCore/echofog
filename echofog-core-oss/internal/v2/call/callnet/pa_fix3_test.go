package callnet

import (
	"errors"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

type failingSignals struct{ err error }

func (f failingSignals) SendSignal([32]byte, bool, []byte) error { return f.err }

// A refused signal must not vanish from the counters: the path feedback a broadcaster needs is
// sent only this way, and the reason it did not leave has to stay visible.
func TestSignalRefusalKeepsItsReasonInStats(t *testing.T) {
	tr := New(Config{Links: links{}, Signals: failingSignals{err: errors.New("signal sink is closed")}})
	if err := tr.SendUnreliableFrame(dev(3), transport.SignalLossy, []byte("feedback")); err == nil {
		t.Fatal("a refused signal was reported as sent")
	}
	st := tr.Stats()
	if st.SignalRefused != 1 || st.SignalReasons["port"] != 1 {
		t.Fatalf("signal refusal not counted: %+v", st)
	}
	if !strings.Contains(st.LastSignalError, "signal sink is closed") {
		t.Fatalf("the reason was lost: %q", st.LastSignalError)
	}
	// A reliable signal that fails is kept the same way.
	if err := tr.SendRawFrame(dev(3), transport.Signal, []byte("manifest")); err == nil {
		t.Fatal("a refused reliable signal was reported as sent")
	}
	if st := tr.Stats(); st.SignalRefused != 2 || st.SignalReasons["port"] != 2 {
		t.Fatalf("reliable refusal not counted: %+v", st)
	}
}

// Under call_route=relay media goes over the relay even when a direct path exists (the signalling
// stream is usually direct), so the peer must get the relay watchdogs: short direct ones would see
// the added hop as a lost parent and rebuild the tree.
func TestRelayedIsTrueWhenRelayRouteUsesADirectSessionToo(t *testing.T) {
	both := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}}
	tr, _ := newT(links{dev(2): both}, route.Relay)
	if !tr.Relayed(dev(2)) {
		t.Fatal("relay route with a direct path reported direct watchdogs")
	}
	// Under Auto media takes the direct path: the peer is not reached only through a relay.
	tr, _ = newT(links{dev(2): both}, route.Auto)
	if tr.Relayed(dev(2)) {
		t.Fatal("auto with a direct path reported a relayed peer")
	}
}
