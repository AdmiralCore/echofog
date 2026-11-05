package relay

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// RegisterIDs is the B side of REGISTER: the IDs land at R and a contact's rendezvous CONNECT finds B.
func TestRegisterIDsClient(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk})
	a := w.join(newTNode(t), "10.0.0.1", true)
	b := w.join(newTNode(t), "10.0.0.2", false)
	n, err := RegisterIDs(w.ctx, b.open, [][32]byte{{1}, {2}}, testDay)
	if err != nil || n != 2 {
		t.Fatalf("Register: count %d, %v", n, err)
	}
	n, err = RegisterIDs(w.ctx, b.open, [][32]byte{{3}}, testDay+1)
	if err != nil || n != 3 {
		t.Fatalf("Register next day: count %d, %v", n, err)
	}
	if err := a.dialRID(append([]byte{1}, make([]byte, 31)...)); err != nil {
		t.Fatalf("rendezvous dial after Register: %v", err)
	}
}

func TestRegisterIDsRefusals(t *testing.T) {
	clk := newFakeClock(dayStart(testDay))
	w := newWorld(t, ServerConfig{Clock: clk})
	b := w.join(newTNode(t), "10.0.0.2", false)
	if _, err := RegisterIDs(w.ctx, b.open, [][32]byte{{1}}, testDay+5); err == nil {
		t.Fatal("a day outside today/tomorrow was accepted")
	}
	if _, err := RegisterIDs(w.ctx, b.open, nil, testDay); err == nil {
		t.Fatal("an empty REGISTER is not worth a round trip")
	}
	many := make([][32]byte, MaxRegisterIDs+1)
	if _, err := RegisterIDs(w.ctx, b.open, many, testDay); err == nil {
		t.Fatal("more IDs than one REGISTER carries")
	}
}

// A node that does not relay for others (metered, phone) still takes INCOMING from its own relay:
// the Incoming hook gets the already-read first frame.
func TestEndpointTakesIncoming(t *testing.T) {
	w := newWorld(t, ServerConfig{Delay: func() time.Duration { return 20 * time.Millisecond }})
	a := w.join(newTNode(t), "10.0.0.1", true)
	b := w.join(newTNode(t), "10.0.0.2", true)
	got := make(chan Incoming, 1)
	ep := NewServer(ServerConfig{Refuse: true, Incoming: func(_ context.Context, _ Peer, st *mux.Stream, first frame.Frame) {
		in, err := ParseIncoming(first.Payload)
		if err != nil {
			t.Errorf("INCOMING: %v", err)
		}
		got <- in
		st.Reset(frame.CodeRefused)
	}})
	w.raw[b.node] = func(st *mux.Stream) { ep.Handle(w.ctx, Peer{}, st) }
	go func() { _, _ = a.dial(b) }()
	select {
	case in := <-got:
		if in.CID == 0 || in.Mode != ModeStream {
			t.Fatalf("incoming %+v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Incoming hook not called")
	}
}

// A refusing endpoint never relays: CONNECT and REGISTER end with an error, not with a circuit.
func TestRefusingServerDoesNotRelay(t *testing.T) {
	w := newWorld(t, ServerConfig{Refuse: true, Delay: func() time.Duration { return 20 * time.Millisecond }})
	a := w.join(newTNode(t), "10.0.0.1", true)
	b := w.join(newTNode(t), "10.0.0.2", true)
	if c, err := a.dial(b); err == nil {
		_ = c.Close()
		t.Fatal("a refusing server built a circuit")
	}
	if _, err := RegisterIDs(w.ctx, b.open, [][32]byte{{1}}, uint32(time.Now().Unix()/86400)); err == nil {
		t.Fatal("a refusing server took a registration")
	}
}
