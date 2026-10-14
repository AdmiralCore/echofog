package network

import (
	"context"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

type StreamMessage struct {
	Type    net_types.MessageType
	Payload []byte
}

type Stream struct {
	StreamID uint64 // local identifier, unique within the peer connection
	RemoteID types.PeerID

	Incoming chan *StreamMessage
	Outgoing chan *StreamMessage

	stream MuxStream

	ctx    context.Context
	cancel context.CancelFunc

	onClose func()
	once    sync.Once
}

func NewStream(ctx context.Context, stream MuxStream, StreamID uint64, remoteID types.PeerID, onClose func()) *Stream {
	childCtx, cancel := context.WithCancel(ctx)
	s := &Stream{
		StreamID: StreamID,
		RemoteID: remoteID,
		Incoming: make(chan *StreamMessage, 100),
		Outgoing: make(chan *StreamMessage, 100),
		stream:   stream,
		ctx:      childCtx,
		cancel:   cancel,
		onClose:  onClose,
	}

	//go s.readLoop()
	//go s.writeLoop()

	return s
}

func (s *Stream) StartLoops() {
	go s.readLoop()
	go s.writeLoop()
}

func (s *Stream) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		s.stream.CancelRead()
		err = s.stream.Close()

		if s.onClose != nil {
			s.onClose()
		}
	})

	return err
}

func (s *Stream) readLoop() {
	defer s.Close()
	for {
		msgType, payload, err := ReadFrame(s.stream)
		if err != nil {
			return
		}

		select {
		case s.Incoming <- &StreamMessage{
			Type:    msgType,
			Payload: payload,
		}:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Stream) writeLoop() {
	defer s.Close()
	for {
		select {
		case msg := <-s.Outgoing:
			WriteFrame(s.stream, msg.Type, msg.Payload)

		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Stream) Send(Type net_types.MessageType, Payload []byte) {
	s.Outgoing <- &StreamMessage{
		Type:    Type,
		Payload: Payload,
	}
}

func (s *Stream) ReadCh() <-chan *StreamMessage {
	return s.Incoming
}

func (s *Stream) ReadFrame() (net_types.MessageType, []byte, error) {
	return ReadFrame(s.stream)
}

func (s *Stream) WriteFrame(msgType net_types.MessageType, data []byte) error {
	return WriteFrame(s.stream, msgType, data)
}

func (s *Stream) ResetDeadline() {
	s.stream.SetDeadline(time.Time{})
}

// Read and Write use the stream as a plain byte pipe (after the framed preamble).
func (s *Stream) Read(p []byte) (int, error)  { return s.stream.Read(p) }
func (s *Stream) Write(p []byte) (int, error) { return s.stream.Write(p) }

func (s *Stream) SetDeadline(t time.Time) error     { return s.stream.SetDeadline(t) }
func (s *Stream) SetReadDeadline(t time.Time) error { return s.stream.SetReadDeadline(t) }
