package dispatcher

import (
	"context"
	"encoding/hex"
	"log/slog"

	"log"
	"reflect"
	"runtime"
	"sync"

	"github.com/echofog/echofog-core/internal/crypto"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

type Handler interface {
	Handle(msg *internal_pb.MessageData, peerID types.PeerID)
}

type RawDataSink interface {
	HandleRawData(sessionID types.SessionID, payload []byte)
}

type Dispatcher struct {
	// The channel on which ALL peers send incoming packets
	ingressChan chan IngressPacket

	handlersMu sync.RWMutex
	handlers   map[reflect.Type]Handler // string = payload type

	workersNum int
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	rawSink RawDataSink

	logger *slog.Logger
}

type IngressPacket struct {
	Envelope   *internal_pb.Envelope
	PeerID     types.PeerID
	FastPath   bool
	SessionID  types.SessionID
	RawPayload []byte
}

func NewDispatcher(ctx context.Context) *Dispatcher {
	ctx, cancel := context.WithCancel(ctx)
	workerNum := runtime.NumCPU() - 1
	if workerNum < 0 {
		workerNum = 1
	}
	return &Dispatcher{
		ingressChan: make(chan IngressPacket, 2000),
		handlers:    make(map[reflect.Type]Handler),
		workersNum:  workerNum,
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (d *Dispatcher) SetLogger(logger *slog.Logger) {
	if d != nil {
		d.logger = logger
	}
}

func (d *Dispatcher) Registry(payloadProto interface{}, handler Handler) {
	t := reflect.TypeOf(payloadProto)
	if t.Kind() != reflect.Ptr {
		t = reflect.PointerTo(t)
	}

	d.handlersMu.Lock()
	defer d.handlersMu.Unlock()

	d.handlers[t] = handler
}

func (d *Dispatcher) SetRawDataHandler(sink RawDataSink) {
	d.rawSink = sink
}

// PushMessage calls Peer when it has read something from the network
func (d *Dispatcher) PushMessage(env *internal_pb.Envelope, peerID types.PeerID) {
	select {
	case d.ingressChan <- IngressPacket{
		Envelope: env,
		PeerID:   peerID,
	}:
	default:
		log.Println("Dispatcher queue full, dropping message from", peerID)
	}
}

func (d *Dispatcher) Start() {
	for i := 0; i < d.workersNum; i++ {
		d.wg.Add(1)
		go d.workerLoop()
	}
}

func (d *Dispatcher) Stop() {
	d.cancel()
	d.wg.Wait()
}

func (d *Dispatcher) workerLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		case packet, ok := <-d.ingressChan:
			if !ok {
				return
			}
			d.processPacket(packet)
		}
	}
}

func (d *Dispatcher) processPacket(packet IngressPacket) {
	if d.ctx.Err() != nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			if d.logger != nil {
				d.logger.Error("Dispatcher: Panic in packet processing", "panic", r, "peer", packet.PeerID.String())
			} else {
				log.Printf("Dispatcher: Panic in packet processing: %v", r)
			}
		}
	}()

	env := packet.Envelope
	if env == nil || len(env.PubKey) != 32 {
		return
	}
	pubKey := types.PeerPublicKey(env.PubKey)
	if !crypto.VerifySignature(pubKey, env.Data, env.Signature) {
		log.Printf("Security Alert: Invalid signature from peer!")
		return
	}

	if packet.FastPath && !packet.SessionID.IsEmpty() {
		if d.rawSink != nil {
			d.rawSink.HandleRawData(packet.SessionID, packet.RawPayload)
		}
		return
	}

	if packet.Envelope == nil {
		return
	}

	var msgData internal_pb.MessageData
	if err := proto.Unmarshal(env.Data, &msgData); err != nil {
		log.Printf("Failed to unmarshal MessageData: %v", err)
		return
	}
	if !originMatchesSigner(&msgData, pubKey) {
		log.Printf("Security Alert: message claims an origin that did not sign it")
		return
	}

	d.route(&msgData, packet.PeerID)
}

// originMatchesSigner: the origin a message names must be the key that signed it, or anyone could
// write in the name of anyone. Gossiped peer lists are the only exception: every hop signs them
// again, and nothing in them depends on who sent them first.
func originMatchesSigner(msg *internal_pb.MessageData, signer types.PeerPublicKey) bool {
	if _, gossip := msg.Payload.(*internal_pb.MessageData_PeerRes); gossip {
		return true
	}
	id := types.PeerPubKeyToID(signer)
	return len(msg.OriginId) == 0 || string(msg.OriginId) == string(id[:])
}

func (d *Dispatcher) route(msg *internal_pb.MessageData, fromPeer types.PeerID) {
	payloadType := reflect.TypeOf(msg.Payload)

	var msgIDStr string
	if len(msg.MessageId) > 0 {
		msgIDStr = hex.EncodeToString(msg.MessageId)
	}
	slog.Info("[STEP 2: IN] Dispatcher routed payload", "type", payloadType.String(), "msgID", msgIDStr, "from", fromPeer)

	d.handlersMu.RLock()
	handler, ok := d.handlers[payloadType]
	d.handlersMu.RUnlock()

	if ok {
		handler.Handle(msg, fromPeer)
	} else {
		log.Printf("No handler registered for type: %v", payloadType)
	}
}
