package objects

import (
	"crypto/sha256"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// ---------------------------------------------------------------------------------------------
// echofog/prekeys/2 (objects.md §5)

const (
	mlkem768KeyLen = 1184
	x25519Len      = 32
	maxOPKs        = 32
)

// Prekeys is a verified chat/2 prekey bundle, signed by a device key.
type Prekeys struct {
	Env       *Envelope
	Body      *pb.Prekeys
	DeviceKey keys.AKey
	DeviceID  [32]byte
}

// VerifyPrekeys verifies a prekey bundle. Every v2 bundle carries the ML-KEM key; one without it is
// invalid (audit item P1). Whether the signing device belongs to an account is checked against the
// account's device list with VouchedBy.
func VerifyPrekeys(b []byte, now time.Time) (*Prekeys, error) {
	e, err := Verify(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelPrekeys); err != nil {
		return nil, err
	}
	body := &pb.Prekeys{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	if _, err := keys.ParseKind(body.GetIk(), keys.X25519); err != nil {
		return nil, invalid("ik: %v", err)
	}
	if len(body.GetSpk()) != x25519Len {
		return nil, invalid("spk of %d bytes", len(body.GetSpk()))
	}
	if len(body.GetKem()) != mlkem768KeyLen {
		return nil, invalid("kem of %d bytes (required, %d)", len(body.GetKem()), mlkem768KeyLen)
	}
	for i, k := range body.GetOpks() {
		if len(k) != x25519Len {
			return nil, invalid("opk %d of %d bytes", i, len(k))
		}
	}
	return &Prekeys{Env: e, Body: body, DeviceKey: e.Signer, DeviceID: e.Signer.ID()}, nil
}

// KEMKeyID returns SHA-256(kem).
func (p *Prekeys) KEMKeyID() [32]byte { return sha256.Sum256(p.Body.GetKem()) }

// OPKs returns the processed one-time prekeys (at most 32).
func (p *Prekeys) OPKs() [][]byte {
	o := p.Body.GetOpks()
	if len(o) > maxOPKs {
		o = o[:maxOPKs]
	}
	return o
}

// VouchedBy reports whether the account's device list lists the bundle's signing device.
func (p *Prekeys) VouchedBy(dl *DeviceList) bool { return dl.Contains(p.DeviceKey) }

// ---------------------------------------------------------------------------------------------
// echofog/mailbox-hint/2 (objects.md §7)

const (
	maxHints      = 4
	queueIDLen    = 16
	maxRelayCards = 1 // per object (objects.md §7)
)

// MailboxHint is a verified mailbox hint.
type MailboxHint struct {
	Env  *Envelope
	Body *pb.MailboxHint
	// RelayCards holds the verified relay cards of the processed hints (nil where none was given).
	RelayCards []*Card
}

// VerifyMailboxHint verifies a mailbox hint. At most one hint carries a relay card, which must verify
// and belong to the hint's relay device.
func VerifyMailboxHint(b []byte, now time.Time) (*MailboxHint, error) {
	e, err := Verify(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelMailboxHint); err != nil {
		return nil, err
	}
	body := &pb.MailboxHint{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	h := &MailboxHint{Env: e, Body: body}
	cards := 0
	for i, x := range body.GetHints() {
		if len(x.GetRelay()) != 32 {
			return nil, invalid("hint %d: relay of %d bytes", i, len(x.GetRelay()))
		}
		if len(x.GetQueue()) != queueIDLen {
			return nil, invalid("hint %d: queue of %d bytes", i, len(x.GetQueue()))
		}
		var card *Card
		if rc := x.GetRelayCard(); len(rc) != 0 {
			if cards++; cards > maxRelayCards {
				return nil, invalid("more than one relay card")
			}
			card, err = VerifyCard(rc, now)
			if err != nil {
				return nil, invalid("hint %d: relay card: %v", i, err)
			}
			if string(card.DeviceID[:]) != string(x.GetRelay()) {
				return nil, invalid("hint %d: relay card of another device", i)
			}
		}
		if i < maxHints {
			h.RelayCards = append(h.RelayCards, card)
		}
	}
	return h, nil
}

// Hints returns the processed hints (at most 4).
func (h *MailboxHint) Hints() []*pb.Hint {
	x := h.Body.GetHints()
	if len(x) > maxHints {
		x = x[:maxHints]
	}
	return x
}

// ---------------------------------------------------------------------------------------------
// echofog/name/2 (objects.md §8). Format only: v2 nodes do not store or serve it in the DHT.

const (
	NameSystemTON      = 1
	NameSystemTelegram = 2
	maxNameOwner       = 64
	maxName            = 253
)

// NameBinding is a verified name binding. Resolution against the chain is not part of v2.
type NameBinding struct {
	Env  *Envelope
	Body *pb.NameBinding
}

// VerifyNameBinding verifies the format of a name binding.
func VerifyNameBinding(b []byte, now time.Time) (*NameBinding, error) {
	e, err := Verify(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelName); err != nil {
		return nil, err
	}
	body := &pb.NameBinding{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	if n := body.GetName(); n == "" || len(n) > maxName {
		return nil, invalid("name of %d bytes", len(n))
	}
	if len(body.GetOwner()) > maxNameOwner {
		return nil, invalid("owner of %d bytes", len(body.GetOwner()))
	}
	return &NameBinding{Env: e, Body: body}, nil
}
