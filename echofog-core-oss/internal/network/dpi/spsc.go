package dpi

import (
	"errors"
	"net/netip"
	"sync/atomic"
)

var ErrNotPowerTwo = errors.New("size must be a power of 2")

type packet struct {
	bufPtr *[]byte
	n      int
	addr   netip.AddrPort
}

type RingBuffer struct {
	head atomic.Uint64
	_    [56]byte

	tail atomic.Uint64
	_    [56]byte

	buffer []packet
	mask   uint64
}

func NewRingBuffer(size uint64) (*RingBuffer, error) {
	if size == 0 || (size&(size-1)) != 0 {
		return nil, ErrNotPowerTwo
	}
	return &RingBuffer{
		buffer: make([]packet, size),
		mask:   size - 1,
	}, nil
}

func (r *RingBuffer) Push(pkt packet) bool {
	h := r.head.Load()
	t := r.tail.Load()

	if h-t >= uint64(len(r.buffer)) {
		return false
	}

	idx := h & r.mask
	r.buffer[idx] = pkt
	r.head.Add(1)

	return true
}

func (r *RingBuffer) Pop() (packet, bool) {
	t := r.tail.Load()
	h := r.head.Load()

	if t == h {
		return packet{}, false
	}

	idx := t & r.mask
	pkt := r.buffer[idx]

	r.buffer[idx] = packet{}
	r.tail.Add(1)

	return pkt, true
}
