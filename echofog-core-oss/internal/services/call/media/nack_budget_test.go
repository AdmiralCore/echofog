package media

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A NACK of about a kilobyte used to pull every fragment of up to 64 units out of the ring, and
// the same units again for every NACK that followed: a child could turn its parent into a
// megabyte-a-second firehose at whoever it liked to be fed. One NACK now resends at most
// nackMaxBytes, the same unit once, and a child gets at most nackBurst plus nackRate a second.
func TestServeNack_ResendIsBounded(t *testing.T) {
	bcast, relay, child := pid(1), pid(2), pid(3)
	engine := tree.NewStreamTreeEngine()
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 1, Subscribers: []types.PeerID{relay, child},
		ParentMap:        map[types.PeerID]types.PeerID{relay: bcast, child: relay},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {relay}, relay: {child}},
	})
	d := NewMediaDispatcher(relay, &mockSender{}, nil, engine)
	defer d.Close()

	const fragSize = 1200
	now := time.Now()
	for seq := uint32(1); seq <= MaxNackEntries; seq++ {
		for i := uint8(0); i < MaxFragments; i++ {
			d.ring.put("s1", seq, i, MaxFragments, make([]byte, fragSize), now)
		}
	}

	same := make([]NackEntry, MaxNackEntries)
	for i := range same {
		same[i] = NackEntry{Seq: 7}
	}
	d.ServeNack(child, "s1", same)
	assert.Equal(t, uint64(MaxFragments), d.Stats().Retransmitted, "a unit named many times is resent once")

	all := make([]NackEntry, MaxNackEntries)
	for i := range all {
		all[i] = NackEntry{Seq: uint32(i + 1)}
	}
	before := d.Stats().Retransmitted
	d.ServeNack(child, "s1", all)
	one := d.Stats().Retransmitted - before
	assert.LessOrEqual(t, one*fragSize, uint64(nackMaxBytes), "one NACK resends at most nackMaxBytes")
	assert.Positive(t, one)

	for range 50 {
		d.ServeNack(child, "s1", all)
	}
	total := d.Stats().Retransmitted * fragSize
	assert.LessOrEqual(t, total, uint64(nackBurst+nackRate), "a child gets at most a burst and a second's worth at once")

	// another child has a budget of its own
	other := pid(4)
	engine.ApplyManifest(&tree.StreamManifest{
		StreamID: "s1", Broadcaster: bcast, Epoch: 2, Subscribers: []types.PeerID{relay, child, other},
		ParentMap:        map[types.PeerID]types.PeerID{relay: bcast, child: relay, other: relay},
		DistributionTree: map[types.PeerID][]types.PeerID{bcast: {relay}, relay: {child, other}},
	})
	before = d.Stats().Retransmitted
	d.ServeNack(other, "s1", []NackEntry{{Seq: 3}})
	require.Equal(t, uint64(MaxFragments), d.Stats().Retransmitted-before)
}
