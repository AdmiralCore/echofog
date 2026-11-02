package routing

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
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

// A pong counts only as the answer to a ping we sent, and the round trip it gives is smoothed.
func TestClientCircuit_RoundTripFromPongs(t *testing.T) {
	c := &ClientCircuit{ID: 1, CreatedAt: time.Now()}
	now := time.Now()
	c.notePong(now)
	if c.RTT() != 0 {
		t.Fatalf("a pong with no ping out must not count, got %v", c.RTT())
	}
	c.pingAt.Store(monoNanos(now.Add(-80 * time.Millisecond)))
	c.notePong(now)
	if c.RTT() != 80*time.Millisecond {
		t.Fatalf("the first answer is the round trip, got %v", c.RTT())
	}
	c.notePong(now.Add(time.Second))
	if c.RTT() != 80*time.Millisecond {
		t.Fatalf("a second pong for the same ping must not count, got %v", c.RTT())
	}
	c.pingAt.Store(monoNanos(now))
	c.notePong(now.Add(40 * time.Millisecond))
	if c.RTT() != 70*time.Millisecond {
		t.Fatalf("later answers are smoothed (80*3+40)/4, got %v", c.RTT())
	}
}

// A ping answered within one tick of the wall clock (on Windows half a millisecond or more: a relay
// on the same machine, a fast LAN) is still a round trip measured, not 0, which reads as "not
// measured yet". pkg/circuitswitch waited for it and failed about every other run on Windows.
func TestClientCircuit_AFastPongIsMeasured(t *testing.T) {
	c := &ClientCircuit{ID: 1, CreatedAt: time.Now()}
	now := time.Now()
	c.pingAt.Store(monoNanos(now))
	c.notePong(now)
	if c.RTT() <= 0 {
		t.Fatalf("an answer at once must count, got %v", c.RTT())
	}
	// the monotonic clock: a real ping and pong a moment apart, however coarse the wall clock
	c2 := &ClientCircuit{ID: 2, CreatedAt: time.Now()}
	sent := time.Now()
	c2.pingAt.Store(monoNanos(sent))
	for time.Since(sent) < 50*time.Microsecond {
	}
	c2.notePong(time.Now())
	if c2.RTT() < 50*time.Microsecond {
		t.Fatalf("a 50 us round trip read %v", c2.RTT())
	}
}

// Another connected peer announcing a circuit "from" Bob cannot take Bob's route when it goes
// quiet, neither by the open nor by sending frames over it, until a fresh envelope signed by
// Bob comes over that circuit. Through the relay that carries the route it may (tested above).
func TestRelayClient_QuietRouteIsNotHijackedThroughAnotherRelay(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	realRelay, attacker := mkID(0x20), mkID(0x66)
	bobPub, bobPriv, _ := ed25519.GenerateKey(nil)
	bob := types.PeerPubKeyToID(types.PeerPublicKey(bobPub))

	const oldCID = 1001
	rc.mu.Lock()
	rc.circuits[oldCID] = &ClientCircuit{ID: oldCID, RelayPeerID: realRelay, RemotePeerID: bob,
		CreatedAt: time.Now().Add(-time.Minute)}
	rc.circuits[oldCID].lastRecv.Store(time.Now().Add(-circuitStaleAfter - time.Second).UnixNano())
	rc.peerToCircuit[bob] = oldCID
	rc.mu.Unlock()

	rc.handleInboundCircuitOpen(inboundOpen(2002, bob), attacker)
	if cid, _ := rc.CircuitTo(bob); cid != oldCID {
		t.Fatalf("an open through another relay took Bob's quiet route: %d", cid)
	}
	frame := func(mt net_types.MessageType, data []byte) []byte {
		b := make([]byte, 5, 5+len(data))
		binary.BigEndian.PutUint32(b[:4], 2002)
		b[4] = byte(mt)
		return append(b, data...)
	}
	rc.handleCircuitData(frame(net_types.TypeHolePunch, nil), attacker)
	if cid, _ := rc.CircuitTo(bob); cid != oldCID {
		t.Fatalf("frames over the attacker's circuit took Bob's quiet route: %d", cid)
	}

	envelope := func(priv ed25519.PrivateKey, ts time.Time) []byte {
		data, _ := proto.Marshal(&internal_pb.MessageData{OriginId: bob[:], Timestamp: uint64(ts.Unix())})
		env, _ := proto.Marshal(&internal_pb.Envelope{PubKey: priv.Public().(ed25519.PublicKey), Data: data,
			Signature: ed25519.Sign(priv, data)})
		return env
	}
	// an old envelope of Bob's, replayed, proves nothing
	rc.handleCircuitData(frame(net_types.TypeChatMessage, envelope(bobPriv, time.Now().Add(-time.Hour))), attacker)
	if cid, _ := rc.CircuitTo(bob); cid != oldCID {
		t.Fatalf("a stale envelope proved the circuit: %d", cid)
	}
	// a fresh one, signed by Bob, does: Bob really moved to this relay
	rc.handleCircuitData(frame(net_types.TypeChatMessage, envelope(bobPriv, time.Now())), attacker)
	if cid, _ := rc.CircuitTo(bob); cid != 2002 {
		t.Fatalf("a circuit proven by Bob should take the quiet route, got %d", cid)
	}
}

// A peer we use as a relay may carry the far end's new circuit onto a quiet route: that is how a
// call follows SwitchCircuit to another relay.
func TestRelayClient_QuietRouteFollowsTheFarEndToAnotherRelay(t *testing.T) {
	rc := NewRelayClient(nil, nil, nil)
	r1, r2, bob := mkID(0x20), mkID(0x21), mkID(0x0b)
	rc.AddPreferred(r2)

	rc.mu.Lock()
	rc.circuits[1] = &ClientCircuit{ID: 1, RelayPeerID: r1, RemotePeerID: bob, CreatedAt: time.Now().Add(-time.Minute)}
	rc.circuits[1].lastRecv.Store(time.Now().Add(-circuitStaleAfter - time.Second).UnixNano())
	rc.peerToCircuit[bob] = 1
	rc.mu.Unlock()

	rc.handleInboundCircuitOpen(inboundOpen(2, bob), r2)
	if cid, _ := rc.CircuitTo(bob); cid != 2 {
		t.Fatalf("the quiet route did not follow bob to the other relay: %d", cid)
	}
}
