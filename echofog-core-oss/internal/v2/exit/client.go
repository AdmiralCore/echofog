package exit

import (
	"context"
	"errors"
	"io"
	mrand "math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

var errNotImplemented = errors.New("exit: client not implemented")

// ClientConfig configures a Client.
type ClientConfig struct {
	// OpenStream opens a stream of exit/2 on the session to the exit (required).
	OpenStream func(ctx context.Context) (*mux.Stream, error)
	// SendDatagram sends an application datagram on the session. Nil means the session cannot
	// carry datagrams, and UDP rides the stream.
	SendDatagram func(typ uint64, payload []byte) error
	// Shape is the shape profile of the exit's address (H1).
	Shape *shape.Profile
	// Sleep waits out the H1 jitter; nil means time.Sleep.
	Sleep func(time.Duration)
	Clock carrier.Clock
}

// Client is exit/2 on the client side.
type Client struct{}

// NewClient returns a client of one exit.
func NewClient(ClientConfig) (*Client, error) { return &Client{}, nil }

// DialExitStream implements core.StreamDialer.
func (c *Client) DialExitStream(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errNotImplemented
}

// DialExitUDP implements core.UDPDialer.
func (c *Client) DialExitUDP(context.Context) (*core.UDPSession, error) {
	return nil, errNotImplemented
}

// HandleDatagram feeds one EXIT_UDP datagram received from the exit.
func (c *Client) HandleDatagram([]byte) {}

// Close ends the client.
func (c *Client) Close() error { return nil }

// wireStream is the part of *mux.Stream the client stream uses.
type wireStream interface {
	WriteFrame(typ uint64, flags byte, payload []byte) error
	ReadFrame() (frame.Frame, error)
	CloseWrite() error
	Close() error
}

var _ wireStream = (*mux.Stream)(nil)

// clientStream is the core.FramedStream of one tunnel on the client side.
type clientStream struct{}

func newClientStream(wireStream, *shape.Profile, func(time.Duration), *mrand.Rand) *clientStream {
	return &clientStream{}
}

func (s *clientStream) Write([]byte) (int, error) { return 0, errNotImplemented }
