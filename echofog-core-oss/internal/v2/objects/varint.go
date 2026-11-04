package objects

import "errors"

// QUIC variable-length integers (RFC 9000 §16), used for the envelope's body_len. Encoders use the
// shortest form; decoders reject longer forms (vectors/varint.json).

var errVarint = errors.New("objects: bad varint")

const maxVarint = 1<<62 - 1

func varintLen(v uint64) int {
	switch {
	case v < 1<<6:
		return 1
	case v < 1<<14:
		return 2
	case v < 1<<30:
		return 4
	default:
		return 8
	}
}

func appendVarint(b []byte, v uint64) []byte {
	switch varintLen(v) {
	case 1:
		return append(b, byte(v))
	case 2:
		return append(b, 0x40|byte(v>>8), byte(v))
	case 4:
		return append(b, 0x80|byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, 0xc0|byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// readVarint decodes a varint and rejects truncated and non-minimal encodings.
func readVarint(b []byte) (uint64, int, error) {
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
	if varintLen(v) != n {
		return 0, 0, errVarint
	}
	return v, n, nil
}
