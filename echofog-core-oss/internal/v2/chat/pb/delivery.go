package chatpb

import "errors"

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

var errNotImplemented = errors.New("chat/pb: not implemented")

// PadPersonal serialises m canonically with its pad field filled so that the result is exactly one
// bucket long. Any pad already in m is replaced.
func PadPersonal(m *PersonalMessageProto) ([]byte, error) { return nil, errNotImplemented }

// ParsePersonal parses a canonical PersonalMessageProto of at most the largest bucket. The control
// field, when set, names exactly one kind.
func ParsePersonal(b []byte) (*PersonalMessageProto, error) { return nil, errNotImplemented }

// ParseInner parses the content of a sealed box: a sender and a device akey (33 bytes each), a
// 16-byte message ID and exactly one body.
func ParseInner(b []byte) (*Inner, error) { return nil, errNotImplemented }

// ParseSealedEnvelope parses a DELIVER payload: to and eph of 32 bytes and a box of BoxTag bytes up
// to MaxEnvelope.
func ParseSealedEnvelope(b []byte) (*SealedEnvelope, error) { return nil, errNotImplemented }
