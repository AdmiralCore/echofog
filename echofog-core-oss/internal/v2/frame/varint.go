// Package frame implements the EchoFog protocol v2 frame format (docs/protocol/v2/wire.md §6):
// QUIC variable-length integers, stream frames, datagrams, the session-level type registry
// (registries.md §1, §2) and the typed payloads of the session-level frames.
//
// The package is pure Go and has no state. It knows nothing about services; service-local frame
// types (0x40–0x3fff) are sized through a Limiter supplied by the caller.
package frame

import (
	"encoding/binary"
	"io"
)

// MaxVarint is the largest value a QUIC varint can carry (2^62 − 1).
const MaxVarint = 1<<62 - 1

// VarintLen returns the length of the shortest encoding of v.
func VarintLen(v uint64) int {
	switch {
	case v < 1<<6:
		return 1
	case v < 1<<14:
		return 2
	case v < 1<<30:
		return 4
	case v <= MaxVarint:
		return 8
	}
	panic("frame: varint out of range")
}

// AppendVarint appends the shortest encoding of v (RFC 9000 §16). It panics if v > MaxVarint;
// callers check values that come from outside.
func AppendVarint(b []byte, v uint64) []byte {
	switch VarintLen(v) {
	case 1:
		return append(b, byte(v))
	case 2:
		return append(b, 0x40|byte(v>>8), byte(v))
	case 4:
		return binary.BigEndian.AppendUint32(b, uint32(v)|0x80000000)
	default:
		return binary.BigEndian.AppendUint64(b, v|0xc000000000000000)
	}
}

// ParseVarint decodes one varint from the start of b. It returns ErrShort when b ends inside
// the varint and ErrNonMinimal for an encoding longer than necessary (wire.md §6.6: decoders
// MUST reject longer forms).
func ParseVarint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrShort
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, ErrShort
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	if n > 1 && VarintLen(v) != n {
		return 0, 0, ErrNonMinimal
	}
	return v, n, nil
}

// ReadVarint reads exactly one varint from r, never more bytes than the varint occupies. An
// end of input before the first byte is io.EOF; inside the varint it is io.ErrUnexpectedEOF.
func ReadVarint(r io.Reader) (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:1]); err != nil {
		return 0, err
	}
	n := 1 << (buf[0] >> 6)
	if n > 1 {
		if _, err := io.ReadFull(r, buf[1:n]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
	}
	v, _, err := ParseVarint(buf[:n])
	return v, err
}
