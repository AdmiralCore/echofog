package handshake

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A minimal protobuf (proto3 wire format) writer and a canonical reader (objects.md §2.3).
//
// The handshake payloads are decoded with the same canonical rules as signed bodies: one
// encoding per message. The specification does not state this for HsInit/HsResp/HsFinish; it is
// recorded as a spec issue (see doc.go).

// ErrNotCanonical is returned for a body that breaks a checkable rule of objects.md §2.3.
var ErrNotCanonical = errors.New("handshake: protobuf body not canonical")

type pbWriter struct{ b []byte }

func (w *pbWriter) tag(n, wt int) { w.b = binary.AppendUvarint(w.b, uint64(n)<<3|uint64(wt)) }

// uint writes a varint field, omitted when zero.
func (w *pbWriter) uint(n int, v uint64) *pbWriter {
	if v != 0 {
		w.tag(n, 0)
		w.b = binary.AppendUvarint(w.b, v)
	}
	return w
}

// bytes writes a length-delimited field, omitted when empty.
func (w *pbWriter) bytes(n int, v []byte) *pbWriter {
	if len(v) != 0 {
		w.lenField(n, v)
	}
	return w
}

// lenField writes a length-delimited field even when empty (embedded messages, repeated elements).
func (w *pbWriter) lenField(n int, v []byte) *pbWriter {
	w.tag(n, 2)
	w.b = binary.AppendUvarint(w.b, uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}

// packed writes a packed repeated varint field, omitted when empty.
func (w *pbWriter) packed(n int, vs ...uint64) *pbWriter {
	if len(vs) == 0 {
		return w
	}
	var inner []byte
	for _, v := range vs {
		inner = binary.AppendUvarint(inner, v)
	}
	return w.lenField(n, inner)
}

func packed32(vs []uint32) []uint64 {
	out := make([]uint64, len(vs))
	for i, v := range vs {
		out[i] = uint64(v)
	}
	return out
}

// fieldKind is the schema of a known field.
type fieldKind uint8

const (
	kScalar   fieldKind = iota + 1 // non-repeated varint; a zero value is omitted
	kBytes                         // non-repeated bytes or string; empty is omitted
	kMsg                           // non-repeated embedded message; present when set, even empty
	kPacked                        // repeated varint, packed; omitted when empty
	kRepeated                      // repeated length-delimited (bytes, strings, messages)
)

type pbField struct {
	num  int
	wt   int
	v    uint64 // varint value (wire type 0), or each packed element via packedVals
	data []byte // length-delimited payload
}

func readUvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, ErrNotCanonical
	}
	if len(binary.AppendUvarint(nil, v)) != n {
		return 0, 0, fmt.Errorf("%w: non-minimal varint", ErrNotCanonical)
	}
	return v, n, nil
}

// parseCanonical splits a body into fields and checks the canonical rules that the schema allows
// it to check. Unknown fields are returned with their raw data and only checked for rules 1, 5, 6.
func parseCanonical(b []byte, schema map[int]fieldKind) ([]pbField, error) {
	var out []pbField
	last := 0
	for len(b) > 0 {
		t, n, err := readUvarint(b)
		if err != nil {
			return nil, err
		}
		b = b[n:]
		if t>>3 == 0 || t>>3 > 1<<29-1 {
			return nil, fmt.Errorf("%w: field number", ErrNotCanonical)
		}
		num, wt := int(t>>3), int(t&7)
		kind, known := schema[num]
		if num < last {
			return nil, fmt.Errorf("%w: field %d after %d", ErrNotCanonical, num, last)
		}
		if num == last && known && kind != kRepeated {
			return nil, fmt.Errorf("%w: field %d repeated", ErrNotCanonical, num)
		}
		last = num
		f := pbField{num: num, wt: wt}
		switch wt {
		case 0:
			if f.v, n, err = readUvarint(b); err != nil {
				return nil, err
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, ErrNotCanonical
			}
			f.data, b = b[:8], b[8:]
		case 5:
			if len(b) < 4 {
				return nil, ErrNotCanonical
			}
			f.data, b = b[:4], b[4:]
		case 2:
			l, n, err := readUvarint(b)
			if err != nil {
				return nil, err
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nil, fmt.Errorf("%w: length", ErrNotCanonical)
			}
			f.data, b = b[:l], b[l:]
		default: // groups (3, 4), 6, 7
			return nil, fmt.Errorf("%w: wire type %d", ErrNotCanonical, wt)
		}
		if known {
			if err := checkKnown(f, kind); err != nil {
				return nil, err
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func checkKnown(f pbField, kind fieldKind) error {
	switch kind {
	case kScalar:
		if f.wt != 0 {
			return fmt.Errorf("%w: field %d wire type", ErrNotCanonical, f.num)
		}
		if f.v == 0 {
			return fmt.Errorf("%w: field %d has the default value", ErrNotCanonical, f.num)
		}
	case kBytes:
		if f.wt != 2 {
			return fmt.Errorf("%w: field %d wire type", ErrNotCanonical, f.num)
		}
		if len(f.data) == 0 {
			return fmt.Errorf("%w: field %d empty", ErrNotCanonical, f.num)
		}
	case kMsg, kRepeated:
		if f.wt != 2 {
			return fmt.Errorf("%w: field %d wire type", ErrNotCanonical, f.num)
		}
	case kPacked:
		if f.wt != 2 {
			return fmt.Errorf("%w: field %d not packed", ErrNotCanonical, f.num)
		}
		if len(f.data) == 0 {
			return fmt.Errorf("%w: field %d empty", ErrNotCanonical, f.num)
		}
		if _, err := packedVals(f.data); err != nil {
			return err
		}
	}
	return nil
}

func packedVals(b []byte) ([]uint64, error) {
	var out []uint64
	for len(b) > 0 {
		v, n, err := readUvarint(b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}

func packedVals32(b []byte) ([]uint32, error) {
	vs, err := packedVals(b)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(vs))
	for i, v := range vs {
		if v > 0xffffffff {
			return nil, fmt.Errorf("handshake: value %d exceeds uint32", v)
		}
		out[i] = uint32(v)
	}
	return out, nil
}

func u32(v uint64) (uint32, error) {
	if v > 0xffffffff {
		return 0, fmt.Errorf("handshake: value %d exceeds uint32", v)
	}
	return uint32(v), nil
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// QUIC variable-length integers (RFC 9000 §16), as in the signed-object envelope.

var errVarint = errors.New("handshake: invalid QUIC varint")

func readQUICVarint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errVarint
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, errVarint
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	if len(appendQUICVarint(nil, v)) != n {
		return 0, 0, fmt.Errorf("%w: non-minimal", errVarint)
	}
	return v, n, nil
}

func appendQUICVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, 0x40|byte(v>>8), byte(v))
	case v < 1<<30:
		return append(b, 0x80|byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<62:
		return append(b, 0xc0|byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	panic("handshake: varint too large")
}
