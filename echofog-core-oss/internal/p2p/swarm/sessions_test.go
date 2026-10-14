package swarm

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type MockSessionSender struct {
	ResponseToReturn     *internal_pb.MessageData
	ResponseTypeToReturn net_types.MessageType
}

func (m *MockSessionSender) SendMessageWithRequest(peerID types.PeerID, msgType net_types.MessageType, msg *internal_pb.MessageData) (net_types.MessageType, *internal_pb.MessageData, error) {
	return m.ResponseTypeToReturn, m.ResponseToReturn, nil
}

func TestSessionManager_CreateAndGet(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	ctx := context.Background()
	peerID := types.PeerID{1, 2, 3}

	sessionID := sm.CreateNewSession(ctx, SessionTypeDag, peerID)

	sess := sm.Get(sessionID)
	if sess == nil {
		t.Fatal("Session should not be nil")
	}

	if sess.Type != SessionTypeDag {
		t.Fatalf("Expected session type %v, got %v", SessionTypeDag, sess.Type)
	}

	if sess.PeerID != peerID {
		t.Fatalf("Expected peer ID %v, got %v", peerID, sess.PeerID)
	}
}

func TestSessionManager_AddStream(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	ctx := context.Background()
	peerID := types.PeerID{1, 2, 3}

	sessionID := sm.CreateNewSession(ctx, SessionTypeDag, peerID)
	sess := sm.Get(sessionID)

	var dummyStream *network.Stream = nil

	success := sm.AddStream(sessionID, dummyStream)
	if !success {
		t.Fatal("Failed to add stream to session")
	}

	select {
	case receivedStream := <-sess.NewStreams:
		if receivedStream != dummyStream {
			t.Fatal("Received stream does not match")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for stream in channel")
	}
}

func TestSessionManager_Concurrency(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	ctx := context.Background()
	peerID := types.PeerID{9, 9, 9}

	sessionID := sm.CreateNewSession(ctx, SessionTypeStream, peerID)

	var wg sync.WaitGroup
	goroutines := 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sm.AddStream(sessionID, nil)
		}()
	}

	go func() {
		sess := sm.Get(sessionID)
		count := 0
		for {
			select {
			case <-sess.NewStreams:
				count++
				if count == goroutines {
					return
				}
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	wg.Wait()
}

func TestSessionMuxing(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peerID := types.PeerID{0xAA, 0xBB, 0xCC}

	dagSessID := sm.CreateNewSession(ctx, SessionTypeDag, peerID)
	rpcSessID := sm.CreateNewSession(ctx, SessionTypeRPC, peerID)

	t.Logf("Created DAG Session: %x", dagSessID)
	t.Logf("Created RPC Session: %x", rpcSessID)

	go func() {
		sm.AcceptStream(dagSessID, nil)
		sm.AcceptStream(rpcSessID, nil)
	}()

	dagSess := sm.Get(dagSessID)
	select {
	case <-dagSess.NewStreams:
		t.Log("DAG Session received its stream")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout: DAG Session didn't get the stream")
	}

	rpcSess := sm.Get(rpcSessID)
	select {
	case <-rpcSess.NewStreams:
		t.Log("RPC Session received its stream")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout: RPC Session didn't get the stream")
	}
}

func TestSessionManager_Close(t *testing.T) {
	sm := NewSessionManager()
	ctx := context.Background()
	sid := sm.CreateNewSession(ctx, SessionTypeDag, types.PeerID{1})

	err := sm.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	sess := sm.Get(sid)
	if sess != nil {
		select {
		case <-sess.Ctx.Done():
		default:
			t.Fatal("Session context should be cancelled after manager close")
		}
	}
}

func TestSessionManager_RegisterHandler(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	mockSender := &MockSessionSender{}
	sm.sender = mockSender

	executed := make(chan struct{})
	sm.RegisterHandler(SessionTypeDag, func(s *Session) {
		close(executed) // the handler runs on its own goroutine
	})

	sid := uuid.New()
	_, _, err := sm.HandleDagSessionReq(&internal_pb.MessageData{
		Payload: &internal_pb.MessageData_SessionReq{
			SessionReq: &internal_pb.SessionInitRequest{
				Type:           internal_pb.SessionType_SESSION_DAG,
				SessionId:      sid[:],
				DesiredStreams: 1,
			},
		},
	}, types.PeerID{2})
	if err != nil {
		t.Fatalf("HandleDagSessionReq failed: %v", err)
	}

	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("Handler was not executed")
	}
}

func TestSessionManager_HandleDagSessionReq(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	mockSender := &MockSessionSender{}
	sm.sender = mockSender

	sid := uuid.New()
	msg := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_SessionReq{
			SessionReq: &internal_pb.SessionInitRequest{
				Type:           internal_pb.SessionType_SESSION_DAG,
				SessionId:      sid[:],
				DesiredStreams: 2,
			},
		},
	}

	respType, respMsg, err := sm.HandleDagSessionReq(msg, types.PeerID{3})
	if err != nil {
		t.Fatalf("HandleDagSessionReq failed: %v", err)
	}

	if respType != net_types.TypeDAGSessionInitResponse {
		t.Fatalf("Expected type %v, got %v", net_types.TypeDAGSessionInitResponse, respType)
	}

	res := respMsg.GetSessionRes()
	if res == nil {
		t.Fatal("Response was empty")
	}
	if !res.Accepted {
		t.Fatal("Expected accepted response")
	}
}

func TestSessionManager_NewDagSessionReq(t *testing.T) {
	sm := NewSessionManager()
	sm.ctx = context.Background()
	mockSender := &MockSessionSender{}
	sm.sender = mockSender

	sid := uuid.New()
	mockSender.ResponseTypeToReturn = net_types.TypeDAGSessionInitResponse
	mockSender.ResponseToReturn = &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_SessionRes{
			SessionRes: &internal_pb.SessionInitResponse{
				Accepted:          true,
				AcceptedSessionId: sid[:],
			},
		},
	}

	sess, err := sm.NewDagSessionReq(types.PeerID{4}, []byte("hash"))
	if err != nil {
		t.Fatalf("NewDagSessionReq failed: %v", err)
	}
	if sess.SessionID != types.SessionID(sid) {
		t.Fatalf("Expected session ID %v, got %v", sid, sess.SessionID)
	}
}

func TestSessionManager_ApprovedStreams(t *testing.T) {
	sm := NewSessionManager()

	approved := sm.getApprovedStreams(10, SessionImportanceMedium)
	if approved != 10 {
		t.Fatalf("Expected 10 streams approved, got %d", approved)
	}

	numCpu := runtime.NumCPU()
	sm.mu.Lock()
	sess := NewSession(context.Background(), [16]byte(uuid.New()), SessionTypeDag, types.PeerID{1}, true)
	for i := 0; i <= numCpu; i++ {
		sess.Streams = append(sess.Streams, nil)
	}
	sm.sessions[sess.SessionID] = sess
	sm.mu.Unlock()

	approved = sm.getApprovedStreams(10, SessionImportanceMedium)
	if approved != 1 {
		t.Fatalf("Expected 1 stream approved under load, got %d", approved)
	}
}

func TestSession_Close(t *testing.T) {
	ctx := context.Background()
	sid := [16]byte(uuid.New())
	sess := NewSession(ctx, sid, SessionTypeDag, types.PeerID{5}, true)

	err := sess.Close()
	if err != nil {
		t.Fatalf("Session.Close failed: %v", err)
	}

	select {
	case <-sess.Ctx.Done():
	default:
		t.Fatal("Session context should be cancelled")
	}

	_, ok := <-sess.NewStreams
	if ok {
		t.Fatal("NewStreams channel should be closed")
	}
}

func TestSession_NetworkFullFlow(t *testing.T) {
	ctxOne, swOne, trOne, storeOne := testSwarmInit(t, "127.0.0.1:0")
	defer swOne.Close()
	defer trOne.Close()
	defer storeOne.CloseAndClear()

	ctxTwo, swTwo, trTwo, storeTwo := testSwarmInit(t, "127.0.0.1:0")
	defer swTwo.Close()
	defer trTwo.Close()
	defer storeTwo.CloseAndClear()

	addrOne := trOne.Addr().String()
	if err := trTwo.DialEarly(ctxTwo, addrOne); err != nil {
		t.Fatalf("Dial failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	if len(swOne.GetAllPeers()) == 0 {
		t.Fatal("Peers didn't connect")
	}

	chunkHash := []byte("fake_chunk_hash_32_bytes_long_!!")
	chunkData := []byte("this is some ultra secret data that we want to transfer across the wire with traffic shaping delays")

	swTwo.sessionManager.RegisterHandler(SessionTypeDag, func(sess *Session) {
		for {
			select {
			case <-sess.Ctx.Done():
				return
			case stream := <-sess.NewStreams:
				go func(s *network.Stream) {
					defer s.Close()
					for {
						select {
						case msg, ok := <-s.ReadCh():
							if !ok {
								return
							}
							if msg.Type == net_types.TypeWantList {
								var wl internal_pb.MessageData
								if err := proto.Unmarshal(msg.Payload, &wl); err != nil {
									continue
								}
								block := &internal_pb.BlockData{
									Hash: chunkHash,
									Data: chunkData,
								}
								b, _ := proto.Marshal(block)
								time.Sleep(100 * time.Millisecond)
								s.WriteFrame(net_types.TypeBlockData, b)
							}
						case <-sess.Ctx.Done():
							return
						}
					}
				}(stream)
			}
		}
	})

	targetID := swTwo.selfID
	sess, err := swOne.sessionManager.NewDagSessionReq(targetID, chunkHash)
	if err != nil {
		t.Fatalf("Failed to create DAG session: %v", err)
	}

	streams, err := swOne.OpenNewStreams(ctxOne, targetID, 1)
	if err != nil || len(streams) == 0 {
		t.Fatalf("Failed to open streams: %v", err)
	}
	stream := streams[0]
	swOne.sessionManager.AddStream(sess.SessionID, stream)

	wantList := &internal_pb.WantList{
		WantedHashes: [][]byte{chunkHash},
	}
	wlMsg := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_WantList{
			WantList: wantList,
		},
	}
	b, _ := proto.Marshal(wlMsg)

	start := time.Now()
	err = stream.WriteFrame(net_types.TypeWantList, b)
	if err != nil {
		t.Fatalf("WriteFrame failed: %v", err)
	}

	respType, respPayload, err := stream.ReadFrame()
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("ReadFrame failed: %v", err)
	}
	if respType != net_types.TypeBlockData {
		t.Fatalf("Expected BlockData, got %v", respType)
	}

	var block internal_pb.BlockData
	if err := proto.Unmarshal(respPayload, &block); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if string(block.Data) != string(chunkData) {
		t.Fatal("Data mismatch")
	}

	if duration < 100*time.Millisecond {
		t.Errorf("Traffic shaping didn't seem to work, duration: %v", duration)
	}

	t.Logf("Success! Transferred %d bytes with %v delay", len(block.Data), duration)
}

func TestSessionManager_RemoveAndPruneInactive(t *testing.T) {
	sm := NewSessionManager()
	ctx, cancel := context.WithCancel(context.Background())

	sid1 := sm.CreateNewSession(ctx, SessionTypeDag, types.PeerID{1})
	sid2 := sm.CreateNewSession(ctx, SessionTypeRPC, types.PeerID{2})

	if sm.Get(sid1) == nil || sm.Get(sid2) == nil {
		t.Fatal("sessions not created")
	}

	// Explicit remove
	sm.RemoveSession(sid1)
	if sm.Get(sid1) != nil {
		t.Fatal("session 1 should be removed")
	}

	// Cancel ctx of session 2
	cancel()
	pruned := sm.PruneInactive(1 * time.Minute)
	if pruned != 1 {
		t.Fatalf("expected 1 pruned session, got %d", pruned)
	}
	if sm.Get(sid2) != nil {
		t.Fatal("session 2 should be pruned after context cancel")
	}
}
