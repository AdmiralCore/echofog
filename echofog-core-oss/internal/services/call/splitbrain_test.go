package call

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// blockLink drops what goes from one peer to another while on() is true, of the frame types
// what says (nil: everything).
func (m *mesh) blockLink(from, to types.PeerID, what func(net_types.MessageType) bool, on func() bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.block
	m.block = func(f, t types.PeerID, mt net_types.MessageType) bool {
		if prev != nil && prev(f, t, mt) {
			return true
		}
		return f == from && t == to && on() && (what == nil || what(mt))
	}
}

// reliableSignals is a stalled stream: signals sent by stream do not arrive, datagrams still do.
func reliableSignals(mt net_types.MessageType) bool { return mt == net_types.TypeCallSignaling }

// heardAgain checks that both members hear each other's voice once the room is whole again.
func heardAgain(t *testing.T, a, b *testNode, seq uint32) {
	t.Helper()
	unit := make([]byte, 8)
	binary.BigEndian.PutUint32(unit, seq)
	require.Eventually(t, func() bool {
		_ = a.cm.dispatcher().BroadcastLocalAudio(unit)
		_ = b.cm.dispatcher().BroadcastLocalAudio(unit)
		return b.log.has(media.FrameAudio, a.id, "voice", seq) && a.log.has(media.FrameAudio, b.id, "voice", seq)
	}, 10*time.Second, 100*time.Millisecond, "the members do not hear each other again")
}

func (l *mediaLog) has(kind media.FrameKind, b types.PeerID, stream string, unit uint32) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.units[fmt.Sprintf("%d/%s/%s", kind, b, stream)][unit]
}

// A 1:1 call over two mobile links: the coordinator's signals stopped reaching the member for a
// while (its stream stalled) as the sound still flowed by datagram. The member took over, the old
// coordinator accepted it, heard nothing from the new one (which had dropped it) and took over
// again two seconds later: two coordinators, each alone, for good. Heartbeats go by datagram too
// now, so the stall splits nothing, and the call is one room with both in it throughout.
func TestRoom_StalledSignalsDoNotSplitTheCall(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 2)
	c, p := nodes[0], nodes[1]
	var stalled atomic.Bool
	stalled.Store(true)
	m.blockLink(c.id, p.id, reliableSignals, stalled.Load)
	until := time.Now().Add(20 * time.Second)
	for time.Now().Before(until) {
		s, _ := p.cm.Snapshot()
		require.Equal(t, c.id, s.Coordinator, "the member took over while the coordinator was alive")
		time.Sleep(200 * time.Millisecond)
	}
	require.True(t, roomConverged(nodes, nil), "the call split during the stall: %v", dumpRoom(nodes, nil))
	heardAgain(t, c, p, 1000)
	stalled.Store(false)
	time.Sleep(time.Second)
	require.True(t, roomConverged(nodes, nil), "%v", dumpRoom(nodes, nil))
}

// If the member takes over anyway (it heard nothing at all for a while), the old coordinator,
// which hears it, joins it at once and waits for it longer than a coordinator's silence: it must
// not take the call back alone two seconds later.
func TestRoom_OldCoordinatorJoinsTheOneThatTookOver(t *testing.T) {
	m := newMesh()
	nodes, _ := newRoom(t, m, 2)
	c, p := nodes[0], nodes[1]
	var cut atomic.Bool
	cut.Store(true)
	m.blockLink(c.id, p.id, nil, cut.Load) // nothing from the coordinator reaches the member
	require.Eventually(t, func() bool {
		s, _ := p.cm.Snapshot()
		return s.Coordinator == p.id
	}, 10*time.Second, 50*time.Millisecond, "the member never took over")
	require.Eventually(t, func() bool {
		s, _ := c.cm.Snapshot()
		return s.Coordinator == p.id
	}, 5*time.Second, 50*time.Millisecond, "the old coordinator never heard of the takeover")
	time.Sleep(4 * time.Second) // the old coordinator used to take the call back alone here
	s, _ := c.cm.Snapshot()
	require.Equal(t, p.id, s.Coordinator, "the old coordinator took the call back alone")
	cut.Store(false)
	require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 10*time.Second, 100*time.Millisecond,
		"the call stayed split: %v", dumpRoom(nodes, nil))
	heardAgain(t, c, p, 1500)
}

// The link between the two goes dark both ways for a while (a tunnel, a cell handover), then comes
// back: each has taken the call over alone by then, and the two lone coordinators must become one
// room again, the same one on both sides.
func TestRoom_BlackoutThenRecoveryMergesTheCall(t *testing.T) {
	for _, dark := range []time.Duration{12 * time.Second, 30 * time.Second} {
		m := newMesh()
		nodes, _ := newRoom(t, m, 2)
		c, p := nodes[0], nodes[1]
		var out atomic.Bool
		out.Store(true)
		m.blockLink(c.id, p.id, nil, out.Load)
		m.blockLink(p.id, c.id, nil, out.Load)
		time.Sleep(dark)
		for _, nd := range nodes {
			s, _ := nd.cm.Snapshot()
			require.Equal(t, nd.id, s.Coordinator, "both lead alone during the blackout")
		}
		out.Store(false)
		require.Eventually(t, func() bool { return roomConverged(nodes, nil) }, 15*time.Second, 100*time.Millisecond,
			"the call stayed split after a %s blackout: %v", dark, dumpRoom(nodes, nil))
		heardAgain(t, c, p, 2000)
	}
}
