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
	// The handshake runs before the peer is authenticated, so cap each frame tightly.
	return ReadFrameLimit(s.stream, HandshakeMaxFrameSize)
}

func (s *StreamWrapper) WriteFrame(msgType net_types.MessageType, data []byte) error {
	return WriteFrame(s.stream, msgType, data)
}
