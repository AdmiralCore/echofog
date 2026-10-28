package routing

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// inboundOpen builds the [cid][origin:32] notification a relay sends to announce an inbound circuit.
func inboundOpen(cid uint32, origin types.PeerID) []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint32(b[:4], cid)
	copy(b[4:36], origin[:])
	return b
}

// A circuit-open notification that names an empty origin carries no usable identity and must be dropped.
func TestRelayClient_InboundOpen_RejectsEmptyOrigin(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay := mkID(0x10)

	rc.handleInboundCircuitOpen(inboundOpen(7, types.PeerID{}), relay)

	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if len(rc.circuits) != 0 {
		t.Fatalf("an open with an empty origin must not create a circuit, got %d", len(rc.circuits))
	}
}

// A spoofed inbound open naming an already-routed peer must not hijack the existing route: the
// claimed origin is not an authenticated identity, so it can never overwrite where we already send
// that peer's traffic (or its hole-punch signals).
func TestRelayClient_InboundOpen_DoesNotHijackExistingRoute(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	realRelay, attacker := mkID(0x20), mkID(0x66)
	bob := mkID(0x0b)

	// Bob is already reachable over a legitimate circuit through realRelay, which carries frames.
	const legitCID = 1001
	rc.mu.Lock()
	rc.circuits[legitCID] = &ClientCircuit{ID: legitCID, RelayPeerID: realRelay, RemotePeerID: bob, CreatedAt: time.Now()}
	rc.peerToCircuit[bob] = legitCID
	rc.mu.Unlock()

	// The attacker (acting as a relay) announces an inbound circuit claiming to come from Bob.
	rc.handleInboundCircuitOpen(inboundOpen(2002, bob), attacker)

	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if rc.peerToCircuit[bob] != legitCID {
		t.Fatalf("spoofed open hijacked Bob's route: peerToCircuit[bob]=%d, want %d",
			rc.peerToCircuit[bob], legitCID)
	}
}

// One relay must not be able to register more than the per-relay cap of circuits against us.
func TestRelayClient_InboundOpen_PerRelayCap(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay := mkID(0x30)

	for i := 0; i < maxCircuitsPerRelay+10; i++ {
		origin := mkID(byte(0x40 + i))
		rc.handleInboundCircuitOpen(inboundOpen(uint32(5000+i), origin), relay)
	}

	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if n := rc.circuitsForRelayLocked(relay); n != maxCircuitsPerRelay {
		t.Fatalf("per-relay cap not enforced: got %d circuits, want %d", n, maxCircuitsPerRelay)
	}
}

// Circuit data frames are only accepted from the relay that carries the circuit; a frame from any
// other peer is dropped and never reaches a listener.
func TestRelayClient_CircuitData_DropsFramesFromNonRelay(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay, attacker, bob := mkID(0x50), mkID(0x66), mkID(0x0b)

	const cid = 9
	rc.mu.Lock()
	rc.circuits[cid] = &ClientCircuit{ID: cid, RelayPeerID: relay, RemotePeerID: bob}
	rc.mu.Unlock()

	var got int
	rc.AddDataListener(func(_ uint32, _ net_types.MessageType, _ []byte, _ types.PeerID) { got++ })

	// A non-HolePunch, non-chat inner type keeps the swarm out of the picture for this unit test.
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[:4], cid)
	frame[4] = byte(net_types.TypeHolePunch)

	rc.handleCircuitData(frame, attacker) // wrong sender: must be ignored
	if got != 0 {
		t.Fatalf("circuit data from a non-relay peer must be dropped, listener fired %d times", got)
	}

	rc.handleCircuitData(frame, relay) // the real relay: delivered
	if got != 1 {
		t.Fatalf("circuit data from the circuit's relay must be delivered, listener fired %d times", got)
	}
}

// A circuit-open response is only honoured for an open we actually initiated through that exact
// relay: a response from a different relay (or for a target we never requested) is ignored.
func TestRelayClient_OpenResponse_OnlyFromRequestedRelay(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	realRelay, otherRelay, bob := mkID(0x70), mkID(0x71), mkID(0x0b)

	ch := make(chan circuitOpenResult, 1)
	rc.mu.Lock()
	rc.pendingOpens[pendingKey{relay: realRelay, target: bob}] = ch
	rc.mu.Unlock()

	// A success response from the wrong relay, naming Bob, must not bind a circuit or satisfy the open.
	resp := make([]byte, 37)
	binary.BigEndian.PutUint32(resp[:4], 1234)
	resp[4] = 1 // success
	copy(resp[5:37], bob[:])

	rc.handleCircuitOpenResponse(resp, otherRelay)

	rc.mu.RLock()
	_, bound := rc.peerToCircuit[bob]
	rc.mu.RUnlock()
	if bound {
		t.Fatal("a response from a relay we did not ask must not bind a circuit")
	}
	select {
	case <-ch:
		t.Fatal("a response from the wrong relay must not satisfy the pending open")
	default:
	}

	// The same response from the relay we actually asked binds the circuit and satisfies the open.
	rc.handleCircuitOpenResponse(resp, realRelay)
	rc.mu.RLock()
	gotCID, bound := rc.peerToCircuit[bob]
	rc.mu.RUnlock()
	if !bound || gotCID != 1234 {
		t.Fatalf("a response from the requested relay must bind the circuit, got cid=%d bound=%v", gotCID, bound)
	}
}

// A route that has gone quiet gives way to a new inbound circuit from the same peer: the peer
// opening a new circuit means the old one died on its side.
func TestRelayClient_InboundOpen_ReplacesQuietRoute(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay, bob := mkID(0x20), mkID(0x0b)

	const oldCID = 1001
	rc.mu.Lock()
	rc.circuits[oldCID] = &ClientCircuit{ID: oldCID, RelayPeerID: relay, RemotePeerID: bob,
		CreatedAt: time.Now().Add(-time.Minute)}
	rc.circuits[oldCID].lastRecv.Store(time.Now().Add(-circuitStaleAfter - time.Second).UnixNano())
	rc.peerToCircuit[bob] = oldCID
	rc.mu.Unlock()

	rc.handleInboundCircuitOpen(inboundOpen(2002, bob), relay)

	if cid, ok := rc.CircuitTo(bob); !ok || cid != 2002 {
		t.Fatalf("a quiet route must give way to the new circuit, got cid=%d ok=%v", cid, ok)
	}
}

// Frames arriving over another circuit from the peer move a quiet route to that circuit; they never
// take over a route that still carries frames.
func TestRelayClient_CircuitData_FollowsTheLiveCircuit(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay, bob := mkID(0x50), mkID(0x0b)

	rc.mu.Lock()
	rc.circuits[1] = &ClientCircuit{ID: 1, RelayPeerID: relay, RemotePeerID: bob, CreatedAt: time.Now()}
	rc.circuits[2] = &ClientCircuit{ID: 2, RelayPeerID: relay, RemotePeerID: bob, CreatedAt: time.Now()}
	rc.peerToCircuit[bob] = 1
	rc.mu.Unlock()

	frame := func(cid uint32) []byte {
		b := make([]byte, 5)
		binary.BigEndian.PutUint32(b[:4], cid)
		b[4] = byte(net_types.TypeHolePunch)
		return b
	}
	rc.handleCircuitData(frame(2), relay)
	if cid, _ := rc.CircuitTo(bob); cid != 1 {
		t.Fatalf("a live route must stay, got %d", cid)
	}

	rc.circuits[1].lastRecv.Store(time.Now().Add(-circuitStaleAfter - time.Second).UnixNano())
	rc.handleCircuitData(frame(2), relay)
	if cid, _ := rc.CircuitTo(bob); cid != 2 {
		t.Fatalf("a quiet route must follow the circuit that carries frames, got %d", cid)
	}
}

// A circuit probed for circuitDeadAfter without an answer is dropped, so the next send opens a new
// one instead of sending into the void; an answer clears the probe.
func TestRelayClient_DeadCircuitIsDropped(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay, bob := mkID(0x50), mkID(0x0b)

	c := &ClientCircuit{ID: 7, RelayPeerID: relay, RemotePeerID: bob, CreatedAt: time.Now().Add(-time.Minute)}
	rc.mu.Lock()
	rc.circuits[7] = c
	rc.peerToCircuit[bob] = 7
	rc.mu.Unlock()

	c.probeSince.Store(time.Now().Add(-circuitDeadAfter / 2).UnixNano())
	if _, ok := rc.CircuitTo(bob); !ok {
		t.Fatal("a circuit probed for a short while is still used")
	}
	pong := make([]byte, 5)
	binary.BigEndian.PutUint32(pong[:4], 7)
	pong[4] = byte(net_types.TypePong)
	rc.handleCircuitData(pong, relay)
	if c.probeSince.Load() != 0 {
		t.Fatal("an answer must clear the probe")
	}

	c.probeSince.Store(time.Now().Add(-circuitDeadAfter - time.Second).UnixNano())
	if _, ok := rc.CircuitTo(bob); ok {
		t.Fatal("a circuit that stopped answering must not be used")
	}
	rc.mu.RLock()
	_, kept := rc.circuits[7]
	rc.mu.RUnlock()
	if kept {
		t.Fatal("a dead circuit must be dropped")
	}
}

// The circuits through a relay that disconnected are forgotten; others stay.
func TestRelayClient_RelayGoneDropsItsCircuits(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	gone, other, bob, carol := mkID(0x50), mkID(0x51), mkID(0x0b), mkID(0x0c)

	rc.mu.Lock()
	rc.circuits[1] = &ClientCircuit{ID: 1, RelayPeerID: gone, RemotePeerID: bob, CreatedAt: time.Now()}
	rc.circuits[2] = &ClientCircuit{ID: 2, RelayPeerID: other, RemotePeerID: carol, CreatedAt: time.Now()}
	rc.peerToCircuit[bob], rc.peerToCircuit[carol] = 1, 2
	rc.mu.Unlock()

	rc.OnPeerDisconnected(gone)

	if _, ok := rc.CircuitTo(bob); ok {
		t.Fatal("the route through a relay that went away must be forgotten")
	}
	if _, ok := rc.CircuitTo(carol); !ok {
		t.Fatal("routes through other relays must stay")
	}
}

// A frame that overtakes its circuit's open (the relay forwards them on different streams) is kept
// for a moment and delivered once the open arrives; a frame from another relay, or one older than
// earlyFrameTTL, is not.
func TestRelayClient_CircuitData_BeforeTheOpenIsDelivered(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	relay, other, alice := mkID(0x51), mkID(0x66), mkID(0x0a)

	var got int
	rc.AddDataListener(func(_ uint32, mt net_types.MessageType, _ []byte, from types.PeerID) {
		if mt == net_types.TypeHolePunch && from == alice {
			got++
		}
	})
	frame := func(cid uint32) []byte {
		b := make([]byte, 5)
		binary.BigEndian.PutUint32(b[:4], cid)
		b[4] = byte(net_types.TypeHolePunch)
		return b
	}

	rc.handleCircuitData(frame(21), relay)
	rc.handleCircuitData(frame(21), other) // not the relay that opens it
	if got != 0 {
		t.Fatalf("delivered before the open: %d", got)
	}
	rc.handleInboundCircuitOpen(inboundOpen(21, alice), relay)
	if got != 1 {
		t.Fatalf("the early frame should arrive once, got %d", got)
	}

	rc.mu.Lock()
	rc.keepEarly(22, relay, frame(22), time.Now().Add(-2*earlyFrameTTL))
	rc.mu.Unlock()
	rc.handleInboundCircuitOpen(inboundOpen(22, alice), relay)
	if got != 1 {
		t.Fatalf("a stale early frame was delivered")
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if len(rc.early) != 1 || rc.early[0].relay != other {
		t.Fatalf("only the other relay's frame should wait (until it expires): %+v", rc.early)
	}
}
