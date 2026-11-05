package reality2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// The end-to-end client test runs without GODEBUG=cryptocustomrand (T12.6): the client picks
// its X25519 key itself and hands it to the token hook, so it does not depend on the toolchain
// honouring tls.Config.Rand.

// TestClientServerEndToEnd dials the listener through the gate and opens a stream: the token
// is admitted, the 4-tuple passes, QUIC completes and both sides agree on the channel
// binding. The cover never sees the client.
func TestClientServerEndToEnd(t *testing.T) {
	coverAddr := newCoverServer(t)
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	local := udp.LocalAddr().(*net.UDPAddr)
	addrStr := AddressString("127.0.0.1", uint32(local.Port))
	cert := testCertificate(t)

	ln, err := Listen(udp, ListenerConfig{
		Cover:     CoverProfile{Group: tls.X25519, Cert: &cert},
		Masks:     []MaskKey{{Private: sk}},
		Addrs:     []string{addrStr},
		CoverAddr: coverAddr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	accepted := make(chan mux.Carrier, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	card := testCard(t, sk)
	addr := testAddrPB(t, "127.0.0.1", uint32(local.Port), "cover.example")
	d := NewDialer(DialerConfig{Group: tls.X25519})
	c, err := d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	sc := <-accepted
	if sc == nil {
		t.Fatal("listener accepted no connection")
	}
	defer sc.Close()
	if sc.ChannelBinding() != c.ChannelBinding() {
		t.Fatal("channel bindings differ")
	}
	if c.IsClient() == sc.IsClient() {
		t.Fatal("both carriers report the same side")
	}

	st, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	ss, err := sc.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("read %q", buf)
	}
}

func testCard(t *testing.T, sk *ecdh.PrivateKey) *objects.Card {
	t.Helper()
	return &objects.Card{
		Body: &pb.NodeCard{
			MaskKeys: []*pb.MaskKey{{
				Key: keys.New(keys.X25519, sk.PublicKey().Bytes()),
			}},
		},
	}
}

func testAddrPB(t *testing.T, host string, port uint32, sni string) *pb.Addr {
	t.Helper()
	params, err := objects.Marshal(&pb.SuiteParams{Sni: sni})
	if err != nil {
		t.Fatal(err)
	}
	return &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: host, Port: port, Mask: 1, Params: params}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func newCoverServer(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(append([]byte("cover:"), buf[:n]...), src)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

// TestCoverPerSource checks that a stranger's packet reaches the cover and its reply comes
// back to that stranger.
func TestCoverPerSource(t *testing.T) {
	coverAddr := newCoverServer(t)
	replies := make(chan fakePacket, 4)
	c, err := NewUDPCover(CoverConfig{
		Addr:  coverAddr,
		Reply: func(p []byte, src net.Addr) { replies <- fakePacket{append([]byte(nil), p...), src} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	src := fakeAddr("198.51.100.1:1111")
	c.Forward(src, []byte("ping"))
	select {
	case r := <-replies:
		if !bytes.Equal(r.p, []byte("cover:ping")) || r.src.String() != src.String() {
			t.Fatalf("reply %q from %v", r.p, r.src)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no cover reply")
	}
}

// TestCoverSharedFallback checks the 1024-session limit: beyond it strangers share one socket
// and replies are routed by connection ID.
func TestCoverSharedFallback(t *testing.T) {
	cover, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, src, err := cover.ReadFromUDP(buf)
			if err != nil {
				return
			}
			// Echo a short-header reply whose DCID is the client's SCID.
			p := buf[:n]
			if len(p) < 8 || p[0]&0x80 == 0 {
				continue
			}
			q := 6 + int(p[5])
			if q >= len(p) {
				continue
			}
			scidLen := int(p[q])
			if q+1+scidLen > len(p) {
				continue
			}
			reply := append([]byte{0x41}, p[q+1:q+1+scidLen]...)
			reply = append(reply, '!')
			_, _ = cover.WriteToUDP(reply, src)
		}
	}()

	replies := make(chan fakePacket, 8)
	c, err := NewUDPCover(CoverConfig{
		Addr:        cover.LocalAddr().(*net.UDPAddr),
		Reply:       func(p []byte, src net.Addr) { replies <- fakePacket{append([]byte(nil), p...), src} },
		MaxSessions: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	first := fakeAddr("198.51.100.1:1")
	second := fakeAddr("198.51.100.2:2")
	c.Forward(first, clientInitialForCID([]byte{1, 2, 3, 4}))
	c.Forward(second, clientInitialForCID([]byte{5, 6, 7, 8}))

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-replies:
			seen[r.src.String()] = true
		case <-time.After(2 * time.Second):
			t.Fatal("no reply from the shared cover socket")
		}
	}
	if !seen[first.String()] || !seen[second.String()] {
		t.Fatalf("replies routed to %v, want both strangers", seen)
	}
}

// clientInitialForCID builds a minimal long header with the given source connection ID.
func clientInitialForCID(scid []byte) []byte {
	p := []byte{0xc0, 0, 0, 0, 1, 1, 0xaa, byte(len(scid))}
	p = append(p, scid...)
	return append(p, 0)
}

// TestReplayPersists checks the Bloom cache across instances through a KV.
func TestReplayPersists(t *testing.T) {
	kv := newMemKV()
	now := time.Unix(1793664000, 0)
	clock := func() time.Time { return now }
	r := NewReplay(ReplayConfig{KV: kv, Now: clock})
	token := []byte("a 32-byte token ................")
	mask := sha256Sum("mask")
	if r.SeenOrAdd(mask, token) {
		t.Fatal("first token seen")
	}
	if !r.SeenOrAdd(mask, token) {
		t.Fatal("same token not seen")
	}
	r.Flush()

	// A second process (or a restarted listener) loads the same filter.
	r2 := NewReplay(ReplayConfig{KV: kv, Now: clock})
	if !r2.SeenOrAdd(mask, token) {
		t.Fatal("replay not rejected after reload")
	}

	// After two rotations the token is forgotten (the cache holds the current and the
	// previous generation).
	now = now.Add(ReplayTTL)
	if r2.SeenOrAdd(mask, []byte("rotate-1")) {
		t.Fatal("fresh token seen")
	}
	now = now.Add(ReplayTTL)
	if r2.SeenOrAdd(mask, []byte("rotate-2")) {
		t.Fatal("fresh token seen")
	}
	if r2.SeenOrAdd(mask, token) {
		t.Fatal("token outlived two rotations")
	}
}

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: make(map[string][]byte)} }

func (kv *memKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	v, ok := kv.m[string(key)]
	return append([]byte(nil), v...), ok, nil
}

func (kv *memKV) Put(_ context.Context, key, value []byte) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.m[string(key)] = append([]byte(nil), value...)
	return nil
}

func sha256Sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}
