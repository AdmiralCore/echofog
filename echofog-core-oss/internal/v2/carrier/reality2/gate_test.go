package reality2

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

// fakePacketConn is an in-memory net.PacketConn for the gate.
type fakePacketConn struct {
	in  chan fakePacket
	out chan fakePacket

	mu     sync.Mutex
	writes int
	reads  int
	closed chan struct{}
	once   sync.Once
}

type fakePacket struct {
	p   []byte
	src net.Addr
}

func newFakeConn() *fakePacketConn {
	return &fakePacketConn{
		in:     make(chan fakePacket, 64),
		out:    make(chan fakePacket, 64),
		closed: make(chan struct{}),
	}
}

func (c *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	select {
	case pkt := <-c.in:
		return copy(p, pkt.p), pkt.src, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *fakePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	select {
	case c.out <- fakePacket{append([]byte(nil), p...), addr}:
	default:
	}
	return len(p), nil
}

func (c *fakePacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *fakePacketConn) LocalAddr() net.Addr              { return fakeAddr("127.0.0.1:443") }
func (c *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakePacketConn) writeCount() int                  { c.mu.Lock(); defer c.mu.Unlock(); return c.writes }
func (c *fakePacketConn) readCount() int                   { c.mu.Lock(); defer c.mu.Unlock(); return c.reads }

// fakeCover records what the gate forwards.
type fakeCover struct {
	mu   sync.Mutex
	pkts [][]byte
	srcs []net.Addr
}

func (c *fakeCover) Forward(src net.Addr, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pkts = append(c.pkts, append([]byte(nil), p...))
	c.srcs = append(c.srcs, src)
}

func (c *fakeCover) Close() error { return nil }

func (c *fakeCover) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pkts)
}

type fakeAddr string

func (a fakeAddr) Network() string { return "udp" }
func (a fakeAddr) String() string  { return string(a) }

const testAddr = "192.0.2.10:443"

// newTestGate builds a gate with one masking key and one token address.
func newTestGate(t *testing.T, conn net.PacketConn, cover Cover, replay *Replay, maxPending int) (*Gate, *ecdh.PrivateKey) {
	t.Helper()
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if replay == nil {
		replay = NewReplay(ReplayConfig{})
	}
	g, err := NewGate(conn, GateConfig{
		Masks:      []MaskKey{{Private: sk}},
		Addrs:      []string{testAddr},
		Cover:      cover,
		Replay:     replay,
		MaxPending: maxPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, sk
}

// tokenHello builds a ClientHello carrying a valid token for addr and a fresh X25519 share.
func tokenHello(t *testing.T, mask *ecdh.PrivateKey, addr string, ts time.Time) []byte {
	t.Helper()
	hello, _, err := tokenHelloAt(t, mask, addr, ts)
	if err != nil {
		t.Fatal(err)
	}
	return hello
}

func tokenHelloAt(t *testing.T, mask *ecdh.PrivateKey, addr string, ts time.Time) ([]byte, *ecdh.PrivateKey, error) {
	t.Helper()
	c, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	maskPub := mask.PublicKey()
	z, err := c.ECDH(maskPub)
	if err != nil {
		return nil, nil, err
	}
	C := c.PublicKey().Bytes()
	ch0 := syntheticHello(t, nil, keyShareX25519(C))
	key := TokenKey(z, maskPub.Bytes(), C, addr)
	if key == nil {
		return nil, nil, errors.New("no token key")
	}
	tok, err := SealToken(key, HelloAAD(ch0), uint32(ts.Unix()))
	if err != nil {
		return nil, nil, err
	}
	return syntheticHello(t, tok, keyShareX25519(C)), c, nil
}

// initialFlight seals hello into two QUIC v1 Initial packets, splitting the CRYPTO stream so
// the gate must reassemble it.
func initialFlight(t *testing.T, hello []byte) [][]byte {
	t.Helper()
	split := len(hello) / 2
	dcid := bytes.Repeat([]byte{0x11}, 8)
	scid := bytes.Repeat([]byte{0x22}, 8)
	first := cryptoFrame(0, hello[:split])
	second := cryptoFrame(split, hello[split:])
	return [][]byte{
		sealInitial(t, dcid, scid, 0, first),
		sealInitial(t, dcid, scid, 1, second),
	}
}

func cryptoFrame(off int, b []byte) []byte {
	out := []byte{0x06}
	out = appendVarint(out, uint64(off))
	out = appendVarint(out, uint64(len(b)))
	return append(out, b...)
}

func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(v>>8)|0x40, byte(v))
	case v < 1<<30:
		return append(b, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, byte(v>>56)|0xc0, byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

func sealInitial(t *testing.T, dcid, scid []byte, pn uint64, plain []byte) []byte {
	t.Helper()
	secret := hkdfExpandLabel(hkdf.Extract(sha256.New, dcid, quicV1Salt), "client in", 32)
	key := hkdfExpandLabel(secret, "quic key", 16)
	iv := hkdfExpandLabel(secret, "quic iv", 12)
	hp := hkdfExpandLabel(secret, "quic hp", 16)

	hdr := []byte{0xc0, 0, 0, 0, 1, byte(len(dcid))}
	hdr = append(hdr, dcid...)
	hdr = append(hdr, byte(len(scid)))
	hdr = append(hdr, scid...)
	hdr = appendVarint(hdr, 0) // token length
	hdr = appendVarint(hdr, uint64(1+len(plain)+16))
	pnOff := len(hdr)
	hdr = append(hdr, byte(pn))

	nonce := make([]byte, 12)
	copy(nonce, iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		t.Fatal(err)
	}
	out := aead.Seal(append([]byte(nil), hdr...), nonce, plain, hdr)

	hpb, err := aes.NewCipher(hp)
	if err != nil {
		t.Fatal(err)
	}
	mask := make([]byte, 16)
	hpb.Encrypt(mask, out[pnOff+4:pnOff+20])
	out[0] ^= mask[0] & 0x0f
	out[pnOff] ^= mask[1]
	return out
}

// readGate reads one packet from the gate in a goroutine with a timeout.
func readGate(t *testing.T, g *Gate) ([]byte, net.Addr, bool) {
	t.Helper()
	type result struct {
		n   int
		src net.Addr
		err error
	}
	ch := make(chan result, 1)
	buf := make([]byte, 64<<10)
	go func() {
		n, src, err := g.ReadFrom(buf)
		ch <- result{n, src, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, nil, false
		}
		return append([]byte(nil), buf[:r.n]...), r.src, true
	case <-time.After(2 * time.Second):
		return nil, nil, false
	}
}

// TestGateAdmitsValidToken checks the whole admission path: two Initials are buffered, the
// ClientHello is reassembled, the token verifies, the 4-tuple is admitted and both packets
// are handed to the QUIC stack in order. The cover sees nothing.
func TestGateAdmitsValidToken(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	g, mask := newTestGate(t, conn, cover, nil, 0)
	now := time.Unix(1793664000, 0)
	g.now = func() time.Time { return now }

	src := fakeAddr("198.51.100.7:5555")
	flight := initialFlight(t, tokenHello(t, mask, testAddr, now))
	for _, p := range flight {
		conn.in <- fakePacket{p, src}
	}

	first, gotSrc, ok := readGate(t, g)
	if !ok {
		t.Fatal("first packet was not delivered")
	}
	if gotSrc.String() != src.String() || !bytes.Equal(first, flight[0]) {
		t.Fatal("first delivered packet is not the first Initial")
	}
	second, _, ok := readGate(t, g)
	if !ok || !bytes.Equal(second, flight[1]) {
		t.Fatal("second Initial was not delivered in order")
	}
	if cover.count() != 0 {
		t.Fatalf("cover got %d packets for an admitted source", cover.count())
	}
	if conn.writeCount() != 0 {
		t.Fatal("the gate itself wrote to the network")
	}

	// Once admitted, a short-header packet from the same 4-tuple goes straight through.
	short := []byte{0x41, 0x00, 0x01, 0x02, 0x03}
	conn.in <- fakePacket{short, src}
	got, _, ok := readGate(t, g)
	if !ok || !bytes.Equal(got, short) {
		t.Fatal("admitted 4-tuple was not passed through")
	}
}

// TestGateStrangersToCover checks every rejection path of wire.md §3.3: the flight goes to
// the cover and the node sends nothing to the stranger.
func TestGateStrangersToCover(t *testing.T) {
	now := time.Unix(1793664000, 0)
	cases := []struct {
		name  string
		build func(t *testing.T, mask *ecdh.PrivateKey) [][]byte
	}{
		{"token for another address", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return initialFlight(t, tokenHello(t, mask, "198.51.100.9:443", now))
		}},
		{"old timestamp", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return initialFlight(t, tokenHello(t, mask, testAddr, now.Add(-301*time.Second)))
		}},
		{"future timestamp", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return initialFlight(t, tokenHello(t, mask, testAddr, now.Add(301*time.Second)))
		}},
		{"garbage", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return [][]byte{bytes.Repeat([]byte{0xa5}, 200)}
		}},
		{"not a v1 initial", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			p := sealInitial(t, bytes.Repeat([]byte{1}, 8), bytes.Repeat([]byte{2}, 8), 0, []byte{0x06, 0x00, 0x00})
			binary.BigEndian.PutUint32(p[1:5], 0xbabababa)
			return [][]byte{p}
		}},
		{"hello without a key share", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return initialFlight(t, syntheticHello(t, make([]byte, 32), nil))
		}},
		{"hello with a zero random", func(t *testing.T, mask *ecdh.PrivateKey) [][]byte {
			return initialFlight(t, syntheticHello(t, nil, keyShareX25519(make([]byte, 32))))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeConn()
			cover := &fakeCover{}
			g, mask := newTestGate(t, conn, cover, nil, 0)
			g.now = func() time.Time { return now }
			src := fakeAddr("203.0.113.50:7777")
			flight := tc.build(t, mask)
			startPump(t, g)
			for _, p := range flight {
				conn.in <- fakePacket{p, src}
			}
			// After the gate has forwarded the flight, it reads again: no packet was
			// handed up, and the node itself wrote nothing.
			waitReads(t, conn, len(flight)+1)
			waitCount(t, cover, len(flight))
			if conn.writeCount() != 0 {
				t.Fatal("the node answered a stranger")
			}
		})
	}
}

// TestGateReplay checks that a captured hello cannot be used twice: the second flight goes to
// the cover even though its token is valid.
func TestGateReplay(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	replay := NewReplay(ReplayConfig{})
	g, mask := newTestGate(t, conn, cover, replay, 0)
	now := time.Unix(1793664000, 0)
	g.now = func() time.Time { return now }

	hello := tokenHello(t, mask, testAddr, now)
	src := fakeAddr("198.51.100.8:4444")
	for _, p := range initialFlight(t, hello) {
		conn.in <- fakePacket{p, src}
	}
	if _, _, ok := readGate(t, g); !ok {
		t.Fatal("first flight was not admitted")
	}

	// The same hello from another source: the token is a replay.
	conn2 := newFakeConn()
	g2, _ := newTestGate(t, conn2, cover, replay, 0)
	g2.masks = g.masks
	g2.now = func() time.Time { return now }
	src2 := fakeAddr("198.51.100.8:4445")
	startPump(t, g2)
	for _, p := range initialFlight(t, hello) {
		conn2.in <- fakePacket{p, src2}
	}
	waitCount(t, cover, 2)
	if conn2.writeCount() != 0 {
		t.Fatal("the node answered a replayed hello")
	}
}

// TestGateLimits checks the two buffers of wire.md §3.3 step 2: a partial flight is evicted
// when the pending table is full, and a source over 8 KiB is evicted at once.
func TestGateLimits(t *testing.T) {
	now := time.Unix(1793664000, 0)
	hello := tokenHello(t, mustMask(t), testAddr, now)

	t.Run("pending table full", func(t *testing.T) {
		conn := newFakeConn()
		cover := &fakeCover{}
		g, mask := newTestGate(t, conn, cover, nil, 1)
		g.now = func() time.Time { return now }
		_ = mask
		partial := initialFlight(t, hello)[0]
		startPump(t, g)

		conn.in <- fakePacket{partial, fakeAddr("198.51.100.1:1")}
		waitReads(t, conn, 2)
		conn.in <- fakePacket{partial, fakeAddr("198.51.100.2:2")}
		// The second arrival evicts the first pending flight to the cover.
		waitCount(t, cover, 1)
		if conn.writeCount() != 0 {
			t.Fatal("the node wrote to a stranger")
		}
	})

	t.Run("per-source bytes", func(t *testing.T) {
		conn := newFakeConn()
		cover := &fakeCover{}
		replay := NewReplay(ReplayConfig{})
		g, err := NewGate(conn, GateConfig{
			Masks:           []MaskKey{{Private: mustMask(t)}},
			Addrs:           []string{testAddr},
			Cover:           cover,
			Replay:          replay,
			MaxPendingBytes: 64,
			Now:             func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		startPump(t, g)
		p := initialFlight(t, hello)[0]
		conn.in <- fakePacket{p, fakeAddr("198.51.100.3:3")}
		waitCount(t, cover, 1)
		if conn.writeCount() != 0 {
			t.Fatal("the node wrote to a stranger")
		}
		_ = g
	})
}

// TestGateWriteToDropsStrangers checks the V1 rule directly: the QUIC stack cannot make the
// gate send to an address that was never admitted.
func TestGateWriteToDropsStrangers(t *testing.T) {
	conn := newFakeConn()
	g, _ := newTestGate(t, conn, &fakeCover{}, nil, 0)
	if n, err := g.WriteTo([]byte("hello"), fakeAddr("203.0.113.99:1")); err != nil || n != 5 {
		t.Fatalf("WriteTo to a stranger: n=%d err=%v", n, err)
	}
	if conn.writeCount() != 0 {
		t.Fatal("WriteTo reached the network for a stranger")
	}
}

// startPump drains the gate as the QUIC stack would, so stranger packets are processed and
// the fake connection's read count advances. It stops when the connection closes.
func startPump(t *testing.T, g *Gate) {
	t.Helper()
	buf := make([]byte, 64<<10)
	go func() {
		for {
			if _, _, err := g.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
}

func mustMask(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func waitReads(t *testing.T, conn *fakePacketConn, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for conn.readCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("gate read %d packets, want at least %d", conn.readCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitCount(t *testing.T, cover *fakeCover, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for cover.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("cover got %d packets, want at least %d", cover.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestParseClientInitial checks the Initial decoder against a packet sealed by the test
// encoder, and that a corrupted packet is rejected.
func TestParseClientInitial(t *testing.T) {
	hello := syntheticHello(t, nil, keyShareX25519(make([]byte, 32)))
	pkt := sealInitial(t, bytes.Repeat([]byte{0x33}, 8), bytes.Repeat([]byte{0x44}, 8), 0, cryptoFrame(0, hello))
	init, err := parseClientInitial(pkt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := assembledClientHello(init.frags)
	if !ok || !bytes.Equal(got, hello) {
		t.Fatal("reassembled ClientHello differs")
	}
	bad := append([]byte(nil), pkt...)
	bad[len(bad)-1] ^= 0xff
	if _, err := parseClientInitial(bad); err == nil {
		t.Fatal("corrupted Initial parsed")
	}
	if _, err := parseClientInitial([]byte{1, 2, 3}); err == nil {
		t.Fatal("short packet parsed")
	}
}

// FuzzGatePacketParser feeds arbitrary bytes to both parsers the gate runs on wire data.
func FuzzGatePacketParser(f *testing.F) {
	hello := syntheticHello(f, nil, keyShareX25519(make([]byte, 32)))
	f.Add(sealInitialF(f, bytes.Repeat([]byte{0x33}, 8), bytes.Repeat([]byte{0x44}, 8), 0, cryptoFrame(0, hello)))
	f.Add(hello)
	f.Add([]byte{0xc0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, p []byte) {
		if init, err := parseClientInitial(p); err == nil {
			_, _ = assembledClientHello(init.frags)
		}
		_, _ = ParseHello(p)
	})
}

// sealInitialF is sealInitial for fuzz seeds, where a testing.TB is not available.
func sealInitialF(tb testing.TB, dcid, scid []byte, pn uint64, plain []byte) []byte {
	tb.Helper()
	secret := hkdfExpandLabel(hkdf.Extract(sha256.New, dcid, quicV1Salt), "client in", 32)
	key := hkdfExpandLabel(secret, "quic key", 16)
	iv := hkdfExpandLabel(secret, "quic iv", 12)
	hp := hkdfExpandLabel(secret, "quic hp", 16)
	hdr := []byte{0xc0, 0, 0, 0, 1, byte(len(dcid))}
	hdr = append(hdr, dcid...)
	hdr = append(hdr, byte(len(scid)))
	hdr = append(hdr, scid...)
	hdr = appendVarint(hdr, 0)
	hdr = appendVarint(hdr, uint64(1+len(plain)+16))
	pnOff := len(hdr)
	hdr = append(hdr, byte(pn))
	nonce := make([]byte, 12)
	copy(nonce, iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(kb)
	out := aead.Seal(append([]byte(nil), hdr...), nonce, plain, hdr)
	hpb, _ := aes.NewCipher(hp)
	mask := make([]byte, 16)
	hpb.Encrypt(mask, out[pnOff+4:pnOff+20])
	out[0] ^= mask[0] & 0x0f
	out[pnOff] ^= mask[1]
	return out
}

var _ = io.ReadFull
