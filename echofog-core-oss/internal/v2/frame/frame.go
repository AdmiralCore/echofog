package frame

import (
	"io"
)

// FlagIgnorable: a receiver that does not know the type skips the frame (wire.md §6.1).
const FlagIgnorable byte = 0x01

// reservedFlags are bits 1–7: sent as 0; a frame with one set is treated as an unknown type.
const reservedFlags byte = 0xfe

// MaxPayload is the absolute cap on any frame payload (1 MiB, wire.md §6.1).
const MaxPayload = 1 << 20

// Frame is one stream frame: type(varint) ‖ flags(u8) ‖ length(varint) ‖ payload.
type Frame struct {
	Type    uint64
	Flags   byte
	Payload []byte
}

// Ignorable reports whether the IGNORABLE flag is set.
func (f Frame) Ignorable() bool { return f.Flags&FlagIgnorable != 0 }

// Limiter returns the maximum payload of a service-local type (0x40–0x3fff) and whether the type
// is known to the service of the stream. A nil Limiter knows no service-local types.
type Limiter func(typ uint64) (max int, known bool)

// AppendFrameHeader appends type ‖ flags ‖ length.
func AppendFrameHeader(b []byte, typ uint64, flags byte, length uint64) []byte {
	b = AppendVarint(b, typ)
	b = append(b, flags)
	return AppendVarint(b, length)
}

// AppendFrame appends the encoding of f.
func AppendFrame(b []byte, f Frame) []byte {
	b = AppendFrameHeader(b, f.Type, f.Flags, uint64(len(f.Payload)))
	return append(b, f.Payload...)
}

// FrameLen returns the encoded length of a frame with the given type and payload length.
func FrameLen(typ uint64, payloadLen int) int {
	return VarintLen(typ) + 1 + VarintLen(uint64(payloadLen)) + payloadLen
}

// classify returns the maximum payload of a frame and whether its type is known. A frame with a
// reserved flag bit is unknown whatever its type (wire.md §6.1).
func classify(typ uint64, flags byte, lim Limiter) (max int, known bool) {
	if flags&reservedFlags == 0 {
		if typ < ServiceLocalMin {
			if info, ok := sessionTypes[typ]; ok {
				return info.Max, true
			}
		} else if typ <= ServiceLocalMax && lim != nil {
			if m, ok := lim(typ); ok {
				if m > MaxPayload {
					m = MaxPayload
				}
				return m, true
			}
		}
	}
	return MaxPayload, false
}

// checkLength applies the length rules: a known type may not exceed its maximum, nothing may
// exceed 1 MiB.
func checkLength(typ uint64, length uint64, max int, known bool) error {
	if length > uint64(max) {
		return &TooLargeError{Type: typ, Length: length, Max: max, Known: known}
	}
	return nil
}

// ParseFrame decodes one frame from the start of b without copying the payload. It returns
// ErrShort if b holds less than a whole frame, a TooLargeError as soon as the header shows an
// oversized length, and an UnknownFrameError (with the consumed length) for an unknown type
// without IGNORABLE. An unknown IGNORABLE frame is returned as is: ParseFrame does not skip.
func ParseFrame(b []byte, lim Limiter) (Frame, int, error) {
	typ, n1, err := ParseVarint(b)
	if err != nil {
		return Frame{}, 0, err
	}
	if len(b) < n1+1 {
		return Frame{}, 0, ErrShort
	}
	flags := b[n1]
	length, n2, err := ParseVarint(b[n1+1:])
	if err != nil {
		return Frame{}, 0, err
	}
	max, known := classify(typ, flags, lim)
	if err := checkLength(typ, length, max, known); err != nil {
		return Frame{}, 0, err
	}
	hdr := n1 + 1 + n2
	if uint64(len(b)-hdr) < length {
		return Frame{}, 0, ErrShort
	}
	end := hdr + int(length)
	f := Frame{Type: typ, Flags: flags, Payload: b[hdr:end:end]}
	if !known && flags&FlagIgnorable == 0 {
		return f, end, &UnknownFrameError{Type: typ, Flags: flags}
	}
	return f, end, nil
}

// Known reports whether a frame's type is known under lim (taking reserved flags into account).
func Known(f Frame, lim Limiter) bool {
	_, k := classify(f.Type, f.Flags, lim)
	return k
}

// readChunk bounds how much the reader allocates ahead of payload bytes that have arrived.
const readChunk = 16 << 10

// Reader reads frames from a byte stream. It never reads past the end of the frame it returns
// (no read-ahead), so a handshake can hand the same stream to the session layer, and never
// allocates more than readChunk bytes ahead of payload bytes that have actually arrived.
//
// ReadFrame skips unknown IGNORABLE frames (counted by Skipped) without allocating, returns
// PADDING frames with a nil payload (their payload is discarded), returns UnknownFrameError for an
// unknown frame without IGNORABLE after consuming it, and TooLargeError without consuming the
// payload.
type Reader struct {
	r       io.Reader
	Lim     Limiter
	skipped uint64
	maxBuf  int
	scratch [512]byte
}

// NewReader returns a Reader on r with the service limiter lim (nil: no service-local types).
func NewReader(r io.Reader, lim Limiter) *Reader { return &Reader{r: r, Lim: lim} }

// Skipped returns the number of unknown ignorable frames skipped so far.
func (r *Reader) Skipped() uint64 { return r.skipped }

func eofInside(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadHeader reads one frame header and returns its fields and the classification under lim.
func (r *Reader) readHeader() (typ uint64, flags byte, length uint64, err error) {
	typ, err = ReadVarint(r.r)
	if err != nil {
		return
	}
	var fb [1]byte
	if _, err = io.ReadFull(r.r, fb[:]); err != nil {
		err = eofInside(err)
		return
	}
	flags = fb[0]
	length, err = ReadVarint(r.r)
	err = eofInside(err)
	return
}

func (r *Reader) discard(n uint64) error {
	for n > 0 {
		c := uint64(len(r.scratch))
		if n < c {
			c = n
		}
		if _, err := io.ReadFull(r.r, r.scratch[:c]); err != nil {
			return eofInside(err)
		}
		n -= c
	}
	return nil
}

func (r *Reader) readPayload(n int) ([]byte, error) {
	first := n
	if first > readChunk {
		first = readChunk
	}
	buf := make([]byte, first, first)
	if first > r.maxBuf {
		r.maxBuf = first
	}
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, eofInside(err)
	}
	for len(buf) < n {
		c := n - len(buf)
		if c > readChunk {
			c = readChunk
		}
		start := len(buf)
		buf = append(buf, make([]byte, c)...)
		if cap(buf) > r.maxBuf {
			r.maxBuf = cap(buf)
		}
		if _, err := io.ReadFull(r.r, buf[start:]); err != nil {
			return nil, eofInside(err)
		}
	}
	return buf, nil
}

// ReadFrame returns the next frame that is not an unknown IGNORABLE frame.
func (r *Reader) ReadFrame() (Frame, error) {
	for {
		typ, flags, length, err := r.readHeader()
		if err != nil {
			return Frame{}, err
		}
		max, known := classify(typ, flags, r.Lim)
		if err := checkLength(typ, length, max, known); err != nil {
			return Frame{}, err
		}
		if !known {
			if err := r.discard(length); err != nil {
				return Frame{}, err
			}
			if flags&FlagIgnorable != 0 {
				r.skipped++
				continue
			}
			return Frame{}, &UnknownFrameError{Type: typ, Flags: flags}
		}
		if typ == TypePadding {
			if err := r.discard(length); err != nil {
				return Frame{}, err
			}
			return Frame{Type: typ, Flags: flags}, nil
		}
		p, err := r.readPayload(int(length))
		if err != nil {
			return Frame{}, err
		}
		return Frame{Type: typ, Flags: flags, Payload: p}, nil
	}
}
