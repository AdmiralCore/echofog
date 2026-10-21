package swarm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/abuse"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"github.com/google/uuid"
)

type SessionContract struct {
	SessionID types.SessionID
	Stream    *network.Stream
}

type SessionResponseSender interface {
	SendMessageWithRequest(peerID types.PeerID, msgType net_types.MessageType, msg *internal_pb.MessageData) (net_types.MessageType, *internal_pb.MessageData, error)
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[types.SessionID]*Session

	handlers map[SessionType]func(*Session)

	respCh chan SessionContract

	sender SessionResponseSender

	ctx context.Context

	report func(types.PeerID, abuse.Reason) // misbehaving peers go here (the swarm); may be nil
}

// Limits on sessions peers open towards this node.
const (
	MaxSessionsPerPeer = 16
	MaxSessions        = 512
	MaxStreamsPerSess  = 32
)

var (
	errSessionsFull     = errors.New("too many sessions")
	errSessionDuplicate = errors.New("session id in use")
)

func (sm *SessionManager) reportPeer(id types.PeerID, r abuse.Reason) {
	if sm.report != nil {
		sm.report(id, r)
	}
}

// admitIncoming checks, under sm.mu, that a peer may open one more session with this ID.
func (sm *SessionManager) admitIncomingLocked(sid types.SessionID, peerID types.PeerID) error {
	if _, taken := sm.sessions[sid]; taken {
		// never replace a session, least of all one of another peer
		return errSessionDuplicate
	}
	if len(sm.sessions) >= MaxSessions {
		return errSessionsFull
	}
	n := 0
	for _, s := range sm.sessions {
		if s.PeerID == peerID && !s.IsOutgoing {
			if n++; n >= MaxSessionsPerPeer {
				return errSessionsFull
			}
		}
	}
	return nil
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[types.SessionID]*Session),
		handlers: make(map[SessionType]func(*Session)),
		respCh:   make(chan SessionContract, 10),
	}
}

func (sm *SessionManager) Close() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, session := range sm.sessions {
		if err := session.Close(); err != nil {
			return err
		}
		delete(sm.sessions, session.SessionID)
	}
	close(sm.respCh)
	return nil
}

func (sm *SessionManager) GetRespCh() chan SessionContract {
	return sm.respCh
}

func (sm *SessionManager) AddSession(sid types.SessionID, ses *Session) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.sessions[sid] = ses
}

// RemoveSession safely closes and removes a session by ID to prevent memory leaks
func (sm *SessionManager) RemoveSession(sid types.SessionID) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sess, ok := sm.sessions[sid]; ok {
		_ = sess.Close()
		delete(sm.sessions, sid)
	}
}

// PruneInactive cleans up sessions that are done (Ctx.Err() != nil) or idle longer than maxIdle
func (sm *SessionManager) PruneInactive(maxIdle time.Duration) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	pruned := 0
	for id, sess := range sm.sessions {
		isDone := false
		if sess.Ctx != nil && sess.Ctx.Err() != nil {
			isDone = true
		} else if maxIdle > 0 && !sess.LastActive.IsZero() && now.Sub(sess.LastActive) > maxIdle {
			isDone = true
		}

		if isDone {
			_ = sess.Close()
			delete(sm.sessions, id)
			pruned++
		}
	}
	return pruned
}

func (sm *SessionManager) RegisterHandler(t SessionType, handler func(*Session)) {
	sm.mu.Lock()
	sm.handlers[t] = handler
	sm.mu.Unlock()
}

func (sm *SessionManager) NewDagSessionReq(peerID types.PeerID, hash []byte) (ses *Session, err error) {
	n, _ := rand.Int(rand.Reader, big.NewInt(1500))
	padding := make([]byte, n.Int64())
	rand.Read(padding)

	sid := uuid.New()
	req := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_SessionReq{
			SessionReq: &internal_pb.SessionInitRequest{
				Type:            internal_pb.SessionType_SESSION_DAG,
				SessionId:       sid[:],
				ContextId:       hash,
				DesiredStreams:  4,
				ProtocolVersion: config.CurrentProtocolVersion,
				Padding:         padding,
			},
		},
	}

	reqType, msgRaw, err := sm.sender.SendMessageWithRequest(peerID, net_types.TypeDAGSessionInitRequest, req)
	if err != nil {
		return nil, err
	}
	if reqType != net_types.TypeDAGSessionInitResponse {
		return nil, fmt.Errorf("unexpected request type: %d", reqType)
	}

	msg := msgRaw.GetSessionRes()
	if msg == nil {
		return nil, fmt.Errorf("unexpected nil session request")
	}

	if !msg.Accepted {
		return nil, fmt.Errorf("session not accepted")
	}

	var acceptedSid types.SessionID
	copy(acceptedSid[:], msg.AcceptedSessionId)

	return sm.createAndRegisterEntitySession(sm.ctx, acceptedSid, SessionTypeDag, peerID, true), nil
}

func (sm *SessionManager) AcceptStream(sessionID types.SessionID, stream *network.Stream) {
	sm.mu.RLock()
	sess, ok := sm.sessions[sessionID]
	sm.mu.RUnlock()

	if ok {
		sess.NewStreams <- stream
	} else {
		stream.Close()
	}
}

func (sm *SessionManager) CreateNewSession(parentCtx context.Context, Type SessionType, PeerID types.PeerID) types.SessionID {
	sessionID := [16]byte(uuid.New())
	transfer := NewSession(parentCtx, sessionID, Type, PeerID, true)

	sm.mu.Lock()
	sm.sessions[sessionID] = transfer
	sm.mu.Unlock()

	return sessionID
}

func (sm *SessionManager) AddStream(sessionID types.SessionID, stream *network.Stream) bool {
	sm.mu.Lock()
	transfer, ok := sm.sessions[sessionID]
	sm.mu.Unlock()
	if !ok {
		return false
	}
	// a stream joins only a session with the peer it came from, and only so many
	if stream != nil && stream.RemoteID != transfer.PeerID {
		return false
	}
	transfer.Mu.Lock()
	full := len(transfer.Streams) >= MaxStreamsPerSess
	transfer.Mu.Unlock()
	if full {
		return false
	}

	if stream != nil && transfer.IsOutgoing {
		err := stream.WriteFrame(net_types.TypeSessionData, sessionID[:])
		if err != nil {
			return false
		}
	}

	transfer.addStream(stream)

	select {
	case transfer.NewStreams <- stream:
		return true
	case <-transfer.Ctx.Done():
		return false
	}
}

func (sm *SessionManager) Register(id types.SessionID, sess *Session) {
	sm.mu.Lock()
	sm.sessions[id] = sess
	sm.mu.Unlock()
}

func (sm *SessionManager) Get(id types.SessionID) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

func (sm *SessionManager) HandleRawData(sid types.SessionID, data []byte) {
	sm.mu.RLock()
	session, ok := sm.sessions[sid]
	sm.mu.RUnlock()

	if !ok {
		return
	}

	_ = session
	panic("add processing raw data in session manager")
}

func (sm *SessionManager) HandleDagSessionReq(msg *internal_pb.MessageData, peerID types.PeerID) (net_types.MessageType, *internal_pb.MessageData, error) {
	req := msg.GetSessionReq()
	if req == nil || req.Type != internal_pb.SessionType_SESSION_DAG {
		return 0, nil, errors.New("get session request nil or invalid type")
	}

	resp, err := sm.prepareSessionAndGetResponse(req.SessionId, peerID, req.DesiredStreams, SessionImportanceLow)
	if err != nil {
		sm.reportPeer(peerID, abuse.Refused)
		return 0, nil, err
	}

	respMsg := &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_SessionRes{
			SessionRes: resp,
		},
	}

	return net_types.TypeDAGSessionInitResponse, respMsg, nil
}

func (sm *SessionManager) prepareSessionAndGetResponse(sidBinary []byte, peerID types.PeerID, desiredStreams uint32, importance SessionImportance) (*internal_pb.SessionInitResponse, error) {
	allowedStreams := sm.getApprovedStreams(desiredStreams, importance)

	if len(sidBinary) != 16 {
		return nil, errors.New("session id length invalid")
	}

	sessionID, err := uuid.FromBytes(sidBinary)
	if err != nil {
		return nil, errors.New("marshal session id invalid")
	}

	var fileSize uint64

	ses := NewSession(sm.ctx, [16]byte(sessionID), SessionTypeDag, peerID, false)

	sm.mu.Lock()
	if err := sm.admitIncomingLocked([16]byte(sessionID), peerID); err != nil {
		sm.mu.Unlock()
		ses.Cancel()
		return nil, err
	}
	sm.sessions[[16]byte(sessionID)] = ses
	sm.mu.Unlock()

	sm.mu.RLock()
	handler, ok := sm.handlers[SessionTypeDag]
	sm.mu.RUnlock()
	if ok {
		go handler(ses)
	}

	resp := &internal_pb.SessionInitResponse{
		Accepted:          true,
		AllowedStreams:    allowedStreams,
		FileSize:          fileSize,
		AcceptedSessionId: sidBinary,
	}

	return resp, nil
}

func (sm *SessionManager) getApprovedStreams(desired uint32, importance SessionImportance) uint32 {
	numCpu := runtime.NumCPU()

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	totalStreamsCount := 0
	for _, stream := range sm.sessions {
		totalStreamsCount += len(stream.Streams)
	}

	if totalStreamsCount > numCpu {
		return 1
	}

	return desired
}

func (sm *SessionManager) createAndRegisterEntitySession(parentCtx context.Context, SessionID types.SessionID, Type SessionType, PeerID types.PeerID, IsOutgoing bool) *Session {
	ses := NewSession(parentCtx, SessionID, Type, PeerID, IsOutgoing)
	sm.Register(SessionID, ses)
	return ses
}

type SessionType uint32

const (
	SessionTypeUnknown SessionType = 0
	SessionTypeDag     SessionType = 1
	SessionTypeStream  SessionType = 2
	SessionTypeRPC     SessionType = 3
)

type SessionImportance uint32

const (
	SessionImportanceUnknown SessionImportance = iota
	SessionImportanceLow
	SessionImportanceMedium
	SessionImportanceHigh
	SessionImportanceSuperHigh
)

type Session struct {
	SessionID    types.SessionID
	Type         SessionType
	PeerID       types.PeerID
	IsOutgoing   bool
	Streams      []*network.Stream
	LastActive   time.Time
	StreamsCount uint32
	Mu           sync.Mutex
	ContextID    []byte

	NewStreams chan *network.Stream

	Ctx    context.Context
	Cancel context.CancelFunc
}

func NewSession(parentCtx context.Context, SessionID types.SessionID, Type SessionType, PeerID types.PeerID, IsOutgoing bool) *Session {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Session{
		SessionID:  SessionID,
		Type:       Type,
		PeerID:     PeerID,
		IsOutgoing: IsOutgoing,
		LastActive: time.Now(),
		Ctx:        ctx,
		Cancel:     cancel,
		NewStreams: make(chan *network.Stream, 10),
	}
}

func (s *Session) Close() error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Cancel()

	for _, stream := range s.Streams {
		stream.Close()
	}

	close(s.NewStreams)
	return nil
}

func (s *Session) addStream(stream *network.Stream) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	s.LastActive = time.Now()
	if s.Streams == nil {
		s.Streams = make([]*network.Stream, 0, 5)
	}
	s.Streams = append(s.Streams, stream)
}
