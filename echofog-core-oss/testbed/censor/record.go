package censor

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Recorder wraps a net.Conn and keeps what a border would see of it: the chunks in each direction
// with their times, and the TLS records of the first 16 KB each way. It is how the tests and the
// harness take real v2 traffic as a Flow without a capture. A chunk is one Read or Write, so it
// stands for the packets of one burst, not for a single packet.
type Recorder struct {
	net.Conn
	mu    sync.Mutex
	start time.Time
	flow  Flow
	scan  [2]recordScanner
	last  time.Duration
}

// NewRecorder starts recording c; times are counted from start.
func NewRecorder(c net.Conn, start time.Time) *Recorder {
	r := &Recorder{Conn: c, start: start}
	r.flow = Flow{Proto: "tcp", Start: start}
	if a, ok := c.LocalAddr().(*net.TCPAddr); ok {
		if ap, ok := netip.AddrFromSlice(a.IP); ok {
			r.flow.Client = netip.AddrPortFrom(ap.Unmap(), uint16(a.Port))
		}
	}
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if ap, ok := netip.AddrFromSlice(a.IP); ok {
			r.flow.Server = netip.AddrPortFrom(ap.Unmap(), uint16(a.Port))
		}
	}
	return r
}

func (r *Recorder) note(b []byte, up bool) {
	if len(b) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	at := time.Since(r.start)
	r.last = at
	f := &r.flow
	f.Packets++
	if len(f.Pkts) < maxPkts {
		f.Pkts = append(f.Pkts, Pkt{At: at, Up: up, Payload: len(b)})
	}
	dir := 0
	if up {
		dir = 1
		f.BytesUp += len(b)
		if f.FirstSize == 0 {
			f.FirstSize = len(b)
		}
	} else {
		f.BytesDn += len(b)
		f.PayDn += len(b)
	}
	r.scan[dir].feed(b, at, up, &f.Records)
}

func (r *Recorder) Write(p []byte) (int, error) {
	n, err := r.Conn.Write(p)
	r.note(p[:n], true)
	return n, err
}

func (r *Recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.note(p[:n], false)
	return n, err
}

// Flow returns what was recorded so far as a TCP flow; the client is the side that wrapped the
// conn.
func (r *Recorder) Flow() Flow {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.flow
	f.Duration = r.last
	f.Pkts = append([]Pkt(nil), f.Pkts...)
	f.Records = append([]Record(nil), f.Records...)
	if len(f.Records) > 0 {
		f.Looks = "TLS"
	}
	return f
}
