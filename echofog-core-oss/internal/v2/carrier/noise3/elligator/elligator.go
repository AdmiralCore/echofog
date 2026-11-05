// Package elligator implements the Elligator2 representative of a "dirty" X25519 ephemeral key
// (docs/protocol/v2/wire.md §2.2).
//
// The initiator's public key is not a plain X25519 key: it is P = [clamp(sk)]B + [sk[0]&7]T, a
// point uniform on the whole curve. Its Montgomery u-coordinate is encoded with the Elligator2
// inverse map, so the 32 bytes look uniformly random. The responder decodes them with the forward
// map (RFC 9380 §6.7.1, Z = 2) and gets the same u; the torsion component changes the encoding but
// not X25519(sk, u), because every clamped scalar is a multiple of 8.
package elligator

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"math/big"

	"filippo.io/edwards25519"
	"filippo.io/edwards25519/field"
	"golang.org/x/crypto/curve25519"
)

// Size is the size of a representative, a u-coordinate and a private key.
const Size = 32

// ErrBadKey means the private key is not 32 bytes.
var ErrBadKey = errors.New("elligator: private key must be 32 bytes")

var (
	// p = 2^255 - 19.
	p = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	// halfP is (p - 1) / 2: the inverse map takes the root no larger than this.
	halfP = new(big.Int).Rsh(new(big.Int).Sub(p, big.NewInt(1)), 1)

	aElement = feUint(486662)
	one      = new(field.Element).One()
	zero     = new(field.Element).Zero()

	// t8 is the order-8 point fixed by wire.md §2.2.
	t8 = mustPoint("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a")
)

func mustPoint(hexBytes string) *edwards25519.Point {
	b := make([]byte, Size)
	// The constant is given as hex; decode it without encoding/hex to keep the import list small.
	for i := 0; i < Size; i++ {
		hi := hexNibble(hexBytes[2*i])
		lo := hexNibble(hexBytes[2*i+1])
		b[i] = hi<<4 | lo
	}
	pt, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		panic("elligator: fixed order-8 point does not parse")
	}
	return pt
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	panic("elligator: bad hex digit")
}

func feUint(v uint64) *field.Element {
	var b [Size]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	e, err := new(field.Element).SetBytes(b[:])
	if err != nil {
		panic("elligator: constant is not a field element")
	}
	return e
}

func fe(b []byte) (*field.Element, error) {
	if len(b) != Size {
		return nil, ErrBadKey
	}
	return new(field.Element).SetBytes(b)
}

func isSquare(x *field.Element) bool {
	_, ok := new(field.Element).SqrtRatio(x, one)
	return ok == 1
}

func leBig(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

// Map is RFC 9380 §6.7.1 map_to_curve_elligator2 for curve25519 (Z = 2): a 32-byte little-endian
// field element to the Montgomery u-coordinate. Forward is Map with the two top bits cleared.
func Map(r []byte) []byte {
	re, err := fe(r)
	if err != nil {
		return make([]byte, Size)
	}
	r2 := new(field.Element).Square(re)
	den := new(field.Element).Add(one, new(field.Element).Add(r2, r2))
	x1 := new(field.Element).Multiply(aElement, new(field.Element).Invert(den))
	x1.Negate(x1)
	g := func(x *field.Element) *field.Element {
		t := new(field.Element).Square(x)
		t.Add(t, new(field.Element).Multiply(aElement, x))
		t.Add(t, one)
		return t.Multiply(t, x)
	}
	if isSquare(g(x1)) {
		return x1.Bytes()
	}
	u := new(field.Element).Negate(x1)
	return u.Subtract(u, aElement).Bytes()
}

// Forward decodes a representative to the Montgomery u-coordinate (wire.md §2.2): bits 254 and
// 255 are cleared first, so every 32-byte string decodes.
func Forward(repr []byte) []byte {
	rb := append([]byte(nil), repr...)
	if len(rb) != Size {
		return make([]byte, Size)
	}
	rb[31] &= 0x3f
	return Map(rb)
}

// Reverse encodes u as a representative whose top two bits are tweak & 0xc0 and whose root is
// selected by tweak & 1. It reports false when u has no representative: u = 0, u = -A, or
// -2u(u+A) is not a square. About half of all u-coordinates fail.
func Reverse(u []byte, tweak byte) ([]byte, bool) {
	ue, err := fe(u)
	if err != nil {
		return nil, false
	}
	upA := new(field.Element).Add(ue, aElement)
	if ue.Equal(zero) == 1 || upA.Equal(zero) == 1 {
		return nil, false
	}
	t := new(field.Element).Multiply(ue, upA)
	t.Add(t, t)
	t.Negate(t)
	if !isSquare(t) {
		return nil, false
	}
	var num, den *field.Element
	if tweak&1 == 0 {
		num = new(field.Element).Negate(ue)
		den = new(field.Element).Add(upA, upA)
	} else {
		num = new(field.Element).Negate(upA)
		den = new(field.Element).Add(ue, ue)
	}
	r, ok := new(field.Element).SqrtRatio(num, den)
	if ok != 1 {
		return nil, false
	}
	nr := new(field.Element).Negate(r)
	chosen := r
	if leBig(nr.Bytes()).Cmp(leBig(r.Bytes())) < 0 {
		chosen = nr
	}
	if leBig(chosen.Bytes()).Cmp(halfP) > 0 {
		return nil, false
	}
	out := chosen.Bytes()
	out[31] |= tweak & 0xc0
	return out, true
}

// DirtyPublic returns the dirty Montgomery u-coordinate of [clamp(sk)]B + [sk[0]&7]T and the plain
// X25519 public key. Both give the same shared secret with every peer scalar.
func DirtyPublic(sk []byte) (dirty, clean []byte, err error) {
	if len(sk) != Size {
		return nil, nil, ErrBadKey
	}
	s, err := new(edwards25519.Scalar).SetBytesWithClamping(sk)
	if err != nil {
		return nil, nil, err
	}
	pt := new(edwards25519.Point).ScalarBaseMult(s)
	clean, err = curve25519.X25519(sk, curve25519.Basepoint)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(pt.BytesMontgomery(), clean) {
		return nil, nil, errors.New("elligator: Ed25519 and X25519 base points disagree")
	}
	for i := 0; i < int(sk[0]&7); i++ {
		pt.Add(pt, t8)
	}
	return pt.BytesMontgomery(), clean, nil
}

// Encode returns the representative of the dirty public key of sk for the given tweak. It is the
// deterministic building block behind Generate; the tweak must be independent of sk.
func Encode(sk []byte, tweak byte) (repr, dirty []byte, ok bool, err error) {
	dirty, _, err = DirtyPublic(sk)
	if err != nil {
		return nil, nil, false, err
	}
	repr, ok = Reverse(dirty, tweak)
	if !ok {
		return nil, dirty, false, nil
	}
	if !bytes.Equal(Forward(repr), dirty) {
		return nil, dirty, false, errors.New("elligator: forward map does not return the dirty key")
	}
	return repr, dirty, true, nil
}

// Generate draws a fresh private key and tweak from r and returns the first pair whose dirty key
// has a representative. About half of the attempts fail, so the loop is expected to run twice.
func Generate(r io.Reader) (sk, repr []byte, err error) {
	if r == nil {
		r = rand.Reader
	}
	var buf [Size + 1]byte
	for attempt := 0; attempt < 1024; attempt++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return nil, nil, err
		}
		sk = append([]byte(nil), buf[:Size]...)
		repr, _, ok, err := Encode(sk, buf[Size])
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return sk, repr, nil
		}
	}
	return nil, nil, errors.New("elligator: no representative in 1024 attempts")
}
