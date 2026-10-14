package swarm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/require"
)

type mockLink struct {
	proto    router.TransportProtocol
	sentMsgs []sentRecord
	mu       sync.Mutex
	closed   bool
}

type sentRecord struct {
	msgType net_types.MessageType
	payload []byte
}

func newMockLink(proto router.TransportProtocol) *mockLink {
	return &mockLink{
		proto:    proto,
		sentMsgs: make([]sentRecord, 0),
	}
}

func (m *mockLink) Protocol() router.TransportProtocol {
	return m.proto
}

func (m *mockLink) Send(msgType net_types.MessageType, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentMsgs = append(m.sentMsgs, sentRecord{msgType: msgType, payload: payload})
	return nil
}

func (m *mockLink) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockLink) GetSent() []sentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]sentRecord, len(m.sentMsgs))
	copy(copied, m.sentMsgs)
	return copied
}

func TestPeer_HeterogeneousMultiTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := dispatcher.NewDispatcher(ctx)

	// Peer A: DirectQUIC
	var pkA types.PeerPublicKey
	copy(pkA[:], []byte("peer_A_public_key_32bytes_len!!!"))
	peerA := NewPeer(ctx, pkA, d, "1.1.1.1:4242", true)
	linkA := newMockLink(router.ProtoDirectQUIC)
	peerA.SetRouter(router.NewPeerTransportRouter(ctx, peerA.ID(), linkA))

	// Peer B: ObfsQUIC (Salamander DPI Bypass)
	var pkB types.PeerPublicKey
	copy(pkB[:], []byte("peer_B_public_key_32bytes_len!!!"))
	peerB := NewPeer(ctx, pkB, d, "2.2.2.2:4242", true)
	linkB := newMockLink(router.ProtoObfsQUIC)
	peerB.SetRouter(router.NewPeerTransportRouter(ctx, peerB.ID(), linkB))

	// Peer C: TCP/TLS Browser Mimicry (Port 443 Reality)
	var pkC types.PeerPublicKey
	copy(pkC[:], []byte("peer_C_public_key_32bytes_len!!!"))
	peerC := NewPeer(ctx, pkC, d, "3.3.3.3:443", true)
	linkC := newMockLink(router.ProtoTCPTLS)
	peerC.SetRouter(router.NewPeerTransportRouter(ctx, peerC.ID(), linkC))

	// 1. Verify that each peer has an independent active transport protocol
	require.Equal(t, router.ProtoDirectQUIC, peerA.ActiveProtocol(), "Peer A should be DirectQUIC")
	require.Equal(t, router.ProtoObfsQUIC, peerB.ActiveProtocol(), "Peer B should be ObfsQUIC")
	require.Equal(t, router.ProtoTCPTLS, peerC.ActiveProtocol(), "Peer C should be TCPTLS_Mimicry")

	// 2. Transmit data across each independent peer link
	err := peerA.SendRaw(net_types.TypePing, []byte("ping-A"))
	require.NoError(t, err)

	err = peerB.SendRaw(net_types.TypePing, []byte("ping-B"))
	require.NoError(t, err)

	err = peerC.SendRaw(net_types.TypePing, []byte("ping-C"))
	require.NoError(t, err)

	require.Len(t, linkA.GetSent(), 1)
	require.Equal(t, []byte("ping-A"), linkA.GetSent()[0].payload)

	require.Len(t, linkB.GetSent(), 1)
	require.Equal(t, []byte("ping-B"), linkB.GetSent()[0].payload)

	require.Len(t, linkC.GetSent(), 1)
	require.Equal(t, []byte("ping-C"), linkC.GetSent()[0].payload)

	// 3. Hot-Migration on Peer B: Failover from ObfsQUIC to TCPTLS
	linkB_Backup := newMockLink(router.ProtoTCPTLS)
	err = peerB.MigrateTransport(linkB_Backup, "TSPU UDP blackout detected")
	require.NoError(t, err)

	require.Equal(t, router.ProtoTCPTLS, peerB.ActiveProtocol(), "Peer B should migrate to TCPTLS")

	// Send message through Peer B after migration
	err = peerB.SendRaw(net_types.TypeChatMessage, []byte("hello through tcp"))
	require.NoError(t, err)

	sentOnBackup := linkB_Backup.GetSent()
	// Should have at least the data message sent over new link
	require.NotEmpty(t, sentOnBackup)

	// Verify Peer A and Peer C were completely unaffected by Peer B's migration
	require.Equal(t, router.ProtoDirectQUIC, peerA.ActiveProtocol(), "Peer A protocol must stay DirectQUIC")
	require.Equal(t, router.ProtoTCPTLS, peerC.ActiveProtocol(), "Peer C protocol must stay TCPTLS")

	peerA.Close()
	peerB.Close()
	peerC.Close()
}

func TestSwarm_TransportMigrationSignaling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Swarm{
		activePeers:  make(map[types.PeerID]*Peer),
		fastHandlers: make(map[net_types.MessageType]FastFrameHandler),
		ctx:          ctx,
		cancel:       cancel,
	}
	s.initCoreFastHandlers()

	// Create test peer in Swarm
	var pk types.PeerPublicKey
	copy(pk[:], []byte("remote_peer_pubkey_32bytes_len!!"))
	peerID := types.PeerPubKeyToID(pk)

	p := NewPeer(ctx, pk, dispatcher.NewDispatcher(ctx), "10.0.0.1:4242", true)
	link := newMockLink(router.ProtoDirectQUIC)
	p.SetRouter(router.NewPeerTransportRouter(ctx, peerID, link))

	s.activePeers[peerID] = p

	// Simulate receiving TypeTransportMigrateRequest frame from remote peer
	reqPayload := &router.MigrationPayload{
		TargetProtocol: router.ProtoObfsQUIC,
		SessionNonce:   998877,
		Timestamp:      time.Now().Unix(),
		Reason:         "DPI detected by peer",
	}
	reqBytes, err := reqPayload.Encode()
	require.NoError(t, err)

	handler := s.fastHandlers[net_types.TypeTransportMigrateRequest]
	require.NotNil(t, handler)

	err = handler(reqBytes, peerID)
	require.NoError(t, err)

	// Verify that swarm immediately responded with TypeTransportMigrateAck to peer
	sentRecords := link.GetSent()
	require.Len(t, sentRecords, 1)
	require.Equal(t, net_types.TypeTransportMigrateAck, sentRecords[0].msgType)

	ackDecoded, err := router.DecodeMigrationPayload(sentRecords[0].payload)
	require.NoError(t, err)
	require.Equal(t, router.ProtoObfsQUIC, ackDecoded.TargetProtocol)
	require.Equal(t, "acknowledged", ackDecoded.Reason)
}
