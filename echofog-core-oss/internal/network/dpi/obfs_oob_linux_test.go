//go:build linux

package dpi

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func udpSegmentCmsg(size uint16) []byte {
	b := make([]byte, unix.CmsgSpace(2))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = unix.IPPROTO_UDP, unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(2))
	binary.NativeEndian.PutUint16(b[unix.CmsgLen(0):], size)
	return b
}

func tosCmsg(tos byte) []byte {
	b := make([]byte, unix.CmsgSpace(1))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = unix.IPPROTO_IP, unix.IP_TOS
	h.SetLen(unix.CmsgLen(1))
	b[unix.CmsgLen(0)] = tos
	return b
}

// A GSO train is cut into its segments, each sealed on its own (own salt) exactly as WriteTo
// would seal it, and goes out as one train with the segment size grown by Overhead; the ECN mark
// is left out.
func TestObfsWriteMsgUDPSealsEachGSOSegment(t *testing.T) {
	o, key := newObfs(t, 1)
	ss, _ := udpPair(t)
	oc, _ := AsOOB(ss)
	rec := &recorder{OOBPacketConn: oc}
	w := WrapPacketConn(rec, o).(*ObfsPacketConnOOB)
	const seg = 1200
	train := make([]byte, 3*seg+345)
	_, _ = rand.Read(train)
	oob := append(tosCmsg(2), udpSegmentCmsg(seg)...)
	if _, _, err := w.WriteMsgUDP(train, oob, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); err != nil {
		t.Fatal(err)
	}
	if got, _ := gsoSegment(rec.oob); got != seg+Overhead {
		t.Fatalf("segment size %d, want %d", got, seg+Overhead)
	}
	if HasECNMark(rec.oob) {
		t.Fatal("a noise train must leave without ECN marks")
	}
	if len(rec.b) != len(train)+4*Overhead {
		t.Fatalf("train of %d bytes, want %d", len(rec.b), len(train)+4*Overhead)
	}
	var k [KeyLen]byte
	copy(k[:], key)
	out := make([]byte, 2048)
	salts := map[string]bool{}
	for i, wire := 0, rec.b; len(wire) > 0; i++ {
		d := wire[:min(seg+Overhead, len(wire))]
		wire = wire[len(d):]
		m, ok := open(&k, d, out)
		if !ok || !bytes.Equal(out[:m], train[i*seg:min((i+1)*seg, len(train))]) {
			t.Fatalf("segment %d does not open to the original", i)
		}
		salts[string(d[:SaltLen])] = true
	}
	if len(salts) != 4 {
		t.Fatal("every segment needs its own salt")
	}
}

// Noise datagrams leave Not-ECT: high-entropy UDP with ECT(0) is a rare feature on the wire,
// which a censor could match (and a v0.2.0 peer, which echoes no ECN counts, made the flow show
// quic-go's validation pattern). A raw (REALITY) peer's datagrams are left to the REALITY layer.
func TestObfsNoiseLeavesWithoutECN(t *testing.T) {
	o, _ := newObfs(t, 1)
	ss, _ := udpPair(t)
	oc, _ := AsOOB(ss)
	rec := &recorder{OOBPacketConn: oc}
	w := WrapPacketConn(rec, o).(*ObfsPacketConnOOB)
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	pkt := make([]byte, 1200)
	_, _ = rand.Read(pkt)
	if _, _, err := w.WriteMsgUDP(pkt, tosCmsg(2), to); err != nil {
		t.Fatal(err)
	}
	if HasECNMark(rec.oob) {
		t.Fatal("a noise datagram must leave without ECN marks")
	}
	if len(rec.b) != len(pkt)+Overhead {
		t.Fatalf("wrote %d bytes", len(rec.b))
	}
}

// Over a real socket the kernel cuts the train into datagrams that a v0.2.0 node (one ReadFrom
// per datagram) opens one by one.
func TestObfsGSOTrainOpensOnOldReceiver(t *testing.T) {
	so, _ := newObfs(t, 1)
	ro, rkey := newObfs(t, 2)
	ss, rs := udpPair(t)
	_ = so.Expect(rs.LocalAddr().(*net.UDPAddr).AddrPort(), rkey)
	w := WrapPacketConn(ss, so).(*ObfsPacketConnOOB)
	r := oldStyleWrap(rs, ro)
	const seg = 1200
	train := make([]byte, 5*seg+77)
	_, _ = rand.Read(train)
	if _, _, err := w.WriteMsgUDP(train, udpSegmentCmsg(seg), rs.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		got, _, err := readOne(t, r, 2*time.Second)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		if !bytes.Equal(got, train[i*seg:min((i+1)*seg, len(train))]) {
			t.Fatalf("datagram %d differs (%d bytes)", i, len(got))
		}
	}
}
