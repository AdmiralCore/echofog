//go:build linux

package dpi

import (
	"net"

	"golang.org/x/net/ipv4"
)

// muxReadBatch is how many datagrams the read loop takes per recvmmsg.
const muxReadBatch = 16

// readLoop reads the shared socket in batches (recvmmsg) with control messages, so quic-go
// behind the multiplexer still gets the ECN bits and the arrival address of every datagram.
func (m *MuxedConn) readLoop() {
	pc := ipv4.NewPacketConn(m.conn)
	msgs := make([]ipv4.Message, muxReadBatch)
	bufs := make([]*[]byte, muxReadBatch)
	for i := range msgs {
		msgs[i].Buffers = make([][]byte, 1)
	}
	defer func() {
		for _, b := range bufs {
			if b != nil {
				packetPool.Put(b)
			}
		}
	}()
	for {
		for i, b := range bufs {
			if b == nil {
				b = packetPool.Get().(*[]byte)
				bufs[i] = b
				msgs[i].Buffers[0] = (*b)[:maxDatagram]
				msgs[i].OOB = (*b)[maxDatagram:]
			}
		}
		n, err := pc.ReadBatch(msgs, 0)
		if err != nil {
			return
		}
		for i := 0; i < n; i++ {
			ua, ok := msgs[i].Addr.(*net.UDPAddr)
			if !ok {
				continue // keeps its buffer for the next read
			}
			m.dispatch(bufs[i], msgs[i].N, msgs[i].NN, ua.AddrPort())
			bufs[i] = nil
		}
	}
}
