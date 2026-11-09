package media

import (
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

// floorSender is a transport that carries at most floor-byte datagrams, as the v2 call transport
// does (callnet.MaxDatagram, the 1232-byte floor of every path).
type floorSender struct {
	floor int
	mu    sync.Mutex
	frags int
}

func (f *floorSender) SendRawFrame(types.PeerID, transport.Kind, []byte) error { return nil }

func (f *floorSender) SendUnreliableFrame(_ types.PeerID, _ transport.Kind, p []byte) error {
	if len(p) > f.floor {
		return transport.ErrDatagramTooLarge
	}
	f.mu.Lock()
	f.frags++
	f.mu.Unlock()
	return nil
}

func (f *floorSender) DatagramFloor() int { return f.floor }

// A full-size fragment must fit the floor the transport reports. media.DatagramBudget is the v1
// QUIC budget (1242); on a v2 path the same fragment is refused with ErrDatagramTooLarge and the
// video is lost. The format does not change: only the payload per fragment shrinks.
func TestMediaFragmentsFitTheV2DatagramFloor(t *testing.T) {
	key := newKey(t)
	id := strings.Repeat("s", MaxStreamIDLen)
	// This unit seals to exactly one fragment of the v1 budget.
	payload := make([]byte, fragmentPayload(len(id))-gcmNonceLen-gcmTagLen)

	v1 := NewMediaDispatcher(pid(1), &mockSender{}, nil, nil)
	require.NoError(t, v1.SetRoomKey(key))
	frags, err := v1.seal(FrameVideo, id, 1, payload, 0)
	require.NoError(t, err)
	require.Len(t, frags, 1)
	require.Greater(t, len(frags[0]), 1232, "the v1 path keeps its 1242-byte budget")

	s := &floorSender{floor: 1232}
	d := NewMediaDispatcher(pid(1), s, nil, nil)
	require.NoError(t, d.SetRoomKey(key))
	frags, err = d.seal(FrameVideo, id, 1, payload, 0)
	require.NoError(t, err)
	require.NotEmpty(t, frags)
	for i, f := range frags {
		if len(f) > 1232 {
			t.Fatalf("fragment %d is %d bytes, over the 1232-byte floor", i, len(f))
		}
		if err := s.SendUnreliableFrame(pid(2), transport.MediaVideo, f); err != nil {
			t.Fatalf("the transport refused fragment %d: %v", i, err)
		}
	}
}
