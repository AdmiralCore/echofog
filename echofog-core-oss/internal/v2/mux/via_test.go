package mux

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// bindRelay binds a second path of kind PathRelay from A and returns its ID.
func bindRelay(t testing.TB, pr *pair) (uint64, *carrierEnds) {
	t.Helper()
	e := newCarrier(t, false)
	ctx := ctxT(t)
	type res struct {
		id  uint64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		id, err := pr.a.BindPath(ctx, e.ga, PathRelay)
		ch <- res{id, err}
	}()
	if _, _, err := pr.binder.Accept(ctx, e.gb, PathRelay); err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.id, e
}

// T9.3: a call route picks the kind of path a media datagram takes.
func TestSendDatagramViaPicksTheKindOfPath(t *testing.T) {
	pr := sessionPair(t, false, nil)
	ctx := ctxT(t)
	relayID, relay := bindRelay(t, pr)
	if _, act, _ := pr.a.Paths(); act == relayID {
		t.Fatal("the relay path became active while a direct path lives")
	}
	if got := pr.a.PathKinds(); got[PathDirect] != 1 || got[PathRelay] != 1 {
		t.Fatalf("PathKinds = %v, want one direct and one relay", got)
	}

	directBefore := pr.path0.ta.Bytes()
	if err := pr.a.SendDatagramVia(PathRelay, frame.DgramCallVideo, []byte("VIA-RELAY-UNIT")); err != nil {
		t.Fatalf("via relay: %v", err)
	}
	d, err := pr.b.ReceiveDatagram(ctx)
	if err != nil || string(d.Payload) != "VIA-RELAY-UNIT" || d.Path != relayID {
		t.Fatalf("via relay arrived as %+v, %v; want path %d", d, err, relayID)
	}
	if bytes.Contains(relay.ta.Bytes(), []byte("VIA-RELAY-UNIT")) {
		t.Fatal("plaintext datagram on a relay path (wire.md 8.4)")
	}
	if len(pr.path0.ta.Bytes()) != len(directBefore) {
		t.Fatal("a datagram sent via relay left on the direct path")
	}

	if err := pr.a.SendDatagramVia(PathDirect, frame.DgramCallAudio, []byte("VIA-DIRECT-UNIT")); err != nil {
		t.Fatalf("via direct: %v", err)
	}
	d, err = pr.b.ReceiveDatagram(ctx)
	if err != nil || string(d.Payload) != "VIA-DIRECT-UNIT" || d.Path == relayID {
		t.Fatalf("via direct arrived as %+v, %v", d, err)
	}
}

// Fail closed: with no path of the kind nothing is sent, on any path.
func TestSendDatagramViaDoesNotFallBack(t *testing.T) {
	pr := sessionPair(t, false, nil)
	before := len(pr.path0.ta.Bytes())
	err := pr.a.SendDatagramVia(PathRelay, frame.DgramCallVideo, []byte("MUST-NOT-LEAVE"))
	if !errors.Is(err, ErrNoPathKind) {
		t.Fatalf("no relay path: %v, want ErrNoPathKind", err)
	}
	time.Sleep(20 * time.Millisecond)
	if bytes.Contains(pr.path0.ta.Bytes(), []byte("MUST-NOT-LEAVE")) || len(pr.path0.ta.Bytes()) != before {
		t.Fatal("the datagram fell back to the direct path")
	}
}

// One nonce counter per direction across all paths: datagrams alternating over a direct and a
// relay path all arrive, none is rejected as a replay or a bad seal (wire.md 8.4).
func TestSendDatagramViaSharesOneCounter(t *testing.T) {
	pr := sessionPair(t, true, nil)
	ctx := ctxT(t)
	bindRelay(t, pr)
	const n = 200
	for i := 0; i < n; i++ {
		kind := PathDirect
		if i%2 == 1 {
			kind = PathRelay
		}
		if err := pr.a.SendDatagramVia(kind, frame.DgramCallAudio, []byte{byte(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	seen := map[byte]bool{}
	for len(seen) < n {
		d, err := pr.b.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("got %d of %d: %v (stats %+v)", len(seen), n, err, pr.b.Stats())
		}
		seen[d.Payload[0]] = true
	}
	if st := pr.b.Stats(); st.DatagramsBad != 0 {
		t.Fatalf("bad datagrams %d", st.DatagramsBad)
	}
}
