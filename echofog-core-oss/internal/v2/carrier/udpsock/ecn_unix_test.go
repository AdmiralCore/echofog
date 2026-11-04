//go:build linux || darwin || freebsd

package udpsock

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// marshalTOS builds an IPv4 IP_TOS control message, the way a sender marks ECN.
func marshalTOS(tos int) []byte {
	b := make([]byte, unix.CmsgSpace(1))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IP
	h.Type = unix.IP_TOS
	h.SetLen(unix.CmsgLen(1))
	b[unix.CmsgSpace(0)] = byte(tos)
	return b
}

// ecnMark reads the two ECN bits out of a received datagram's control messages.
func ecnMark(oob []byte) (uint8, bool) {
	var mark uint8
	found := false
	rest := oob
	for len(rest) >= unix.SizeofCmsghdr {
		hdr, body, remainder, err := unix.ParseOneSocketControlMessage(rest)
		if err != nil {
			return 0, false
		}
		switch {
		case hdr.Level == unix.IPPROTO_IP && hdr.Type == unix.IP_TOS && len(body) == 1:
			mark, found = body[0]&0x3, true
		case hdr.Level == unix.IPPROTO_IPV6 && hdr.Type == unix.IPV6_TCLASS && len(body) == 4:
			mark, found = uint8(body[0])&0x3, true
		}
		rest = remainder
	}
	return mark, found
}

// TestECNMarkRoundTrip sends one datagram marked ECT(0) and checks the receiver reads the mark
// back. ECN is wire-neutral: the mark rides in the IP header as before.
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
	mark, ok := ecnMark(ms[0].OOB[:ms[0].NN])
	if !ok {
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
	mark, ok := ecnMark(ms[0].OOB[:ms[0].NN])
	if !ok {
		t.Fatalf("no TOS control message in %d bytes", ms[0].NN)
	}
	if mark != 0 {
		t.Fatalf("a restricted socket sent ECN mark %d", mark)
	}
}
