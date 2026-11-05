package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// sealedCounters parses the records R copied from sender on circuit cid and returns the counter
// n of every sealed record (wire.md §8.4: record = u16 len ‖ kind ‖ body; body = n ‖ ...).
func (l *tapLog) sealedCounters(t *testing.T, cid uint64, sender [32]byte) []uint64 {
	t.Helper()
	l.mu.Lock()
	b := append([]byte(nil), l.byKey[tapKey{cid, sender}]...)
	l.mu.Unlock()
	var ns []uint64
	for len(b) >= 3 {
		n := int(binary.BigEndian.Uint16(b))
		if n < 1 || len(b) < 2+n {
			break
		}
		kind, body := b[2], b[3:2+n]
		if kind == mux.RecordSealed && len(body) >= 8 {
			ns = append(ns, binary.BigEndian.Uint64(body))
		}
		b = b[2+n:]
	}
	return ns
}

// addRelayPath binds a second circuit through R to the session as an extra path (PATH_BIND over
// the circuit) and returns its circuit ID.
func (w *world) addRelayPath(a, b *side, aEst, bEst *session.Established, binder *mux.Binder) uint64 {
	t := w.t
	t.Helper()
	circ, err := a.dial(b)
	if err != nil {
		t.Fatalf("second circuit: %v", err)
	}
	in := <-b.in
	bound := make(chan error, 1)
	go func() {
		_, err := session.Accept(w.ctx, &oneListener{c: in.Stream}, session.AcceptConfig{
			Local: b.n.local(true), Kind: mux.PathRelay, Binder: binder})
		bound <- err
	}()
	if _, err := aEst.Session.BindPath(w.ctx, mux.NewGuard(circ), mux.PathRelay); err != nil {
		t.Fatalf("BindPath over the circuit: %v", err)
	}
	if err := <-bound; !errors.Is(err, session.ErrPathBound) {
		t.Fatalf("B accept: %v, want ErrPathBound", err)
	}
	return circ.CID()
}

// One counter per direction per session, shared by all paths: with two relay paths no nonce is
// ever used twice (wire.md §8.4).
func TestTwoRelayPathsShareOneNonceCounter(t *testing.T) {
	log := &tapLog{}
	w := newWorld(t, ServerConfig{})
	w.srv.tap = log.tap
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	aEst, bEst, binder := w.relayedSession(a, b)
	first := a.cidOfLast(w)
	second := w.addRelayPath(a, b, aEst, bEst, binder)

	ids, _, _ := aEst.Session.Paths()
	if len(ids) != 2 {
		t.Fatalf("A has %d paths, want 2", len(ids))
	}
	for round := 0; round < 3; round++ {
		for _, id := range ids {
			if err := aEst.Session.SetActivePath(id); err != nil {
				t.Fatal(err)
			}
			echoOnce(t, aEst.Session, bEst.Session, []byte("on a path"))
		}
	}
	c1 := log.sealedCounters(t, first, a.node)
	c2 := log.sealedCounters(t, second, a.node)
	if len(c1) == 0 || len(c2) == 0 {
		t.Fatalf("sealed records: path 1 %d, path 2 %d; both paths must have carried traffic", len(c1), len(c2))
	}
	seen := map[uint64]bool{}
	for _, n := range append(c1, c2...) {
		if seen[n] {
			t.Fatalf("nonce counter %d used twice by A across the two relay paths", n)
		}
		seen[n] = true
	}
}

// When a relay path dies the session moves to the other one without a new handshake.
func TestRelayPathFailover(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	aEst, bEst, binder := w.relayedSession(a, b)
	first := a.cidOfLast(w)
	w.addRelayPath(a, b, aEst, bEst, binder)

	w.srv.mu.Lock()
	c := w.srv.circuits[first]
	w.srv.mu.Unlock()
	w.srv.kill(c)

	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, _, _ := aEst.Session.Paths()
		if len(ids) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dead path not removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	echoOnce(t, aEst.Session, bEst.Session, []byte("after failover"))
	if aEst.Session.Err() != nil {
		t.Fatalf("session ended: %v", aEst.Session.Err())
	}
}

// Datagrams of a relayed session travel on a datagram circuit as SEALED datagrams that R
// forwards unchanged.
func TestSessionDatagramsOverDatagramCircuit(t *testing.T) {
	w := newWorld(t, ServerConfig{})
	a, b := w.join(newTNode(t), "10.0.0.1", true), w.join(newTNode(t), "10.0.0.2", true)
	ctx := w.ctx

	da, err := DialDatagram(ctx, a.open, a.hub, func(typ uint64, p []byte) error { return a.toR.SendDatagram(typ, p) }, w.rNode, b.node, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inD := <-b.in
	if inD.Datagram == nil {
		t.Fatal("expected a datagram circuit at B")
	}
	circ, err := a.dial(b)
	if err != nil {
		t.Fatal(err)
	}
	inS := <-b.in
	circ.AttachDatagrams(da)
	inS.Stream.AttachDatagrams(inD.Datagram)

	binder := mux.NewBinder()
	accepted := make(chan *session.Established, 1)
	go func() {
		est, err := session.Accept(ctx, &oneListener{c: inS.Stream}, session.AcceptConfig{
			Local: b.n.local(true), Kind: mux.PathRelay, Binder: binder})
		if err != nil {
			t.Error(err)
		}
		accepted <- est
	}()
	aEst, err := session.Dial(ctx, b.n.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier}, a.n.local(false),
		session.WithCarrier(circ), session.WithPathKind(mux.PathRelay))
	if err != nil {
		t.Fatal(err)
	}
	bEst := <-accepted
	t.Cleanup(func() { _ = aEst.Session.Close(); _ = bEst.Session.Close() })

	payload := []byte("sealed datagram payload")
	if err := aEst.Session.SendDatagram(frame.DgramCallAudio, payload); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := bEst.Session.ReceiveDatagram(rctx)
	if err != nil || string(d.Payload) != string(payload) || d.Type != frame.DgramCallAudio {
		t.Fatalf("B got %+v, %v", d, err)
	}
	if w.srv.Stats().Forwarded.Load() == 0 {
		t.Fatal("the datagram did not travel on the datagram circuit")
	}
}

// cidOfLast returns the ID of the most recent circuit R created for the world's stream circuits.
func (s *side) cidOfLast(w *world) uint64 {
	w.srv.mu.Lock()
	defer w.srv.mu.Unlock()
	var latest *circuit
	for _, c := range w.srv.circuits {
		if latest == nil || c.created.After(latest.created) {
			latest = c
		}
	}
	return latest.cid
}
