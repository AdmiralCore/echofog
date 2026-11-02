package network

import (
	"net"

	"github.com/apernet/quic-go"
	"github.com/echofog/echofog-core/internal/network/dpi"
	"golang.org/x/net/ipv4"
)

var _ quic.OOBCapablePacketConn = (*quicRealityOOB)(nil)

// quicRealityOOB is a QuicRealityConn over an OOB-capable connection (a *net.UDPConn, the
// multiplexer, the obfuscation over either), with the methods quic-go needs to keep GSO, ECN and
// batch reads (see dpi.OOBPacketConn). Every datagram goes through the same REALITY gating as
// ReadFrom; on Windows quic-go keeps calling ReadFrom and WriteTo.
type quicRealityOOB struct {
	*QuicRealityConn
	oc dpi.OOBPacketConn
}

// PacketConn is what to give quic-go: c itself, or over an OOB-capable connection, a view of c
// that keeps GSO, ECN and batch reads.
func (c *QuicRealityConn) PacketConn() net.PacketConn {
	if oc, ok := dpi.AsOOB(c.base); ok {
		return &quicRealityOOB{QuicRealityConn: c, oc: oc}
	}
	return c
}

// ReadBatch reads datagrams in batches and hands quic-go those that pass the REALITY gating, in
// arrival order: the datagrams of a ClientHello flight verified in the middle of a batch come
// before the ones that followed it.
func (c *quicRealityOOB) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	for {
		if k := c.fromDrain(ms); k > 0 {
			return k, nil
		}
		got, err := c.oc.ReadBatch(ms, flags)
		if err != nil {
			return 0, err
		}
		k := 0
		for i := 0; i < got; i++ {
			m := &ms[i]
			data, oob := m.Buffers[0][:m.N], m.OOB[:m.NN]
			if !c.admit(data, oob, m.Addr) {
				continue
			}
			if c.drainN.Load() > 0 {
				c.pushDrain(drainPkt{b: append([]byte(nil), data...), oob: append([]byte(nil), oob...), addr: m.Addr})
				continue
			}
			if k != i {
				// copy, not swap: quic-go pairs each message with its own packet buffer
				d := &ms[k]
				d.N = copy(d.Buffers[0], data)
				d.NN = copy(d.OOB, oob)
				d.Flags, d.Addr = m.Flags, m.Addr
			}
			k++
		}
		if k > 0 {
			return k, nil
		}
	}
}

// fromDrain fills ms with buffered datagrams of verified flights.
func (c *quicRealityOOB) fromDrain(ms []ipv4.Message) int {
	k := 0
	for k < len(ms) {
		d, ok := c.popDrain()
		if !ok {
			break
		}
		m := &ms[k]
		m.N = copy(m.Buffers[0], d.b)
		m.NN = copy(m.OOB, d.oob)
		m.Flags, m.Addr = 0, d.addr
		k++
	}
	return k
}

// ReadMsgUDP reads one datagram that passes the gating.
func (c *quicRealityOOB) ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	ms := []ipv4.Message{{Buffers: [][]byte{b}, OOB: oob}}
	if _, err := c.ReadBatch(ms, 0); err != nil {
		return 0, 0, 0, nil, err
	}
	ua, _ := ms[0].Addr.(*net.UDPAddr)
	return ms[0].N, ms[0].NN, ms[0].Flags, ua, nil
}

// WriteMsgUDP writes b (one packet or a GSO train) with quic-go's control messages. Packets that
// pass for a browser's (REALITY) leave without ECN marks, as Chrome's do and as they did before
// quic-go could mark them here; quic-go then sees no marks echoed and turns ECN off for that
// path.
func (c *quicRealityOOB) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	c.followRebinding(b, addr)
	if dpi.HasECNMark(oob) && c.passesForBrowser(addr) {
		oob = dpi.StripECN(make([]byte, 0, len(oob)), oob)
	}
	return c.oc.WriteMsgUDP(b, oob, addr)
}

// passesForBrowser: packets to addr are REALITY's raw QUIC, which must look like a browser's.
func (c *QuicRealityConn) passesForBrowser(addr net.Addr) bool {
	if c.browserClient {
		return true
	}
	c.mu.Lock()
	server := c.serverMode
	c.mu.Unlock()
	if !server {
		return false
	}
	if c.qt == nil || c.qt.obfs == nil {
		return true
	}
	return c.qt.obfs.IsRaw(addr)
}
