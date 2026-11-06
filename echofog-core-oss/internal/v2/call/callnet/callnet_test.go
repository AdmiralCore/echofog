package callnet

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type sent struct {
	kind mux.PathKind
	typ  uint64
	data []byte
}

type fakeLink struct {
	mu    sync.Mutex
	kinds map[mux.PathKind]int
	out   []sent
}

func (f *fakeLink) PathKinds() map[mux.PathKind]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[mux.PathKind]int{}
	for k, v := range f.kinds {
		m[k] = v
	}
	return m
}

func (f *fakeLink) SendDatagramVia(k mux.PathKind, typ uint64, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kinds[k] == 0 {
		return mux.ErrNoPathKind
	}
	f.out = append(f.out, sent{k, typ, append([]byte(nil), p...)})
	return nil
}

type links map[[32]byte]*fakeLink

func (l links) Link(d [32]byte) (route.Link, bool) {
	f, ok := l[d]
	if !ok || f == nil {
		return nil, false
	}
	return f, true
}

type signalRec struct {
	to    [32]byte
	lossy bool
	data  []byte
}

type signalLog struct {
	mu  sync.Mutex
	got []signalRec
}

func (s *signalLog) SendSignal(to [32]byte, lossy bool, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, signalRec{to, lossy, append([]byte(nil), p...)})
	return nil
}

func dev(b byte) [32]byte { return [32]byte{b} }

func newT(l links, r route.Route) (*Transport, *signalLog) {
	sig := &signalLog{}
	return New(Config{Links: l, Signals: sig, Route: func() route.Route { return r }}), sig
}

func TestMediaGoesAsDatagramsOnTheRoutePath(t *testing.T) {
	both := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}}
	cases := []struct {
		name  string
		r     route.Route
		kind  transport.Kind
		typ   uint64
		onKnd mux.PathKind
	}{
		{"auto audio", route.Auto, transport.MediaAudio, frame.DgramCallAudio, mux.PathDirect},
		{"auto video", route.Auto, transport.MediaVideo, frame.DgramCallVideo, mux.PathDirect},
		{"relay audio", route.Relay, transport.MediaAudio, frame.DgramCallAudio, mux.PathRelay},
		{"relay video", route.Relay, transport.MediaVideo, frame.DgramCallVideo, mux.PathRelay},
		{"direct video", route.Direct, transport.MediaVideo, frame.DgramCallVideo, mux.PathDirect},
	}
	for _, c := range cases {
		both.out = nil
		tr, _ := newT(links{dev(2): both}, c.r)
		if err := tr.SendUnreliableFrame(dev(2), c.kind, []byte("unit")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(both.out) != 1 || both.out[0].kind != c.onKnd || both.out[0].typ != c.typ || string(both.out[0].data) != "unit" {
			t.Fatalf("%s: sent %+v", c.name, both.out)
		}
	}
}

func TestMediaFailsClosedAndNeverUsesAReliableStream(t *testing.T) {
	direct := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1}}
	tr, sig := newT(links{dev(2): direct}, route.Relay)
	if err := tr.SendUnreliableFrame(dev(2), transport.MediaVideo, []byte("v")); !errors.Is(err, route.ErrNoPath) {
		t.Fatalf("relay route, direct-only peer: %v, want ErrNoPath", err)
	}
	if len(direct.out) != 0 {
		t.Fatal("media left on a direct path under call_route=relay")
	}
	if tr.Stats().SendNoPath != 1 {
		t.Fatalf("stats %+v", tr.Stats())
	}
	if err := tr.SendRawFrame(dev(2), transport.MediaVideo, []byte("v")); !errors.Is(err, ErrMediaNeedsDatagram) {
		t.Fatalf("reliable media: %v", err)
	}
	if len(sig.got) != 0 {
		t.Fatal("media was sent as a signal")
	}
	if err := tr.SendUnreliableFrame(dev(9), transport.MediaAudio, []byte("a")); !errors.Is(err, ErrNoLink) {
		t.Fatalf("unknown peer: %v", err)
	}
	both := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1}}
	tr, _ = newT(links{dev(2): both}, route.Auto)
	if err := tr.SendUnreliableFrame(dev(2), transport.MediaVideo, make([]byte, MaxDatagram+1)); !errors.Is(err, transport.ErrDatagramTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	if err := tr.SendUnreliableFrame(dev(2), transport.MediaVideo, make([]byte, MaxDatagram)); err != nil {
		t.Fatalf("floor-size: %v", err)
	}
}

func TestSignalsGoToTheSignalPort(t *testing.T) {
	tr, sig := newT(links{}, route.Auto)
	_ = tr.SendRawFrame(dev(3), transport.Signal, []byte("s1"))
	_ = tr.SendUnreliableFrame(dev(3), transport.SignalLossy, []byte("s2"))
	if len(sig.got) != 2 || sig.got[0].lossy || !sig.got[1].lossy || sig.got[0].to != dev(3) || string(sig.got[1].data) != "s2" {
		t.Fatalf("%+v", sig.got)
	}
	if err := tr.SendUnreliableFrame(dev(3), transport.Signal, []byte("x")); err == nil {
		t.Fatal("a reliable signal kind was accepted on the unreliable call")
	}
}

func TestDeliverRoutesByTypeAndKnowsTheSender(t *testing.T) {
	tr, _ := newT(links{}, route.Auto)
	var mu sync.Mutex
	var got []string
	for _, k := range []transport.Kind{transport.MediaAudio, transport.MediaVideo, transport.SignalLossy} {
		tr.RegisterFastFrameHandler(k, func(p []byte, from types.PeerID) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, string(p)+"@"+string(rune('0'+from[0])))
			return nil
		})
	}
	tr.Deliver(Inbound{Device: dev(4), Type: frame.DgramCallAudio, Payload: []byte("a"), Path: mux.PathDirect})
	tr.Deliver(Inbound{Device: dev(5), Type: frame.DgramCallVideo, Payload: []byte("v"), Path: mux.PathRelay})
	tr.Deliver(Inbound{Device: dev(6), Type: frame.DgramCallSignal, Payload: []byte("s"), Path: mux.PathRelay})
	tr.Deliver(Inbound{Device: dev(7), Type: frame.DgramCallAuth, Payload: []byte("?"), Path: mux.PathRelay})
	if len(got) != 3 || got[0] != "a@4" || got[1] != "v@5" || got[2] != "s@6" {
		t.Fatalf("%v", got)
	}
	if tr.Stats().DroppedUnknown != 1 {
		t.Fatalf("stats %+v", tr.Stats())
	}
}

func TestDeliverDropsMediaTheRouteForbidsOrNobodyAdmits(t *testing.T) {
	r := route.Relay
	admit := map[[32]byte]bool{dev(4): true}
	tr := New(Config{Links: links{}, Route: func() route.Route { return r }, Admit: func(d [32]byte) bool { return admit[d] }})
	n := 0
	tr.RegisterFastFrameHandler(transport.MediaVideo, func([]byte, types.PeerID) error { n++; return nil })

	tr.Deliver(Inbound{Device: dev(4), Type: frame.DgramCallVideo, Payload: []byte("v"), Path: mux.PathDirect})
	if n != 0 || tr.Stats().DroppedRoute != 1 {
		t.Fatalf("direct media under call_route=relay: n=%d %+v", n, tr.Stats())
	}
	tr.Deliver(Inbound{Device: dev(4), Type: frame.DgramCallVideo, Payload: []byte("v"), Path: mux.PathRelay})
	if n != 1 {
		t.Fatal("relayed media from a member was dropped")
	}
	tr.Deliver(Inbound{Device: dev(8), Type: frame.DgramCallVideo, Payload: []byte("v"), Path: mux.PathRelay})
	if n != 1 || tr.Stats().DroppedAdmit != 1 {
		t.Fatalf("media from a stranger reached the call layer: n=%d %+v", n, tr.Stats())
	}
	r = route.Direct
	tr.Deliver(Inbound{Device: dev(4), Type: frame.DgramCallVideo, Payload: []byte("v"), Path: mux.PathRelay})
	if n != 1 {
		t.Fatal("relayed media under call_route=direct was delivered")
	}
}

func TestRelayedAndDirectLinkFollowTheRoute(t *testing.T) {
	relayOnly := &fakeLink{kinds: map[mux.PathKind]int{mux.PathRelay: 1}}
	direct := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1}}
	lk := links{dev(2): relayOnly, dev(3): direct}
	cases := []struct {
		name            string
		r               route.Route
		peer            [32]byte
		relayed, direct bool
	}{
		{"auto relay-only peer", route.Auto, dev(2), true, false},
		{"auto direct peer", route.Auto, dev(3), false, true},
		// Under call_route=relay the call layer must not try to dial a direct connection.
		{"relay route relay-only peer", route.Relay, dev(2), true, true},
		{"relay route direct-only peer", route.Relay, dev(3), false, true},
		{"direct route relay-only peer", route.Direct, dev(2), false, false},
		{"unknown peer", route.Auto, dev(9), false, false},
	}
	for _, c := range cases {
		tr, _ := newT(lk, c.r)
		if got := tr.Relayed(c.peer); got != c.relayed {
			t.Errorf("%s: Relayed = %v", c.name, got)
		}
		if got := tr.DirectLink(c.peer); got != c.direct {
			t.Errorf("%s: DirectLink = %v", c.name, got)
		}
	}
}

func TestRouteIsReadOnEverySend(t *testing.T) {
	l := &fakeLink{kinds: map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}}
	r := route.Auto
	tr := New(Config{Links: links{dev(2): l}, Route: func() route.Route { return r }})
	_ = tr.SendUnreliableFrame(dev(2), transport.MediaAudio, []byte("1"))
	r = route.Relay
	_ = tr.SendUnreliableFrame(dev(2), transport.MediaAudio, []byte("2"))
	if len(l.out) != 2 || l.out[0].kind != mux.PathDirect || l.out[1].kind != mux.PathRelay {
		t.Fatalf("%+v", l.out)
	}
}

func TestDeliverBoundsTheRatePerDevice(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tr := New(Config{Links: links{}, MaxPerSecond: 100, Now: func() time.Time { return now }})
	n := 0
	tr.RegisterFastFrameHandler(transport.MediaAudio, func([]byte, types.PeerID) error { n++; return nil })
	send := func(d byte, k int) {
		for i := 0; i < k; i++ {
			tr.Deliver(Inbound{Device: dev(d), Type: frame.DgramCallAudio, Payload: []byte("a"), Path: mux.PathDirect})
		}
	}
	send(1, 150)
	if n != 100 || tr.Stats().DroppedRate != 50 {
		t.Fatalf("a flood from one device: %d passed, stats %+v; want 100 and 50 dropped", n, tr.Stats())
	}
	send(2, 100)
	if n != 200 {
		t.Fatalf("another device shares the budget of the flooder: %d", n)
	}
	now = now.Add(500 * time.Millisecond)
	send(1, 100)
	if n != 250 {
		t.Fatalf("half a second refills half the budget: %d, want 250", n)
	}
	now = now.Add(10 * time.Second)
	send(1, 150)
	if n != 350 {
		t.Fatalf("a long pause refills at most one second: %d, want 350", n)
	}
}

func TestDeliverBucketsAreBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tr := New(Config{Links: links{}, MaxPerSecond: 10, Now: func() time.Time { return now }})
	for i := 0; i < 5000; i++ {
		var d [32]byte
		d[0], d[1] = byte(i), byte(i>>8)
		tr.Deliver(Inbound{Device: d, Type: frame.DgramCallAudio, Payload: []byte("a"), Path: mux.PathDirect})
	}
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	if len(tr.rate) > maxSources {
		t.Fatalf("%d buckets kept, limit %d", len(tr.rate), maxSources)
	}
}
