package relay

import (
	"context"
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// ErrCircuitClosed: the datagram circuit ended.
var ErrCircuitClosed = errors.New("relay: datagram circuit closed")

const dgramQueue = 256

// Hub routes RELAY_DGRAM received on the sessions with relays to the datagram circuits of this
// node, by circuit ID. One Hub serves all circuits of a node.
type Hub struct {
	mu       sync.Mutex
	circuits map[uint64]*DatagramCircuit
	dropped  uint64
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{circuits: map[uint64]*DatagramCircuit{}} }

// Dropped counts datagrams for unknown circuits and for full queues.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// DatagramCircuit carries the sealed datagrams of a session over R (mode 2). Its payloads are
// whole mux datagrams (`type ‖ body`, normally a SEALED datagram): R forwards them unchanged.
type DatagramCircuit struct {
	cid    uint64
	hub    *Hub
	send   func(typ uint64, p []byte) error
	anchor *mux.Stream
	in     chan []byte
	done   chan struct{}
	once   sync.Once
}

func (h *Hub) newCircuit(cid uint64, send func(uint64, []byte) error, anchor *mux.Stream) *DatagramCircuit {
	d := &DatagramCircuit{cid: cid, hub: h, send: send, anchor: anchor,
		in: make(chan []byte, dgramQueue), done: make(chan struct{})}
	h.mu.Lock()
	h.circuits[cid] = d
	h.mu.Unlock()
	if anchor != nil {
		// The circuit lives as long as its anchor stream: R resets it when the circuit ends.
		go func() {
			_, _ = anchor.ReadFrame() // nothing is ever sent on it; EOF, reset or a frame ends it
			d.Close()
		}()
	}
	return d
}

// CID is the circuit ID.
func (d *DatagramCircuit) CID() uint64 { return d.cid }

// Send sends one datagram best effort.
func (d *DatagramCircuit) Send(dg []byte) error {
	select {
	case <-d.done:
		return ErrCircuitClosed
	default:
	}
	b := frame.AppendVarint(make([]byte, 0, len(dg)+9), d.cid)
	return d.send(frame.DgramRelay, append(b, dg...))
}

// Receive returns the next datagram. extra (the owner's done channel) may end the wait.
func (d *DatagramCircuit) Receive(ctx context.Context, extra <-chan struct{}) ([]byte, error) {
	select {
	case b := <-d.in:
		return b, nil
	case <-d.done:
		return nil, ErrCircuitClosed
	case <-extra:
		return nil, ErrCircuitClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close ends the circuit.
func (d *DatagramCircuit) Close() {
	d.once.Do(func() {
		close(d.done)
		d.hub.mu.Lock()
		if d.hub.circuits[d.cid] == d {
			delete(d.hub.circuits, d.cid)
		}
		d.hub.mu.Unlock()
		if d.anchor != nil {
			d.anchor.Reset(frame.CodePathFailed)
		}
	})
}

// HandleDatagram delivers a RELAY_DGRAM payload (`varint cid ‖ datagram`) received from a relay.
// Unknown circuits and full queues drop (a datagram is never answered, wire.md §6.5).
func (h *Hub) HandleDatagram(payload []byte) {
	cid, n, err := frame.ParseVarint(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.circuits[cid]
	if err != nil || d == nil {
		h.dropped++
		return
	}
	select {
	case d.in <- append([]byte(nil), payload[n:]...):
	default:
		h.dropped++
	}
}
