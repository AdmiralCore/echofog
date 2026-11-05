package relay

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

func dgramPayload(cid uint64, body string) []byte {
	return append(frame.AppendVarint(nil, cid), body...)
}

func recvNow(t *testing.T, d *DatagramCircuit) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	b, err := d.Receive(ctx, nil)
	return string(b), err == nil
}

// M2 (accept 1): the same circuit ID at two relays is two circuits, and a datagram only reaches
// the circuit of the relay it came from.
func TestHubKeysCircuitsByRelayAndID(t *testing.T) {
	h := NewHub()
	r1, r2, r3 := [32]byte{1}, [32]byte{2}, [32]byte{3}
	nop := func(uint64, []byte) error { return nil }
	d1, err := h.newCircuit(r1, 5, nop, nil)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := h.newCircuit(r2, 5, nop, nil)
	if err != nil {
		t.Fatalf("same cid at another relay must be a separate circuit: %v", err)
	}
	h.HandleDatagram(r1, dgramPayload(5, "to-1"))
	h.HandleDatagram(r2, dgramPayload(5, "to-2"))
	if got, ok := recvNow(t, d1); !ok || got != "to-1" {
		t.Fatalf("circuit via R1 got %q, %v", got, ok)
	}
	if got, ok := recvNow(t, d2); !ok || got != "to-2" {
		t.Fatalf("circuit via R2 got %q, %v", got, ok)
	}
	// A third node that is not the relay of the circuit cannot feed it.
	before := h.Dropped()
	h.HandleDatagram(r3, dgramPayload(5, "forged"))
	if got, ok := recvNow(t, d1); ok {
		t.Fatalf("datagram from a foreign session reached the circuit: %q", got)
	}
	if h.Dropped() != before+1 {
		t.Fatal("foreign datagram not counted as dropped")
	}
}

// A second circuit with the same (relay, cid) is refused and does not displace the first.
func TestHubRefusesDuplicateCircuit(t *testing.T) {
	h := NewHub()
	r1 := [32]byte{1}
	nop := func(uint64, []byte) error { return nil }
	d1, err := h.newCircuit(r1, 9, nop, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.newCircuit(r1, 9, nop, nil); err == nil {
		t.Fatal("duplicate (relay, cid) accepted")
	}
	h.HandleDatagram(r1, dgramPayload(9, "still-first"))
	if got, ok := recvNow(t, d1); !ok || got != "still-first" {
		t.Fatalf("first circuit lost its route: %q, %v", got, ok)
	}
	// After the first closes, the ID is free again.
	d1.Close()
	if _, err := h.newCircuit(r1, 9, nop, nil); err != nil {
		t.Fatalf("id not reusable after Close: %v", err)
	}
}
