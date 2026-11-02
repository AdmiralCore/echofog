package dpi

import (
	"crypto/rand"
	"net"

	"golang.org/x/net/ipv4"
)

// obfsReadBatch is how many datagrams one ReadBatch takes from the socket; quic-go asks for 8 on
// Linux.
const obfsReadBatch = 8

var _ OOBPacketConn = (*ObfsPacketConnOOB)(nil)

// ObfsPacketConnOOB is the obfuscation over a connection with control messages and batch reads
// (see OOBPacketConn), so quic-go keeps GSO, ECN and batch reads under it. Every datagram on the
// wire is sealed exactly as WriteTo seals it: a GSO train from quic-go is cut into its segments,
// each sealed on its own with its own salt, and handed to the kernel as one train again with the
// segment size grown by Overhead. A receiver that reads one datagram at a time sees no difference.
type ObfsPacketConnOOB struct {
	*ObfsPacketConnUDP
	oc OOBPacketConn

	rms []ipv4.Message // raw datagrams of one batch, under readMu

	gsoBuf []byte // sealed train, under writeMu
	salts  []byte
	oobBuf []byte
}

// ReadBatch reads up to len(ms) datagrams and returns those meant for us, unmasked, with their
// control messages. It blocks until at least one is.
func (c *ObfsPacketConnOOB) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	raw := c.rms[:min(len(ms), len(c.rms))]
	for {
		got, err := c.oc.ReadBatch(raw, flags)
		if err != nil {
			return 0, err
		}
		k := 0
		for i := 0; i < got; i++ {
			src, dst := &raw[i], &ms[k]
			n, ok := c.unmask(src.Buffers[0][:src.N], src.Addr, dst.Buffers[0])
			if !ok {
				continue // not for us: a scanner, a stray packet, or a peer with another key
			}
			dst.N, dst.NN, dst.Flags, dst.Addr = n, copy(dst.OOB, src.OOB[:src.NN]), src.Flags, src.Addr
			k++
		}
		if k > 0 {
			return k, nil
		}
	}
}

// ReadMsgUDP reads one datagram meant for us.
func (c *ObfsPacketConnOOB) ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	return readMsgViaBatch(c, b, oob)
}

// WriteMsgUDP seals b, or each GSO segment of it, and sends it with its control messages.
func (c *ObfsPacketConnOOB) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if c.obfs.IsRaw(addr) {
		return c.oc.WriteMsgUDP(b, oob, addr)
	}
	key := c.obfs.sendKey(addr)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	seg, at := gsoSegment(oob)
	if seg <= 0 || seg >= len(b) {
		seg = len(b) // one datagram
	}
	nseg := 1
	if seg > 0 {
		nseg = (len(b) + seg - 1) / seg
	}
	need := len(b) + nseg*Overhead
	if cap(c.gsoBuf) < need {
		c.gsoBuf = make([]byte, need)
	}
	buf := c.gsoBuf[:need]
	if cap(c.salts) < nseg*SaltLen {
		c.salts = make([]byte, nseg*SaltLen)
	}
	salts := c.salts[:nseg*SaltLen]
	if _, err := rand.Read(salts); err != nil {
		return 0, 0, err
	}
	off := 0
	for i := 0; i < nseg; i++ {
		s := b[i*seg : min((i+1)*seg, len(b))]
		m, err := seal(&key, s, buf[off:], salts[i*SaltLen:(i+1)*SaltLen])
		if err != nil {
			return 0, 0, err
		}
		off += m
	}
	if at >= 0 {
		// the kernel cuts the train at the segment size: make it the sealed segment's size
		c.oobBuf = append(c.oobBuf[:0], oob...)
		setGSOSegment(c.oobBuf, at, seg+Overhead)
		oob = c.oobBuf
	}
	_, oobn, err := c.oc.WriteMsgUDP(buf[:off], oob, addr)
	if err != nil {
		return 0, oobn, err
	}
	return len(b), oobn, nil
}
