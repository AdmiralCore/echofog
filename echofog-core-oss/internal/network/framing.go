package network

import (
	"io"

	net_types "github.com/echofog/echofog-core/internal/network/net_types"
)

// StreamWrapper implements handshake.FrameReadWriter over any stream.
type StreamWrapper struct {
	stream io.ReadWriter
}

func (s *StreamWrapper) ReadFrame() (net_types.MessageType, []byte, error) {
	return ReadFrame(s.stream)
}

func (s *StreamWrapper) WriteFrame(msgType net_types.MessageType, data []byte) error {
	return WriteFrame(s.stream, msgType, data)
}
