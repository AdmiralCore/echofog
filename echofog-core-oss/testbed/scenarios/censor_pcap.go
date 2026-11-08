package scenarios

import (
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

// TCP flag bits as they are in the header.
const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpRST = 0x04
	tcpPSH = 0x08
	tcpACK = 0x10
)

// PcapWriter writes a classic pcap stream (link type Ethernet, microsecond stamps) of IPv4 packets
// that a tap saw, with the addresses the tap chooses to show. The IP header checksum is right; the
// TCP and UDP checksums are left zero, which no analyzer here reads. Safe for concurrent use.
type PcapWriter struct {
	mu sync.Mutex
	w  io.Writer
	id uint16
}

// NewPcapWriter writes the pcap file header to w and returns the writer.
func NewPcapWriter(w io.Writer) (*PcapWriter, error) {
	var h [24]byte
	binary.LittleEndian.PutUint32(h[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 65535)
	binary.LittleEndian.PutUint32(h[20:], 1) // Ethernet
	if _, err := w.Write(h[:]); err != nil {
		return nil, err
	}
	return &PcapWriter{w: w}, nil
}

// UDP writes one datagram.
func (p *PcapWriter) UDP(at time.Time, src, dst netip.AddrPort, payload []byte) error {
	if len(payload) > 65535-28 {
		return errors.New("scenarios: datagram too large for one IPv4 packet")
	}
	l4 := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(l4[0:], src.Port())
	binary.BigEndian.PutUint16(l4[2:], dst.Port())
	binary.BigEndian.PutUint16(l4[4:], uint16(len(l4)))
	copy(l4[8:], payload)
	return p.write(at, 17, src.Addr(), dst.Addr(), l4)
}

// TCP writes one segment.
func (p *PcapWriter) TCP(at time.Time, src, dst netip.AddrPort, seq, ack uint32, flags uint8, payload []byte) error {
	if len(payload) > 65535-40 {
		return errors.New("scenarios: segment too large for one IPv4 packet")
	}
	l4 := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(l4[0:], src.Port())
	binary.BigEndian.PutUint16(l4[2:], dst.Port())
	binary.BigEndian.PutUint32(l4[4:], seq)
	binary.BigEndian.PutUint32(l4[8:], ack)
	l4[12] = 5 << 4
	l4[13] = flags
	binary.BigEndian.PutUint16(l4[14:], 65535)
	copy(l4[20:], payload)
	return p.write(at, 6, src.Addr(), dst.Addr(), l4)
}

func (p *PcapWriter) write(at time.Time, proto byte, src, dst netip.Addr, l4 []byte) error {
	if !src.Is4() || !dst.Is4() {
		return errors.New("scenarios: the pcap writer is IPv4 only")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.id++
	frame := make([]byte, 16+14+20+len(l4))
	rec, eth, ip := frame[:16], frame[16:30], frame[30:50]
	copy(frame[50:], l4)

	binary.LittleEndian.PutUint32(rec[0:], uint32(at.Unix()))
	binary.LittleEndian.PutUint32(rec[4:], uint32(at.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(frame)-16))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(frame)-16))

	s4, d4 := src.As4(), dst.As4()
	copy(eth[0:], []byte{0x02, 0, 0, 0, 0, d4[3]})
	copy(eth[6:], []byte{0x02, 0, 0, 0, 0, s4[3]})
	binary.BigEndian.PutUint16(eth[12:], 0x0800)

	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+len(l4)))
	binary.BigEndian.PutUint16(ip[4:], p.id)
	binary.BigEndian.PutUint16(ip[6:], 0x4000) // DF
	ip[8], ip[9] = 64, proto
	copy(ip[12:], s4[:])
	copy(ip[16:], d4[:])
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip))

	_, err := p.w.Write(frame)
	return err
}

func ipChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(h[i])<<8 | uint32(h[i+1])
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// newCapture returns a PcapWriter that feeds the censor's analyzer directly, and a function that
// ends the stream and returns the flows. The function may be called once.
func newCapture() (*PcapWriter, func() ([]censor.Flow, error)) {
	w, wait := censor.AnalyzeStream()
	pw, err := NewPcapWriter(w)
	if err != nil {
		return nil, func() ([]censor.Flow, error) { _ = w.Close(); return nil, err }
	}
	return pw, func() ([]censor.Flow, error) {
		_ = w.Close()
		return wait()
	}
}
