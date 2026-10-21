package dispatcher

import (
	"context"
	"encoding/hex"
	"log/slog"

	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/abuse"
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

	// fairness: each peer may hold only perPeer packets of the queue, so one peer cannot fill it
	// and starve the others
	queuedMu sync.Mutex
	queued   map[types.PeerID]int
	perPeer  int

	reporter atomic.Pointer[abuse.Reporter]

	dropsShare atomic.Uint64 // dropped because the peer was over its share
	dropsFull  atomic.Uint64 // dropped because the whole queue was full
	lastLog    atomic.Int64  // unix nanoseconds of the last drop report in the log
}

const (
	queueSize = 2000
	// peerShare is the fraction of the queue one peer may hold.
	peerShare = 8
	// dropLogEvery: drops are summed up in one log line at most this often.
	dropLogEvery = 10 * time.Second
)

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
	if workerNum < 1 {
		workerNum = 1
	}
	return &Dispatcher{
		ingressChan: make(chan IngressPacket, queueSize),
		handlers:    make(map[reflect.Type]Handler),
		workersNum:  workerNum,
		ctx:         ctx,
		cancel:      cancel,
		queued:      make(map[types.PeerID]int),
		perPeer:     queueSize / peerShare,
	}
}

func (d *Dispatcher) SetLogger(logger *slog.Logger) {
	if d != nil {
		d.logger = logger
	}
}

func (d *Dispatcher) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// SetReporter sets where peers that break the protocol are reported (the swarm); nil: nowhere.
func (d *Dispatcher) SetReporter(r abuse.Reporter) {
	if r == nil {
		d.reporter.Store(nil)
		return
	}
	d.reporter.Store(&r)
}

func (d *Dispatcher) report(peer types.PeerID, r abuse.Reason) {
	if rep := d.reporter.Load(); rep != nil {
		(*rep).ReportPeer(peer, r)
	}
}

// Dropped is how many packets were dropped because their peer was over its share of the queue,
// and how many because the whole queue was full.
func (d *Dispatcher) Dropped() (overShare, queueFull uint64) {
	return d.dropsShare.Load(), d.dropsFull.Load()
}

// noteDrop counts a dropped packet and writes a summary line at most every dropLogEvery: under a
// flood a line per packet would itself be the flood.
func (d *Dispatcher) noteDrop(peer types.PeerID, overShare bool) {
	if overShare {
		d.dropsShare.Add(1)
		d.report(peer, abuse.Flood)
	} else {
		d.dropsFull.Add(1)
	}
	now := time.Now().UnixNano()
	last := d.lastLog.Load()
	if now-last < int64(dropLogEvery) || !d.lastLog.CompareAndSwap(last, now) {
		return
	}
	share, full := d.Dropped()
	d.log().Warn("Dispatcher: dropping messages", "last_peer", peer.String(), "over_peer_share_total", share, "queue_full_total", full)
}

func (d *Dispatcher) dequeued(peer types.PeerID) {
	d.queuedMu.Lock()
	if n := d.queued[peer] - 1; n > 0 {
		d.queued[peer] = n
	} else {
		delete(d.queued, peer)
	}
	d.queuedMu.Unlock()
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
// A peer may hold only its share of the queue; past it its packets are dropped (and reported),
// while the other peers' still get in.
func (d *Dispatcher) PushMessage(env *internal_pb.Envelope, peerID types.PeerID) {
	d.queuedMu.Lock()
	if d.queued[peerID] >= d.perPeer {
		d.queuedMu.Unlock()
		d.noteDrop(peerID, true)
		return
	}
	d.queued[peerID]++
	d.queuedMu.Unlock()
	select {
	case d.ingressChan <- IngressPacket{
		Envelope: env,
		PeerID:   peerID,
	}:
	default:
		d.dequeued(peerID)
		d.noteDrop(peerID, false)
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
			d.dequeued(packet.PeerID)
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
			d.log().Error("Dispatcher: Panic in packet processing", "panic", r, "peer", packet.PeerID.String())
		}
	}()

	env := packet.Envelope
	if env == nil || len(env.PubKey) != 32 {
		return
	}
	pubKey := types.PeerPublicKey(env.PubKey)
	// blame the peer that sent it only for what it signed itself: an envelope it forwards for
	// somebody else (a relay, gossip) may be bad without its fault
	ownEnvelope := types.PeerPubKeyToID(pubKey) == packet.PeerID
	if !crypto.VerifySignature(pubKey, env.Data, env.Signature) {
		d.log().Debug("Dispatcher: invalid signature", "peer", packet.PeerID.String())
		if ownEnvelope {
			d.report(packet.PeerID, abuse.BadSignature)
		}
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
		d.log().Debug("Dispatcher: undecodable message", "peer", packet.PeerID.String(), "err", err)
		if ownEnvelope {
			d.report(packet.PeerID, abuse.BadFrame)
		}
		return
	}
	if !originMatchesSigner(&msgData, pubKey) {
		d.log().Debug("Dispatcher: message claims an origin that did not sign it", "peer", packet.PeerID.String())
		if ownEnvelope {
			d.report(packet.PeerID, abuse.BadSignature)
		}
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

	if l := d.log(); l.Enabled(d.ctx, slog.LevelDebug) {
		var msgIDStr string
		if len(msg.MessageId) > 0 {
			msgIDStr = hex.EncodeToString(msg.MessageId)
		}
		l.Debug("[STEP 2: IN] Dispatcher routed payload", "type", payloadType.String(), "msgID", msgIDStr, "from", fromPeer)
	}

	d.handlersMu.RLock()
	handler, ok := d.handlers[payloadType]
	d.handlersMu.RUnlock()

	if ok {
		handler.Handle(msg, fromPeer)
	} else {
		d.log().Debug("Dispatcher: no handler registered", "type", payloadType)
	}
}
