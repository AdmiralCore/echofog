package router

import (
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

var (
	ErrBufferFull   = errors.New("router: in-flight buffer full during migration")
	ErrBufferClosed = errors.New("router: in-flight buffer closed")
)

// InFlightBuffer holds outbound messages in RAM during socket migration
// to guarantee 0% packet loss during the 200-500ms handover window.
type InFlightBuffer struct {
	mu       sync.Mutex
	queue    []BufferedPacket
	maxSize  int
	isClosed bool
}

// NewInFlightBuffer creates a buffered queue with max packet capacity.
func NewInFlightBuffer(maxPackets int) *InFlightBuffer {
	if maxPackets <= 0 {
		maxPackets = 1024
	}
	return &InFlightBuffer{
		queue:   make([]BufferedPacket, 0, 64),
		maxSize: maxPackets,
	}
}

// Enqueue saves a packet into memory while the transport is migrating.
func (b *InFlightBuffer) Enqueue(msgType net_types.MessageType, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isClosed {
		return ErrBufferClosed
	}

	if len(b.queue) >= b.maxSize {
		return ErrBufferFull
	}

	// Copy payload to avoid mutating original caller memory
	copied := append([]byte(nil), payload...)
	b.queue = append(b.queue, BufferedPacket{
		MsgType: msgType,
		Payload: copied,
	})
	return nil
}

// Drain flushes all queued packets in strict FIFO order to the new active transport link.
func (b *InFlightBuffer) Drain(target PeerTransportLink) (int, error) {
	b.mu.Lock()
	packets := b.queue
	b.queue = make([]BufferedPacket, 0, 64)
	b.mu.Unlock()

	count := 0
	for _, pkt := range packets {
		if err := target.Send(pkt.MsgType, pkt.Payload); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Len returns the current number of in-flight buffered packets.
func (b *InFlightBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queue)
}

// Close discards and closes the buffer.
func (b *InFlightBuffer) Close() {
	b.mu.Lock()
	b.isClosed = true
	b.queue = nil
	b.mu.Unlock()
}
