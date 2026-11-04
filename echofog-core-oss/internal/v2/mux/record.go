package mux

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// Records (wire.md §8.4):
//
//	record = u16(1 + len(body)) ‖ kind(u8) ‖ body
//	kind 0: plaintext frames (only before the handshake completes, only HS_* frames)
//	kind 1: body = sealed
const (
	RecordPlain  byte = 0
	RecordSealed byte = 1
	// MaxRecord bounds the u16 length value (kind byte and body, without the 2-byte prefix):
	// 16384 + 64 (wire.md §8.4).
	MaxRecord = 16384 + 64
	// maxRecordPlaintext is the plaintext a sender puts into one sealed record.
	maxRecordPlaintext = 16384
)

var (
	ErrRecordTooLarge = errors.New("mux: record too large")
	ErrRecordKind     = errors.New("mux: unexpected record kind")
	errSealingOff     = errors.New("mux: sealed record before sealing started")
	errPlainAfter     = errors.New("mux: plaintext record after sealing started")
)

func writeRecord(w io.Writer, kind byte, body []byte) error {
	if 1+len(body) > MaxRecord {
		return ErrRecordTooLarge
	}
	b := make([]byte, 0, 3+len(body))
	b = binary.BigEndian.AppendUint16(b, uint16(1+len(body)))
	b = append(b, kind)
	b = append(b, body...)
	_, err := w.Write(b)
	return err
}

// readRecord reads one record; buf is reused for the body if large enough. Unknown kinds,
// empty records and lengths over MaxRecord are errors: they end the path.
func readRecord(r io.Reader, buf []byte) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:2]); err != nil {
		return 0, nil, err
	}
	l := int(binary.BigEndian.Uint16(hdr[:2]))
	if l == 0 {
		return 0, nil, ErrRecordKind
	}
	if l > MaxRecord {
		return 0, nil, ErrRecordTooLarge
	}
	if _, err := io.ReadFull(r, hdr[2:3]); err != nil {
		return 0, nil, eofInside(err)
	}
	kind := hdr[2]
	if kind != RecordPlain && kind != RecordSealed {
		return 0, nil, ErrRecordKind
	}
	if cap(buf) < l-1 {
		buf = make([]byte, l-1)
	}
	buf = buf[:l-1]
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, eofInside(err)
	}
	return kind, buf, nil
}

func eofInside(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// RecordConn presents a record stream (one pipe, wire.md §8.4) as a byte stream: a relay stream
// circuit or a byte-stream carrier in sealed mode (whose sealed plaintext is connection-level mux
// frames, so a Conn runs on top of it), or one QUIC stream of a path in sealed mode.
//
// Before StartSealing, WritePlain and ReadPlain carry kind-0 records for a relayed handshake.
// After it, Write and Read carry kind-1 records; a kind-0 record then ends the stream (the
// switch is explicit). Records of the pipe must carry strictly growing counters.
type RecordConn struct {
	rw io.ReadWriteCloser

	wmu     sync.Mutex
	sealOut *Sealer
	pathID  uint64
	pipe    uint64
	wbuf    []byte

	rmu     sync.Mutex
	sealIn  *Sealer
	pst     PipeState
	pending []byte
	rbuf    []byte
	rerr    error
}

// NewRecordConn wraps a record stream.
func NewRecordConn(rw io.ReadWriteCloser) *RecordConn { return &RecordConn{rw: rw} }

// newPipeRecordConn starts sealed at once (a QUIC stream of a path in sealed mode: the handshake
// already ran on stream 0 in plaintext frames).
func newPipeRecordConn(rw io.ReadWriteCloser, s *Sealer, pathID, pipe uint64) *RecordConn {
	return &RecordConn{rw: rw, sealOut: s, sealIn: s, pathID: pathID, pipe: pipe}
}

// StartSealing switches both directions to kind-1 records of pipe (pathID, pipe).
func (c *RecordConn) StartSealing(s *Sealer, pathID, pipe uint64) {
	c.wmu.Lock()
	c.sealOut, c.pathID, c.pipe = s, pathID, pipe
	c.wmu.Unlock()
	c.rmu.Lock()
	c.sealIn = s
	c.rmu.Unlock()
}

// WritePlain writes one kind-0 record (handshake frames).
func (c *RecordConn) WritePlain(body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sealOut != nil {
		return errPlainAfter
	}
	return writeRecord(c.rw, RecordPlain, body)
}

// ReadPlain reads one kind-0 record.
func (c *RecordConn) ReadPlain() ([]byte, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.sealIn != nil {
		return nil, errPlainAfter
	}
	kind, body, err := readRecord(c.rw, nil)
	if err != nil {
		return nil, err
	}
	if kind != RecordPlain {
		return nil, errSealingOff
	}
	return body, nil
}

// Write seals p into records of at most 16384 plaintext bytes. Sealing and writing a record
// happen under one lock, so counters grow along the pipe.
func (c *RecordConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sealOut == nil {
		return 0, errSealingOff
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxRecordPlaintext)
		var err error
		c.wbuf, err = c.sealOut.Seal(c.wbuf[:0], p[:n], ClassRecord, c.pathID, c.pipe)
		if err != nil {
			return written, err
		}
		if err := writeRecord(c.rw, RecordSealed, c.wbuf); err != nil {
			return written, err
		}
		p = p[n:]
		written += n
	}
	return written, nil
}

// Read returns plaintext of kind-1 records.
func (c *RecordConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.pending) == 0 {
		if c.rerr != nil {
			return 0, c.rerr
		}
		kind, body, err := readRecord(c.rw, c.rbuf)
		if err != nil {
			c.rerr = err
			continue
		}
		c.rbuf = body[:0]
		switch {
		case kind == RecordPlain:
			c.rerr = ErrRecordKind // kind 0 after the switch ends the path
			continue
		case c.sealIn == nil:
			c.rerr = errSealingOff
			continue
		}
		pt, err := c.sealIn.OpenRecord(nil, body, c.pathID, c.pipe, &c.pst)
		if err != nil {
			c.rerr = err
			continue
		}
		c.pending = pt
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// Close closes the underlying record stream.
func (c *RecordConn) Close() error { return c.rw.Close() }
