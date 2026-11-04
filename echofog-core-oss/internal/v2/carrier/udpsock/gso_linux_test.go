//go:build linux

package udpsock

import (
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// TestWriteMsgUDPGSORefusedSplitsTrain covers the path a QUIC transport takes: it hands one
// sendmsg with a UDP_SEGMENT control message, the kernel refuses it, and the datagrams still go
// out one by one.
func TestWriteMsgUDPGSORefusedSplitsTrain(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{})
	if !a.gso.Load() {
		t.Skip("the platform has no GSO")
	}
	dst := addrOf(t, b)
	refused := errors.New("UDP_SEGMENT refused")
	var trains atomic.Int32
	a.writeMsgFn = func(p, oob []byte, addr *net.UDPAddr) (int, int, error) {
		if seg, _ := gsoSegment(oob); seg > 0 {
			trains.Add(1)
			return 0, 0, refused
		}
		return a.writeRaw(p, oob, addr)
	}

	const seg = 300
	const nseg = 4
	train := make([]byte, seg*nseg)
	for i := 0; i < nseg; i++ {
		for j := 0; j < seg; j++ {
			train[i*seg+j] = byte(i + 1)
		}
	}
	n, _, err := a.WriteMsgUDP(train, appendGSOSegment(nil, seg), dst)
	if err != nil || n != len(train) {
		t.Fatalf("write: %d of %d %v", n, len(train), err)
	}
	if a.gso.Load() {
		t.Fatal("GSO is still on after the kernel refused a train")
	}
	if got := trains.Load(); got != 1 {
		t.Fatalf("GSO trains tried %d times, want 1", got)
	}
	got := readMessages(t, b, nseg)
	if len(got) != nseg {
		t.Fatalf("%d datagrams arrived, want %d", len(got), nseg)
	}
	for i := range got {
		want := bytes.Repeat([]byte{byte(i + 1)}, seg)
		if !bytes.Equal(got[i], want) {
			t.Fatalf("datagram %d: %d bytes, pattern %v", i, len(got[i]), got[i][:1])
		}
	}

	// From here on the socket never asks for a train again.
	before := a.writeMsgFn
	a.writeMsgFn = func(p, oob []byte, addr *net.UDPAddr) (int, int, error) {
		if seg, _ := gsoSegment(oob); seg > 0 {
			t.Error("GSO was tried again after the fallback")
		}
		return before(p, oob, addr)
	}
	if n, _, err := a.WriteMsgUDP(train, appendGSOSegment(nil, seg), dst); err != nil || n != len(train) {
		t.Fatalf("second write: %d %v", n, err)
	}
	if got := readMessages(t, b, nseg); len(got) != nseg {
		t.Fatalf("second train: %d datagrams arrived", len(got))
	}
}
