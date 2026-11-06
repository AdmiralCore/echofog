package chatpb

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Plaintext buckets (services.md 4.3): the serialised PersonalMessageProto is padded to the next of
// these sizes. A message that does not fit the last one is sent as a file (files/2).
var Buckets = []int{256, 1024, 4096, 16384, 57344}

// MaxEnvelope is the largest SealedEnvelope (services.md 4.1: DELIVER carries at most 61440 bytes).
const MaxEnvelope = 61440

// Lengths fixed by the constructions.
const (
	AKeyLen   = 33 // tag byte and a 32-byte key
	MessageID = 16
	BoxTag    = 16 // ChaCha20-Poly1305
)

// ErrTooLarge is a plaintext above the last bucket.
var ErrTooLarge = errors.New("chat/pb: message does not fit the largest bucket, send it as a file")

// MaxReceiptIDs bounds one receipt.
const MaxReceiptIDs = 256

// PadPersonal serialises m canonically with its pad field filled so that the result is exactly one
// bucket long. Any pad already in m is replaced.
func PadPersonal(m *PersonalMessageProto) ([]byte, error) {
	base := proto.Clone(m).(*PersonalMessageProto)
	base.Pad = nil
	plain, err := objects.Marshal(base)
	if err != nil {
		return nil, err
	}
	for _, size := range Buckets {
		n := size - len(plain)
		switch {
		case n < 0:
			continue
		case n == 0:
			return plain, nil
		}
		// the pad field costs a tag byte, its length varint and the pad itself; an empty pad is
		// omitted by the canonical encoding, so it is at least one byte
		for k := 1; k <= 3; k++ {
			p := n - 1 - k
			if p >= 1 && protowire.SizeVarint(uint64(p)) == k {
				base.Pad = make([]byte, p)
				out, err := objects.Marshal(base)
				if err != nil {
					return nil, err
				}
				if len(out) != size {
					return nil, errors.New("chat/pb: padding miscounted")
				}
				return out, nil
			}
		}
		// no pad length gives this size exactly (one byte short of a varint step): next bucket
	}
	return nil, ErrTooLarge
}

// ParsePersonal parses a canonical PersonalMessageProto of at most the largest bucket. The control
// field, when set, names exactly one kind.
func ParsePersonal(b []byte) (*PersonalMessageProto, error) {
	if len(b) > Buckets[len(Buckets)-1] {
		return nil, invalid("plaintext of %d bytes", len(b))
	}
	m := &PersonalMessageProto{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if c := m.GetControl(); c != nil {
		switch k := c.GetKind().(type) {
		case *Control_Knock:
			if len(k.Knock.GetCard()) > 4096 {
				return nil, invalid("knock card of %d bytes", len(k.Knock.GetCard()))
			}
		case *Control_Reset_:
		case *Control_Read:
			if err := checkReceipt(k.Read); err != nil {
				return nil, err
			}
		case *Control_Delivered:
			if err := checkReceipt(k.Delivered); err != nil {
				return nil, err
			}
		default:
			return nil, invalid("control without a kind")
		}
	}
	return m, nil
}

func checkReceipt(r *Receipt) error {
	if len(r.GetIds()) > MaxReceiptIDs {
		return invalid("receipt of %d ids", len(r.GetIds()))
	}
	for _, id := range r.GetIds() {
		if len(id) != MessageID {
			return invalid("receipt id of %d bytes", len(id))
		}
	}
	return nil
}

// ParseInner parses the content of a sealed box: a sender and a device akey (33 bytes each), a
// 16-byte message ID and exactly one body.
func ParseInner(b []byte) (*Inner, error) {
	m := &Inner{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if len(m.GetSender()) != AKeyLen || len(m.GetDevice()) != AKeyLen || len(m.GetId()) != MessageID {
		return nil, invalid("sender, device or id of a wrong length")
	}
	if (len(m.GetPqxdhInit()) == 0) == (len(m.GetRatchet()) == 0) {
		return nil, invalid("not exactly one body")
	}
	return m, nil
}

// ParseSealedEnvelope parses a DELIVER payload: to and eph of 32 bytes and a box of BoxTag bytes up
// to MaxEnvelope.
func ParseSealedEnvelope(b []byte) (*SealedEnvelope, error) {
	if len(b) > MaxEnvelope {
		return nil, invalid("envelope of %d bytes", len(b))
	}
	m := &SealedEnvelope{}
	if err := parse(b, m); err != nil {
		return nil, err
	}
	if len(m.GetTo()) != KeyLen || len(m.GetEph()) != KeyLen || len(m.GetBox()) < BoxTag {
		return nil, invalid("to, eph or box of a wrong length")
	}
	return m, nil
}
