package routing

import (
	"encoding/binary"
	"testing"
	"time"
)

// A single peer must not be able to hold more than the per-peer cap of circuits, even while the
// node-wide MaxCircuits is nowhere near full.
func TestRelayServer_PerPeerCircuitCap(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil) // default policy: enabled, public, MaxCircuits=200
	attacker := mkID(0x66)

	// The attacker already holds its full share of circuits.
	rs.circuitsMu.Lock()
	for i := 0; i < defaultCircuitsPerPeer; i++ {
		rs.circuits[uint32(1000+i)] = &Circuit{ID: uint32(1000 + i), PeerA: attacker, PeerB: mkID(byte(i))}
	}
	rs.circuitsMu.Unlock()

	// A fresh open request from the same peer must be refused without creating another circuit.
	target := mkID(0x0b)
	rs.handleCircuitOpenReq(target[:], attacker)

	rs.circuitsMu.RLock()
	defer rs.circuitsMu.RUnlock()
	if n := rs.circuitsForPeerLocked(attacker); n != defaultCircuitsPerPeer {
		t.Fatalf("per-peer cap not enforced: peer holds %d circuits, want %d", n, defaultCircuitsPerPeer)
	}
	if rs.limitRefused.Load() != 1 {
		t.Fatalf("the refusal must be counted, got %d", rs.limitRefused.Load())
	}
}

// Idle circuits (no frames for longer than the timeout) are reaped; active ones are kept.
func TestRelayServer_ReapIdleCircuits(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)

	fresh := &Circuit{ID: 1, PeerA: mkID(1), PeerB: mkID(2), CreatedAt: time.Now()}
	fresh.LastActive.Store(time.Now().UnixNano())

	idle := &Circuit{ID: 2, PeerA: mkID(3), PeerB: mkID(4), CreatedAt: time.Now().Add(-time.Hour)}
	idle.LastActive.Store(time.Now().Add(-time.Hour).UnixNano())

	// LastActive never written: the CreatedAt fallback decides, and this one is old.
	staleCreated := &Circuit{ID: 3, PeerA: mkID(5), PeerB: mkID(6), CreatedAt: time.Now().Add(-time.Hour)}

	rs.circuitsMu.Lock()
	rs.circuits[1] = fresh
	rs.circuits[2] = idle
	rs.circuits[3] = staleCreated
	rs.circuitsMu.Unlock()

	reaped := rs.reapIdleCircuits(defaultCircuitIdle, defaultCircuitIdleKB<<10)
	if reaped != 2 {
		t.Fatalf("expected 2 idle circuits reaped, got %d", reaped)
	}

	rs.circuitsMu.RLock()
	defer rs.circuitsMu.RUnlock()
	if _, ok := rs.circuits[1]; !ok {
		t.Fatal("the active circuit must be kept")
	}
	if _, ok := rs.circuits[2]; ok {
		t.Fatal("the idle circuit must be reaped")
	}
	if _, ok := rs.circuits[3]; ok {
		t.Fatal("the long-untouched circuit must be reaped")
	}
}

// A circuit-close from a peer that is not a participant of the circuit must be ignored.
func TestRelayServer_CircuitClose_IgnoresNonParticipant(t *testing.T) {
	rs := NewRelayServer(nil, nil, nil)
	alice, bob, stranger := mkID(0xa1), mkID(0xb2), mkID(0x66)

	const cid = 42
	rs.circuitsMu.Lock()
	rs.circuits[cid] = &Circuit{ID: cid, PeerA: alice, PeerB: bob}
	rs.circuitsMu.Unlock()

	closePkt := make([]byte, 4)
	binary.BigEndian.PutUint32(closePkt, cid)
	rs.handleCircuitClose(closePkt, stranger)

	rs.circuitsMu.RLock()
	defer rs.circuitsMu.RUnlock()
	if _, ok := rs.circuits[cid]; !ok {
		t.Fatal("a close from a non-participant must not tear down the circuit")
	}
}
