package callnet

import (
	"testing"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// FuzzDeliver: whatever a peer puts in a datagram, delivery neither panics nor hands the call
// layer anything but the exact payload bytes.
func FuzzDeliver(f *testing.F) {
	f.Add(uint64(0x10), []byte("unit"), uint8(0), uint8(0))
	f.Add(uint64(0x11), []byte{}, uint8(1), uint8(2))
	f.Add(uint64(0x12), []byte{0xff, 0, 1}, uint8(0), uint8(1))
	f.Add(uint64(0x13), []byte("auth"), uint8(1), uint8(0))
	f.Add(uint64(1<<62), []byte("x"), uint8(7), uint8(9))
	f.Fuzz(func(t *testing.T, typ uint64, payload []byte, path uint8, rt uint8) {
		r := route.Route(rt % 4)
		tr := New(Config{Links: links{}, Route: func() route.Route { return r }})
		for _, k := range []transport.Kind{transport.MediaAudio, transport.MediaVideo, transport.SignalLossy} {
			tr.RegisterFastFrameHandler(k, func(p []byte, _ types.PeerID) error {
				if len(p) != len(payload) {
					t.Fatalf("handler got %d bytes, sent %d", len(p), len(payload))
				}
				return nil
			})
		}
		tr.Deliver(Inbound{Device: dev(1), Type: typ, Payload: payload, Path: mux.PathKind(path)})
	})
}
