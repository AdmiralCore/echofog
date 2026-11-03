// Primitives for the clean-room handshake (handshake.md §2), written from the spec text only.
//
// There is deliberately no SymmetricState or CipherState type: every handshake function keeps
// h, ck, k and n as its own local variables and calls the pure helpers below. A shared mistake
// with a state-machine implementation is therefore less likely.
package main

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
)

const fixedTime = 1793664000 // vectors/README.md: 2026-11-03T00:00:00Z

// seed is vectors/README.md: SHA-256("echofog v2 test vector seed: " ‖ name).
func seed(name string) []byte {
	s := sha256.Sum256([]byte("echofog v2 test vector seed: " + name))
	return s[:]
}

func join(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func hexs(b []byte) string { return hex.EncodeToString(b) }

func fromHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- HASH = BLAKE2s-256, HMAC-HASH, Noise HKDF ------------------------------------------------

func newBlake() hash.Hash {
	h, _ := blake2s.New256(nil)
	return h
}

func blake(parts ...[]byte) []byte {
	h := newBlake()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func hmacBlake(key []byte, parts ...[]byte) []byte {
	m := hmac.New(newBlake, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// noiseHKDF2 returns o1, o2 of handshake.md §2 HKDF(ck, ikm, 2).
func noiseHKDF2(ck, ikm []byte) ([]byte, []byte) {
	t := hmacBlake(ck, ikm)
	o1 := hmacBlake(t, []byte{1})
	o2 := hmacBlake(t, o1, []byte{2})
	return o1, o2
}

// initialH is Noise §5.2 InitializeSymmetric: pad to HASHLEN if short, else HASH(name).
func initialH(name string) []byte {
	if len(name) <= 32 {
		h := make([]byte, 32)
		copy(h, name)
		return h
	}
	return blake([]byte(name))
}

// ---- AEAD -------------------------------------------------------------------------------------

func nonce12(n uint64) []byte {
	b := make([]byte, 12) // 32 zero bits ‖ u64le(n)
	binary.LittleEndian.PutUint64(b[4:], n)
	return b
}

func aeadSeal(k []byte, n uint64, ad, pt []byte) []byte {
	a, err := chacha20poly1305.New(k)
	if err != nil {
		panic(err)
	}
	return a.Seal(nil, nonce12(n), pt, ad)
}

func aeadOpen(k []byte, n uint64, ad, ct []byte) ([]byte, error) {
	a, err := chacha20poly1305.New(k)
	if err != nil {
		return nil, err
	}
	return a.Open(nil, nonce12(n), ct, ad)
}

// ---- Noise symmetric operations as pure functions --------------------------------------------
//
// k == nil means "no key" (Noise: k = empty).

func mixHash(h, data []byte) []byte { return blake(h, data) }

// mixKey returns the new ck and k; the caller resets its n to 0.
func mixKey(ck, ikm []byte) (newCK, k []byte) { return noiseHKDF2(ck, ikm) }

// encHash is EncryptAndHash: returns the bytes to append, the new h and the new n.
func encHash(h, k []byte, n uint64, pt []byte) (out, newH []byte, newN uint64) {
	if k == nil {
		return append([]byte(nil), pt...), mixHash(h, pt), n
	}
	ct := aeadSeal(k, n, h, pt)
	return ct, mixHash(h, ct), n + 1
}

// decHash is DecryptAndHash.
func decHash(h, k []byte, n uint64, in []byte) (pt, newH []byte, newN uint64, err error) {
	if k == nil {
		return append([]byte(nil), in...), mixHash(h, in), n, nil
	}
	pt, err = aeadOpen(k, n, h, in)
	if err != nil {
		return nil, nil, 0, err
	}
	return pt, mixHash(h, in), n + 1, nil
}

// ---- DH, KEM ----------------------------------------------------------------------------------

func x25519Public(sk []byte) []byte {
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		panic(err)
	}
	return k.PublicKey().Bytes()
}

var errZeroDH = errors.New("all-zero X25519 output")

// dh is X25519; crypto/ecdh already refuses an all-zero result, and we check again.
func dh(sk, pub []byte) ([]byte, error) {
	k, err := ecdh.X25519().NewPrivateKey(sk)
	if err != nil {
		return nil, err
	}
	p, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	z, err := k.ECDH(p)
	if err != nil {
		return nil, errZeroDH
	}
	var acc byte
	for _, b := range z {
		acc |= b
	}
	if acc == 0 {
		return nil, errZeroDH
	}
	return z, nil
}

// kemGenerate is KEM_GENERATE from the 64-byte seed d ‖ z (FIPS 203 ML-KEM.KeyGen_internal).
func kemGenerate(dz []byte) (*mlkem.DecapsulationKey768, []byte) {
	dk, err := mlkem.NewDecapsulationKey768(dz)
	if err != nil {
		panic(err)
	}
	return dk, dk.EncapsulationKey().Bytes()
}

// kemEncap runs the FIPS 203 encapsulation-key check, then encapsulates with fixed randomness m.
func kemEncap(ekBytes, m []byte) (ct, ss []byte, err error) {
	ek, err := mlkem.NewEncapsulationKey768(ekBytes) // modulus / length check
	if err != nil {
		return nil, nil, err
	}
	ss, ct, err = mlkemtest.Encapsulate768(ek, m)
	return ct, ss, err
}

// ---- SHA-256 helpers (outside Noise) ---------------------------------------------------------

func sha(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func hmacSHA(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func u16be(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u64be(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// quicVarint is RFC 9000 §16 (frame type and length, wire.md §6.1).
func quicVarint(v uint64) []byte {
	switch {
	case v < 64:
		return []byte{byte(v)}
	case v < 16384:
		return []byte{0x40 | byte(v>>8), byte(v)}
	case v < 1<<30:
		return []byte{0x80 | byte(v>>24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
	b := u64be(v)
	b[0] |= 0xc0
	return b
}

// streamFrame is wire.md §6.1: type(varint) ‖ flags(u8) ‖ length(varint) ‖ payload.
func streamFrame(typ uint64, payload []byte) []byte {
	return join(quicVarint(typ), []byte{0}, quicVarint(uint64(len(payload))), payload)
}
