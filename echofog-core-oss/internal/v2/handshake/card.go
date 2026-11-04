package handshake

import (
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Card checks of handshake.md §7. Cards are verified by internal/v2/objects, the one card parser
// in the tree, with every rule of objects.md §3; this file maps the result to what the handshake
// uses and the objects errors to the handshake ones.

// Labels and algorithm tags (registries.md §7.1, §8).
const (
	LabelCard    = "echofog/card/2"
	LabelDevices = "echofog/devices/2"
	AlgEd25519   = 0x01
	AlgX25519    = 0x02
)

// Card verification errors.
var (
	ErrCardMalformed = errors.New("handshake: malformed card")
	ErrCardSignature = errors.New("handshake: card signature invalid")
	ErrCardExpired   = errors.New("handshake: card expired")
	ErrCardLifetime  = errors.New("handshake: card lifetime too long")
	ErrCardDevices   = errors.New("handshake: card device list invalid")
)

// CardInfo is what the handshake uses from a verified card.
type CardInfo struct {
	DeviceID       [32]byte // SHA-256 of the signer akey
	NodeID         [32]byte // SHA-256 of the account akey
	HsKey          []byte   // X25519 public key (32 bytes, without the tag)
	PrevHsKey      []byte   // nil if none
	PrevHsNotAfter uint64
	L1Suites       []uint16
	Scope          uint32
	Flags          uint32
	InviteID       []byte
	Seq            uint64
	NotAfter       uint64
	Raw            []byte // the exact envelope bytes
}

// CardVerifier verifies a signed node card.
type CardVerifier interface {
	VerifyCard(envelope []byte, now time.Time) (*CardInfo, error)
}

// DefaultCardVerifier verifies a card with objects.VerifyCard: the envelope (RFC 8032 with
// canonical S and no small-order signer), lifetimes, the canonical body, every card rule, and the
// embedded device list (signed by the account, listing the signer).
type DefaultCardVerifier struct{}

// VerifyCard implements CardVerifier.
func (DefaultCardVerifier) VerifyCard(env []byte, now time.Time) (*CardInfo, error) {
	if len(env) > MaxCardLen {
		return nil, ErrCardMalformed
	}
	c, err := objects.VerifyCard(env, now)
	if err != nil {
		return nil, cardError(err)
	}
	b := c.Body
	info := &CardInfo{
		DeviceID: c.DeviceID,
		NodeID:   c.NodeID,
		HsKey:    clone(c.HsKey.Key()),
		Scope:    b.GetScope(),
		Flags:    b.GetFlags(),
		InviteID: clone(b.GetInviteId()),
		Seq:      c.Env.Seq,
		NotAfter: c.Env.NotAfter,
		Raw:      clone(env),
	}
	for _, v := range b.GetL1Suites() {
		if v <= 0xffff {
			info.L1Suites = append(info.L1Suites, uint16(v))
		}
	}
	if ph := b.GetPrevHs(); ph != nil {
		info.PrevHsKey = clone(ph.GetKey()[1:]) // objects checked the x25519 akey
		info.PrevHsNotAfter = ph.GetNotAfter()
	}
	return info, nil
}

// cardError maps an objects error to the handshake errors, keeping the cause.
func cardError(err error) error {
	var e error
	switch {
	case errors.Is(err, objects.ErrExpired):
		e = ErrCardExpired
	case errors.Is(err, objects.ErrLifetime):
		e = ErrCardLifetime
	case errors.Is(err, objects.ErrDevices):
		e = ErrCardDevices
	case errors.Is(err, keys.ErrBadSignature), errors.Is(err, keys.ErrSmallOrder):
		e = ErrCardSignature
	default:
		e = ErrCardMalformed
	}
	return fmt.Errorf("%w: %w", e, err)
}
