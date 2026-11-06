package files

import (
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// Range is a run of chunks: Start, Start+1, … Start+Count-1.
type Range struct {
	Start uint64
	Count uint32
}

// Want is the body of WANT: { bytes fid = 1; repeated Range ranges = 2; }, at most 256 chunks in all.
type Want struct {
	FID    [16]byte
	Ranges []Range
}

// Chunk is the body of CHUNK: { bytes fid = 1; uint64 index = 2; repeated bytes proof = 3; bytes data = 4; }.
type Chunk struct {
	FID   [16]byte
	Index uint64
	Proof [][32]byte
	Data  []byte
}

// Cancel is the body of CANCEL: { bytes fid = 1; uint32 reason = 2; }.
type Cancel struct {
	FID    [16]byte
	Reason uint32
}

// Done is the body of DONE: { bytes fid = 1; }.
type Done struct{ FID [16]byte }

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }

func appendBytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func appendVarintField(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

// walk calls fn for every field of a protobuf message. Unknown fields are the caller's to skip;
// groups and truncated values are malformed.
func walk(p []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error) error {
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 || num < 1 {
			return malformed("tag")
		}
		p = p[n:]
		var v uint64
		var s []byte
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(p)
		case protowire.BytesType:
			s, n = protowire.ConsumeBytes(p)
		case protowire.Fixed32Type:
			var x uint32
			x, n = protowire.ConsumeFixed32(p)
			v = uint64(x)
		case protowire.Fixed64Type:
			v, n = protowire.ConsumeFixed64(p)
		default:
			return malformed("wire type")
		}
		if n < 0 {
			return malformed("value")
		}
		p = p[n:]
		if err := fn(num, typ, v, s); err != nil {
			return err
		}
	}
	return nil
}

// fidField reads a 16-byte file ID; a second occurrence or another length is malformed.
func fidField(dst *[16]byte, seen *bool, typ protowire.Type, s []byte) error {
	if typ != protowire.BytesType || len(s) != 16 || *seen {
		return malformed("fid")
	}
	copy(dst[:], s)
	*seen = true
	return nil
}

// Append encodes the body canonically: ascending field numbers, default values omitted.
func (m Want) Append(b []byte) []byte {
	b = appendBytesField(b, 1, m.FID[:])
	for _, r := range m.Ranges {
		var rb []byte
		rb = appendVarintField(rb, 1, r.Start)
		rb = appendVarintField(rb, 2, uint64(r.Count))
		b = appendBytesField(b, 2, rb)
	}
	return b
}

// Append encodes the body canonically.
func (m Chunk) Append(b []byte) []byte {
	b = appendBytesField(b, 1, m.FID[:])
	b = appendVarintField(b, 2, m.Index)
	for i := range m.Proof {
		b = appendBytesField(b, 3, m.Proof[i][:])
	}
	if len(m.Data) > 0 {
		b = appendBytesField(b, 4, m.Data)
	}
	return b
}

// Append encodes the body canonically.
func (m Cancel) Append(b []byte) []byte {
	b = appendBytesField(b, 1, m.FID[:])
	return appendVarintField(b, 2, uint64(m.Reason))
}

// Append encodes the body canonically.
func (m Done) Append(b []byte) []byte { return appendBytesField(b, 1, m.FID[:]) }

// ParseWant decodes WANT; it checks the bounds (1…256 chunks in all, no empty range).
func ParseWant(p []byte) (Want, error) {
	var m Want
	var haveFID bool
	var total uint64
	err := walk(p, func(num protowire.Number, typ protowire.Type, _ uint64, s []byte) error {
		switch num {
		case 1:
			return fidField(&m.FID, &haveFID, typ, s)
		case 2:
			if typ != protowire.BytesType {
				return malformed("range")
			}
			r, err := parseRange(s)
			if err != nil {
				return err
			}
			total += uint64(r.Count)
			if total > MaxWantChunks {
				return malformed("more than 256 chunks")
			}
			m.Ranges = append(m.Ranges, r)
		}
		return nil
	})
	if err != nil {
		return Want{}, err
	}
	if !haveFID || total == 0 {
		return Want{}, malformed("want")
	}
	return m, nil
}

func parseRange(p []byte) (Range, error) {
	var r Range
	err := walk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("range start")
			}
			r.Start = v
		case 2:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("range count")
			}
			r.Count = uint32(v)
		}
		return nil
	})
	if err != nil {
		return Range{}, err
	}
	if r.Count == 0 || r.Start+uint64(r.Count) < r.Start {
		return Range{}, malformed("range")
	}
	return r, nil
}

// ParseChunk decodes CHUNK.
func ParseChunk(p []byte) (Chunk, error) {
	var m Chunk
	var haveFID bool
	err := walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			return fidField(&m.FID, &haveFID, typ, s)
		case 2:
			if typ != protowire.VarintType {
				return malformed("index")
			}
			m.Index = v
		case 3:
			if typ != protowire.BytesType || len(s) != 32 || len(m.Proof) >= MaxProof {
				return malformed("proof")
			}
			var h [32]byte
			copy(h[:], s)
			m.Proof = append(m.Proof, h)
		case 4:
			if typ != protowire.BytesType || len(s) > ChunkSize {
				return malformed("data")
			}
			m.Data = s
		}
		return nil
	})
	if err != nil {
		return Chunk{}, err
	}
	if !haveFID {
		return Chunk{}, malformed("fid")
	}
	return m, nil
}

// ParseCancel decodes CANCEL. An unknown reason parses; the caller treats it as a cancellation.
func ParseCancel(p []byte) (Cancel, error) {
	var m Cancel
	var haveFID bool
	err := walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			return fidField(&m.FID, &haveFID, typ, s)
		case 2:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("reason")
			}
			m.Reason = uint32(v)
		}
		return nil
	})
	if err != nil {
		return Cancel{}, err
	}
	if !haveFID {
		return Cancel{}, malformed("fid")
	}
	return m, nil
}

// ParseDone decodes DONE.
func ParseDone(p []byte) (Done, error) {
	var m Done
	var haveFID bool
	err := walk(p, func(num protowire.Number, typ protowire.Type, _ uint64, s []byte) error {
		if num == 1 {
			return fidField(&m.FID, &haveFID, typ, s)
		}
		return nil
	})
	if err != nil {
		return Done{}, err
	}
	if !haveFID {
		return Done{}, malformed("fid")
	}
	return m, nil
}
