package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func bootstrapPair(t *testing.T) (client, server *testNode, pair *memcarrier.Pair) {
	t.Helper()
	queue := newCarrierQueue()
	client = newTestNode(t, cards.Hidden, false, nil, nil)
	server = newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	server.start(t)
	client.start(t)
	pair = memcarrier.NewQUIC()
	queue.push(pair.Server)
	return client, server, pair
}

func testTarget(expected *node.DeviceID) node.BootstrapTarget {
	return node.BootstrapTarget{
		Addr:     &pb.Addr{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Port: 4430, Mask: 1},
		Expected: expected,
	}
}

// A bare address gives an XX session under a throwaway identity; the card it returns is a
// candidate and is nowhere in the client's tables (handshake.md section 4.2).
func TestDialBootstrapBareAddressIsUnverified(t *testing.T) {
	client, server, pair := bootstrapPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := client.rt.DialBootstrap(ctx, testTarget(nil), node.DialOpt{Carrier: pair.Client})
	if err != nil {
		t.Fatalf("DialBootstrap: %v", err)
	}
	if res.Verified {
		t.Fatal("a bare address must not be verified")
	}
	if res.Card == nil || res.Card.DeviceID != [32]byte(server.id.DeviceID) {
		t.Fatal("the result does not carry the responder card")
	}
	if client.rt.RoutingTable().Has(dht.DeviceID(server.id.DeviceID)) {
		t.Fatal("the card of a bare address is in the DHT table")
	}
	if client.rt.Cards().Peer(server.id.DeviceID) != nil {
		t.Fatal("the card of a bare address is in the card book")
	}
	if n := node.OpenSessions(client.rt); n != 0 {
		t.Fatalf("the bootstrap session stayed in the session table: %d", n)
	}
}

// The list names the device: the handshake checks it, and the result is verified.
func TestDialBootstrapExpectedDevice(t *testing.T) {
	client, server, pair := bootstrapPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	want := server.id.DeviceID
	res, err := client.rt.DialBootstrap(ctx, testTarget(&want), node.DialOpt{Carrier: pair.Client})
	if err != nil {
		t.Fatalf("DialBootstrap: %v", err)
	}
	if !res.Verified || res.Card.DeviceID != [32]byte(want) {
		t.Fatalf("verified=%v card=%v", res.Verified, res.Card != nil)
	}
}

// Another device answers on the address: the session fails and nothing is kept.
func TestDialBootstrapWrongDeviceFails(t *testing.T) {
	client, server, pair := bootstrapPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wrong := server.id.DeviceID
	wrong[0] ^= 0xff
	if _, err := client.rt.DialBootstrap(ctx, testTarget(&wrong), node.DialOpt{Carrier: pair.Client}); err == nil {
		t.Fatal("a wrong device answered and the dial succeeded")
	}
	if client.rt.RoutingTable().Has(dht.DeviceID(server.id.DeviceID)) || client.rt.Cards().Peer(server.id.DeviceID) != nil {
		t.Fatal("a failed bootstrap left the card behind")
	}
}

func TestDialBootstrapBadTarget(t *testing.T) {
	client, _, pair := bootstrapPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, tg := range map[string]node.BootstrapTarget{
		"no addr":       {},
		"no port":       {Addr: &pb.Addr{Suite: objects.SuiteNoise3, Host: "127.0.0.1", Mask: 1}},
		"ws2":           {Addr: &pb.Addr{Suite: objects.SuiteWS2, Url: "wss://example.invalid/x", Mask: 1}},
		"unknown suite": {Addr: &pb.Addr{Suite: 99, Host: "127.0.0.1", Port: 1, Mask: 1}},
	} {
		if _, err := client.rt.DialBootstrap(ctx, tg, node.DialOpt{Carrier: pair.Client}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
