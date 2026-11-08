package scenarios

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// The lab echo service: the experimental range, the same shape as efv2's.
const (
	echoServiceID uint64 = service.ExperimentalMin
	echoVersion   uint64 = 1
	echoFrameData uint64 = 0x40
	echoChunk            = 64 << 10
	echoWindow           = 8 // frames in flight
)

var echoSpec = service.Spec{
	ID: echoServiceID, Name: "echo",
	Versions: map[uint64]service.Version{echoVersion: {Types: []service.FrameType{{Type: echoFrameData, Name: "ECHO_DATA", Max: echoChunk}}}},
}

// EchoOptions describes an echo transfer.
type EchoOptions struct {
	Total int64         // bytes to send
	Chunk int           // frame size; zero means 16 KiB
	Pace  time.Duration // pause after every frame; zero means none
	// Hook is called after each frame is written, with the bytes sent so far. It runs on the
	// sender's goroutine: a scenario cuts a path from it at a chosen byte.
	Hook func(sent int64)
}

// EchoRun is what an echo transfer did.
type EchoRun struct {
	Sent, Received int64
	// MaxPause is the longest time between two echoed frames, the pause a user would feel.
	MaxPause time.Duration
	Elapsed  time.Duration
	Err      error
}

// RegisterEcho adds the lab echo service to the node. Both ends register it, so the handshake
// offers and selects it.
func RegisterEcho(n *Node) {
	n.RT.Register(echoSpec, func(_ context.Context, _ node.Peer, st *mux.Stream) {
		defer st.Close()
		for {
			f, err := st.ReadFrame()
			if err != nil {
				return
			}
			if f.Type != echoFrameData {
				_ = st.CloseWithCode(frame.CodeProtocolViolation, "echo: unexpected frame")
				return
			}
			if err := st.WriteFrame(echoFrameData, 0, f.Payload); err != nil {
				return
			}
		}
	})
}

// EchoStream opens the echo service from n to to over the session n already has (or dials one) and
// sends the bytes through it, reading them back. A cancelled context ends the call at once even when
// a write is stuck on flow control (a cut path); the stream is closed in the background and the
// goroutines go when the world is closed.
func (n *Node) EchoStream(ctx context.Context, to *Node, o EchoOptions) EchoRun {
	var run EchoRun
	card, err := to.PeerCard()
	if err != nil {
		run.Err = err
		return run
	}
	_, st, err := n.RT.Open(ctx, to.ID.DeviceID, echoServiceID, node.DialOpt{Card: card})
	if err != nil {
		run.Err = fmt.Errorf("open echo: %w", err)
		return run
	}
	chunk := o.Chunk
	if chunk <= 0 {
		chunk = 16 << 10
	}
	start := time.Now()
	var sent, received, maxPause atomic.Int64
	sem := make(chan struct{}, echoWindow)
	readDone := make(chan error, 1)
	sendDone := make(chan error, 1)

	go func() {
		last := time.Now()
		for received.Load() < o.Total {
			f, err := st.ReadFrame()
			if err != nil {
				readDone <- err
				return
			}
			now := time.Now()
			if p := int64(now.Sub(last)); p > maxPause.Load() {
				maxPause.Store(p)
			}
			last = now
			received.Add(int64(len(f.Payload)))
			<-sem
		}
		readDone <- nil
	}()
	go func() {
		buf := make([]byte, chunk)
		for sent.Load() < o.Total {
			n := int64(chunk)
			if left := o.Total - sent.Load(); left < n {
				n = left
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				sendDone <- ctx.Err()
				return
			}
			_, _ = rand.Read(buf[:n])
			if err := st.WriteFrame(echoFrameData, 0, buf[:n]); err != nil {
				sendDone <- err
				return
			}
			sent.Add(n)
			if o.Hook != nil {
				o.Hook(sent.Load())
			}
			if o.Pace > 0 {
				time.Sleep(o.Pace)
			}
		}
		sendDone <- nil
	}()

	sending := true
loop:
	for {
		select {
		case err := <-sendDone:
			sending = false
			if err != nil {
				run.Err = err
				break loop
			}
			sendDone = nil
		case err := <-readDone:
			run.Err = err
			break loop
		case <-ctx.Done():
			run.Err = ctx.Err()
			break loop
		}
	}
	_ = sending
	go func() { _ = st.Close() }()
	run.Sent, run.Received = sent.Load(), received.Load()
	run.MaxPause, run.Elapsed = time.Duration(maxPause.Load()), time.Since(start)
	if run.Err == nil && run.Received != run.Sent {
		run.Err = fmt.Errorf("echo returned %d of %d bytes", run.Received, run.Sent)
	}
	return run
}
