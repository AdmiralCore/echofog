package censor

import "time"

// recordWindow is how much of a stream, in each direction, is read for TLS records: the first
// 16 KB, which is where a handshake and the first exchange of a tunnel are.
const recordWindow = 16 << 10

// recordScanner reads TLS record headers out of one direction of a TCP stream. It holds at most
// one partial header, whatever the record sizes are.
type recordScanner struct {
	pos  int // bytes of the stream seen so far
	skip int // bytes of the current record's body still to come
	hdr  [5]byte
	nh   int
	bad  bool // not TLS: stop looking
}

func (s *recordScanner) feed(b []byte, at time.Duration, up bool, out *[]Record) {
	for len(b) > 0 && !s.bad && s.pos < recordWindow+5 {
		if s.skip > 0 {
			n := min(s.skip, len(b))
			s.skip -= n
			s.pos += n
			b = b[n:]
			continue
		}
		n := copy(s.hdr[s.nh:], b)
		s.nh += n
		b = b[n:]
		s.pos += n
		if s.nh < 5 {
			return
		}
		s.nh = 0
		typ, maj := s.hdr[0], s.hdr[1]
		if typ < 20 || typ > 23 || maj != 3 {
			s.bad = true
			return
		}
		body := int(s.hdr[3])<<8 | int(s.hdr[4])
		start := s.pos - 5
		if start >= recordWindow {
			s.bad = true // the record starts after the window
			return
		}
		*out = append(*out, Record{At: at, Up: up, Type: typ, Len: 5 + body})
		s.skip = body
	}
}
