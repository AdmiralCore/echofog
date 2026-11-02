package node

import (
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A keep-alive of a group call does not go to every member: the others in the call hear it, the
// members we have a link to hear it, and only the speaker of the call tells the rest, a bounded
// number a round. Before, every member in the call sent it to every member, offline ones into
// mailboxes, every 45 s.
func TestGroupsFacade_CallKeepAliveFanOutIsBounded(t *testing.T) {
	h := newHub()
	a := h.node(t, newMemStore())
	g, _ := a.f.Create("Crew", "", "group")
	var rest []*testNode
	for i := 0; i < 8; i++ {
		n := h.node(t, newMemStore())
		invite, _ := a.f.Invite(g.ID)
		_, err := n.f.Join(invite)
		require.NoError(t, err)
		rest = append(rest, n)
	}
	b, linked := rest[0], rest[1:4]

	call, err := a.f.StartCall(g.ID, "")
	require.NoError(t, err)
	b.f.handle(a.id, encodeWire(groupWire{Kind: "callalive", Group: g.ID, CallID: call.CallID, Coord: call.Coordinator, TS: time.Now().Unix()}))
	a.f.handle(b.id, encodeWire(groupWire{Kind: "callalive", Group: g.ID, CallID: call.CallID, Coord: call.Coordinator, TS: time.Now().Unix()}))

	onlyLinked := func(ids ...*testNode) func(types.PeerID) bool {
		return func(p types.PeerID) bool {
			for _, n := range ids {
				if n.id == p {
					return true
				}
			}
			return false
		}
	}
	count := func(n *testNode) map[types.PeerID]int {
		sent := map[types.PeerID]int{}
		send := n.f.sendSilent
		n.f.sendSilent = func(to types.PeerID, text string) (bool, error) { sent[to]++; return send(to, text) }
		return sent
	}
	a.f.online = onlyLinked(linked...)
	b.f.online = onlyLinked()
	fromA, fromB := count(a), count(b)

	require.NoError(t, a.f.KeepCallAlive(g.ID, "", call.CallID, call.Coordinator))
	require.NoError(t, b.f.KeepCallAlive(g.ID, "", call.CallID, call.Coordinator))

	// a, the coordinator, is the speaker: in a group this small it tells everybody
	assert.Len(t, fromA, 8)
	// b: only a, the other one in the call; it has no links, and the speaker tells the rest
	assert.Equal(t, map[types.PeerID]int{a.id: 1}, fromB)

	// past the caps, the speaker tells the others in the call, some of its links and some of
	// the rest (others in the next rounds)
	defer func(l, r int) { maxAliveLinked, maxAliveRelayed = l, r }(maxAliveLinked, maxAliveRelayed)
	maxAliveLinked, maxAliveRelayed = 2, 2
	fromA = count(a)
	a.f.aliveSent = map[string]int64{} // as if the last round were long ago
	require.NoError(t, a.f.KeepCallAlive(g.ID, "", call.CallID, call.Coordinator))
	assert.Equal(t, 1, fromA[b.id], "the other member of the call must hear it")
	assert.Len(t, fromA, 1+2+2)
}
