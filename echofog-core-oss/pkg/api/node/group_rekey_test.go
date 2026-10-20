package node

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func epochsAt(n *meshNode, group string) int {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	return len(n.f.keyring(group).Epochs)
}

func rejoin(t *testing.T, m *mesh, owner, n *meshNode, group string) {
	t.Helper()
	invite, err := owner.f.Invite(group)
	require.NoError(t, err)
	_, err = n.f.Join(invite)
	require.NoError(t, err)
	m.settle()
}

// somebody leaving and joining in a loop: the first departure in a quiet group replaces the epoch
// at once; the rest wait for the gap, and since they keep coming back, no key is minted for them
func TestLeaveJoinLoopMintsNoKeys(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 4)
	a, c := ns[0], ns[2]
	m.tick(time.Hour) // a quiet group

	require.NoError(t, c.f.Leave(g))
	m.settle()
	assert.Equal(t, 2, epochsAt(a, g), "the first departure in a quiet group: a new epoch at once")
	rejoin(t, m, a, c, g)

	snaps := m.kind("snap")
	for i := 0; i < 10; i++ {
		require.NoError(t, c.f.Leave(g))
		m.settle()
		rejoin(t, m, a, c, g)
	}
	assert.Equal(t, 2, epochsAt(a, g), "ten departures within the gap mint nothing yet")
	assert.LessOrEqual(t, m.kind("snap")-snaps, 0, "the owner does not answer the same newcomer twice a minute")

	m.tick(rekeyGapMax) // the waiting replacement comes due: c is back, so none is made
	assert.Equal(t, 2, epochsAt(a, g), "no key for somebody who is back")

	// c can still read and write: it got the epoch every time it came back
	c.say(t, g, "still here")
	assert.Contains(t, a.texts(t, g), "still here")

	// and when c finally stays away, one new epoch follows
	require.NoError(t, c.f.Leave(g))
	m.settle()
	m.tick(rekeyGapMax)
	assert.Equal(t, 3, epochsAt(a, g))
	assert.False(t, a.keyring(g).Current().Has(c.id.String()))
}

// an admin kicking somebody who keeps coming back (an open invite) mints no key per kick
func TestKickAndComeBackMintsNoKey(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 4)
	a, d := ns[0], ns[3]
	m.tick(time.Hour)
	require.NoError(t, a.f.Remove(g, d.id.String(), false))
	m.settle()
	assert.Equal(t, 2, epochsAt(a, g), "the first kick in a quiet group: a new epoch at once")
	for i := 0; i < 5; i++ {
		rejoin(t, m, a, d, g)
		require.NoError(t, a.f.Remove(g, d.id.String(), false))
		m.settle()
	}
	rejoin(t, m, a, d, g) // back once more before the gap is over
	m.tick(rekeyGapMax)
	assert.Equal(t, 2, epochsAt(a, g), "kicks of somebody who is back make no key")
}

// several people leaving within the gap are covered by one new epoch
func TestDeparturesWithinTheGapShareOneEpoch(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 6)
	a := ns[0]
	m.tick(time.Hour)
	require.NoError(t, ns[1].f.Leave(g))
	m.settle()
	require.Equal(t, 2, epochsAt(a, g))
	for _, n := range ns[2:5] {
		require.NoError(t, n.f.Leave(g))
		m.settle()
	}
	assert.Equal(t, 2, epochsAt(a, g), "the next three wait for the gap")
	m.tick(rekeyGap)
	assert.Equal(t, 3, epochsAt(a, g), "and are covered by one epoch")
	cur := a.keyring(g).Current()
	for _, n := range ns[1:5] {
		assert.False(t, cur.Has(n.id.String()))
	}
	for _, n := range []*meshNode{a, ns[5]} {
		assert.Equal(t, cur.ID(), n.current(g))
	}
}

// with more than rekeyBurst epochs in an hour the gap doubles, up to rekeyGapMax
func TestRekeyGapGrowsWithChurn(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 2)
	a := ns[0]
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	assert.Equal(t, rekeyGap, a.f.rekeyGapFor(g))
	for i := 0; i < 4; i++ { // 5 epochs in the last hour
		_, err := a.f.newEpoch(g)
		require.NoError(t, err)
	}
	assert.Equal(t, 4*rekeyGap, a.f.rekeyGapFor(g))
	for i := 0; i < 20; i++ {
		_, _ = a.f.newEpoch(g)
	}
	assert.Equal(t, rekeyGapMax, a.f.rekeyGapFor(g))
	// an hour later they no longer count
	m.mu.Lock()
	m.clock = m.clock.Add(2 * time.Hour)
	m.mu.Unlock()
	assert.Equal(t, rekeyGap, a.f.rekeyGapFor(g))
}

// a replacement that waits survives being asked for again, and the real node arms a timer for it
func TestWaitingReplacementRunsOnItsTimer(t *testing.T) {
	m := newMesh()
	g, ns := newGroupOf(t, m, 3)
	a, c := ns[0], ns[2]
	a.f.now = nil // real time and a real timer for this one
	a.f.async = true
	require.NoError(t, c.f.Leave(g))
	m.settle()
	a.f.mu.Lock()
	p := a.f.rekeys[g]
	require.NotNil(t, p, "the group is fresh: the replacement waits")
	assert.NotNil(t, p.timer)
	p.timer.Stop()
	p.due = time.Now().Add(-time.Second) // as if the gap were over
	a.f.mu.Unlock()
	a.f.SyncGroups() // what the periodic sync does with a replacement that is due
	m.settle()
	assert.Equal(t, 2, epochsAt(a, g))
	a.f.mu.Lock()
	assert.Empty(t, a.f.rekeys)
	a.f.mu.Unlock()
}
