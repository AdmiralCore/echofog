package objects

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// FormatV2 is the envelope format byte of protocol v2.
const FormatV2 = 0x02

const maxLabelLen = 64

// Envelope is a parsed signed object (objects.md §2.1). All byte slices alias Raw.
type Envelope struct {
	Label    string
	Alg      keys.Alg
	Signer   keys.AKey // akey of the signer (tag ‖ key)
	Seq      uint64
	NotAfter uint64
	Body     []byte
	Sig      []byte
	// Raw is the exact received envelope. It is what is stored and forwarded.
	Raw []byte
}

// SignedBytes returns the prefix covered by the signature: every byte before sig.
func (e *Envelope) SignedBytes() []byte { return e.Raw[:len(e.Raw)-len(e.Sig)] }

// ID returns object_id = SHA-256(envelope).
func (e *Envelope) ID() [32]byte { return sha256.Sum256(e.Raw) }

// SignerID returns SHA-256 of the signer's akey: the device ID or the node ID of the signer.
func (e *Envelope) SignerID() [32]byte { return e.Signer.ID() }

// Known reports whether this build knows the envelope's label.
func (e *Envelope) Known() bool { _, ok := labels[e.Label]; return ok }

// StoreExpiry returns the unix time until which a store keeps the object: not_after, clipped to
// receipt + 24 h for labels this build does not know (objects.md §2.2 step 4).
func (e *Envelope) StoreExpiry(receipt time.Time) uint64 {
	if e.Known() {
		return e.NotAfter
	}
	clip := unix(receipt) + UnknownLabelStoreLifetime
	if e.NotAfter < clip {
		return e.NotAfter
	}
	return clip
}

func unix(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}

func validLabel(l []byte) bool {
	if len(l) < 1 || len(l) > maxLabelLen {
		return false
	}
	for _, c := range l {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// Parse parses an envelope and checks its structure and size limit (objects.md §2.2 steps 1–2). It
// does not verify the signature. An unknown signature algorithm gives ErrUnusable.
func Parse(b []byte) (*Envelope, error) {
	if len(b) < 2 {
		return nil, malformed("short")
	}
	if b[0] != FormatV2 {
		return nil, malformed("format 0x%02x", b[0])
	}
	ll := int(b[1])
	if len(b) < 2+ll+1 || !validLabel(b[2:2+ll]) {
		return nil, malformed("label")
	}
	e := &Envelope{Label: string(b[2 : 2+ll]), Raw: b}
	limit := UnknownLabelMaxEnvelope
	info, known := labels[e.Label]
	if known {
		limit = info.MaxEnvelope
	}
	if limit > 0 && len(b) > limit {
		return nil, fmt.Errorf("%w: %d-byte envelope, limit %d", ErrTooLarge, len(b), limit)
	}
	p := 2 + ll
	e.Alg = keys.Alg(b[p])
	if !e.Alg.IsSignature() {
		return nil, fmt.Errorf("%w: %s", ErrUnusable, e.Alg)
	}
	kl := e.Alg.KeyLen()
	if len(b) < p+1+kl+16 {
		return nil, malformed("short")
	}
	e.Signer = keys.AKey(b[p : p+1+kl])
	p += 1 + kl
	e.Seq = binary.BigEndian.Uint64(b[p:])
	e.NotAfter = binary.BigEndian.Uint64(b[p+8:])
	p += 16
	if e.Seq == 0 {
		return nil, malformed("seq 0")
	}
	bl, n, err := readVarint(b[p:])
	if err != nil {
		return nil, malformed("body_len")
	}
	p += n
	sl := e.Alg.SigLen()
	if bl > uint64(len(b)) || uint64(len(b)-p) != bl+uint64(sl) {
		return nil, malformed("lengths")
	}
	if known && int(bl) > info.MaxBody {
		return nil, fmt.Errorf("%w: %d-byte body, limit %d", ErrTooLarge, bl, info.MaxBody)
	}
	e.Body = b[p : p+int(bl)]
	e.Sig = b[p+int(bl):]
	return e, nil
}

// Verify runs the generic checks of objects.md §2.2 in order: structure and size, algorithm,
// signature over the exact prefix, expiry (60 s skew) and lifetime, and, for a known label, that the
// body is canonical and parses as the label's message. Label-specific rules are applied by the
// Verify* function of each label.
//
// For an unknown label the body is checked against the rules that need no schema, and the lifetime
// is not limited; stores clip it with StoreExpiry.
func Verify(b []byte, now time.Time) (*Envelope, error) {
	return verifyAt(b, &now)
}

// verifyAt is Verify; a nil now skips the time checks (used when sealing).
func verifyAt(b []byte, now *time.Time) (*Envelope, error) {
	e, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if err := keys.Verify(e.Signer, e.SignedBytes(), e.Sig); err != nil {
		return nil, err
	}
	info, known := labels[e.Label]
	if now != nil {
		t := unix(*now)
		if e.NotAfter <= t && t-e.NotAfter >= ClockSkew {
			return nil, ErrExpired
		}
		if known && e.NotAfter > t && e.NotAfter-t > info.MaxLifetime+ClockSkew {
			return nil, ErrLifetime
		}
	}
	if known {
		if err := unmarshalCanonical(e.Body, newBody(e.Label)); err != nil {
			return nil, err
		}
	} else if err := checkUnknownBody(e.Body); err != nil {
		return nil, err
	}
	return e, nil
}

func newBody(label string) proto.Message {
	switch label {
	case LabelCard:
		return &pb.NodeCard{}
	case LabelPrekeys:
		return &pb.Prekeys{}
	case LabelDevices:
		return &pb.DeviceList{}
	case LabelMailboxHint:
		return &pb.MailboxHint{}
	case LabelName:
		return &pb.NameBinding{}
	case LabelRelease:
		return &pb.ReleaseObject{}
	}
	return nil
}

// Seal builds and signs an envelope. The body must already be canonical (see Marshal); a known label's
// body is checked against its schema.
func Seal(label string, s *keys.Signer, seq, notAfter uint64, body []byte) ([]byte, error) {
	if !validLabel([]byte(label)) {
		return nil, malformed("label")
	}
	if seq == 0 {
		return nil, malformed("seq 0")
	}
	if m := newBody(label); m != nil {
		if err := CheckCanonical(body, m.ProtoReflect().Descriptor()); err != nil {
			return nil, err
		}
	} else if err := checkUnknownBody(body); err != nil {
		return nil, err
	}
	pub := s.Public()
	out := make([]byte, 0, 2+len(label)+len(pub)+16+8+len(body)+pub.Alg().SigLen())
	out = append(out, FormatV2, byte(len(label)))
	out = append(out, label...)
	out = append(out, pub...)
	out = binary.BigEndian.AppendUint64(out, seq)
	out = binary.BigEndian.AppendUint64(out, notAfter)
	out = appendVarint(out, uint64(len(body)))
	out = append(out, body...)
	out = append(out, s.Sign(out)...)
	if _, err := Parse(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Equivocation reports whether a and b are two different objects with the same (signer, label, seq).
func Equivocation(a, b *Envelope) bool {
	return a.Label == b.Label && a.Seq == b.Seq && a.Signer.Equal(b.Signer) && !bytes.Equal(a.Raw, b.Raw)
}

// Prefer returns the object every receiver keeps of two objects with the same (signer, label): the
// higher seq, and for equal seq (equivocation) the smaller object_id (objects.md §2.2).
func Prefer(a, b *Envelope) *Envelope {
	if a.Seq != b.Seq {
		if a.Seq > b.Seq {
			return a
		}
		return b
	}
	ia, ib := a.ID(), b.ID()
	if bytes.Compare(ib[:], ia[:]) < 0 {
		return b
	}
	return a
}
