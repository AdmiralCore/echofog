package frame

import (
	"math"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// GoAway is the payload of GOAWAY (wire.md §7.3):
// GoAway { uint32 code = 1; string reason = 2; uint32 retry_after = 3; }
type GoAway struct {
	Code       uint64
	Reason     string
	RetryAfter uint32
}

// Append encodes g canonically: fields in ascending order, default values omitted.
func (g GoAway) Append(b []byte) []byte {
	b = pbUint(b, 1, g.Code)
	b = pbString(b, 2, g.Reason)
	return pbUint(b, 3, uint64(g.RetryAfter))
}

// ParseGoAway decodes GOAWAY, skipping unknown fields (versioning.md §3.5).
func ParseGoAway(p []byte) (GoAway, error) {
	var g GoAway
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("goaway.code")
			}
			g.Code = v
		case 2:
			if typ != protowire.BytesType || !utf8.Valid(s) {
				return malformed("goaway.reason")
			}
			g.Reason = string(s)
		case 3:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("goaway.retry_after")
			}
			g.RetryAfter = uint32(v)
		}
		return nil
	})
	return g, err
}

// Reject is the payload of REJECT (handshake.md §7):
// Reject { uint32 reason = 1; uint32 min = 2; uint32 max = 3; string detail = 4; uint32 retry_after = 5; }
type Reject struct {
	Reason     uint32
	Min        uint32
	Max        uint32
	Detail     string
	RetryAfter uint32
}

// MaxRejectDetail is the longest Reject.detail (handshake.md §7).
const MaxRejectDetail = 200

func (r Reject) Append(b []byte) []byte {
	b = pbUint(b, 1, uint64(r.Reason))
	b = pbUint(b, 2, uint64(r.Min))
	b = pbUint(b, 3, uint64(r.Max))
	b = pbString(b, 4, r.Detail)
	return pbUint(b, 5, uint64(r.RetryAfter))
}

func ParseReject(p []byte) (Reject, error) {
	var r Reject
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		u32 := func(dst *uint32, name string) error {
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("reject." + name)
			}
			*dst = uint32(v)
			return nil
		}
		switch num {
		case 1:
			return u32(&r.Reason, "reason")
		case 2:
			return u32(&r.Min, "min")
		case 3:
			return u32(&r.Max, "max")
		case 4:
			if typ != protowire.BytesType || len(s) > MaxRejectDetail || !utf8.Valid(s) {
				return malformed("reject.detail")
			}
			r.Detail = string(s)
		case 5:
			return u32(&r.RetryAfter, "retry_after")
		}
		return nil
	})
	return r, err
}

func pbUint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func pbString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

// pbWalk calls fn for every field of a protobuf message: v for varint fields, s for
// length-delimited ones. Unknown fields reach fn too, which ignores them by number. Malformed
// wire data (bad tag, truncated value, group types) is ErrMalformed.
func pbWalk(p []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error) error {
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 {
			return malformed("protobuf tag")
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
			return malformed("protobuf wire type")
		}
		if n < 0 {
			return malformed("protobuf value")
		}
		p = p[n:]
		if err := fn(num, typ, v, s); err != nil {
			return err
		}
	}
	return nil
}
