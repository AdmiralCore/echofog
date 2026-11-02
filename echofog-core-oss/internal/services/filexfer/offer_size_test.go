package filexfer

import (
	"math"
	"testing"

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
