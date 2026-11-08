package scenarios

import (
	"errors"
	"io"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

// TCP flag bits as they are in the header.
const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpPSH = 0x08
	tcpACK = 0x10
)

var errNotWritten = errors.New("scenarios: not written")

// PcapWriter writes a classic pcap stream (Ethernet, IPv4, UDP or TCP) of packets a tap saw.
type PcapWriter struct{}

// NewPcapWriter writes the pcap file header to w and returns the writer.
func NewPcapWriter(w io.Writer) (*PcapWriter, error) { return nil, errNotWritten }

// UDP writes one datagram.
func (p *PcapWriter) UDP(at time.Time, src, dst netip.AddrPort, payload []byte) error {
	return errNotWritten
}

// TCP writes one segment.
func (p *PcapWriter) TCP(at time.Time, src, dst netip.AddrPort, seq, ack uint32, flags uint8, payload []byte) error {
	return errNotWritten
}

// newCapture returns a PcapWriter that feeds the censor's analyzer directly, and a function that
// ends the stream and returns the flows.
func newCapture() (*PcapWriter, func() ([]censor.Flow, error)) {
	return nil, func() ([]censor.Flow, error) { return nil, errNotWritten }
}
