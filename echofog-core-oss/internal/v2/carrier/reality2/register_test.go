package reality2

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func TestRegister(t *testing.T) {
	reg := carrier.NewRegistry()
	d := NewDialer(DialerConfig{})
	if err := Register(reg, d, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.DialerFor(objects.SuiteReality2QUIC); !ok {
		t.Fatal("dialer not registered under the suite number")
	}
	if _, ok := reg.ListenerFor(objects.SuiteReality2QUIC); ok {
		t.Fatal("listener registered though nil")
	}
	if err := Register(nil, d, nil); err == nil {
		t.Fatal("nil registry accepted")
	}
	if err := Register(carrier.NewRegistry(), nil, nil); err == nil {
		t.Fatal("empty registration accepted")
	}
}

func TestListenerRequiresCover(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Listen(conn, ListenerConfig{CoverAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}}); !errors.Is(err, ErrCover) {
		t.Fatalf("listener without a cover: %v, want ErrCover", err)
	}
}

func TestDialerRejectsUnusableAddress(t *testing.T) {
	d := NewDialer(DialerConfig{})
	card := &objects.Card{Body: &pb.NodeCard{}}
	if _, err := d.Dial(context.Background(), carrier.DialRequest{
		Card: card,
		Addr: &pb.Addr{Suite: objects.SuiteReality2TCP, Host: "192.0.2.1", Port: 443, Mask: 1},
	}); !errors.Is(err, ErrCard) {
		t.Fatalf("wrong suite: %v", err)
	}
	if _, err := d.Dial(context.Background(), carrier.DialRequest{
		Card: card,
		Addr: &pb.Addr{Suite: objects.SuiteReality2QUIC, Host: "192.0.2.1", Port: 443, Mask: 1},
	}); !errors.Is(err, ErrCard) {
		t.Fatalf("missing mask key: %v", err)
	}
}

// TestGateTriesEveryAddress checks wire.md §3.2: the server tries each address of its live
// cards that uses the masking key, so a token minted for the second address is accepted.
func TestGateTriesEveryAddress(t *testing.T) {
	conn := newFakeConn()
	cover := &fakeCover{}
	sk := mustMask(t)
	g, err := NewGate(conn, GateConfig{
		Masks:  []MaskKey{{Private: sk}},
		Addrs:  []string{"192.0.2.10:443", "192.0.2.11:443"},
		Cover:  cover,
		Replay: NewReplay(ReplayConfig{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1793664000, 0)
	g.now = func() time.Time { return now }
	src := fakeAddr("198.51.100.4:4")
	for _, p := range initialFlight(t, tokenHello(t, sk, "192.0.2.11:443", now)) {
		conn.in <- fakePacket{p, src}
	}
	if _, _, ok := readGate(t, g); !ok {
		t.Fatal("token for the second address was not admitted")
	}
	if cover.count() != 0 {
		t.Fatal("admitted flight went to the cover")
	}
}
