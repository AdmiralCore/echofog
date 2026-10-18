package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	net_types "github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type PeerWrapper struct {
	conn   MuxConn
	peerID types.PeerID

	ctx    context.Context
	cancel context.CancelFunc

	streamsMu sync.RWMutex
	streams   map[uint64]*Stream
	nextID    atomic.Uint64

	onData      func(msgType net_types.MessageType, payload []byte, peerID types.PeerID)
	onNewStream func(stream *Stream)
}

func NewPeerWrapper(parentCtx context.Context, conn MuxConn, peerID types.PeerID) *PeerWrapper {
	ctx, cancel := context.WithCancel(parentCtx)
	return &PeerWrapper{
		conn:    conn,
		ctx:     ctx,
		peerID:  peerID,
		cancel:  cancel,
		streams: make(map[uint64]*Stream),
	}
}

func (p *PeerWrapper) Close() error {
	p.cancel()

	p.streamsMu.Lock()
	streams := make([]*Stream, 0, len(p.streams))
	for _, s := range p.streams {
		streams = append(streams, s)
	}
	p.streamsMu.Unlock()

	for _, s := range streams {
		s.Close()
	}

	return p.conn.CloseWithError(ErrCodeNormalClose, "normal close")
}

func (p *PeerWrapper) OnData(onData func(msgType net_types.MessageType, payload []byte, peerID types.PeerID)) {
	// The fast-path callers (datagram loop, gossip stream) route through dispatchData, which
	// recovers from a panic deep in a handler, so one malformed frame cannot crash the node.
	p.onData = onData
}

func (p *PeerWrapper) OnNewStream(handler func(stream *Stream)) {
	p.onNewStream = handler
}

func (p *PeerWrapper) GetConn() MuxConn {
	return p.conn
}

// Protocol is the transport carrying this peer: "quic" or "tcp".
func (p *PeerWrapper) Protocol() string {
	return p.conn.Protocol()
}

func (p *PeerWrapper) RemoteAddr() net.Addr {
	return p.conn.RemoteAddr()
}

func (p *PeerWrapper) StartLoops() {
	go p.AcceptUniLoop(p.ctx)
	go p.AcceptDatagramLoop(p.ctx)
	go p.AcceptStreamLoop(p.ctx)
}

func (p *PeerWrapper) SendGossipMessage(msgType net_types.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := p.conn.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}

	stream.SetWriteDeadline(time.Now().Add(1 * time.Second))

	if err = WriteFrame(stream, msgType, data); err != nil {
		return err
	}

	return stream.Close()
}

func (p *PeerWrapper) SendDataWithRequest(msgType net_types.MessageType, data []byte) (net_types.MessageType, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("write error: %w", err)
	}
	defer stream.Close()

	if d, ok := ctx.Deadline(); ok {
		stream.SetDeadline(d)
	}

	err = WriteFrame(stream, msgType, data)
	if err != nil {
		stream.CancelRead()
		return 0, nil, err
	}

	resType, resData, err := ReadFrame(stream)
	if err != nil {
		stream.CancelRead()
		if errors.Is(err, context.DeadlineExceeded) || (ctx.Err() != nil) {
			return 0, nil, fmt.Errorf("request timed out: %w", err)
		}
		return 0, nil, fmt.Errorf("read error: %w", err)
	}

	return resType, resData, nil
}

// ErrDatagramTooLarge is returned when a frame does not fit into a single QUIC datagram.
var ErrDatagramTooLarge = errors.New("frame does not fit into a QUIC datagram")

// SendDatagramFrame sends a typed frame as an unreliable QUIC datagram: [1 byte type][payload].
// Real-time media uses it to avoid stream setup cost and head-of-line blocking on loss.
func (p *PeerWrapper) SendDatagramFrame(msgType net_types.MessageType, data []byte) error {
	buf := make([]byte, 1+len(data))
	buf[0] = byte(msgType)
	copy(buf[1:], data)
	err := p.conn.SendDatagram(buf)
	var tooLarge *DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		// sizes of data, without the type byte, so a caller can cut its frames to fit
		return &DatagramTooLargeError{Size: len(data), Max: tooLarge.Max - 1}
	}
	return err
}

func (p *PeerWrapper) AcceptUniLoop(ctx context.Context) {
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		stream, err := p.conn.AcceptUniStream(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Debug("Peer disconnected", "err", err)
			return
		}

		go p.handleGossipStream(stream)
	}
}

func (p *PeerWrapper) AcceptDatagramLoop(ctx context.Context) {
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		msg, err := p.conn.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		// quic-go buffers only 128 datagrams, so handlers must stay non-blocking
		if len(msg) < 1 || p.onData == nil {
			continue
		}
		msgType := net_types.MessageType(msg[0])
		if !net_types.IsUnreliableType(msgType) {
			continue // control traffic must arrive over authenticated reliable streams
		}
		p.dispatchData(msgType, msg[1:])
	}
}

// dispatchData hands a received frame to the data handler, recovering from any panic the handler
// raises so that a malformed or malicious frame cannot crash the node. The offending frame is
// dropped; its payload bytes are never logged. Callers must ensure p.onData is non-nil.
func (p *PeerWrapper) dispatchData(msgType net_types.MessageType, payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("PeerWrapper: panic in data handler, dropping frame",
				"peer", p.peerID.String(), "frame_type", msgType, "panic", r)
		}
	}()
	p.onData(msgType, payload, p.peerID)
}

func (p *PeerWrapper) AcceptStreamLoop(ctx context.Context) {
	go func() {
		for {
			stream, err := p.conn.AcceptStream(ctx)
			if err != nil {
				return
			}

			stream.SetDeadline(time.Now().Add(10 * time.Second))

			streamWrapper := p.wrapAndRegister(stream)

			if p.onNewStream != nil {
				go p.onNewStream(streamWrapper)
			}
		}
	}()
}

func (p *PeerWrapper) handleGossipStream(stream MuxRecvStream) {
	// release the stream once its one frame is read: QUIC would free it by itself, smux keeps it
	// counted against the session until the receiver closes it too
	defer stream.CancelRead()
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))

	msgType, msg, err := ReadFrame(stream)
	if err != nil {
		stream.CancelRead()
		return
	}

	if p.onData != nil {
		p.dispatchData(msgType, msg)
	}
}

func (p *PeerWrapper) OpenBidirectionalStream(ctx context.Context) (*Stream, error) {
	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}

	return p.wrapAndRegister(stream), nil
}

func (p *PeerWrapper) wrapAndRegister(qStream MuxStream) *Stream {
	id := p.nextID.Add(1)
	cleanup := func() {
		p.removeStream(id)
	}

	stream := NewStream(p.ctx, qStream, id, p.peerID, cleanup)

	p.streamsMu.Lock()
	p.streams[stream.StreamID] = stream
	p.streamsMu.Unlock()

	return stream
}

func (p *PeerWrapper) removeStream(id uint64) {
	p.streamsMu.Lock()
	delete(p.streams, id)
	p.streamsMu.Unlock()
}
