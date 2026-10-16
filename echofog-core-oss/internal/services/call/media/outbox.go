package media

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// FrameSender abstracts the peer transport. SendRawFrame is reliable (signaling),
// SendUnreliableFrame is best-effort (media datagrams).
type FrameSender interface {
	SendRawFrame(peerID types.PeerID, msgType net_types.MessageType, payload []byte) error
	SendUnreliableFrame(peerID types.PeerID, msgType net_types.MessageType, payload []byte) error
}

// TrafficClass orders outbound frames: control beats audio, audio beats video.
type TrafficClass int

const (
	ClassControl TrafficClass = iota
	ClassAudio
	ClassVideo
)

const (
	controlQueueLen = 256
	audioQueueLen   = 256
	videoQueueLen   = 1024
)

type outFrame struct {
	msgType net_types.MessageType
	payload []byte
}

type peerQueue struct {
	control chan outFrame
	audio   chan outFrame
	video   chan outFrame
	cancel  context.CancelFunc
}

// OutboxStats counts frames per class; dropped means the peer queue was full.
type OutboxStats struct {
	Sent    [3]uint64
	Dropped [3]uint64
	Failed  [3]uint64
}

// Outbox fans frames out to peers through one worker per peer, so a slow or dead peer
// only loses its own frames and never stalls forwarding to the rest of the tree.
type Outbox struct {
	sender FrameSender
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	peers map[types.PeerID]*peerQueue

	sent, dropped, failed [3]atomic.Uint64
}

func NewOutbox(sender FrameSender) *Outbox {
	ctx, cancel := context.WithCancel(context.Background())
	return &Outbox{sender: sender, ctx: ctx, cancel: cancel, peers: make(map[types.PeerID]*peerQueue)}
}

// Send enqueues without blocking; it returns false if the frame was dropped.
func (o *Outbox) Send(peer types.PeerID, class TrafficClass, msgType net_types.MessageType, payload []byte) bool {
	if o == nil || o.sender == nil {
		return false
	}
	q := o.queue(peer)
	if q == nil {
		return false
	}
	ch := q.video
	switch class {
	case ClassControl:
		ch = q.control
	case ClassAudio:
		ch = q.audio
	}
	select {
	case ch <- outFrame{msgType, payload}:
		return true
	default:
		o.dropped[class].Add(1)
		return false
	}
}

// Forget stops the worker of a peer that left the room.
func (o *Outbox) Forget(peer types.PeerID) {
	o.mu.Lock()
	q := o.peers[peer]
	delete(o.peers, peer)
	o.mu.Unlock()
	if q != nil {
		q.cancel()
	}
}

func (o *Outbox) Close() {
	o.cancel()
	o.mu.Lock()
	o.peers = make(map[types.PeerID]*peerQueue)
	o.mu.Unlock()
}

func (o *Outbox) Stats() OutboxStats {
	var s OutboxStats
	for i := 0; i < 3; i++ {
		s.Sent[i], s.Dropped[i], s.Failed[i] = o.sent[i].Load(), o.dropped[i].Load(), o.failed[i].Load()
	}
	return s
}

// VideoBacklog is the deepest video queue across peers: a growing backlog means the path
// cannot carry what we send.
func (o *Outbox) VideoBacklog() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	deepest := 0
	for _, q := range o.peers {
		if n := len(q.video); n > deepest {
			deepest = n
		}
	}
	return deepest
}

// VideoQueueLen is the capacity of each peer's video queue.
const VideoQueueLen = videoQueueLen

func (o *Outbox) queue(peer types.PeerID) *peerQueue {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ctx.Err() != nil {
		return nil
	}
	if q := o.peers[peer]; q != nil {
		return q
	}
	ctx, cancel := context.WithCancel(o.ctx)
	q := &peerQueue{
		control: make(chan outFrame, controlQueueLen),
		audio:   make(chan outFrame, audioQueueLen),
		video:   make(chan outFrame, videoQueueLen),
		cancel:  cancel,
	}
	o.peers[peer] = q
	go o.run(ctx, peer, q)
	return q
}

func (o *Outbox) run(ctx context.Context, peer types.PeerID, q *peerQueue) {
	for {
		// strict priority: drain control, then audio, before touching video
		select {
		case f := <-q.control:
			o.deliver(peer, ClassControl, f)
			continue
		default:
		}
		select {
		case f := <-q.audio:
			o.deliver(peer, ClassAudio, f)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return
		case f := <-q.control:
			o.deliver(peer, ClassControl, f)
		case f := <-q.audio:
			o.deliver(peer, ClassAudio, f)
		case f := <-q.video:
			o.deliver(peer, ClassVideo, f)
		}
	}
}

func (o *Outbox) deliver(peer types.PeerID, class TrafficClass, f outFrame) {
	var err error
	if class == ClassControl {
		err = o.sender.SendRawFrame(peer, f.msgType, f.payload)
	} else {
		err = o.sender.SendUnreliableFrame(peer, f.msgType, f.payload)
		if errors.Is(err, network.ErrDatagramTooLarge) {
			// path MTU smaller than expected: keep the call alive over the reliable path
			err = o.sender.SendRawFrame(peer, f.msgType, f.payload)
		}
	}
	if err != nil {
		o.failed[class].Add(1)
		return
	}
	o.sent[class].Add(1)
}
