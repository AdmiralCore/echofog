package scenarios

import (
	"context"
	"crypto/rand"
	"fmt"
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
// sends the bytes through it, reading them back.
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
	defer st.Close()
	// a cancelled context closes the stream under a blocked read
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()

	chunk := o.Chunk
	if chunk <= 0 {
		chunk = 16 << 10
	}
	start := time.Now()
	type got struct {
		n   int64
		err error
	}
	reads := make(chan got, 1)
	sem := make(chan struct{}, echoWindow)
	done := make(chan struct{})
	var received int64
	var maxPause time.Duration
	go func() {
		defer close(done)
		last := time.Now()
		for received < o.Total {
			f, err := st.ReadFrame()
			if err != nil {
				reads <- got{err: err}
				return
			}
			now := time.Now()
			if p := now.Sub(last); p > maxPause {
				maxPause = p
			}
			last = now
			received += int64(len(f.Payload))
			<-sem
		}
		reads <- got{n: received}
	}()

	buf := make([]byte, chunk)
	var sent int64
	for sent < o.Total {
		n := int64(chunk)
		if o.Total-sent < n {
			n = o.Total - sent
		}
		select {
		case sem <- struct{}{}:
		case <-done:
			goto finish
		case <-ctx.Done():
			run.Err = ctx.Err()
			goto finish
		}
		_, _ = rand.Read(buf[:n])
		if err := st.WriteFrame(echoFrameData, 0, buf[:n]); err != nil {
			run.Err = err
			goto finish
		}
		sent += n
		if o.Hook != nil {
			o.Hook(sent)
		}
		if o.Pace > 0 {
			time.Sleep(o.Pace)
		}
	}
finish:
	run.Sent = sent
	if run.Err == nil {
		select {
		case r := <-reads:
			run.Err = r.err
		case <-ctx.Done():
			run.Err = ctx.Err()
		}
	}
	_ = st.Close()
	<-done
	run.Received, run.MaxPause, run.Elapsed = received, maxPause, time.Since(start)
	if run.Err == nil && run.Received != run.Sent {
		run.Err = fmt.Errorf("echo returned %d of %d bytes", run.Received, run.Sent)
	}
	return run
}
