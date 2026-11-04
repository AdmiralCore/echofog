//go:build linux || darwin || freebsd

package udpsock

import (
	"testing"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func marshalTOS(tos int) []byte {
	return ipv4.ControlMessage{TOS: tos}.Marshal()
}

// TestECNMarkRoundTrip sends one datagram marked ECT(0) and checks the receiver reads the mark
// back as a control message. ECN is wire-neutral: the mark rides in the IP header as before.
func TestECNMarkRoundTrip(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{})
	if !a.ecnOn || !b.ecnOn {
		t.Skip("the kernel does not give this socket ECN")
	}
	if _, _, err := b.WriteMsgUDP([]byte("ecn"), marshalTOS(2), addrOf(t, a)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ms := messages(1, 64)
	n, err := a.ReadBatch(ms, 0)
	if err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	if got := string(ms[0].Buffers[0][:ms[0].N]); got != "ecn" {
		t.Fatalf("payload %q, want %q", got, "ecn")
	}
	if ms[0].NN == 0 {
		t.Fatal("no control messages came with the datagram")
	}
	var mark uint8
	var found bool
	rest := ms[0].OOB[:ms[0].NN]
	for len(rest) >= unix.SizeofCmsghdr {
		hdr, body, remainder, err := unix.ParseOneSocketControlMessage(rest)
		if err != nil {
			t.Fatalf("control message: %v", err)
		}
		if hdr.Level == unix.IPPROTO_IP && hdr.Type == unix.IP_TOS && len(body) == 1 {
			mark, found = body[0]&0x3, true
		}
		rest = remainder
	}
	if !found {
		t.Fatalf("no TOS control message in %d bytes", ms[0].NN)
	}
	if mark != 2 {
		t.Fatalf("ECN mark %d, want 2 (ECT(0))", mark)
	}
}

// TestRestrictedStripsECN checks the udp=restricted rule on the write side: the mark is dropped
// before the datagram reaches the kernel.
func TestRestrictedStripsECN(t *testing.T) {
	a := listen(t, Options{})
	b := listen(t, Options{Restricted: true})
	oob := marshalTOS(2)
	if !hasECN(oob) {
		t.Fatal("the test control message carries no ECN mark")
	}
	if _, _, err := b.WriteMsgUDP([]byte("plain"), oob, addrOf(t, a)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ms := messages(1, 64)
	if n, err := a.ReadBatch(ms, 0); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	if hasECN(ms[0].OOB[:ms[0].NN]) {
		t.Fatal("a restricted socket sent an ECN mark")
	}
}
