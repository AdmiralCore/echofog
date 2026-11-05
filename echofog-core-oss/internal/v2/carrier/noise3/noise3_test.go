package noise3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	mrand "math/rand/v2"
	"net"
	"os"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/carriertest"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

func newMaskKey(t *testing.T, now time.Time) (MaskKey, []byte) {
	t.Helper()
	var sk [32]byte
	if _, err := rand.Read(sk[:]); err != nil {
		t.Fatal(err)
	}
	pub, err := maskPublic(sk)
	if err != nil {
		t.Fatal(err)
	}
	return MaskKey{Private: sk, NotAfter: now.Add(time.Hour)}, pub[:]
}

func suiteParams(t *testing.T, id uint32) []byte {
	t.Helper()
	b, err := objects.Marshal(&pb.SuiteParams{Shape: &pb.ShapeProfile{Id: id}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func noise3Addr(t *testing.T, l *Listener, mask int32) *pb.Addr {
	t.Helper()
	port := l.Addr().(*net.UDPAddr).Port
	return &pb.Addr{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: uint32(port), Mask: uint32(mask), Params: suiteParams(t, shape.IDDefaultUDP)}
}

func cardWith(t *testing.T, pub []byte) *objects.Card {
	t.Helper()
	return &objects.Card{Body: &pb.NodeCard{MaskKeys: []*pb.MaskKey{{
		Key:      append([]byte{0x02}, pub...),
		NotAfter: uint64(time.Now().Add(time.Hour).Unix()),
	}}}}
}

// startNoise3 starts a listener and returns it with card/address helpers.
func startNoise3(t *testing.T) (*Listener, *objects.Card, *pb.Addr) {
	t.Helper()
	mk, pub := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{
		Keys:    []MaskKey{mk},
		Profile: mustProfile(t),
		Rand:    mrand.New(mrand.NewPCG(1, 2)),
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, cardWith(t, pub), noise3Addr(t, l, 1)
}

func mustProfile(t *testing.T) *shape.Profile {
	t.Helper()
	p, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newClient(t *testing.T) *Dialer {
	t.Helper()
	return NewDialer(DialerConfig{})
}

func TestListenerDialerStream(t *testing.T) {
	l, card, addr := startNoise3(t)
	d := newClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type dialResult struct {
		c   mux.Carrier
		err error
	}
	ch := make(chan dialResult, 1)
	go func() {
		c, err := d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
		ch <- dialResult{c, err}
	}()
	server, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	client := r.c
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	cs, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := cs.Write([]byte("noise3 hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cs.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	ss, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	got, err := io.ReadAll(ss)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte("noise3 hello")) {
		t.Fatalf("read %q", got)
	}

	if err := client.SendDatagram([]byte("dgram")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	got, err = server.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if !bytes.Equal(got, []byte("dgram")) {
		t.Fatalf("datagram = %q", got)
	}
}

func TestCarrierConformance(t *testing.T) {
	carriertest.Run(t, func(t *testing.T) (mux.Carrier, mux.Carrier) {
		l, card, addr := startNoise3(t)
		d := newClient(t)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		type res struct {
			c   mux.Carrier
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
			ch <- res{c, err}
		}()
		s, err := l.Accept(ctx)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		r := <-ch
		if r.err != nil {
			t.Fatalf("Dial: %v", r.err)
		}
		return r.c, s
	})
}

func TestShortInitGetsNoReply(t *testing.T) {
	mk, pub := newMaskKey(t, time.Now())
	_ = pub
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("listener answered %d bytes to a short Init", n)
	}
}

func TestGarbageInitGetsNoReply(t *testing.T) {
	mk, _ := newMaskKey(t, time.Now())
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: []MaskKey{mk}, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pkt := make([]byte, 1300)
	if _, err := rand.Read(pkt); err != nil {
		t.Fatal(err)
	}
	// Make the length plausible but leave the selector random.
	binary.BigEndian.PutUint32(pkt[48:52], 1)
	for i := 0; i < 20; i++ {
		_, _ = conn.Write(pkt)
	}
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("listener answered %d bytes to a garbage Init", n)
	}
}

func TestPreludeBeforeFirstInit(t *testing.T) {
	var maskSK [32]byte
	if _, err := rand.Read(maskSK[:]); err != nil {
		t.Fatal(err)
	}
	maskPub, err := maskPublic(maskSK)
	if err != nil {
		t.Fatal(err)
	}
	pc := newFakePC()
	profile := &shape.Profile{Junk: &shape.Junk{
		Count: shape.Range{Min: 1, Max: 1},
		Len:   shape.Range{Min: 32, Max: 32},
		Gap:   shape.Range{Min: 0, Max: 0},
	}}
	c := newDialConn(DialerConfig{},
		&fakeClock{t: time.Unix(5000, 0)}, profile, maskPub,
		func(context.Context) (net.PacketConn, net.Addr, error) {
			return pc, flowAddr{id: 3}, nil
		}, flowAddr{id: 3})
	c.conn = pc
	if _, err := c.write(make([]byte, 1200)); err != nil {
		t.Fatal(err)
	}
	writes := pc.snapshot()
	if len(writes) != 2 {
		t.Fatalf("wrote %d datagrams, want junk + Init", len(writes))
	}
	if len(writes[0]) != 32 {
		t.Fatalf("first datagram is %d bytes, want the 32-byte junk", len(writes[0]))
	}
	if len(writes[1]) < packet.MinInitLen {
		t.Fatalf("second datagram is %d bytes, want an Init", len(writes[1]))
	}
}

// TestLoopback1GiB is the throughput check of the task's "Готово": one gibibyte through a noise3
// carrier over loopback without errors. It is off by default because it moves real memory; run
// with NOISE3_1GIB=1.
func TestLoopback1GiB(t *testing.T) {
	if os.Getenv("NOISE3_1GIB") == "" {
		t.Skip("set NOISE3_1GIB=1 to run the one-gibibyte loopback check")
	}
	l, card, addr := startNoise3(t)
	d := newClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	type result struct {
		c   mux.Carrier
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.Dial(ctx, carrier.DialRequest{Card: card, Addr: addr})
		ch <- result{c, err}
	}()
	server, err := l.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("Dial: %v", r.err)
	}
	client := r.c
	defer client.Close()
	defer server.Close()

	cs, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const total = int64(1 << 30)
	const chunk = 64 << 10
	werr := make(chan error, 1)
	go func() {
		buf := make([]byte, chunk)
		for sent := int64(0); sent < total; {
			n := int64(chunk)
			if total-sent < n {
				n = total - sent
			}
			if _, err := cs.Write(buf[:n]); err != nil {
				werr <- err
				return
			}
			sent += n
		}
		_ = cs.CloseWrite()
		werr <- nil
	}()
	ss, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.CopyN(io.Discard, ss, total)
	if err != nil {
		select {
		case e := <-werr:
			t.Fatalf("read after %d bytes: %v (writer: %v)", n, err, e)
		default:
			t.Fatalf("read after %d bytes: %v", n, err)
		}
	}
	if n != total {
		t.Fatalf("read %d bytes, want %d", n, total)
	}
}
