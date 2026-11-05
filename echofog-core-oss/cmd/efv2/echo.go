package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// The lab echo service lives in the experimental service range (registries.md §3). It exists
// only in efv2: no module registers it in the runtime.
const (
	echoServiceID uint64 = service.ExperimentalMin // 0xf000
	echoVersion   uint64 = 1
	echoFrameData uint64 = 0x40 // ECHO_DATA, a raw byte run
	echoChunk            = 64 << 10
	echoWindow           = 8 // frames in flight, 512 KiB of buffering

	echoDgramType    uint64 = service.ExperimentalMin
	echoDgramRequest byte   = 0
	echoDgramReply   byte   = 1
)

var echoSpec = service.Spec{
	ID:   echoServiceID,
	Name: "echo",
	Versions: map[uint64]service.Version{
		echoVersion: {Types: []service.FrameType{{
			Type: echoFrameData, Name: "ECHO_DATA", Max: echoChunk,
		}}},
	},
}

// registerEcho registers the test service and its stream handler. Every app registers it so the
// service is offered and selected in the handshake; only `run` also starts the datagram echo.
func registerEcho(rt *node.Runtime) {
	rt.Register(echoSpec, func(_ context.Context, _ node.Peer, st *mux.Stream) {
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

// startDatagramEcho answers request datagrams with a reply datagram. The request/reply byte keeps
// two nodes that both run this handler from echoing each other forever.
func startDatagramEcho(ctx context.Context, rt *node.Runtime) {
	port := rt.Datagrams(echoDgramType)
	go func() {
		defer port.Close()
		for {
			d, err := port.Receive(ctx)
			if err != nil {
				return
			}
			if len(d.Payload) == 0 || d.Payload[0] != echoDgramRequest {
				continue
			}
			reply := append([]byte{echoDgramReply}, d.Payload[1:]...)
			_ = port.Send(ctx, d.Peer.Device, echoDgramType, reply)
		}
	}()
}

// echoResult is the machine-readable outcome of `efv2 echo`, also used by the test bed report.
type echoResult struct {
	Suite             uint16  `json:"suite"`
	Path              string  `json:"path"`
	RTTMs             float64 `json:"rtt_ms"`
	BytesSent         int64   `json:"bytes_sent"`
	BytesReceived     int64   `json:"bytes_received"`
	Seconds           float64 `json:"seconds"`
	Mbps              float64 `json:"mbps"`
	DatagramsSent     int     `json:"datagrams_sent"`
	DatagramsReceived int     `json:"datagrams_received"`
	PeerNodeID        string  `json:"peer_node_id"`
	PeerDeviceID      string  `json:"peer_device_id"`
}

// Echo dials the card, opens the echo service, streams the requested number of bytes through it,
// then exchanges request/reply datagrams. Throughput is measured on the stream only.
func (a *app) Echo(ctx context.Context, doc cardDoc, bytes int64, datagrams int) (echoResult, error) {
	if bytes < 0 {
		return echoResult{}, fmt.Errorf("bytes %d: want a non-negative count", bytes)
	}
	card, _, err := verifyCardDoc(doc, "", a.clock.Now())
	if err != nil {
		return echoResult{}, err
	}
	device := node.DeviceID(card.DeviceID)

	peer, sess, err := a.rt.Session(ctx, device, node.DialOpt{Card: card})
	if err != nil {
		return echoResult{}, fmt.Errorf("dial: %w", err)
	}
	rtt, err := sess.Ping(ctx)
	if err != nil {
		return echoResult{}, fmt.Errorf("rtt: %w", err)
	}
	_, stream, err := a.rt.Open(ctx, device, echoServiceID, node.DialOpt{Card: card})
	if err != nil {
		return echoResult{}, fmt.Errorf("open echo service: %w", err)
	}
	defer stream.Close()

	start := time.Now()
	sent, received, err := echoStream(ctx, stream, bytes)
	elapsed := time.Since(start)
	if err != nil {
		return echoResult{}, err
	}
	sentDgrams, receivedDgrams, err := a.echoDatagrams(ctx, sess, device, datagrams)
	if err != nil {
		return echoResult{}, err
	}

	res := echoResult{
		Path:              pathName(peer.Path),
		RTTMs:             float64(rtt) / float64(time.Millisecond),
		BytesSent:         sent,
		BytesReceived:     received,
		Seconds:           elapsed.Seconds(),
		DatagramsSent:     sentDgrams,
		DatagramsReceived: receivedDgrams,
		PeerNodeID:        peer.Node.String(),
		PeerDeviceID:      peer.Device.String(),
	}
	if elapsed > 0 {
		res.Mbps = float64(sent*8) / elapsed.Seconds() / 1e6
	}
	if s, ok := a.sessionStatus(device); ok {
		res.Suite = s.Suite
		res.Path = pathName(s.Path)
	}
	return res, nil
}

// echoStream writes the byte run through the stream and reads it back. A bounded window of
// frames in flight keeps the server's echo from filling both directions while the writer still
// has not read anything.
func echoStream(ctx context.Context, st *mux.Stream, total int64) (sent, received int64, err error) {
	if total == 0 {
		return 0, 0, nil
	}

	sem := make(chan struct{}, echoWindow)
	readerErr := make(chan error, 1)
	readerGot := make(chan int64, 1)
	go func() {
		var got int64
		for got < total {
			f, ferr := st.ReadFrame()
			if ferr != nil {
				_ = st.Close()
				readerErr <- ferr
				return
			}
			if f.Type != echoFrameData {
				_ = st.Close()
				readerErr <- fmt.Errorf("echo: frame %#x, want %#x", f.Type, echoFrameData)
				return
			}
			got += int64(len(f.Payload))
			if got > total {
				_ = st.Close()
				readerErr <- fmt.Errorf("echo: peer sent %d bytes, want %d", got, total)
				return
			}
			<-sem
		}
		readerGot <- got
	}()

	chunk := make([]byte, echoChunk)
	for sent < total {
		n := int64(echoChunk)
		if total-sent < n {
			n = total - sent
		}
		select {
		case sem <- struct{}{}:
		case rerr := <-readerErr:
			return sent, 0, rerr
		case <-ctx.Done():
			return sent, 0, ctx.Err()
		}
		if _, rerr := rand.Read(chunk[:n]); rerr != nil {
			return sent, 0, rerr
		}
		if werr := st.WriteFrame(echoFrameData, 0, chunk[:n]); werr != nil {
			return sent, 0, werr
		}
		sent += n
	}

	select {
	case rerr := <-readerErr:
		return sent, 0, rerr
	case got := <-readerGot:
		return sent, got, nil
	case <-ctx.Done():
		return sent, 0, ctx.Err()
	}
}

// echoDatagrams sends count request datagrams and waits for the matching replies. It uses the
// session directly for sending: the port is for receiving, where the runtime fans datagrams in
// from every live session.
func (a *app) echoDatagrams(ctx context.Context, sess *mux.Session, device node.DeviceID, count int) (sent, received int, err error) {
	if count <= 0 {
		return 0, 0, nil
	}
	port := a.rt.Datagrams(echoDgramType)
	defer port.Close()

	payload := make([]byte, 32)
	for i := 0; i < count; i++ {
		payload[0] = echoDgramRequest
		binary.BigEndian.PutUint32(payload[1:5], uint32(i))
		if _, rerr := rand.Read(payload[5:]); rerr != nil {
			return sent, received, rerr
		}
		if serr := sess.SendDatagram(echoDgramType, payload); serr != nil {
			return sent, received, fmt.Errorf("datagram %d: %w", i, serr)
		}
		sent++
		deadline := time.Now().Add(3 * time.Second)
		for {
			waitCtx, cancel := context.WithDeadline(ctx, deadline)
			d, rerr := port.Receive(waitCtx)
			cancel()
			if rerr != nil {
				return sent, received, fmt.Errorf("datagram %d reply: %w", i, rerr)
			}
			if len(d.Payload) < 5 || d.Payload[0] != echoDgramReply {
				continue
			}
			if binary.BigEndian.Uint32(d.Payload[1:5]) != uint32(i) {
				continue
			}
			received++
			break
		}
	}
	return sent, received, nil
}

func pathName(k mux.PathKind) string {
	if k == mux.PathRelay {
		return "relay"
	}
	return "direct"
}
