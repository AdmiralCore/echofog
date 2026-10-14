package router

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type mockLink struct {
	proto    TransportProtocol
	mu       sync.Mutex
	sent     []BufferedPacket
	isClosed bool
}

func newMockLink(p TransportProtocol) *mockLink {
	return &mockLink{
		proto: p,
		sent:  make([]BufferedPacket, 0),
	}
}

func (m *mockLink) Protocol() TransportProtocol {
	return m.proto
}

func (m *mockLink) Send(msgType net_types.MessageType, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, BufferedPacket{
		MsgType: msgType,
		Payload: append([]byte(nil), payload...),
	})
	return nil
}

func (m *mockLink) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isClosed = true
	return nil
}

func (m *mockLink) GetSent() []BufferedPacket {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BufferedPacket(nil), m.sent...)
}

func TestPeerTransportRouter_SeamlessMigration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dummyPeerID types.PeerID
	copy(dummyPeerID[:], []byte("test_peer_destination_id_32b___"))

	linkQUIC := newMockLink(ProtoDirectQUIC)
	router := NewPeerTransportRouter(ctx, dummyPeerID, linkQUIC)
	defer router.Close()

	if router.ActiveProtocol() != ProtoDirectQUIC {
		t.Fatalf("expected active proto DirectQUIC, got %v", router.ActiveProtocol())
	}

	// 1. Send normal packet over DirectQUIC
	msg1 := []byte("Initial message over DirectQUIC")
	if err := router.Send(net_types.TypeChatMessage, msg1); err != nil {
		t.Fatalf("failed to send initial message: %v", err)
	}

	sentOnQUIC := linkQUIC.GetSent()
	if len(sentOnQUIC) != 1 || !bytes.Equal(sentOnQUIC[0].Payload, msg1) {
		t.Fatalf("message not delivered to QUIC link")
	}

	// 2. Prepare new link: TCPTLS (masqueraded HTTPS)
	linkTCP := newMockLink(ProtoTCPTLS)

	// Set state to migrating manually to simulate active migration window
	router.state.Store(int32(StateMigrating))

	// Send 5 messages while migration is in-flight (network socket being switched)
	for i := 0; i < 5; i++ {
		packetData := []byte("In-flight buffered message during migration")
		if err := router.Send(net_types.TypeChatMessage, packetData); err != nil {
			t.Fatalf("send during migration failed: %v", err)
		}
	}

	if router.buffer.Len() != 5 {
		t.Fatalf("expected 5 buffered packets, got %d", router.buffer.Len())
	}

	// 3. Complete migration to TCPTLS
	if err := router.InitiateMigration(linkTCP, "Simulated DPI throttling on UDP"); err != nil {
		t.Fatalf("InitiateMigration failed: %v", err)
	}

	if router.ActiveProtocol() != ProtoTCPTLS {
		t.Fatalf("expected active proto TCPTLS after migration, got %v", router.ActiveProtocol())
	}

	if router.State() != StateActive {
		t.Fatalf("expected router state Active, got %v", router.State())
	}

	// Verify that all 5 in-flight packets were drained into linkTCP with zero loss
	sentOnTCP := linkTCP.GetSent()
	// Total sent on TCP: 5 drained packets
	if len(sentOnTCP) != 5 {
		t.Fatalf("expected 5 packets on TCP link, got %d", len(sentOnTCP))
	}

	// Verify buffer is now empty
	if router.buffer.Len() != 0 {
		t.Fatalf("expected empty buffer after drain, got %d", router.buffer.Len())
	}
}

func TestInFlightBuffer_FIFO_Order(t *testing.T) {
	buf := NewInFlightBuffer(10)
	defer buf.Close()

	for i := 0; i < 5; i++ {
		_ = buf.Enqueue(net_types.TypeChatMessage, []byte{byte(i)})
	}

	if buf.Len() != 5 {
		t.Fatalf("expected len 5, got %d", buf.Len())
	}

	drainTarget := newMockLink(ProtoTCPTLS)
	drained, err := buf.Drain(drainTarget)
	if err != nil {
		t.Fatalf("drain error: %v", err)
	}
	if drained != 5 {
		t.Fatalf("expected 5 drained, got %d", drained)
	}

	sent := drainTarget.GetSent()
	for i := 0; i < 5; i++ {
		if sent[i].Payload[0] != byte(i) {
			t.Fatalf("FIFO violation at index %d: expected %d, got %d", i, i, sent[i].Payload[0])
		}
	}
}
