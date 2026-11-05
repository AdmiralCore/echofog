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

// hubKey identifies a circuit at a node: the circuit ID is only unique at the relay that drew it.
type hubKey struct {
	relay [32]byte
	cid   uint64
}

// ErrDuplicateCircuit: the relay offered a circuit ID this node already holds for it.
var ErrDuplicateCircuit = errors.New("relay: duplicate circuit id")

// Hub routes RELAY_DGRAM received on the sessions with relays to the datagram circuits of this
// node, by (relay, circuit ID). One Hub serves all circuits of a node.
type Hub struct {
	mu       sync.Mutex
	circuits map[hubKey]*DatagramCircuit
	dropped  uint64
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{circuits: map[hubKey]*DatagramCircuit{}} }

// Dropped counts datagrams for unknown circuits and for full queues.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// DatagramCircuit carries the sealed datagrams of a session over R (mode 2). Its payloads are
// whole mux datagrams (`type ‖ body`, normally a SEALED datagram): R forwards them unchanged.
type DatagramCircuit struct {
	key    hubKey
	cid    uint64
	hub    *Hub
	send   func(typ uint64, p []byte) error
	anchor *mux.Stream
	in     chan []byte
	done   chan struct{}
	once   sync.Once
}

func (h *Hub) newCircuit(relay [32]byte, cid uint64, send func(uint64, []byte) error, anchor *mux.Stream) (*DatagramCircuit, error) {
	key := hubKey{relay, cid}
	d := &DatagramCircuit{key: key, cid: cid, hub: h, send: send, anchor: anchor,
		in: make(chan []byte, dgramQueue), done: make(chan struct{})}
	h.mu.Lock()
	if _, dup := h.circuits[key]; dup {
		h.mu.Unlock()
		return nil, ErrDuplicateCircuit
	}
	h.circuits[key] = d
	h.mu.Unlock()
	if anchor != nil {
		// The circuit lives as long as its anchor stream: R resets it when the circuit ends.
		go func() {
			_, _ = anchor.ReadFrame() // nothing is ever sent on it; EOF, reset or a frame ends it
			d.Close()
		}()
	}
	return d, nil
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
		if d.hub.circuits[d.key] == d {
			delete(d.hub.circuits, d.key)
		}
		d.hub.mu.Unlock()
		if d.anchor != nil {
			d.anchor.Reset(frame.CodePathFailed)
		}
	})
}

// HandleDatagram delivers a RELAY_DGRAM payload (`varint cid ‖ datagram`) received on the session
// with the node `from`. Only the circuit of that relay can receive it: a datagram from any other
// session, an unknown circuit or a full queue is dropped (a datagram is never answered,
// wire.md §6.5).
func (h *Hub) HandleDatagram(from [32]byte, payload []byte) {
	cid, n, err := frame.ParseVarint(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.circuits[hubKey{from, cid}]
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
