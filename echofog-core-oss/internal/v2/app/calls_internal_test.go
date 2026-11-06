package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

func TestMemberCacheAdmitsMembersOnly(t *testing.T) {
	now := time.Unix(100, 0)
	var calls atomic.Int32
	members := []types.PeerID{{1}, {2}}
	m := newMemberCache(func() []types.PeerID { calls.Add(1); return members }, func() time.Time { return now }, 100*time.Millisecond)
	require.True(t, m.allow([32]byte{1}))
	require.True(t, m.allow([32]byte{2}))
	require.False(t, m.allow([32]byte{3}))
	require.EqualValues(t, 1, calls.Load(), "the roster is read once inside the TTL, strangers included")
}

func TestMemberCacheRefreshesAfterTheTTL(t *testing.T) {
	now := time.Unix(100, 0)
	members := []types.PeerID{{1}}
	m := newMemberCache(func() []types.PeerID { return members }, func() time.Time { return now }, 100*time.Millisecond)
	require.False(t, m.allow([32]byte{2}))
	members = []types.PeerID{{1}, {2}}
	require.False(t, m.allow([32]byte{2}), "inside the TTL the old roster stands")
	now = now.Add(101 * time.Millisecond)
	require.True(t, m.allow([32]byte{2}), "a new member is admitted within the TTL")
	members = nil
	now = now.Add(101 * time.Millisecond)
	require.False(t, m.allow([32]byte{2}), "a member who left is dropped within the TTL")
}

func TestMemberCacheWithoutASourceAdmitsNobody(t *testing.T) {
	m := newMemberCache(nil, time.Now, time.Second)
	require.False(t, m.allow([32]byte{1}))
}

type dialHost struct {
	mu       sync.Mutex
	sessions []node.DeviceID
}

func (*dialHost) CallTransport() *callnet.Transport       { return nil }
func (*dialHost) SetCallAdmit(func(device [32]byte) bool) {}
func (h *dialHost) Session(_ context.Context, p node.DeviceID, _ node.DialOpt) (node.Peer, *mux.Session, error) {
	h.mu.Lock()
	h.sessions = append(h.sessions, p)
	h.mu.Unlock()
	return node.Peer{}, nil, nil
}

func TestPeerConnectorDialsTheDeviceSession(t *testing.T) {
	h := &dialHost{}
	c := &Calls{host: h}
	require.NoError(t, c.connect(context.Background(), types.PeerID{7}))
	require.Equal(t, []node.DeviceID{{7}}, h.sessions)
}
