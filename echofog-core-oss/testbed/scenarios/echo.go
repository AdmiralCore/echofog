package scenarios

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// EchoOptions describes an echo transfer.
type EchoOptions struct {
	Total int64         // bytes to send
	Chunk int           // frame size; zero means 16 KiB
	Pace  time.Duration // pause after every frame; zero means none
	// Hook is called after each frame is written, with the bytes sent so far. It runs on the
	// sender's goroutine: a scenario cuts a path from it at a chosen byte.
	Hook func(sent int64)
	// Card dials with this node's card; via is the noise3 address index, or -1 for the whole plan.
	Via int
}

// EchoRun is what an echo transfer did.
type EchoRun struct {
	Sent, Received int64
	// MaxPause is the longest time between two echoed frames, the pause a user would feel.
	MaxPause time.Duration
	Elapsed  time.Duration
	Err      error
}

// RegisterEcho adds the lab echo service (the experimental range, as efv2 does) to the node. Both
// ends register it, so the handshake offers and selects it.
func RegisterEcho(n *Node) { panic("not implemented") }

// EchoStream opens the echo service from n to to and sends the bytes through it, reading them back.
func (n *Node) EchoStream(ctx context.Context, to *Node, o EchoOptions) EchoRun {
	return EchoRun{Err: errors.New("not implemented")}
}

var _ = node.DialOpt{}
