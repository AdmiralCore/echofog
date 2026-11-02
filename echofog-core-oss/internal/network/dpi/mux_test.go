package dpi

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func udpPair(t *testing.T) (*net.UDPConn, *net.UDPConn) {
	t.Helper()
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	return listen(), listen()
}

// stunMessage builds a minimal binding request: type, length 0, magic cookie, transaction ID.
func stunMessage() []byte {
	m := make([]byte, 20)
	binary.BigEndian.PutUint16(m[0:2], 0x0001)
	binary.BigEndian.PutUint32(m[4:8], stunMagicCookie)
	_, _ = rand.Read(m[8:])
	return m
}

func TestMuxedConn_Routing(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatalf("Failed to create MuxedConn: %v", err)
	}
	defer mux.Close()

	buf := make([]byte, 4096)

	stun := stunMessage()
	if _, err := clientConn.WriteTo(stun, serverConn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	mux.GetSTUNConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := mux.GetSTUNConn().ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], stun) {
		t.Fatalf("the STUN message must arrive on the STUN side: n=%d err=%v", n, err)
	}

	quic := make([]byte, 40)
	quic[0] = 0xC0 // QUIC long header
	if _, err := clientConn.WriteTo(quic, serverConn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	mux.GetQUICConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err = mux.GetQUICConn().ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], quic) {
		t.Fatalf("the QUIC packet must arrive on the QUIC side: n=%d err=%v", n, err)
	}
}

func TestIsSTUNChecksStructureNotJustTheFirstByte(t *testing.T) {
	if !isSTUN(stunMessage()) {
		t.Fatal("a binding request is STUN")
	}

	short := stunMessage()[:19]
	if isSTUN(short) {
		t.Fatal("too short to be STUN")
	}

	noCookie := stunMessage()
	noCookie[4] ^= 0xFF
	if isSTUN(noCookie) {
		t.Fatal("without the magic cookie it is not STUN")
	}

	topBits := stunMessage()
	topBits[0] = 0x40 // a QUIC short header has this bit set
	if isSTUN(topBits) {
		t.Fatal("STUN messages start with two zero bits")
	}

	// obfuscated QUIC is random bytes: a first byte of 0 or 1 used to misroute about 1% of it
	misrouted := 0
	pkt := make([]byte, 1200)
	for i := 0; i < 20000; i++ {
		_, _ = rand.Read(pkt)
		pkt[0] = byte(i % 2) // the worst case for the old check
		if isSTUN(pkt) {
			misrouted++
		}
	}
	if misrouted != 0 {
		t.Fatalf("%d of 20000 random packets were taken for STUN", misrouted)
	}
}

func TestMuxedConnDoesNotLoseObfuscatedPackets(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	// packets whose first byte is 0 or 1, as obfuscated QUIC produces by chance
	for first := byte(0); first <= 1; first++ {
		pkt := make([]byte, 300)
		_, _ = rand.Read(pkt)
		pkt[0] = first
		if _, err := clientConn.WriteTo(pkt, serverConn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		mux.GetQUICConn().SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := mux.GetQUICConn().ReadFrom(buf)
		if err != nil || !bytes.Equal(buf[:n], pkt) {
			t.Fatalf("packet starting with %d was lost or misrouted: n=%d err=%v", first, n, err)
		}
	}
}

func TestMuxedConnReadHonoursDeadlineAndClose(t *testing.T) {
	serverConn, _ := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	q := mux.GetQUICConn()
	q.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	start := time.Now()
	if _, _, err := q.ReadFrom(make([]byte, 100)); err == nil {
		t.Fatal("expected a deadline error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the deadline was not honoured")
	}

	_ = mux.Close()
	if _, _, err := q.ReadFrom(make([]byte, 100)); err != ErrClosed {
		t.Fatalf("a closed connection must report ErrClosed, got %v", err)
	}
}

// A datagram longer than the reader's buffer is truncated, as the kernel does. It used to be an
// error, and quic-go closes its transport on a read error: one oversized datagram from anybody
// ended the node's QUIC listener (plain mode, where quic-go reads the multiplexer directly).
func TestMuxedConnTruncatesOversizedDatagram(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	big := make([]byte, 1600)
	_, _ = rand.Read(big)
	big[0] = 0xC0
	if _, err := clientConn.WriteTo(big, serverConn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	q := mux.GetQUICConn()
	q.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1452) // quic-go's receive buffer
	n, _, err := q.ReadFrom(buf)
	if err != nil {
		t.Fatalf("an oversized datagram must not be a read error: %v", err)
	}
	if n != len(buf) || !bytes.Equal(buf, big[:len(buf)]) {
		t.Fatalf("want the first %d bytes, got %d", len(buf), n)
	}
}

// A datagram longer than the multiplexer's buffer is dropped and reading goes on. On Windows the
// read fails with WSAEMSGSIZE, which used to end the read loop: from then on the node received no
// UDP at all, QUIC or STUN, until it restarted.
func TestMuxedConnKeepsReadingAfterAnOversizedDatagram(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	huge := make([]byte, 3*maxDatagram) // IP-fragmented on the wire, one datagram to the socket
	_, _ = rand.Read(huge)
	huge[0] = 0xC0
	for i := 0; i < 3; i++ {
		if _, err := clientConn.WriteTo(huge, serverConn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	q := mux.GetQUICConn()
	buf := make([]byte, 2048)
	small := make([]byte, 100)
	_, _ = rand.Read(small)
	small[0] = 0xC0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := clientConn.WriteTo(small, serverConn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		q.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			n, _, err := q.ReadFrom(buf)
			if err != nil {
				break
			}
			if bytes.Equal(buf[:n], small) {
				return // still receiving
			}
			// a truncated copy of the oversized datagram (platforms that truncate) is fine
		}
	}
	t.Fatal("no datagram arrived after an oversized one: the read loop stopped")
}
