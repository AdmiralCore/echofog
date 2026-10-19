package filexfer

import (
	"bytes"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupOf starts a sender and n members on one network.
func groupOf(t *testing.T, n int, tune func(*Options)) (*fakeNet, *side, []*side) {
	net := &fakeNet{nodes: map[types.PeerID]*Service{}}
	sender := newSide(t, net, peerID(1), tune)
	var members []*side
	for i := 0; i < n; i++ {
		members = append(members, newSide(t, net, peerID(byte(10+i)), tune))
	}
	return net, sender, members
}

func offerToGroup(t *testing.T, from *side, group string, to []*side, path string) Offer {
	ids := []types.PeerID{from.id} // the sender is a member too: it must not count itself
	for _, m := range to {
		ids = append(ids, m.id)
	}
	o, err := from.svc.PrepareGroup(group, ids, path)
	require.NoError(t, err)
	assert.Equal(t, group, o.Group)
	for _, m := range to {
		_, err := m.svc.OnOffer(from.id, o)
		require.NoError(t, err)
	}
	return o
}

// One offer, one key, several members: each pulls the file on its own and the sender counts who
// has it. A member who declines does not stop the others.
func TestGroupFileToSeveralMembers(t *testing.T) {
	net, alice, m := groupOf(t, 3, nil)
	net.lossPct = 5
	src, data := randomFile(t, alice.dir, "slides.pdf", 120*ChunkSize+77)
	o := offerToGroup(t, alice, "g1", m, src)

	out, _ := alice.svc.Get(o.ID)
	assert.Equal(t, "g1", out.Group)
	assert.Equal(t, 3, out.Members)
	assert.Empty(t, out.Peer)
	in, _ := m[0].svc.Get(o.ID)
	assert.Equal(t, "g1", in.Group)
	assert.Equal(t, alice.id.String(), in.Peer)

	require.NoError(t, m[2].svc.Decline(o.ID))
	for _, r := range m[:2] {
		_, err := r.svc.Accept(o.ID)
		require.NoError(t, err)
	}
	for _, r := range m[:2] {
		done := waitState(t, r.svc, o.ID, StateDone, 30*time.Second)
		got, err := os.ReadFile(done.Path)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(data, got), "the received file differs")
	}
	net.lossPct = 0
	require.Eventually(t, func() bool { i, _ := alice.svc.Get(o.ID); return i.Delivered == 2 && i.Fetching == 0 }, 10*time.Second, 10*time.Millisecond)
	out, _ = alice.svc.Get(o.ID)
	assert.Equal(t, StateActive, out.State, "a group offer stays open for the members who have not taken it yet")
	assert.Equal(t, out.Size, out.Done)

	// the one who declined changes its mind... it cannot: a decline is final on its side
	_, err := m[2].svc.Accept(o.ID)
	assert.Error(t, err)

	// the delivered count outlives a restart of the sender
	alice.svc.Stop()
	alice.start(t, net, nil)
	out, _ = alice.svc.Get(o.ID)
	assert.Equal(t, 2, out.Delivered)
	assert.Equal(t, 3, out.Members)
}

// Members fetch at the same time from one open file; one member cancelling midway leaves the
// others going.
func TestGroupMemberCancelLeavesTheOthers(t *testing.T) {
	_, alice, m := groupOf(t, 2, nil)
	src, data := randomFile(t, alice.dir, "big.bin", 400*ChunkSize)
	o := offerToGroup(t, alice, "g1", m, src)
	for _, r := range m {
		_, err := r.svc.Accept(o.ID)
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { i, _ := m[0].svc.Get(o.ID); return i.Done > 0 }, 10*time.Second, time.Millisecond)
	require.NoError(t, m[0].svc.Cancel(o.ID))
	done := waitState(t, m[1].svc, o.ID, StateDone, 30*time.Second)
	got, err := os.ReadFile(done.Path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, got))
	require.Eventually(t, func() bool { i, _ := alice.svc.Get(o.ID); return i.Delivered == 1 }, 10*time.Second, 10*time.Millisecond)
	out, _ := alice.svc.Get(o.ID)
	assert.Equal(t, StateActive, out.State)
}

// When the sender withdraws a group file, every member hears of it: those fetching stop, and
// those who never accepted see the offer gone.
func TestGroupSenderCancelReachesEveryMember(t *testing.T) {
	net, alice, m := groupOf(t, 3, nil)
	src, _ := randomFile(t, alice.dir, "big.bin", 400*ChunkSize)
	o := offerToGroup(t, alice, "g1", m, src)
	_, err := m[0].svc.Accept(o.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := m[0].svc.Get(o.ID); return i.Done > 0 }, 10*time.Second, time.Millisecond)
	net.hold.Store(true)
	require.NoError(t, alice.svc.Cancel(o.ID))
	for _, r := range m {
		waitState(t, r.svc, o.ID, StateCancelled, 5*time.Second)
	}
}

// Only members get the file. Someone who learnt the key (a member who left, say) but is not one is
// not served; a member who joined after the offer is, when the group says so.
func TestGroupFileOnlyForMembers(t *testing.T) {
	var joined atomic.Bool
	isMember := func(o *Options) {
		o.IsMember = func(group string, p types.PeerID) bool { return group == "g1" && p == peerID(99) && joined.Load() }
	}
	net, alice, m := groupOf(t, 1, isMember)
	eve := newSide(t, net, peerID(99), nil)
	src, data := randomFile(t, alice.dir, "notes.txt", 10*ChunkSize)
	o := offerToGroup(t, alice, "g1", m, src)
	_, err := eve.svc.OnOffer(alice.id, o)
	require.NoError(t, err)
	_, err = eve.svc.Accept(o.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return alice.svc.Dropped("member") > 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	in, _ := eve.svc.Get(o.ID)
	assert.Zero(t, in.Done, "a non-member gets nothing")
	assert.Zero(t, net.chunks.Load())

	// once the group counts eve as a member (she joined after the offer), she is served
	joined.Store(true)
	done := waitState(t, eve.svc, o.ID, StateDone, 20*time.Second)
	got, err := os.ReadFile(done.Path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, got))
	require.Eventually(t, func() bool { i, _ := alice.svc.Get(o.ID); return i.Delivered == 1 }, 5*time.Second, 10*time.Millisecond)
}
