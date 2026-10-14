package network

import (
	"bytes"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// FuzzReadFrame feeds arbitrary bytes to the frame reader that sits on every peer stream: it must
// never panic or allocate beyond the limit, and what it accepts must survive a round trip.
func FuzzReadFrame(f *testing.F) {
	var ok bytes.Buffer
	_ = WriteFrame(&ok, 5, []byte("hello"))
	f.Add(ok.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0})                      // empty frame
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3}) // absurd length
	f.Add([]byte{0, 0, 0, 5, 1})                   // truncated
	f.Add(append([]byte{0, 0, 0, 3}, 7, 8, 9))     // minimal
	f.Fuzz(func(t *testing.T, data []byte) {
		typ, payload, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := WriteFrame(&out, typ, payload); err != nil {
			t.Fatalf("write of a frame that was read: %v", err)
		}
		typ2, payload2, err := ReadFrame(&out)
		if err != nil || typ2 != typ || !bytes.Equal(payload, payload2) {
			t.Fatalf("round trip changed the frame: %v %v/%v", err, typ, typ2)
		}
	})
}

// FuzzPeerAddr: "host:port/<peer id>" is how nodes are named everywhere; splitting must never
// panic and joining what was split must give the same node.
func FuzzPeerAddr(f *testing.F) {
	var id types.PeerID
	id[0], id[31] = 1, 2
	f.Add(JoinPeerAddr("203.0.113.5:4242", id))
	f.Add("host:1")
	f.Add("a/b/c")
	f.Add("/")
	f.Add("1.2.3.4:5/zz")
	f.Add("[::1]:4242/" + id.String())
	f.Fuzz(func(t *testing.T, s string) {
		addr, got, named := SplitPeerAddr(s)
		if !named {
			if addr != s {
				t.Fatalf("an address without id must come back unchanged: %q -> %q", s, addr)
			}
			return
		}
		again, id2, named2 := SplitPeerAddr(JoinPeerAddr(addr, got))
		if !named2 || again != addr || id2 != got {
			t.Fatalf("join/split is not stable for %q", s)
		}
	})
}
