package objects

import (
	"fmt"
	"math"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// maxNesting bounds the recursion of the canonical check.
const maxNesting = 32

// CheckCanonical checks that b is the canonical encoding (objects.md §2.3) of a message described by
// md. Fields md does not know are checked only for order (rule 1), shortest varints (rule 5) and the
// absence of groups (rule 6); they are allowed and stay in the bytes.
func CheckCanonical(b []byte, md protoreflect.MessageDescriptor) error {
	return checkMessage(b, md, 0)
}

// checkUnknownBody checks the rules that apply to a body of an unknown label: every field is unknown.
func checkUnknownBody(b []byte) error {
	return checkMessage(b, (&emptypb.Empty{}).ProtoReflect().Descriptor(), 0)
}

func nonCanonical(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNonCanonical, fmt.Sprintf(format, args...))
}

// consumeVarint is a protobuf varint (LEB128) with the shortest-encoding check.
func consumeVarint(b []byte) (uint64, int, error) {
	v, n := protowire.ConsumeVarint(b)
	if n < 0 {
		return 0, 0, nonCanonical("truncated varint")
	}
	if n != protowire.SizeVarint(v) {
		return 0, 0, nonCanonical("non-minimal varint")
	}
	return v, n, nil
}

func isPackable(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind, protoreflect.GroupKind:
		return false
	}
	return true
}

func wireTypeOf(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind:
		return protowire.BytesType
	case protoreflect.GroupKind:
		return protowire.StartGroupType
	}
	return protowire.VarintType
}

// checkVarintRange rejects varint values that a re-encoding of the field's type would change.
func checkVarintRange(k protoreflect.Kind, v uint64) error {
	switch k {
	case protoreflect.BoolKind:
		if v > 1 {
			return nonCanonical("bool value %d", v)
		}
	case protoreflect.Uint32Kind, protoreflect.Sint32Kind:
		if v > math.MaxUint32 {
			return nonCanonical("32-bit value out of range")
		}
	case protoreflect.Int32Kind, protoreflect.EnumKind:
		if v > math.MaxInt32 && v < uint64(1<<64-1<<31) {
			return nonCanonical("int32 value out of range")
		}
	}
	return nil
}

func checkMessage(b []byte, md protoreflect.MessageDescriptor, depth int) error {
	if depth > maxNesting {
		return nonCanonical("nesting too deep")
	}
	fields := md.Fields()
	var last protowire.Number
	for len(b) > 0 {
		tag, n, err := consumeVarint(b)
		if err != nil {
			return err
		}
		b = b[n:]
		if tag>>3 > uint64(protowire.MaxValidNumber) {
			return nonCanonical("field number out of range")
		}
		num, wt := protowire.Number(tag>>3), protowire.Type(tag&7)
		if num < protowire.MinValidNumber {
			return nonCanonical("field number 0")
		}
		fd := fields.ByNumber(num)
		if num < last {
			return nonCanonical("field %d after field %d", num, last)
		}
		if num == last {
			// Equal numbers only for consecutive elements of a repeated, non-packed field; unknown
			// fields may be repeated.
			if fd != nil && (!fd.IsList() || isPackable(fd.Kind())) {
				return nonCanonical("field %d appears twice", num)
			}
		}
		last = num

		switch wt {
		case protowire.VarintType:
			v, n, err := consumeVarint(b)
			if err != nil {
				return err
			}
			b = b[n:]
			if fd != nil {
				if fd.IsList() || wireTypeOf(fd.Kind()) != protowire.VarintType {
					return nonCanonical("field %d: wrong wire type (repeated scalars must be packed)", num)
				}
				if v == 0 {
					return nonCanonical("field %d: default value not omitted", num)
				}
				if err := checkVarintRange(fd.Kind(), v); err != nil {
					return err
				}
			}
		case protowire.Fixed32Type, protowire.Fixed64Type:
			size := 4
			if wt == protowire.Fixed64Type {
				size = 8
			}
			if len(b) < size {
				return nonCanonical("truncated fixed field")
			}
			v := b[:size]
			b = b[size:]
			if fd != nil {
				if fd.IsList() || wireTypeOf(fd.Kind()) != wt {
					return nonCanonical("field %d: wrong wire type", num)
				}
				if allZero(v) {
					return nonCanonical("field %d: default value not omitted", num)
				}
			}
		case protowire.BytesType:
			l, n, err := consumeVarint(b)
			if err != nil {
				return err
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nonCanonical("truncated length-delimited field")
			}
			v := b[:l]
			b = b[l:]
			if fd != nil {
				if err := checkBytesField(fd, v, depth); err != nil {
					return err
				}
			}
		case protowire.StartGroupType, protowire.EndGroupType:
			return nonCanonical("group (wire type %d)", wt)
		default:
			return nonCanonical("invalid wire type %d", wt)
		}
	}
	return nil
}

func checkBytesField(fd protoreflect.FieldDescriptor, v []byte, depth int) error {
	k := fd.Kind()
	switch {
	case fd.IsList() && isPackable(k):
		if len(v) == 0 {
			return nonCanonical("field %d: empty packed field", fd.Number())
		}
		return checkPacked(k, v)
	case k == protoreflect.MessageKind:
		return checkMessage(v, fd.Message(), depth+1)
	case k == protoreflect.StringKind || k == protoreflect.BytesKind:
		if !fd.IsList() && len(v) == 0 {
			return nonCanonical("field %d: empty value not omitted", fd.Number())
		}
		if k == protoreflect.StringKind && !utf8.Valid(v) {
			return nonCanonical("field %d: invalid UTF-8", fd.Number())
		}
		return nil
	}
	return nonCanonical("field %d: wrong wire type", fd.Number())
}

func checkPacked(k protoreflect.Kind, v []byte) error {
	switch wireTypeOf(k) {
	case protowire.Fixed32Type:
		if len(v)%4 != 0 {
			return nonCanonical("packed fixed32 length")
		}
	case protowire.Fixed64Type:
		if len(v)%8 != 0 {
			return nonCanonical("packed fixed64 length")
		}
	default:
		for len(v) > 0 {
			x, n, err := consumeVarint(v)
			if err != nil {
				return err
			}
			if err := checkVarintRange(k, x); err != nil {
				return err
			}
			v = v[n:]
		}
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// Marshal encodes m canonically. Fields are written in field-number order and unknown fields (kept
// from a parsed message) after them; the result is checked against the canonical rules.
func Marshal(m proto.Message) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	return b, nil
}

// unmarshalCanonical checks b against m's descriptor and parses it into m, keeping unknown fields.
func unmarshalCanonical(b []byte, m proto.Message) error {
	if err := CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return err
	}
	if err := (proto.UnmarshalOptions{}).Unmarshal(b, m); err != nil {
		return fmt.Errorf("%w: %v", ErrNonCanonical, err)
	}
	return nil
}
