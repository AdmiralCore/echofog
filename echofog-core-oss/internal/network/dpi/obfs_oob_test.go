package dpi

import (
	"bytes"
	"crypto/rand"
	"net"
	"runtime"
	"testing"
	"time"
)

// oldStyleWrap is WrapPacketConn as v0.2.0 had it: no control messages, no batch reads, so
// quic-go reads and writes one packet at a time through it.
func oldStyleWrap(conn net.PacketConn, obfs *NodeObfs) net.PacketConn {
	base := &ObfsPacketConn{
		Conn:     conn,
		obfs:     obfs,
		readBuf:  make([]byte, maxPacketBufferSize),
		writeBuf: make([]byte, maxPacketBufferSize),
		cands:    make([][KeyLen]byte, 0, 10),
	}
	if udp, ok := conn.(UDPLikePacketConn); ok {
		return &ObfsPacketConnUDP{ObfsPacketConn: base, UDPConn: udp}
	}
	return base
}

// obfsQUICPair: a server node and a client node with obfuscation, the client expecting the
// server's key; wrapServer and wrapClient wrap each one's socket.
func obfsQUICPair(t *testing.T, wrapServer, wrapClient func(net.PacketConn, *NodeObfs) net.PacketConn) (srv, cli net.PacketConn) {
	t.Helper()
	so, skey := newObfs(t, 1)
	co, _ := newObfs(t, 2)
	ss, cs := udpPair(t)
	if err := co.Expect(ss.LocalAddr().(*net.UDPAddr).AddrPort(), skey); err != nil {
		t.Fatal(err)
	}
	return wrapServer(ss, so), wrapClient(cs, co)
}

// The interop guarantee: a node that writes GSO trains under the obfuscation and a v0.2.0 node
// that reads and writes one datagram at a time exchange data both ways, so every datagram on the
// wire is sealed exactly as before.
func TestObfsGSOInteropWithOldStyle(t *testing.T) {
	t.Run("new server sends to old client", func(t *testing.T) {
		srv, cli := obfsQUICPair(t, WrapPacketConn, oldStyleWrap)
		spy := &spyConn{OOBPacketConn: srv.(OOBPacketConn)}
		quicDownload(t, spy, cli, 8<<20)
		t.Logf("server: %d writes, %d GSO trains, %d reads, %d ECN-marked", spy.writes.Load(), spy.gsoWrites.Load(), spy.reads.Load(), spy.ecnReads.Load())
		checkFastPath(t, spy, false) // a v0.2.0 node sends no ECN marks
	})
	t.Run("old server sends to new client", func(t *testing.T) {
		srv, cli := obfsQUICPair(t, oldStyleWrap, WrapPacketConn)
		spy := &spyConn{OOBPacketConn: cli.(OOBPacketConn)}
		quicDownload(t, srv, spy, 8<<20)
		if fastPathOS && spy.reads.Load() == 0 {
			t.Fatal("the new client must read through ReadBatch")
		}
	})
	t.Run("both new, server behind the multiplexer", func(t *testing.T) {
		so, skey := newObfs(t, 1)
		co, _ := newObfs(t, 2)
		ss, cs := udpPair(t)
		_ = co.Expect(ss.LocalAddr().(*net.UDPAddr).AddrPort(), skey)
		mux, err := NewMuxedConn(ss)
		if err != nil {
			t.Fatal(err)
		}
		defer mux.Close()
		srv := WrapPacketConn(mux.GetQUICConn(), so)
		spy := &spyConn{OOBPacketConn: srv.(OOBPacketConn)}
		quicDownload(t, spy, WrapPacketConn(cs, co), 8<<20)
		checkFastPath(t, spy, true)
	})
}

// recorder is an OOBPacketConn that keeps what is written to it.
type recorder struct {
	OOBPacketConn
	b, oob []byte
}

func (r *recorder) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	r.b, r.oob = append([]byte(nil), b...), append([]byte(nil), oob...)
	return len(b), len(oob), nil
}

// Without GSO a datagram is sealed as WriteTo seals it and its control messages (ECN, source
// address) pass unchanged; a raw (REALITY) peer's packets pass untouched.
func TestObfsWriteMsgUDPOneDatagram(t *testing.T) {
	o, key := newObfs(t, 1)
	ss, _ := udpPair(t)
	oc, _ := AsOOB(ss)
	rec := &recorder{OOBPacketConn: oc}
	w := WrapPacketConn(rec, o).(*ObfsPacketConnOOB)
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	pkt := make([]byte, 1200)
	_, _ = rand.Read(pkt)
	oob := []byte{1, 2, 3, 4} // opaque to the obfuscation: it holds no UDP_SEGMENT
	n, _, err := w.WriteMsgUDP(pkt, oob, to)
	if err != nil || n != len(pkt) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(rec.b) != len(pkt)+Overhead || !bytes.Equal(rec.oob, oob) {
		t.Fatalf("wrote %d bytes, oob %x", len(rec.b), rec.oob)
	}
	var k [KeyLen]byte
	copy(k[:], key)
	out := make([]byte, 2048)
	m, ok := open(&k, rec.b, out)
	if !ok || !bytes.Equal(out[:m], pkt) {
		t.Fatal("the datagram must open with the node key")
	}

	o.SetRaw(to.AddrPort(), true)
	if _, _, err := w.WriteMsgUDP(pkt, oob, to); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.b, pkt) {
		t.Fatal("a raw peer's packet must pass untouched")
	}
}

// ReadBatch returns only the datagrams meant for us, unmasked, with their senders.
func TestObfsReadBatchDropsForeign(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("x/net reads no batches on Windows; quic-go never asks for them there")
	}
	p := newPair(t)
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyB)
	b := p.b.(*ObfsPacketConnOOB)
	var want [][]byte
	for i := 0; i < 6; i++ {
		pkt := make([]byte, 200+i)
		_, _ = rand.Read(pkt)
		if i%2 == 0 {
			if _, err := p.a.WriteTo(pkt, p.rb.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			want = append(want, pkt)
		} else if _, err := p.ra.WriteTo(pkt, p.rb.LocalAddr()); err != nil { // noise nobody sealed
			t.Fatal(err)
		}
	}
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	ms := newBatch(8, 1500)
	var got [][]byte
	for len(got) < len(want) {
		n, err := b.ReadBatch(ms, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if ms[i].Addr.String() != p.ra.LocalAddr().String() {
				t.Fatalf("sender %v", ms[i].Addr)
			}
			got = append(got, append([]byte(nil), ms[i].Buffers[0][:ms[i].N]...))
		}
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("datagram %d differs", i)
		}
	}
}
