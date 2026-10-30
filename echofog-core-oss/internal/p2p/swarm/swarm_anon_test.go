package swarm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Anonymous through the own relay: the pinned relay is the only guard (the others are let go and
// none is taken), every other peer is met at it, the ones on the local network excepted.
func TestOnlyPinnedGuards(t *testing.T) {
	s, _ := newTestSwarm(t)
	withGuardMode(s, "off")
	var cands []guardCandidate
	for i := 0; i < 4; i++ {
		cands = append(cands, guardServer(s, byte(i), byte(10+i)))
	}
	now := time.Now()
	s.planGuards(now, cands, nil)
	require.Len(t, s.Guards(), guardsWanted, "guards taken the usual way first")

	s.SetOnlyPinnedGuards(true)
	assert.True(t, s.GuardModeActive(), "on whatever guard_mode says")
	pin := guardServer(s, 30, 40)
	s.planGuards(now.Add(time.Minute), cands, []guardCandidate{pin})
	gs := s.Guards()
	require.Len(t, gs, 1, "the pinned relay and nothing else")
	assert.Equal(t, pin.id, gs[0].ID)

	other := idOf(7)
	assert.True(t, s.UserLinkViaRelay(other, "203.0.113.5:4242"), "a stranger is met at the relay")
	assert.True(t, s.UserLinkViaRelay(cands[0].id, cands[0].addr), "a server too")
	assert.True(t, s.UserLinkViaRelay(other, ""), "no address known: the relay")
	assert.False(t, s.UserLinkViaRelay(other, "192.168.1.20:4242"), "the local network is reached directly")
	assert.False(t, s.UserLinkViaRelay(pin.id, pin.addr), "the relay itself is linked to")
	assert.True(t, s.acrossBorder("198.51.100.7:4242"), "every public address is across")

	s.SetOnlyPinnedGuards(false)
	assert.False(t, s.GuardModeActive())
}
