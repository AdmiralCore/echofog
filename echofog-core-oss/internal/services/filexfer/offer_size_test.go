package filexfer

import (
	"math"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// An offer names its size and the receiver sizes its bookkeeping from it before the user accepts:
// a huge or overflowing size from a contact or a group member must be refused, not kept (it made
// the node panic or run out of memory, and again on every start).
func TestOfferWithAbsurdSizeIsRefused(t *testing.T) {
	_, alice, bob := pair(t, nil)
	src, _ := randomFile(t, alice.dir, "a.bin", 100)
	for _, size := range []int64{math.MaxInt64, 1 << 46, maxOfferSize + 1} {
		o, err := alice.svc.Prepare(bob.id, src)
		require.NoError(t, err)
		o.Size = size
		_, err = bob.svc.OnOffer(alice.id, o)
		require.Error(t, err, "size %d", size)
		_, err = bob.svc.Get(o.ID)
		require.Error(t, err, "an offer of size %d was kept", size)
	}
}

// A node that kept such an offer before the check existed must start, and drop it, rather than
// crash again at every start.
func TestStoredAbsurdOfferIsDroppedAtStart(t *testing.T) {
	net := &fakeNet{nodes: map[types.PeerID]*Service{}}
	for _, state := range []State{StateOffered, StateActive} {
		s := &side{id: peerID(2), store: newMemStore(), dir: t.TempDir()}
		id := strings.Repeat("cd", idLen)
		require.NoError(t, saveRecord(s.store, &Record{ID: id, Peer: peerID(1).String(), Dir: Incoming, Name: "x", Size: math.MaxInt64,
			Root: strings.Repeat("00", 32), Key: strings.Repeat("11", 32), State: state}))
		s.start(t, net, nil)
		_, err := s.svc.Get(id)
		require.Error(t, err, "the stored offer (%s) was kept", state)
	}
}
