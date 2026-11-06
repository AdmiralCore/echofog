package convsync

import (
	"context"
	"testing"

	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// FuzzReceive feeds the engine arbitrary event and sync bytes from a peer: nothing may panic, and
// the log stays usable.
func FuzzReceive(f *testing.F) {
	cl := newCluster(f, 2, 2)
	f.Add([]byte{}, []byte{})
	f.Add([]byte{0x0a, 0x10}, []byte{0x0a, 0x00})
	f.Add(bytesOf(0xff, 64), bytesOf(0x12, 40))
	f.Fuzz(func(t *testing.T, ev, sy []byte) {
		a, b := cl.members[0], cl.members[1]
		b.e.Receive(context.Background(), a.a.node, &chatpb.PersonalMessageProto{ConvEvent: ev})
		b.e.Receive(context.Background(), a.a.node, &chatpb.PersonalMessageProto{ConvSync: sy})
		if _, _, err := b.e.Log().History(b.e.DM(a.a.pub), "", 5); err != nil {
			t.Fatalf("the log is unusable after input: %v", err)
		}
		_ = convlog.KindMsg
	})
}

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
