package swarm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func testSwarmInit(t *testing.T, addr string) (context.Context, *Swarm, *network.QuicTransport, *testutil.TestingBadgerStorage) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	im, testStore := testutil.SetupTestIdentity(t)

	account, err := im.GetActiveAccount()
	if err != nil {
		t.Fatalf("Failed to get active account: %v", err)
	}

	privKeyBytes, pubKeyBytes, err := account.GetIdentityKey()
	if err != nil {
		t.Fatalf("Failed to get identity key: %v", err)
	}

	lockedPriv, err := privKeyBytes.Open()
	if err != nil {
		t.Fatalf("Failed to open private key enclave: %v", err)
	}
	defer lockedPriv.Destroy()
	var peerPrivKey types.PeerPrivateKey
	copy(peerPrivKey[:32], lockedPriv.Bytes())
	copy(peerPrivKey[32:], pubKeyBytes)

	var peerPubKey types.PeerPublicKey
	copy(peerPubKey[:], pubKeyBytes)

	peerID := types.PeerPubKeyToID(peerPubKey)

	_, tls := testutil.GetRandomKeyAndTls(t)
	tr, err := network.NewQUICTransport(addr, tls, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}

	if err := tr.ListenEarly(ctx); err != nil {
		t.Fatalf("Failed to start transport: %v", err)
	}

	d := dispatcher.NewDispatcher(ctx)
	go d.Start()

	sw := NewSwarm(ctx, tr, peerID, d, im, tr.ConnChan(), testStore.St, config.DefaultConfig(""))

	return ctx, sw, tr, testStore
}

func TestSwarm_Connection(t *testing.T) {
	ctxOne, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	addrOne := trOne.Addr().String()

	err := trTwo.DialEarly(ctxTwo, addrOne)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for peers to connect")
		case <-ticker.C:
			if len(swOne.GetAllPeers()) > 0 && len(swTwo.GetAllPeers()) > 0 {
				t.Log("Peers successfully connected")
				return
			}
		case <-ctxOne.Done():
			return
		case <-ctxTwo.Done():
			return
		}
	}
}

func TestSwarm_Messaging(t *testing.T) {
	_, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	msgReceived := make(chan *internal_pb.MessageData, 1)
	handler := &mockHandler{callback: func(msg *internal_pb.MessageData) {
		msgReceived <- msg
	}}
	swTwo.dispatcher.Registry(&internal_pb.MessageData_Ping{}, handler)

	addrOne := trOne.Addr().String()
	trTwo.DialEarly(ctxTwo, addrOne)

	time.Sleep(500 * time.Millisecond)

	peers := swOne.GetAllPeers()
	if len(peers) == 0 {
		t.Fatal("Swarm One has no peers")
	}
	targetID := peers[0].ID()

	testMsg := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_Ping{
			Ping: &internal_pb.Ping{Nonce: 12345},
		},
	}

	err := swOne.SendDataForPeer(targetID, net_types.TypePing, testMsg)
	if err != nil {
		t.Fatalf("Failed to send data: %v", err)
	}

	select {
	case msg := <-msgReceived:
		ping := msg.GetPing()
		if ping.Nonce != 12345 {
			t.Errorf("Expected nonce 12345, got %d", ping.Nonce)
		}
		t.Log("Message successfully received and verified")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for message")
	}
}

func TestSwarm_Banning(t *testing.T) {
	_, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	addrOne := trOne.Addr().String()
	trTwo.DialEarly(ctxTwo, addrOne)

	time.Sleep(500 * time.Millisecond)
	if len(swOne.GetAllPeers()) == 0 {
		t.Fatal("Peers didn't connect")
	}

	peerIDTwo := swTwo.selfID

	swOne.BanPeer(peerIDTwo)

	time.Sleep(200 * time.Millisecond)

	if len(swOne.GetAllPeers()) > 0 {
		t.Errorf("Peer was not removed from map after banning")
	}

	trTwo.DialEarly(ctxTwo, addrOne)
	time.Sleep(200 * time.Millisecond)

	if len(swOne.GetAllPeers()) > 0 {
		t.Errorf("Banned peer was allowed to reconnect")
	}

	if !swOne.CheckOnBan(peerIDTwo) {
		t.Error("Peer ID not found in ban list")
	} else {
		t.Log("Peer is correctly identified as banned and reconnection was rejected")
	}
}

func TestSwarm_OpenStreams(t *testing.T) {
	ctxOne, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	addrOne := trOne.Addr().String()
	trTwo.DialEarly(ctxTwo, addrOne)

	time.Sleep(500 * time.Millisecond)
	peers := swOne.GetAllPeers()
	if len(peers) == 0 {
		t.Fatal("No peers connected")
	}
	targetID := peers[0].ID()

	streams, err := swOne.OpenNewStreams(ctxOne, targetID, 3)
	if err != nil {
		t.Fatalf("Failed to open streams: %v", err)
	}
	defer func() {
		for _, s := range streams {
			s.Close()
		}
	}()

	if len(streams) != 3 {
		t.Errorf("Expected 3 streams, got %d", len(streams))
	}

	for i, s := range streams {
		if s == nil {
			t.Errorf("Stream %d is nil", i)
		}
	}
	t.Log("Successfully opened and verified multiple streams")
}

func TestSwarm_Concurrency(t *testing.T) {
	numNodes := 5
	_, swMaster, trMaster, storeMaster := testSwarmInit(t, "127.0.0.1:0")
	defer swMaster.Close()
	defer trMaster.Close()
	defer storeMaster.CloseAndClear()

	masterAddr := trMaster.Addr().String()
	var wg sync.WaitGroup

	for i := 0; i < numNodes; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, sw, tr, store := testSwarmInit(t, "127.0.0.1:0")
			defer sw.Close()
			defer tr.Close()
			defer store.CloseAndClear()

			err := tr.DialEarly(ctx, masterAddr)
			if err != nil {
				return
			}
			time.Sleep(2 * time.Second)
		}(i)
	}

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			wg.Wait()
			t.Fatalf("Only %d/%d nodes connected to master", len(swMaster.GetAllPeers()), numNodes)
		case <-ticker.C:
			if len(swMaster.GetAllPeers()) >= numNodes {
				t.Logf("All %d nodes successfully connected concurrently", numNodes)
				wg.Wait()
				return
			}
		}
	}
}

func TestSwarm_SendMessageWithRequest(t *testing.T) {
	ctxOne, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	_ = ctxOne

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	swTwo.RegisterRPC(net_types.TypePing, func(msg *internal_pb.MessageData, peerID types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error) {
		ping := msg.GetPing()
		if ping == nil {
			return 0, nil, errors.New("received invalid payload, expected Ping")
		}

		t.Logf("Bob (swTwo) received Ping with Nonce: %d", ping.Nonce)

		resp := &internal_pb.MessageData{
			MessageId: msg.MessageId,
			Payload: &internal_pb.MessageData_Pong{
				Pong: &internal_pb.Pong{Nonce: ping.Nonce},
			},
		}

		return net_types.TypePong, resp, nil
	})

	addrOne := trOne.Addr().String()
	if err := trTwo.DialEarly(ctxTwo, addrOne); err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	peers := swOne.GetAllPeers()
	if len(peers) == 0 {
		t.Fatal("Swarm One has no connected peers")
	}
	targetID := peers[0].ID()

	testRequest := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_Ping{
			Ping: &internal_pb.Ping{Nonce: 777888},
		},
	}

	t.Log("Alice (swOne) is sending RPC request...")

	respType, respMsg, err := swOne.SendMessageWithRequest(targetID, net_types.TypePing, testRequest)
	if err != nil {
		t.Fatalf("SendMessageWithRequest failed: %v", err)
	}

	if respType != net_types.TypePong {
		t.Errorf("Expected response type %v, got %v", net_types.TypePong, respType)
	}

	pong := respMsg.GetPong()
	if pong == nil {
		t.Fatal("Response payload is not Pong")
	}

	if pong.Nonce != 777888 {
		t.Errorf("Nonce mismatch! Sent 777888, received %d", pong.Nonce)
	}

	t.Log("SUCCESS: Request-Response cycle completed perfectly via Stream RPC!")
}

func TestSwarm_FastPathRouting(t *testing.T) {
	ctxOne, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	addrOne := trOne.Addr().String()

	err := trTwo.DialEarly(ctxTwo, addrOne)
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}

	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	connected := false
	for !connected {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for peers to connect")
		case <-ticker.C:
			if len(swOne.GetAllPeers()) > 0 && len(swTwo.GetAllPeers()) > 0 {
				connected = true
			}
		case <-ctxOne.Done():
			return
		case <-ctxTwo.Done():
			return
		}
	}

	var fastPathReceived atomic.Bool
	expectedData := []byte("merkle-dag-chunk-bytes-0123456789")

	// Register Fast Path handler on swTwo
	swTwo.RegisterFastFrameHandler(net_types.TypeCircuitData, func(payload []byte, peerID types.PeerID) error {
		if string(payload) == string(expectedData) {
			fastPathReceived.Store(true)
		}
		return nil
	})

	// swOne sends raw fast path frame directly to swTwo
	targetID := swTwo.selfID
	if err := swOne.SendRawFrame(targetID, net_types.TypeCircuitData, expectedData); err != nil {
		t.Fatalf("SendRawFrame failed: %v", err)
	}

	// Wait for Fast Path receipt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fastPathReceived.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !fastPathReceived.Load() {
		t.Fatalf("Fast path frame was not received via RegisterFastFrameHandler")
	}
	t.Log("SUCCESS: Zero-Copy FastPath frame routed and verified!")
}

type mockHandler struct {
	callback func(msg *internal_pb.MessageData)
}

func (m *mockHandler) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	m.callback(msg)
}
