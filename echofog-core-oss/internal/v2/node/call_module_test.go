package node_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// TestCallModuleServesSignalStream is the T9.1 wiring test: two runtimes offer call/2, the
// service opens, a well-formed hop-local SIGNAL is accepted, and a malformed wrapper ends the
// stream with CLOSE{protocol-violation} as services.md 1 requires.
func TestCallModuleServesSignalStream(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	b.start(t)
	a.start(t)

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.Call, node.DialOpt{
		Card:    b.peerCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Open call/2: %v", err)
	}
	defer stream.Close()

	msg := &pb.CallSignal{
		Action: call.ActionPing, CallId: "call-1", Sender: a.id.DeviceID[:],
		MsgId: 1, Timestamp: 1,
	}
	raw, err := call.Encode(msg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.WriteFrame(call.TypeSignal, 0, raw); err != nil {
		t.Fatalf("write SIGNAL: %v", err)
	}
	// The origin field must be omitted when empty; an explicit empty value is non-canonical and
	// the service closes the stream with protocol-violation.
	if err := stream.WriteFrame(call.TypeSignal, 0, []byte{0x0a, 0x00}); err != nil {
		t.Fatalf("write malformed SIGNAL: %v", err)
	}
	_, err = stream.ReadFrame()
	var ce *mux.CloseError
	if !errors.As(err, &ce) || ce.Code != frame.CodeProtocolViolation {
		t.Fatalf("read after malformed SIGNAL: %v, want CLOSE{protocol-violation}", err)
	}
}
