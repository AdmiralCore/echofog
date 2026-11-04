package handshake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"filippo.io/edwards25519"
)

// Card checks of handshake.md §7. The full object rules live in internal/v2/objects; this file
// implements the subset the handshake needs, behind the CardVerifier interface, so the node can
// plug in the objects package later.

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

// DefaultCardVerifier checks the envelope, the signature (RFC 8032 with canonical S and no
// small-order signer), lifetimes, the canonical top-level body, hs_key, and the embedded device
// list (signed by the account, listing the signer).
type DefaultCardVerifier struct{}

type envelope struct {
	label    string
	signer   []byte
	seq      uint64
	notAfter uint64
	body     []byte
}

func smallOrder(pub []byte) bool {
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return true // not a point: unusable
	}
	return new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1
}

func parseEnvelope(b []byte, maxLen int) (*envelope, error) {
	if len(b) > maxLen || len(b) < 2 || b[0] != 0x02 {
		return nil, ErrCardMalformed
	}
	ll := int(b[1])
	if ll < 1 || ll > 64 || len(b) < 2+ll+1 {
		return nil, ErrCardMalformed
	}
	e := &envelope{label: string(b[2 : 2+ll])}
	p := 2 + ll
	if b[p] != AlgEd25519 {
		return nil, fmt.Errorf("%w: unknown algorithm", ErrCardMalformed)
	}
	p++
	if len(b) < p+32+16 {
		return nil, ErrCardMalformed
	}
	e.signer = b[p : p+32]
	p += 32
	e.seq = binary.BigEndian.Uint64(b[p:])
	e.notAfter = binary.BigEndian.Uint64(b[p+8:])
	p += 16
	bl, n, err := readQUICVarint(b[p:])
	if err != nil {
		return nil, ErrCardMalformed
	}
	p += n
	if uint64(len(b)-p) != bl+ed25519.SignatureSize {
		return nil, ErrCardMalformed
	}
	e.body = b[p : p+int(bl)]
	sig := b[p+int(bl):]
	if e.seq < 1 {
		return nil, ErrCardMalformed
	}
	if smallOrder(e.signer) {
		return nil, fmt.Errorf("%w: small-order signer", ErrCardSignature)
	}
	// crypto/ed25519 rejects S ≥ L (RFC 8032 §5.1.7) and verifies over the exact bytes.
	if !ed25519.Verify(ed25519.PublicKey(e.signer), b[:p+int(bl)], sig) {
		return nil, ErrCardSignature
	}
	return e, nil
}

func checkLifetime(notAfter uint64, now time.Time, max time.Duration) error {
	n := now.Unix()
	if int64(notAfter) <= n-60 {
		return ErrCardExpired
	}
	if int64(notAfter)-n > int64(max/time.Second)+60 {
		return ErrCardLifetime
	}
	return nil
}

// nodeCard holds the parsed fields the handshake uses.
type nodeCard struct {
	hsKey          []byte
	l1Suites       []uint16
	flags          uint32
	account        []byte
	prevHs         []byte
	prevHsNotAfter uint64
	scope          uint32
	inviteID       []byte
	devices        []byte
}

var (
	schemaNodeCard = map[int]fieldKind{1: kScalar, 2: kBytes, 3: kPacked, 4: kRepeated, 5: kRepeated, 6: kScalar,
		7: kScalar, 8: kRepeated, 9: kPacked, 10: kMsg, 11: kRepeated, 12: kBytes, 13: kBytes, 14: kBytes, 15: kMsg,
		16: kScalar, 17: kBytes, 18: kBytes}
	schemaHsKey      = map[int]fieldKind{1: kBytes, 2: kScalar}
	schemaDeviceList = map[int]fieldKind{1: kRepeated}
	schemaDevice     = map[int]fieldKind{1: kBytes, 2: kScalar, 3: kScalar, 4: kBytes}
)

func x25519Akey(b []byte) ([]byte, error) {
	if len(b) != 33 || b[0] != AlgX25519 {
		return nil, ErrCardMalformed
	}
	return clone(b[1:]), nil
}

func parseNodeCard(body []byte) (*nodeCard, error) {
	fs, err := parseCanonical(body, schemaNodeCard)
	if err != nil {
		return nil, err
	}
	c := &nodeCard{}
	for _, f := range fs {
		switch f.num {
		case 2:
			if c.hsKey, err = x25519Akey(f.data); err != nil {
				return nil, err
			}
		case 3:
			vs, err := packedVals(f.data)
			if err != nil {
				return nil, err
			}
			for _, v := range vs {
				if v <= 0xffff {
					c.l1Suites = append(c.l1Suites, uint16(v))
				}
			}
		case 7:
			if c.flags, err = u32(f.v); err != nil {
				return nil, err
			}
		case 13:
			c.account = f.data
		case 15:
			hf, err := parseCanonical(f.data, schemaHsKey)
			if err != nil {
				return nil, err
			}
			for _, x := range hf {
				switch x.num {
				case 1:
					if c.prevHs, err = x25519Akey(x.data); err != nil {
						return nil, err
					}
				case 2:
					c.prevHsNotAfter = x.v
				}
			}
		case 16:
			if c.scope, err = u32(f.v); err != nil {
				return nil, err
			}
		case 17:
			c.inviteID = f.data
		case 18:
			c.devices = f.data
		}
	}
	if c.hsKey == nil || len(c.account) != 33 || c.account[0] != AlgEd25519 || c.devices == nil {
		return nil, fmt.Errorf("%w: required field missing", ErrCardMalformed)
	}
	return c, nil
}

const (
	flagInviteScoped = 1 << 4
	scopeInvite      = 2
)

// VerifyCard implements CardVerifier.
func (DefaultCardVerifier) VerifyCard(env []byte, now time.Time) (*CardInfo, error) {
	e, err := parseEnvelope(env, MaxCardLen)
	if err != nil {
		return nil, err
	}
	if e.label != LabelCard {
		return nil, fmt.Errorf("%w: label %q", ErrCardMalformed, e.label)
	}
	c, err := parseNodeCard(e.body)
	if err != nil {
		return nil, err
	}
	max := 7 * 24 * time.Hour
	if c.scope == scopeInvite || c.flags&flagInviteScoped != 0 {
		max = 30 * 24 * time.Hour
	}
	if err := checkLifetime(e.notAfter, now, max); err != nil {
		return nil, err
	}
	// The device list: signed by the account key, valid now, lists the signer.
	d, err := parseEnvelope(c.devices, 6144)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardDevices, err)
	}
	if d.label != LabelDevices || !bytes.Equal(d.signer, c.account[1:]) {
		return nil, ErrCardDevices
	}
	if err := checkLifetime(d.notAfter, now, 30*24*time.Hour); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardDevices, err)
	}
	if !deviceListed(d.body, e.signer) {
		return nil, ErrCardDevices
	}
	info := &CardInfo{
		DeviceID:       sha256.Sum256(append([]byte{AlgEd25519}, e.signer...)),
		NodeID:         sha256.Sum256(c.account),
		HsKey:          c.hsKey,
		PrevHsKey:      c.prevHs,
		PrevHsNotAfter: c.prevHsNotAfter,
		L1Suites:       c.l1Suites,
		Scope:          c.scope,
		Flags:          c.flags,
		InviteID:       clone(c.inviteID),
		Seq:            e.seq,
		NotAfter:       e.notAfter,
		Raw:            clone(env),
	}
	return info, nil
}

func deviceListed(body, signer []byte) bool {
	fs, err := parseCanonical(body, schemaDeviceList)
	if err != nil {
		return false
	}
	want := append([]byte{AlgEd25519}, signer...)
	for _, f := range fs {
		if f.num != 1 {
			continue
		}
		df, err := parseCanonical(f.data, schemaDevice)
		if err != nil {
			return false
		}
		for _, x := range df {
			if x.num == 1 && bytes.Equal(x.data, want) {
				return true
			}
		}
	}
	return false
}
