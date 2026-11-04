package core

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// A UDP session's datagrams used to ride its reliable stream, so one lost packet held up every
// datagram behind it: head-of-line blocking that browsers' HTTP/3, games and calls feel at once.
// Over a datagram-capable carrier they travel as DATAGRAM frames, the way hysteria carries UDP:
// the stream only sets the session up and gives it an ID. Over the TCP fallback, and for the rare
// datagram too big to fragment sensibly, the stream still carries them.

// UDPHelloDatagrams in the TypeExitUDP request says the client can take datagrams; an exit that
// can too answers with resultOK followed by the 4-byte session ID. Older peers ignore the byte
// or do not send the ID, and both sides then stay on the stream.
const UDPHelloDatagrams byte = 1

const (
	// UDPDgramHeader is session ID, packet ID, fragment index, count, address length.
	UDPDgramHeader = 4 + 2 + 1 + 1 + 2
	// udpMaxFragments: past this a datagram goes on the stream. Every fragment must arrive for
	// the datagram to count, so a big one cut in many pieces is lost far more often than it is
	// delayed on the stream.
	udpMaxFragments = 8
)

// DatagramTooLargeError is returned by a UDPLink that refused a frame larger than the path takes.
// Max is the largest frame the link accepts.
type DatagramTooLargeError struct {
	Size int
	Max  int
}

func (e *DatagramTooLargeError) Error() string {
	return "datagram too large for the path"
}

// UDPLink carries one direction of exit UDP datagrams to the peer at the other end of a stream,
// and delivers what that peer sends the other way.
type UDPLink interface {
	Send(frame []byte) error
	// Subscribe hands fn every frame for session id until the returned func is called; nil
	// when id is already taken. fn runs on the connection's datagram loop and must not block.
	Subscribe(id uint32, fn func([]byte)) func()
}

// DatagramStream is a stream that can give its UDP session a datagram link (a swarm stream does,
// over QUIC). toExit: the link sends towards the exit, as the client does.
type DatagramStream interface {
	UDPLink(toExit bool) UDPLink
}

// encodeUDPFragment builds one datagram frame.
func encodeUDPFragment(id uint32, pkt uint16, idx, count uint8, addr string, data []byte) []byte {
	out := make([]byte, UDPDgramHeader+len(addr)+len(data))
	binary.BigEndian.PutUint32(out, id)
	binary.BigEndian.PutUint16(out[4:], pkt)
	out[6], out[7] = idx, count
	binary.BigEndian.PutUint16(out[8:], uint16(len(addr)))
	copy(out[UDPDgramHeader:], addr)
	copy(out[UDPDgramHeader+len(addr):], data)
	return out
}

type udpFragment struct {
	pkt        uint16
	idx, count uint8
	addr       string
	data       []byte
}

// decodeUDPFragment parses a frame without its session ID (the demux took it).
func decodeUDPFragment(b []byte) (udpFragment, bool) {
	if len(b) < UDPDgramHeader-4 {
		return udpFragment{}, false
	}
	f := udpFragment{pkt: binary.BigEndian.Uint16(b), idx: b[2], count: b[3]}
	n := int(binary.BigEndian.Uint16(b[4:]))
	b = b[6:]
	if n == 0 || n > maxTargetLen || len(b) < n || f.count == 0 || f.count > udpMaxFragments || f.idx >= f.count {
		return udpFragment{}, false
	}
	f.addr, f.data = string(b[:n]), b[n:]
	return f, true
}

// defrag puts fragmented datagrams back together. Like hysteria's it keeps one datagram in
// progress: a sender emits a datagram's fragments back to back, and a fragment of a newer one
// means the old one lost a piece.
type defrag struct {
	mu    sync.Mutex
	pkt   uint16
	frags [][]byte
	got   int
	size  int
}

func (d *defrag) feed(f udpFragment) ([]byte, bool) {
	if f.count == 1 {
		return f.data, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frags == nil || f.pkt != d.pkt || len(d.frags) != int(f.count) {
		d.pkt, d.frags, d.got, d.size = f.pkt, make([][]byte, f.count), 0, 0
	}
	if d.frags[f.idx] != nil {
		return nil, false
	}
	d.frags[f.idx] = f.data
	d.got++
	d.size += len(f.data)
	if d.got < len(d.frags) {
		return nil, false
	}
	out := make([]byte, 0, d.size)
	for _, p := range d.frags {
		out = append(out, p...)
	}
	d.frags = nil
	return out, true
}

// dgramSender sends one session's datagrams over its link, cut to what the path takes.
type dgramSender struct {
	link    UDPLink
	id      uint32
	pkt     atomic.Uint32
	max     atomic.Int64 // largest frame the path takes, margin included; 0: not probed yet
	probeAt atomic.Int64 // unix nanos of the last probe
}

const (
	// udpDgramSlack is kept free under the limit the link reports. quic-go v0.58 reports the
	// whole packet size once path MTU discovery has grown it, not what is left for a datagram
	// after the packet header and the AEAD tag, and silently drops a frame between the two.
	udpDgramSlack = 48
	// udpProbeEvery: the limit is asked again this often, as path MTU discovery moves it.
	udpProbeEvery = 10 * time.Second
	// udpProbeSize is larger than any datagram a QUIC connection takes (quic-go never grows a
	// packet past 1452 bytes), so asking queues nothing.
	udpProbeSize = 4096
)

// limit returns the largest frame to send now, asking the link when it is not known or stale.
// An oversized frame is refused before anything is queued, which tells the limit for free.
func (s *dgramSender) limit() int64 {
	now := time.Now().UnixNano()
	if m := s.max.Load(); m > 0 && now-s.probeAt.Load() < int64(udpProbeEvery) {
		return m
	}
	s.probeAt.Store(now)
	probe := make([]byte, udpProbeSize) // fragment count 0: a peer that got it would drop it
	binary.BigEndian.PutUint32(probe, s.id)
	var tooLarge *DatagramTooLargeError
	if err := s.link.Send(probe); errors.As(err, &tooLarge) {
		s.max.Store(max(int64(tooLarge.Max)-udpDgramSlack, 0))
	} else if s.max.Load() == 0 {
		s.max.Store(1200 - udpDgramSlack) // what every QUIC path carries
	}
	return s.max.Load()
}

// Send reports false when data must go on the stream instead: too big to fragment sensibly, or
// the link refused it outright.
func (s *dgramSender) Send(addr string, data []byte) bool {
	pkt := uint16(s.pkt.Add(1))
	limit := int(s.limit())
	if UDPDgramHeader+len(addr)+len(data) <= limit {
		return s.link.Send(encodeUDPFragment(s.id, pkt, 0, 1, addr, data)) == nil
	}
	per := limit - UDPDgramHeader - len(addr)
	if per < 64 {
		return false
	}
	count := (len(data) + per - 1) / per
	if count > udpMaxFragments {
		return false
	}
	for i := 0; i < count; i++ {
		chunk := data[i*per : min(len(data), (i+1)*per)]
		if err := s.link.Send(encodeUDPFragment(s.id, pkt, uint8(i), uint8(count), addr, chunk)); err != nil {
			return false
		}
	}
	return true
}
