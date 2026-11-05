package relay

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// addrWorld joins A and B (listed) and lets B answer with the given address policy.
func addrWorld(t *testing.T, bAgrees bool) (w *world, a, b *side) {
	t.Helper()
	w = newWorld(t, ServerConfig{Delay: func() time.Duration { return 50 * time.Millisecond }})
	a = w.join(newTNode(t), "10.0.0.1", true)
	b = w.join(newTNode(t), "10.0.0.2", true)
	w.raw[b.node] = func(st *mux.Stream) {
		in, err := AnswerAddr(st, w.rNode, b.hub, nil, func(Incoming) bool { return true },
			func(in Incoming) bool { return bAgrees })
		if err == nil {
			b.in <- in
		}
	}
	return w, a, b
}

func dialAddr(t *testing.T, a, b *side, want bool) (*Circuit, Inbound) {
	t.Helper()
	type res struct {
		c   *Circuit
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := Dial(a.w.ctx, a.open, a.w.rNode, b.node, ConnectOptions{WantAddr: want})
		ch <- res{c, err}
	}()
	var in Inbound
	select {
	case in = <-b.in:
	case <-time.After(5 * time.Second):
		t.Fatal("no INCOMING at B")
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	return r.c, in
}

// session runs the end-to-end handshake over the circuit, proving the bytes after PEER_ADDR are
// intact and not shifted by the extra frame.
func e2e(t *testing.T, w *world, a, b *side, circ *Circuit, in Inbound) {
	t.Helper()
	binder := mux.NewBinder()
	ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
	defer cancel()
	acc := make(chan error, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: in.Stream}, session.AcceptConfig{
			Local: b.n.local(true), Kind: mux.PathRelay, Binder: binder,
			OnEstablished: func(s *mux.Session) { binder.Add(s) }})
		if err == nil {
			t.Cleanup(func() { _ = est.Session.Close() })
		}
		acc <- err
	}()
	est, err := session.Dial(ctx, b.n.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier}, a.n.local(false),
		session.WithCarrier(circ), session.WithPathKind(mux.PathRelay))
	if err != nil {
		t.Fatalf("A dial through R: %v", err)
	}
	t.Cleanup(func() { _ = est.Session.Close() })
	if err := <-acc; err != nil {
		t.Fatalf("B accept: %v", err)
	}
}

func TestPeerAddrBothAgree(t *testing.T) {
	w, a, b := addrWorld(t, true)
	circ, in := dialAddr(t, a, b, true)
	if got, want := circ.PeerAddr(), netip.MustParseAddrPort("10.0.0.2:4000"); got != want {
		t.Fatalf("A learned %v, want B at %v", got, want)
	}
	if got, want := in.PeerAddr, netip.MustParseAddrPort("10.0.0.1:4000"); got != want {
		t.Fatalf("B learned %v, want A at %v", got, want)
	}
	e2e(t, w, a, b, circ, in)
}

// B declines the exchange: A hears nothing about B, B hears nothing about A, and the circuit still
// carries a session.
func TestPeerAddrBDeclines(t *testing.T) {
	w, a, b := addrWorld(t, false)
	circ, in := dialAddr(t, a, b, true)
	if circ.PeerAddr().IsValid() || in.PeerAddr.IsValid() {
		t.Fatalf("addresses leaked: A saw %v, B saw %v", circ.PeerAddr(), in.PeerAddr)
	}
	e2e(t, w, a, b, circ, in)
}

// A never asked: B cannot agree on its own, nothing is sent, and the stream starts at the record.
func TestPeerAddrAWantsNothing(t *testing.T) {
	w, a, b := addrWorld(t, true)
	circ, in := dialAddr(t, a, b, false)
	if circ.PeerAddr().IsValid() || in.PeerAddr.IsValid() {
		t.Fatalf("addresses sent without want_addr: %v %v", circ.PeerAddr(), in.PeerAddr)
	}
	e2e(t, w, a, b, circ, in)
}

// A node without a usable observed address (zero Remote) gets an empty answer, not a made-up one.
func TestPeerAddrUnknownRemote(t *testing.T) {
	w, a, b := addrWorld(t, true)
	b.addr = netip.AddrPort{}
	circ, in := dialAddr(t, a, b, true)
	if circ.PeerAddr().IsValid() {
		t.Fatalf("A saw %v for a peer R cannot place", circ.PeerAddr())
	}
	e2e(t, w, a, b, circ, in)
}
