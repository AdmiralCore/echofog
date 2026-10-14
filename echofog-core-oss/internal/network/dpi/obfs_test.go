package dpi

import (
	"bytes"
	"crypto/rand"
	"math/bits"
	"net"
	"net/netip"
	"testing"
	"time"
)

func idOf(b byte) [32]byte {
	var id [32]byte
	for i := range id {
		id[i] = b
	}
	return id
}

func newObfs(t *testing.T, id byte) (*NodeObfs, []byte) {
	t.Helper()
	key := NodeKey("", idOf(id))
	o, err := NewNodeObfs(key)
	if err != nil {
		t.Fatal(err)
	}
	return o, key
}

func TestNodeKeyDependsOnIDAndSecret(t *testing.T) {
	a, b := NodeKey("", idOf(1)), NodeKey("", idOf(2))
	if bytes.Equal(a, b) {
		t.Fatal("different nodes must have different keys")
	}
	if bytes.Equal(a, NodeKey("private-net", idOf(1))) {
		t.Fatal("a network secret must change the key")
	}
	if !bytes.Equal(a, NodeKey("", idOf(1))) {
		t.Fatal("the key must be deterministic")
	}
	if len(a) != KeyLen {
		t.Fatalf("key length %d", len(a))
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	_, k := newObfs(t, 1)
	var key [KeyLen]byte
	copy(key[:], k)
	payload := []byte("QUIC_INITIAL_PACKET_WITH_TLS_CLIENT_HELLO_AND_SNI")
	salt := make([]byte, SaltLen)
	_, _ = rand.Read(salt)
	wire := make([]byte, 1024)
	n, err := seal(&key, payload, wire, salt)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload)+Overhead {
		t.Fatalf("wire length %d, want %d", n, len(payload)+Overhead)
	}
	if bytes.Contains(wire[:n], payload) {
		t.Fatal("plaintext leaked onto the wire")
	}
	out := make([]byte, 1024)
	m, ok := open(&key, wire[:n], out)
	if !ok || !bytes.Equal(out[:m], payload) {
		t.Fatalf("round trip failed: ok=%v %q", ok, out[:m])
	}
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	_, ka := newObfs(t, 1)
	_, kb := newObfs(t, 2)
	var a, b [KeyLen]byte
	copy(a[:], ka)
	copy(b[:], kb)
	wire := make([]byte, 256)
	salt := bytes.Repeat([]byte{7}, SaltLen)
	n, _ := seal(&a, []byte("hello hello hello hello"), wire, salt)
	out := make([]byte, 256)
	if _, ok := open(&b, wire[:n], out); ok {
		t.Fatal("a packet sealed for one node opened with another node's key")
	}
	for i := 0; i < n; i++ {
		flipped := append([]byte(nil), wire[:n]...)
		flipped[i] ^= 0x01
		if _, ok := open(&a, flipped, out); ok {
			t.Fatalf("a packet with byte %d flipped was accepted", i)
		}
	}
	if _, ok := open(&a, wire[:Overhead], out); ok {
		t.Fatal("a packet with no payload was accepted")
	}
}

// Wire bytes must look random: balanced bits and no repetition between two identical payloads.
func TestWireLooksRandom(t *testing.T) {
	_, k := newObfs(t, 3)
	var key [KeyLen]byte
	copy(key[:], k)
	payload := bytes.Repeat([]byte{0}, 1200) // the worst case: all zeroes
	ones, total := 0, 0
	var prev []byte
	for i := 0; i < 50; i++ {
		salt := make([]byte, SaltLen)
		_, _ = rand.Read(salt)
		wire := make([]byte, 1300)
		n, _ := seal(&key, payload, wire, salt)
		if prev != nil && bytes.Equal(prev[SaltLen:n-TagLen], wire[SaltLen:n-TagLen]) {
			t.Fatal("two packets with the same payload have the same body")
		}
		prev = append(prev[:0], wire[:n]...)
		for _, b := range wire[:n] {
			ones += bits.OnesCount8(b)
		}
		total += n * 8
	}
	if ratio := float64(ones) / float64(total); ratio < 0.49 || ratio > 0.51 {
		t.Fatalf("share of one-bits %.4f, want about 0.5", ratio)
	}
}

type pair struct {
	a, b       net.PacketConn // wrapped
	ra, rb     *net.UDPConn   // raw sockets under them
	oa, ob     *NodeObfs
	keyA, keyB []byte
}

func newPair(t *testing.T) *pair {
	t.Helper()
	p := &pair{}
	p.oa, p.keyA = newObfs(t, 1)
	p.ob, p.keyB = newObfs(t, 2)
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	p.ra, p.rb = listen(), listen()
	p.a, p.b = WrapPacketConn(p.ra, p.oa), WrapPacketConn(p.rb, p.ob)
	return p
}

func readOne(t *testing.T, c net.PacketConn, wait time.Duration) ([]byte, net.Addr, error) {
	t.Helper()
	buf := make([]byte, 2048)
	_ = c.SetReadDeadline(time.Now().Add(wait))
	n, addr, err := c.ReadFrom(buf)
	return buf[:n], addr, err
}

// A dials B knowing B's ID; B answers without knowing A's: both directions work.
func TestDialerAndAnswerer(t *testing.T) {
	p := newPair(t)
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyB)

	if _, err := p.a.WriteTo([]byte("ping"), p.rb.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	got, from, err := readOne(t, p.b, 2*time.Second)
	if err != nil || string(got) != "ping" {
		t.Fatalf("B got %q err=%v", got, err)
	}
	if _, err := p.b.WriteTo([]byte("pong"), from); err != nil {
		t.Fatal(err)
	}
	got, _, err = readOne(t, p.a, 2*time.Second)
	if err != nil || string(got) != "pong" {
		t.Fatalf("A got %q err=%v", got, err)
	}
}

// Both sides dial each other at once (hole punching): A seals for B, B seals for A.
func TestSimultaneousDial(t *testing.T) {
	p := newPair(t)
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyB)
	_ = p.ob.Expect(p.ra.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyA)

	_, _ = p.a.WriteTo([]byte("from A"), p.rb.LocalAddr())
	_, _ = p.b.WriteTo([]byte("from B"), p.ra.LocalAddr())
	if got, _, err := readOne(t, p.b, 2*time.Second); err != nil || string(got) != "from A" {
		t.Fatalf("B got %q err=%v", got, err)
	}
	if got, _, err := readOne(t, p.a, 2*time.Second); err != nil || string(got) != "from B" {
		t.Fatalf("A got %q err=%v", got, err)
	}
}

// A packet that the wrong key sealed, or no key at all, must vanish without any reaction.
func TestForeignPacketsAreDropped(t *testing.T) {
	p := newPair(t)
	scanner, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()

	plainQUIC := make([]byte, 1200)
	plainQUIC[0] = 0xC0 // a QUIC long header in the clear
	_, _ = scanner.WriteTo(plainQUIC, p.rb.LocalAddr())
	noise := make([]byte, 1200)
	_, _ = rand.Read(noise)
	_, _ = scanner.WriteTo(noise, p.rb.LocalAddr())

	if got, _, err := readOne(t, p.b, 300*time.Millisecond); err == nil {
		t.Fatalf("a foreign packet was delivered: %d bytes", len(got))
	}
	_ = scanner.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := scanner.ReadFrom(make([]byte, 64)); err == nil {
		t.Fatalf("the node answered a scanner with %d bytes", n)
	}

	// and it still serves legitimate traffic afterwards
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyB)
	_, _ = p.a.WriteTo([]byte("real"), p.rb.LocalAddr())
	if got, _, err := readOne(t, p.b, 2*time.Second); err != nil || string(got) != "real" {
		t.Fatalf("legitimate packet lost after probes: %q %v", got, err)
	}
}

// Dialing with the wrong ID for the address gets nowhere.
func TestWrongIDGetsNoAnswer(t *testing.T) {
	p := newPair(t)
	wrong := NodeKey("", idOf(9))
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), wrong)
	_, _ = p.a.WriteTo([]byte("ping"), p.rb.LocalAddr())
	if got, _, err := readOne(t, p.b, 300*time.Millisecond); err == nil {
		t.Fatalf("delivered %q to a node that was dialed with another node's key", got)
	}
}

// A peer that answers from another port of the same host is still understood.
func TestAnswerFromOtherPortOfTheSameHost(t *testing.T) {
	p := newPair(t)
	_ = p.oa.Expect(p.rb.LocalAddr().(*net.UDPAddr).AddrPort(), p.keyB)
	other, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	o2 := WrapPacketConn(other, p.ob)
	_, _ = o2.WriteTo([]byte("hi"), p.ra.LocalAddr()) // sealed with B's own key, from another port
	if got, _, err := readOne(t, p.a, 2*time.Second); err != nil || string(got) != "hi" {
		t.Fatalf("A got %q err=%v", got, err)
	}
}

func TestExpectIsBounded(t *testing.T) {
	o, k := newObfs(t, 1)
	for i := 0; i < maxPeerKeys+10; i++ {
		ap := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), 4242)
		if err := o.Expect(ap, k); err != nil {
			t.Fatal(err)
		}
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if len(o.peers) > maxPeerKeys {
		t.Fatalf("%d peer keys kept", len(o.peers))
	}
	if err := o.Expect(netip.MustParseAddrPort("1.2.3.4:5"), []byte("short")); err == nil {
		t.Fatal("a key of the wrong size was accepted")
	}
}
