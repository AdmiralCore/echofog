package dpi

import (
	"net"

	"golang.org/x/net/ipv4"
)

// quic-go turns on its UDP fast path -- GSO (one sendmsg for a train of packets), ECN and batch
// reads (recvmmsg) -- only for a connection that has the methods of quic.OOBCapablePacketConn and
// either ReadBatch or net.Conn. Without ReadBatch it would fall back to reading the socket's file
// descriptor itself, past every wrapper, so each wrapper below that offers ReadMsgUDP and
// WriteMsgUDP offers ReadBatch too. quic-go uses the fast path on Linux, macOS and FreeBSD; on
// Windows it reads and writes one packet at a time with ReadFrom/WriteTo whatever the connection
// offers, so there the methods below are never called.

// OOBBufferSize is the room for control messages (ECN, packet info) of one datagram, as quic-go
// gives it.
const OOBBufferSize = 128

// BatchReader reads several datagrams in one system call where the platform can (recvmmsg on
// Linux; one datagram per call elsewhere).
type BatchReader interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
}

// OOBPacketConn is a UDP-like connection with control messages and batch reads: what quic-go
// needs for GSO, ECN and batch reads.
type OOBPacketConn interface {
	UDPLikePacketConn
	ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error)
	WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error)
	BatchReader
}

// udpOOB gives a *net.UDPConn the batch reader of x/net, as quic-go does for one.
type udpOOB struct {
	*net.UDPConn
	pc *ipv4.PacketConn
}

func (u *udpOOB) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	return u.pc.ReadBatch(ms, flags)
}

// AsOOB returns c as an OOBPacketConn: c itself when it has all the methods, a *net.UDPConn with a
// batch reader, or false.
func AsOOB(c net.PacketConn) (OOBPacketConn, bool) {
	switch v := c.(type) {
	case OOBPacketConn:
		return v, true
	case *net.UDPConn:
		return &udpOOB{UDPConn: v, pc: ipv4.NewPacketConn(v)}, true
	}
	return nil, false
}

// readMsgViaBatch implements ReadMsgUDP with a one-message batch.
func readMsgViaBatch(r BatchReader, b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	ms := []ipv4.Message{{Buffers: [][]byte{b}, OOB: oob}}
	got, err := r.ReadBatch(ms, 0)
	if err != nil || got == 0 {
		return 0, 0, 0, nil, err
	}
	ua, _ := ms[0].Addr.(*net.UDPAddr)
	return ms[0].N, ms[0].NN, ms[0].Flags, ua, nil
}

// newBatch makes n messages with their own data and control-message buffers.
func newBatch(n, dataSize int) []ipv4.Message {
	ms := make([]ipv4.Message, n)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, dataSize)}
		ms[i].OOB = make([]byte, OOBBufferSize)
	}
	return ms
}
