// Command gen writes the EchoFog protocol v2 test vectors (docs/protocol/v2/vectors/*.json).
//
// It is a reference generator, not product code. It uses only the Go standard library,
// golang.org/x/crypto and filippo.io/edwards25519. Every input is derived from a named seed,
//
//	seed(name) = SHA-256("echofog v2 test vector seed: " ‖ name)
//
// and every file lists the seeds it used, so each vector can be reproduced by hand.
//
// Run from this directory (the module is outside the repository's go.work):
//
//	GOWORK=off go run . -out ..
package main

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"math/big"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/edwards25519"
	"filippo.io/edwards25519/field"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// ---------------------------------------------------------------------------------------------
// Inputs

const seedPrefix = "echofog v2 test vector seed: "

// usedSeeds records every seed a file used, in order.
type seedLog struct{ o O }

func (s *seedLog) get(name string) []byte {
	h := sha256.Sum256([]byte(seedPrefix + name))
	s.o = append(s.o, kv{name, hx(h[:])})
	return h[:]
}

// fixedTime is the clock of every vector: 2026-11-03T00:00:00Z.
var fixedTime = uint64(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC).Unix())

// ---------------------------------------------------------------------------------------------
// Ordered JSON

type kv struct {
	k string
	v any
}

// O is a JSON object that keeps its key order.
type O []kv

func (o O) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshal(e.k)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func writeFile(dir, name string, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	must(enc.Encode(v))
	must(os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644))
	fmt.Println("wrote", name)
}

// ---------------------------------------------------------------------------------------------
// Small helpers

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func check(ok bool, what string) {
	if !ok {
		panic("self-check failed: " + what)
	}
}

func hx(b []byte) string { return hex.EncodeToString(b) }

func unhex(s string) []byte {
	b, err := hex.DecodeString(s)
	must(err)
	return b
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func sha(parts ...[]byte) []byte {
	h := sha256.Sum256(cat(parts...))
	return h[:]
}

func hmacSHA(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(cat(parts...))
	return m.Sum(nil)
}

func hkdfSHA(ikm, salt, info []byte, n int) []byte {
	out, err := hkdf.Key(sha256.New, ikm, salt, string(info), n)
	must(err)
	return out
}

func x25519Pub(sk []byte) []byte {
	k, err := ecdh.X25519().NewPrivateKey(sk)
	must(err)
	return k.PublicKey().Bytes()
}

func x25519(sk, pub []byte) []byte {
	k, err := ecdh.X25519().NewPrivateKey(sk)
	must(err)
	p, err := ecdh.X25519().NewPublicKey(pub)
	must(err)
	z, err := k.ECDH(p)
	must(err)
	return z
}

func chachaSeal(key, nonce, pt, aad []byte) []byte {
	a, err := chacha20poly1305.New(key)
	must(err)
	return a.Seal(nil, nonce, pt, aad)
}

func chachaOpen(key, nonce, ct, aad []byte) []byte {
	a, err := chacha20poly1305.New(key)
	must(err)
	pt, err := a.Open(nil, nonce, ct, aad)
	must(err)
	return pt
}

// counterNonce is 32 zero bits followed by the little-endian 64-bit counter (Noise style).
func counterNonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint64(b[4:], n)
	return b
}

func pattern(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

// ---------------------------------------------------------------------------------------------
// QUIC varint (RFC 9000 §16)

func qv(v uint64) []byte {
	switch {
	case v < 1<<6:
		return []byte{byte(v)}
	case v < 1<<14:
		return []byte{0x40 | byte(v>>8), byte(v)}
	case v < 1<<30:
		b := u32(uint32(v))
		b[0] |= 0x80
		return b
	case v < 1<<62:
		b := u64(v)
		b[0] |= 0xc0
		return b
	}
	panic("varint too large")
}

// qvDecode decodes a varint and reports whether its encoding was the shortest one.
func qvDecode(b []byte) (v uint64, n int, minimal bool) {
	n = 1 << (b[0] >> 6)
	v = uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, len(qv(v)) == n
}

func frame(typ uint64, flags byte, payload []byte) []byte {
	return cat(qv(typ), []byte{flags}, qv(uint64(len(payload))), payload)
}

// ---------------------------------------------------------------------------------------------
// Protobuf (proto3 wire format), written in canonical form by construction

type pb struct{ b []byte }

func pv(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func (p *pb) tag(n, wt int) { p.b = pv(p.b, uint64(n)<<3|uint64(wt)) }

func (p *pb) u(n int, v uint64) *pb {
	if v != 0 {
		p.tag(n, 0)
		p.b = pv(p.b, v)
	}
	return p
}

func (p *pb) by(n int, v []byte) *pb {
	if len(v) != 0 {
		p.tag(n, 2)
		p.b = pv(p.b, uint64(len(v)))
		p.b = append(p.b, v...)
	}
	return p
}

func (p *pb) s(n int, v string) *pb { return p.by(n, []byte(v)) }

func (p *pb) packed(n int, vs ...uint64) *pb {
	if len(vs) == 0 {
		return p
	}
	var inner []byte
	for _, v := range vs {
		inner = pv(inner, v)
	}
	return p.by(n, inner)
}

// checkCanonicalTop checks rules 1, 2 and 5 of objects.md §2.3 at the top level of a message.
func checkCanonicalTop(b []byte, repeated map[int]bool) {
	last := 0
	for len(b) > 0 {
		t, n := binary.Uvarint(b)
		check(n > 0 && len(binary.AppendUvarint(nil, t)) == n, "minimal tag")
		b = b[n:]
		num, wt := int(t>>3), int(t&7)
		check(num > last || (num == last && repeated[num]), fmt.Sprintf("field order at %d", num))
		last = num
		switch wt {
		case 0:
			v, n := binary.Uvarint(b)
			check(n > 0 && len(binary.AppendUvarint(nil, v)) == n, "minimal varint")
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			check(n > 0, "length")
			b = b[n+int(l):]
		default:
			panic("unexpected wire type")
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Signed objects (objects.md §2)

type signer struct {
	seed []byte
	priv ed25519.PrivateKey
	pub  []byte
}

func newSigner(seed []byte) signer {
	priv := ed25519.NewKeyFromSeed(seed)
	return signer{seed, priv, priv.Public().(ed25519.PublicKey)}
}

func (s signer) akey() []byte   { return cat([]byte{0x01}, s.pub) }
func (s signer) nodeID() []byte { return sha(s.akey()) }

func envelope(label string, s signer, seq, notAfter uint64, body []byte) (env, signed, sig []byte) {
	signed = cat([]byte{0x02, byte(len(label))}, []byte(label), []byte{0x01}, s.pub, u64(seq), u64(notAfter),
		qv(uint64(len(body))), body)
	sig = ed25519.Sign(s.priv, signed)
	return cat(signed, sig), signed, sig
}

// verifyEnvelope parses an Ed25519 envelope and checks its signature.
func verifyEnvelope(env []byte) (label string, body []byte) {
	check(env[0] == 0x02, "fmt")
	ll := int(env[1])
	label = string(env[2 : 2+ll])
	p := 2 + ll
	check(env[p] == 0x01, "alg")
	pub := env[p+1 : p+33]
	p += 33 + 16
	bl, n, minimal := qvDecode(env[p:])
	check(minimal, "body_len minimal")
	p += n
	body = env[p : p+int(bl)]
	p += int(bl)
	check(len(env) == p+64, "length")
	check(ed25519.Verify(pub, env[:p], env[p:]), "signature")
	return label, body
}

func rev(b []byte) []byte {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return r
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ---------------------------------------------------------------------------------------------
// Elligator2 over Curve25519 (wire.md §2.2)

var (
	feOne  = new(field.Element).One()
	feZero = new(field.Element).Zero()
	feA    = feUint(486662)
	pBig   = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
)

func feUint(v uint64) *field.Element {
	var b [32]byte
	binary.LittleEndian.PutUint64(b[:], v)
	e, err := new(field.Element).SetBytes(b[:])
	must(err)
	return e
}

func feBytes(b []byte) *field.Element {
	e, err := new(field.Element).SetBytes(b)
	must(err)
	return e
}

func isSquare(x *field.Element) bool {
	_, ok := new(field.Element).SqrtRatio(x, feOne)
	return ok == 1
}

func leBig(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

// ellForward maps a representative to a Montgomery u-coordinate (RFC 9380 §6.7.1, Z = 2).
func ellForward(repr []byte) []byte {
	rb := append([]byte(nil), repr...)
	rb[31] &= 0x3f
	return ellMap(rb)
}

// ellMap is map_to_curve_elligator2 of RFC 9380 §6.7.1 for curve25519 on a field element r
// (32 bytes little-endian), returning the u-coordinate.
func ellMap(rb []byte) []byte {
	r := feBytes(rb)
	r2 := new(field.Element).Square(r)
	den := new(field.Element).Add(feOne, new(field.Element).Add(r2, r2))
	x1 := new(field.Element).Multiply(feA, new(field.Element).Invert(den))
	x1.Negate(x1)
	// g(x) = x·(x² + A·x + 1)
	g := func(x *field.Element) *field.Element {
		t := new(field.Element).Square(x)
		t.Add(t, new(field.Element).Multiply(feA, x))
		t.Add(t, feOne)
		return t.Multiply(t, x)
	}
	if isSquare(g(x1)) {
		return x1.Bytes()
	}
	u := new(field.Element).Negate(x1)
	return u.Subtract(u, feA).Bytes()
}

// ellReverse returns the representative of u for the given tweak, or nil if u has none.
func ellReverse(uBytes []byte, tweak byte) []byte {
	u := feBytes(uBytes)
	upA := new(field.Element).Add(u, feA)
	if u.Equal(feZero) == 1 || upA.Equal(feZero) == 1 {
		return nil
	}
	t := new(field.Element).Multiply(u, upA)
	t.Add(t, t)
	t.Negate(t)
	if !isSquare(t) {
		return nil
	}
	var num, den *field.Element
	if tweak&1 == 0 {
		num = new(field.Element).Negate(u)
		den = new(field.Element).Add(upA, upA)
	} else {
		num = new(field.Element).Negate(upA)
		den = new(field.Element).Add(u, u)
	}
	r, ok := new(field.Element).SqrtRatio(num, den)
	check(ok == 1, "sqrt exists")
	rn := new(field.Element).Negate(r)
	chosen := r
	if leBig(rn.Bytes()).Cmp(leBig(r.Bytes())) < 0 {
		chosen = rn
	}
	half := new(big.Int).Rsh(new(big.Int).Sub(pBig, big.NewInt(1)), 1)
	check(leBig(chosen.Bytes()).Cmp(half) <= 0, "r ≤ (p−1)/2")
	out := chosen.Bytes()
	out[31] |= tweak & 0xc0
	return out
}

// t8 is the order-8 point of wire.md §2.2.
var t8 = func() *edwards25519.Point {
	p, err := new(edwards25519.Point).SetBytes(unhex("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"))
	must(err)
	acc := edwards25519.NewIdentityPoint()
	for i := 1; i <= 8; i++ {
		acc.Add(acc, p)
		if i == 4 {
			check(acc.Equal(edwards25519.NewIdentityPoint()) == 0, "T has order 8, not 4")
		}
	}
	check(acc.Equal(edwards25519.NewIdentityPoint()) == 1, "8T = identity")
	return p
}()

// dirtyPub returns the Montgomery u of [clamp(sk)]B + [sk[0] & 7]T and the clean X25519 public key.
func dirtyPub(sk []byte) (dirty, clean []byte) {
	s, err := new(edwards25519.Scalar).SetBytesWithClamping(sk)
	must(err)
	p := new(edwards25519.Point).ScalarBaseMult(s)
	check(bytes.Equal(p.BytesMontgomery(), x25519Pub(sk)), "Edwards and X25519 base points agree")
	clean, err = curve25519.X25519(sk, curve25519.Basepoint)
	must(err)
	for i := 0; i < int(sk[0]&7); i++ {
		p.Add(p, t8)
	}
	return p.BytesMontgomery(), clean
}

type ellKey struct {
	sk, dirty, clean, repr []byte
	tweak                  byte
	attempt                int
	failed                 int
}

// ellKeygen derives sk_i = SHA-256(base ‖ u32(i)) for i = 0, 1, … until the dirty key has a
// representative.
func ellKeygen(base []byte, tweak byte) ellKey {
	failed := 0
	for i := 0; ; i++ {
		sk := sha(base, u32(uint32(i)))
		dirty, clean := dirtyPub(sk)
		repr := ellReverse(dirty, tweak)
		if repr == nil {
			failed++
			continue
		}
		check(bytes.Equal(ellForward(repr), dirty), "forward(reverse(u)) = u")
		return ellKey{sk, dirty, clean, repr, tweak, i, failed}
	}
}

// ---------------------------------------------------------------------------------------------
// Noise symmetric-state pieces (handshake.md §2), BLAKE2s

func newB2s() hash.Hash {
	h, err := blake2s.New256(nil)
	must(err)
	return h
}

func b2s(parts ...[]byte) []byte {
	h := newB2s()
	h.Write(cat(parts...))
	return h.Sum(nil)
}

func hmacB2s(key []byte, parts ...[]byte) []byte {
	m := hmac.New(newB2s, key)
	m.Write(cat(parts...))
	return m.Sum(nil)
}

func noiseHKDF(ck, ikm []byte) (o1, o2 []byte) {
	t := hmacB2s(ck, ikm)
	o1 = hmacB2s(t, []byte{0x01})
	o2 = hmacB2s(t, o1, []byte{0x02})
	return
}

func noiseInit(name string) []byte {
	if len(name) <= 32 {
		h := make([]byte, 32)
		copy(h, name)
		return h
	}
	return b2s([]byte(name))
}

// ---------------------------------------------------------------------------------------------

func main() {
	out := flag.String("out", "..", "directory to write the JSON files to")
	flag.Parse()
	must(os.MkdirAll(*out, 0o755))

	writeFile(*out, "varint.json", genVarint())
	writeFile(*out, "frames.json", genFrames())
	ell := genElligator()
	writeFile(*out, "elligator2.json", ell)
	n3, maskPub3 := genNoise3()
	writeFile(*out, "noise3.json", n3)
	rl, maskPubR := genReality()
	writeFile(*out, "reality2.json", rl)
	so := genSignedObject()
	writeFile(*out, "signed-object.json", so)
	card, cardEnv, cardSigner, hsPub := genCard(maskPub3, maskPubR)
	writeFile(*out, "node-card.json", card)
	writeFile(*out, "link.json", genLink(cardEnv))
	writeFile(*out, "handshake.json", genHandshake(cardEnv, hsPub))
	writeFile(*out, "kdf.json", genKDF(cardSigner, hsPub))
}

// ---------------------------------------------------------------------------------------------

func genVarint() any {
	var valid []any
	// The first five values and encodings are the examples of RFC 9000 Appendix A.1.
	rfc := map[uint64]string{151288809941952652: "c2197c5eff14e88c", 494878333: "9d7f3e7d", 15293: "7bbd", 37: "25"}
	for _, v := range []uint64{0, 37, 63, 64, 15293, 16383, 16384, 494878333, 1073741823, 1073741824,
		151288809941952652, 1<<62 - 1} {
		enc := qv(v)
		if want, ok := rfc[v]; ok {
			check(hx(enc) == want, "RFC 9000 A.1 example")
		}
		d, n, minimal := qvDecode(enc)
		check(d == v && n == len(enc) && minimal, "round trip")
		valid = append(valid, O{{"value", v}, {"encoded", hx(enc)}})
	}
	var invalid []any
	for _, e := range []string{"4025", "8000000025", "c000000000000025", "4001", "800000ff"} {
		v, _, minimal := qvDecode(unhex(e))
		check(!minimal, "non-minimal")
		invalid = append(invalid, O{{"encoded", e}, {"would_decode_to", v}, {"reason", "non-minimal encoding: MUST be rejected"}})
	}
	return O{
		{"description", "QUIC variable-length integers (RFC 9000 §16) as used by frame v2 and the signed-object envelope. Encoders MUST use the shortest form; decoders MUST reject longer forms."},
		{"spec", "wire.md §6, README.md §6"},
		{"valid", valid},
		{"invalid", invalid},
	}
}

func genFrames() any {
	var fr []any
	add := func(name string, typ uint64, flags byte, payload []byte, note string) {
		e := O{{"name", name}, {"type", typ}, {"flags", flags}, {"payload", hx(payload)}, {"encoded", hx(frame(typ, flags, payload))}}
		if note != "" {
			e = append(e, kv{"note", note})
		}
		fr = append(fr, e)
	}
	add("PADDING", 0x01, 0x01, make([]byte, 4), "IGNORABLE set")
	hello := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0})
	add("HS_HELLO", 0x02, 0, hello, "offered [0x0001, 0x0002], chosen 0x0001, no puzzle")
	cookie := pattern(16, 0xa0)
	helloP := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{1}, cookie, u64(4242))
	add("HS_HELLO (puzzle)", 0x02, 0, helloP, "flags 0x01, cookie a0..af, nonce 4242")
	add("HS_RETRY", 0x04, 0, cat(cookie, []byte{16}), "difficulty 16")
	rej := (&pb{}).u(1, 1).u(2, 200).u(3, 200).b
	add("REJECT", 0x05, 0, rej, "Reject{reason=1 version, min=200, max=200}")
	add("PING", 0x06, 0, unhex("0102030405060708"), "")
	add("PONG", 0x07, 0, unhex("0102030405060708"), "")
	ga := (&pb{}).u(1, 8).s(2, "restart").u(3, 30).b
	add("GOAWAY", 0x08, 0, ga, "GoAway{code=8 going-away, reason=\"restart\", retry_after=30}")
	add("CLOSE", 0x09, 0, cat(qv(4), []byte("service-unsupported")), "code 4")
	add("OPEN chat/2", 0x0a, 0, cat(qv(3), qv(2), []byte{0}), "")
	add("OPEN dht/2 EARLY", 0x0a, 0, cat(qv(1), qv(2), []byte{1}), "0-RTT stream")
	add("UNSUPPORTED", 0x0b, 0, cat(qv(9), qv(0), qv(0)), "service 9 not served")
	add("OBSERVED_ADDR v4", 0x0c, 0, cat([]byte{4}, []byte{192, 0, 2, 33}, u16(51234)), "192.0.2.33:51234")
	add("OBSERVED_ADDR v6", 0x0c, 0, cat([]byte{6}, unhex("20010db8000000000000000000000001"), u16(443)), "[2001:db8::1]:443")
	add("PATH_BIND_OK", 0x11, 0, qv(2), "")
	add("PATH_ABANDON", 0x12, 0, cat(qv(2), qv(9)), "code 9 path-failed")
	add("MUX_STREAM", 0x18, 0, cat(qv(4), []byte{0x01}, []byte("hi")), "stream 4, FIN")
	add("MUX_RESET", 0x19, 0, cat(qv(4), qv(5)), "code 5 refused")
	add("MUX_WINDOW", 0x1a, 0, cat(qv(4), qv(131072)), "")
	add("MUX_MAX_DATA", 0x1b, 0, qv(2097152), "")
	add("dht/2 FIND_NODE", 0x40, 0, (&pb{}).u(1, 7).by(2, pattern(32, 0)).b, "service-local type, req=7, target 00..1f")
	add("unknown ignorable", 0x3e, 0x01, []byte{0xff}, "a receiver that does not know 0x3e skips it")

	var dg []any
	adddg := func(name string, typ uint64, payload []byte) {
		dg = append(dg, O{{"name", name}, {"type", typ}, {"payload", hx(payload)}, {"encoded", hx(cat(qv(typ), payload))}})
	}
	adddg("PADDING", 0x01, make([]byte, 8))
	adddg("RELAY_DGRAM", 0x30, cat(qv(77), []byte("sealed…")))
	adddg("EXIT_UDP fragment 0", 0x20, cat(qv(5), u16(1), []byte{0, 1}, []byte{14}, []byte("192.0.2.53:53"), []byte{0xab, 0xcd}))
	return O{
		{"description", "frame v2: type(varint) ‖ flags(u8) ‖ length(varint) ‖ payload; datagrams: type(varint) ‖ payload"},
		{"spec", "wire.md §6, registries.md §1–§2"},
		{"frames", fr},
		{"datagrams", dg},
	}
}

func genElligator() any {
	var sl seedLog
	var samples []any
	for i := 0; i < 4; i++ {
		base := sl.get(fmt.Sprintf("elligator2/sample%d", i))
		tweak := sl.get(fmt.Sprintf("elligator2/tweak%d", i))[0]
		k := ellKeygen(base, tweak)
		samples = append(samples, O{
			{"seed_base", hx(base)},
			{"attempt", k.attempt},
			{"sk", hx(k.sk)},
			{"sk0_low3", k.sk[0] & 7},
			{"tweak", fmt.Sprintf("%02x", tweak)},
			{"clean_pub", hx(k.clean)},
			{"dirty_pub", hx(k.dirty)},
			{"repr", hx(k.repr)},
			{"forward_repr", hx(ellForward(k.repr))},
			{"keys_without_representative_before", k.failed},
		})
	}
	// Forward map of fixed byte strings (any 32 bytes decode).
	var fwd []any
	for _, r := range [][]byte{make([]byte, 32), pattern(32, 1), bytes.Repeat([]byte{0xff}, 32)} {
		fwd = append(fwd, O{{"repr", hx(r)}, {"u", hx(ellForward(r))}})
	}
	// Cross-check of the forward map against RFC 9380 Appendix J.4.2
	// (curve25519_XMD:SHA-512_ELL2_NU_): u[0] → Q.x, both given big-endian in the RFC.
	var rfc []any
	for _, v := range [][2]string{
		{"608d892b641f0328523802a6603427c26e55e6f27e71a91a478148d45b5093cd", "51125222da5e763d97f3c10fcc92ea6860b9ccbbd2eb1285728f566721c1e65b"},
		{"46f5b22494bfeaa7f232cc8d054be68561af50230234d7d1d63d1d9abeca8da5", "7d56d1e08cb0ccb92baf069c18c49bb5a0dcd927eff8dcf75ca921ef7f3e6eeb"},
	} {
		got := ellMap(rev(unhex(v[0])))
		check(hx(rev(got)) == v[1], "RFC 9380 J.4.2 map_to_curve")
		rfc = append(rfc, O{{"u0_be", v[0]}, {"Q.x_be", v[1]}})
	}
	// Torsion does not change the shared secret.
	peer := sl.get("elligator2/peer_sk")
	k0 := ellKeygen(sl.get("elligator2/sample0"), sl.get("elligator2/tweak0")[0])
	zDirty := x25519(peer, k0.dirty)
	zClean := x25519(peer, k0.clean)
	zInit := x25519(k0.sk, x25519Pub(peer))
	check(bytes.Equal(zDirty, zClean) && bytes.Equal(zClean, zInit), "dirty key gives the same ECDH")
	return O{
		{"description", "Elligator2 for Curve25519 with dirty ephemeral keys: P = [clamp(sk)]B + [sk[0]&7]T, u = Montgomery u of P; repr from the inverse map with tweak bit 0 selecting the root formula and tweak bits 6-7 as the two top bits; forward map per RFC 9380 §6.7.1 (Z = 2). sk_i = SHA-256(seed_base ‖ u32(i)) for the first i whose dirty key has a representative."},
		{"spec", "wire.md §2.2"},
		{"T_order8", "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"},
		{"seeds", sl.o},
		{"samples", samples},
		{"forward_only", fwd},
		{"rfc9380_crosscheck", O{{"note", "the generator checks its forward map on full field elements against RFC 9380 Appendix J.4.2 (u[0] → Q.x); values big-endian as in the RFC"}, {"cases", rfc}}},
		{"ecdh_check", O{
			{"peer_sk", hx(peer)},
			{"x25519(peer_sk, sample0.dirty_pub)", hx(zDirty)},
			{"x25519(peer_sk, sample0.clean_pub)", hx(zClean)},
			{"x25519(sample0.sk, peer_pub)", hx(zInit)},
		}},
	}
}

func genNoise3() (any, []byte) {
	var sl seedLog
	maskSK := sl.get("noise3/mask_sk")
	maskPub := x25519Pub(maskSK)
	base := sl.get("noise3/ephemeral")
	tweak := sl.get("noise3/tweak")[0]
	k := ellKeygen(base, tweak)

	kSel := hmacSHA(maskPub, []byte("echofog/noise3/selector"))
	sel := hmacSHA(kSel, k.repr)[:16]
	z := x25519(k.sk, maskPub)
	uResp := ellForward(k.repr)
	check(bytes.Equal(x25519(maskSK, uResp), z), "responder derives the same z")
	okm := hkdfSHA(z, nil, cat([]byte("echofog/noise3/keys"), maskPub, k.dirty), 160)
	kInit, kI2R, kR2I, tI2R, tR2I := okm[0:32], okm[32:64], okm[64:96], okm[96:128], okm[128:160]

	inner := pattern(1200, 0) // synthetic stand-in for a QUIC Initial
	total := 1300
	padLen := total - 71 - len(inner)
	initPlain := cat(u32(uint32(fixedTime)), []byte{0}, u16(uint16(len(inner))), inner, make([]byte, padLen))
	box := chachaSeal(kInit, make([]byte, 12), initPlain, cat(k.repr, sel))
	initDgram := cat(k.repr, sel, box)
	check(len(initDgram) == total, "Init length")
	check(bytes.Equal(chachaOpen(kInit, make([]byte, 12), box, cat(k.repr, sel)), initPlain), "Init opens")

	data := func(dir string, tkey, key []byte, n uint64, inner []byte, pad int) O {
		tag := hmacSHA(tkey, u64(n))[:8]
		pt := cat(u16(uint16(len(inner))), inner, make([]byte, pad))
		ct := chachaSeal(key, counterNonce(n), pt, tag)
		return O{{"dir", dir}, {"n", n}, {"inner", hx(inner)}, {"pad_len", pad}, {"tag", hx(tag)},
			{"nonce", hx(counterNonce(n))}, {"plaintext", hx(pt)}, {"datagram", hx(cat(tag, ct))}}
	}
	return O{
		{"description", "noise3 Init datagram and data packets for fixed keys. The inner datagram is a synthetic byte pattern standing in for a QUIC Initial."},
		{"spec", "wire.md §2.3–§2.6"},
		{"seeds", sl.o},
		{"mask_sk", hx(maskSK)},
		{"mask_pub", hx(maskPub)},
		{"ephemeral", O{{"seed_base", hx(base)}, {"attempt", k.attempt}, {"sk", hx(k.sk)}, {"tweak", fmt.Sprintf("%02x", tweak)},
			{"dirty_pub", hx(k.dirty)}, {"repr", hx(k.repr)}}},
		{"k_sel", hx(kSel)},
		{"sel", hx(sel)},
		{"z", hx(z)},
		{"hkdf_info", hx(cat([]byte("echofog/noise3/keys"), maskPub, k.dirty))},
		{"okm", O{{"k_init", hx(kInit)}, {"k_i2r", hx(kI2R)}, {"k_r2i", hx(kR2I)}, {"t_i2r", hx(tI2R)}, {"t_r2i", hx(tR2I)}}},
		{"init", O{
			{"ts", fixedTime},
			{"inner", "00 01 02 … (1200 bytes, byte i = i mod 256)"},
			{"inner_len", len(inner)},
			{"pad_len", padLen},
			{"total_len", total},
			{"init_plain_sha256", hx(sha(initPlain))},
			{"datagram", hx(initDgram)},
		}},
		{"data", []any{
			data("i2r", tI2R, kI2R, 0, pattern(16, 0x10), 5),
			data("r2i", tR2I, kR2I, 0, pattern(24, 0x30), 0),
			data("i2r", tI2R, kI2R, 1, nil, 12),
		}},
	}, maskPub
}

func genReality() (any, []byte) {
	var sl seedLog
	maskSK := sl.get("reality2/mask_sk")
	maskPub := x25519Pub(maskSK)
	c := sl.get("reality2/client_x25519")
	C := x25519Pub(c)
	z := x25519(c, maskPub)
	check(bytes.Equal(z, x25519(maskSK, C)), "server derives z")
	kTok := hkdfSHA(z, nil, cat([]byte("echofog/reality2/token-key"), maskPub, C), 32)

	// Synthetic ClientHello handshake message: type 1, 3-byte length, legacy_version 0x0303,
	// random (32 bytes, zeroed: the token field), then a fixed pattern standing in for the rest.
	rest := pattern(474, 0x40)
	body := cat([]byte{0x03, 0x03}, make([]byte, 32), rest)
	ch0 := cat([]byte{0x01, 0x00, byte(len(body) >> 8), byte(len(body))}, body)
	aad := sha([]byte("echofog/reality2/hello"), ch0)
	pt := cat([]byte{0x01}, u32(uint32(fixedTime)), make([]byte, 11))
	blk, err := aes.NewCipher(kTok)
	must(err)
	gcm, err := cipher.NewGCM(blk)
	must(err)
	token := gcm.Seal(nil, make([]byte, 12), pt, aad)
	check(len(token) == 32, "token is 32 bytes")
	ch := append([]byte(nil), ch0...)
	copy(ch[6:38], token)
	return O{
		{"description", "REALITY v2 token for a fixed masking key and client X25519 key share. CH0 is a synthetic ClientHello (type, length, version, zeroed random, then a byte pattern); real ClientHellos are Chrome-shaped."},
		{"spec", "wire.md §3.2"},
		{"seeds", sl.o},
		{"mask_sk", hx(maskSK)},
		{"mask_pub", hx(maskPub)},
		{"client_sk_c", hx(c)},
		{"client_share_C", hx(C)},
		{"z", hx(z)},
		{"hkdf_info", hx(cat([]byte("echofog/reality2/token-key"), maskPub, C))},
		{"k_tok", hx(kTok)},
		{"ch0", hx(ch0)},
		{"aad", hx(aad)},
		{"ts", fixedTime},
		{"plaintext", hx(pt)},
		{"token", hx(token)},
		{"client_hello_with_token", hx(ch)},
		{"placement", "QUIC: ClientHello.random (bytes 6..37 of the handshake message, as here); TCP and ws2: legacy_session_id"},
	}, maskPub
}

func genSignedObject() any {
	// RFC 8032 §7.1 TEST 1 key, so the Ed25519 layer can be checked against the RFC.
	rfcSeed := unhex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	s := newSigner(rfcSeed)
	check(hx(s.pub) == "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "RFC 8032 TEST 1 public key")
	check(hx(ed25519.Sign(s.priv, nil)) == "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b", "RFC 8032 TEST 1 signature")

	policy := (&pb{}).s(1, "proto").u(2, 200).u(3, 200).b
	art := (&pb{}).s(1, "windows-amd64").s(2, "EchoFog-0.3.0-windows-amd64.zip").by(3, sha([]byte("artifact bytes"))).u(4, 52428800).s(5, "https://example.org/releases/0.3.0/EchoFog-0.3.0-windows-amd64.zip").b
	body := (&pb{}).s(1, "0.3.0").u(2, fixedTime).by(3, policy).by(4, art).s(6, "https://example.org/releases/0.3.0").b
	checkCanonicalTop(body, map[int]bool{3: true, 4: true, 5: true})
	env, signed, sig := envelope("echofog/release/2", s, 30, fixedTime+30*86400, body)
	label, gotBody := verifyEnvelope(env)
	check(label == "echofog/release/2" && bytes.Equal(gotBody, body), "envelope parses")

	// A minimal object with an empty body, to show the smallest envelope.
	envEmpty, _, _ := envelope("echofog/devices/2", s, 1, fixedTime+86400, nil)
	verifyEnvelope(envEmpty)

	return O{
		{"description", "Signed object envelope (fmt 0x02) with the RFC 8032 TEST 1 Ed25519 key. The signature covers every byte before it."},
		{"spec", "objects.md §1–§2, §9"},
		{"signer_seed", hx(rfcSeed)},
		{"signer_pub", hx(s.pub)},
		{"signer_akey", hx(s.akey())},
		{"signer_text", "ed25519:" + b64(s.pub)},
		{"node_id", hx(s.nodeID())},
		{"objects", []any{
			O{
				{"label", "echofog/release/2"},
				{"seq", 30},
				{"not_after", fixedTime + 30*86400},
				{"body_fields", "Release{version=\"0.3.0\", released_at=ts, policies=[{component=\"proto\", min_supported=200, recommended=200}], artifacts=[{windows-amd64 …}], notes_url}"},
				{"body", hx(body)},
				{"signed_bytes", hx(signed)},
				{"sig", hx(sig)},
				{"envelope", hx(env)},
				{"object_id", hx(sha(env))},
			},
			O{
				{"label", "echofog/devices/2"},
				{"seq", 1},
				{"not_after", fixedTime + 86400},
				{"body", ""},
				{"envelope", hx(envEmpty)},
			},
		}},
	}
}

func genCard(maskPub3, maskPubR []byte) (any, []byte, signer, []byte) {
	var sl seedLog
	s := newSigner(sl.get("card/identity_seed"))
	hsSK := sl.get("card/hs_sk")
	hsPub := x25519Pub(hsSK)
	hsAkey := cat([]byte{0x02}, hsPub)

	shape := func(id uint64) []byte { return (&pb{}).u(1, id).b }
	params := func(sni string, shapeID uint64) []byte { return (&pb{}).s(1, sni).by(2, shape(shapeID)).b }
	addr := func(suite uint64, host string, port uint64, url string, mask uint64, prm []byte, flags uint64) []byte {
		return (&pb{}).u(1, suite).s(2, host).u(3, port).s(4, url).u(5, mask).by(6, prm).u(7, flags).b
	}
	addrs := [][]byte{
		addr(1, "192.0.2.10", 443, "", 1, params("", 1), 0),
		addr(2, "192.0.2.10", 443, "", 2, params("www.example.org", 1), 1),
		addr(3, "198.51.100.7", 443, "", 2, params("www.example.org", 2), 0),
		addr(4, "", 0, "wss://cdn.example.org/ef/3c9a7d", 2, params("inner.example.org", 2), 0),
	}
	notAfter := fixedTime + 6*86400
	mask := func(pub []byte) []byte { return (&pb{}).by(1, pub).u(2, notAfter+86400).b }
	svc := func(id uint64) []byte { return (&pb{}).u(1, id).packed(2, 2).b }
	exit := (&pb{}).u(1, 500).u(2, 10240).u(3, 100000).s(4, "NL").u(5, 0b1011).packed(7, 1, 2).b

	p := &pb{}
	p.u(1, 200).by(2, hsAkey).packed(3, 1, 2)
	for _, a := range addrs {
		p.by(4, a)
	}
	p.by(5, mask(maskPub3)).by(5, mask(maskPubR))
	p.u(6, 1).u(7, 0b100110) // server; relay | dht-server | exit
	for _, id := range []uint64{1, 2, 5, 6} {
		p.by(8, svc(id))
	}
	p.packed(9, 1, 3, 4, 5, 6).by(10, exit).s(11, "https://shop.example.org/echofog").s(12, "0.3.0")
	body := p.b
	checkCanonicalTop(body, map[int]bool{4: true, 5: true, 8: true, 11: true})
	env, signed, sig := envelope("echofog/card/2", s, fixedTime, notAfter, body)
	verifyEnvelope(env)
	check(len(env) <= 4096, "card size")

	return O{
		{"description", "A server node card: noise3, reality2-quic, reality2-tcp and ws2 addresses, two masking keys (the noise3 and reality2 vector keys), services dht/pex/relay/exit, an exit offer and one shop link."},
		{"spec", "objects.md §3–§4"},
		{"seeds", sl.o},
		{"identity_seed", hx(s.seed)},
		{"identity_pub", hx(s.pub)},
		{"identity_text", "ed25519:" + b64(s.pub)},
		{"node_id", hx(s.nodeID())},
		{"hs_sk", hx(hsSK)},
		{"hs_akey", hx(hsAkey)},
		{"fields", O{
			{"proto_ver", 200},
			{"l1_suites", []int{1, 2}},
			{"addrs", []string{
				"noise3 192.0.2.10:443 mask 1 shape 1",
				"reality2-quic 192.0.2.10:443 mask 2 sni www.example.org shape 1, bootstrap",
				"reality2-tcp 198.51.100.7:443 mask 2 sni www.example.org shape 2",
				"ws2 wss://cdn.example.org/ef/3c9a7d mask 2 sni inner.example.org shape 2",
			}},
			{"mask_keys", []string{hx(maskPub3), hx(maskPubR)}},
			{"mask_not_after", notAfter + 86400},
			{"role", 1},
			{"flags", "0x26 (relay, dht-server, exit)"},
			{"services", "dht/2, pex/2, mailbox/2, exit/2 (ids 1, 2, 5, 6 — see note)"},
			{"features", []int{1, 3, 4, 5, 6}},
			{"exit", "price 500 micro-USDT/MiB, free 10240 KiB, 100000 kbit/s, NL, policy 0x0b, assets [1,2]"},
			{"shop", []string{"https://shop.example.org/echofog"}},
			{"release", "0.3.0"},
		}},
		{"note", "service ids 1, 2, 5, 6 are dht, pex, relay, exit (registries.md §3)"},
		{"seq", fixedTime},
		{"not_after", notAfter},
		{"body", hx(body)},
		{"signed_bytes_sha256", hx(sha(signed))},
		{"sig", hx(sig)},
		{"envelope", hx(env)},
		{"envelope_len", len(env)},
		{"object_id", hx(sha(env))},
	}, env, s, hsPub
}

func genLink(cardEnv []byte) any {
	var sl seedLog
	secret := sl.get("link/access_secret")[:16]
	raw := cat([]byte{0x00}, cardEnv)
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	must(err)
	_, err = w.Write(cardEnv)
	must(err)
	must(w.Close())
	comp := cat([]byte{0x01}, z.Bytes())
	inflated, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(comp[1:])), 4097))
	must(err)
	check(bytes.Equal(inflated, cardEnv), "inflate round trip")
	best := raw
	if len(comp) < len(raw) {
		best = comp
	}
	contact := "echofog://join?v=2&card=" + b64(best)
	invite := contact + "&s=" + b64(secret)
	return O{
		{"description", "Links v2. The DEFLATE bytes are informative (encoders may differ); decoding is normative. The card is the node-card.json envelope; the secret is shown with an invitation only for format (a real invitation card is invite-scoped)."},
		{"spec", "objects.md §10"},
		{"seeds", sl.o},
		{"card_envelope_sha256", hx(sha(cardEnv))},
		{"method0_len", len(raw)},
		{"method1_len", len(comp)},
		{"method1_data", hx(comp)},
		{"chosen_method", int(best[0])},
		{"contact_link", contact},
		{"access_secret", hx(secret)},
		{"secret64", b64(secret)},
		{"invitation_link", invite},
		{"web_form", "https://example.org/join/#" + strings.TrimPrefix(invite, "echofog://")},
	}
}

func genHandshake(cardEnv, hsPub []byte) any {
	var sl seedLog
	names := []string{
		"EchoFog/2/Noise_XKhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s",
		"EchoFog/2/Noise_XXhfs_25519+MLKEM768_ChaChaPoly_BLAKE2s",
	}
	var init []any
	for i, n := range names {
		h := noiseInit(n)
		init = append(init, O{{"suite_id", i + 1}, {"protocol_name", n}, {"name_len", len(n)}, {"h0", hx(h)}, {"ck0", hx(h)}})
	}
	cb := sl.get("hs/cb")
	hello := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0})
	prologue := cat([]byte("echofog/hs2/prologue"), cb, []byte{byte(len(hello))}, hello)
	h := noiseInit(names[0])
	hP := b2s(h, prologue)
	hRS := b2s(hP, hsPub)

	// Payloads.
	secret := sl.get("link/access_secret")[:16]
	h1 := sl.get("hs/h1")
	passID := hmacSHA(secret, []byte("echofog/hs2/pass-id"))[:8]
	proof := hmacSHA(secret, []byte("echofog/hs2/access-proof"), h1)
	access := (&pb{}).by(1, passID).by(2, proof).b
	svcOffer := func(id uint64) []byte { return (&pb{}).u(1, id).packed(2, 2).b }
	pi := &pb{}
	pi.u(1, 200).packed(2, 1, 3, 4, 5, 0x2a2a)
	for _, id := range []uint64{1, 2, 3, 4, 5, 6, 7, 8} {
		pi.by(3, svcOffer(id))
	}
	pi.by(4, access).u(5, fixedTime-86400).u(6, fixedTime)
	hsInit := pi.b
	checkCanonicalTop(hsInit, map[int]bool{3: true})

	choice := func(id uint64) []byte { return (&pb{}).u(1, id).u(2, 2).b }
	pr := &pb{}
	pr.u(1, 200).packed(2, 1, 3, 4, 5)
	for _, id := range []uint64{1, 2, 3} {
		pr.by(3, choice(id))
	}
	pr.by(8, cat([]byte{4}, []byte{192, 0, 2, 33}, u16(51234))).u(9, fixedTime)
	hsResp := pr.b
	checkCanonicalTop(hsResp, map[int]bool{3: true})
	hsFinish := (&pb{}).by(1, cardEnv).b

	// Puzzle.
	rSecret := sl.get("hs/cookie_secret")
	minute := fixedTime / 60
	cookie := hmacSHA(rSecret, []byte("echofog/hs2/cookie"), cb, u64(minute))[:16]
	var nonce uint64
	var ph []byte
	for nonce = 0; ; nonce++ {
		ph = sha([]byte("echofog/hs2/puzzle"), cookie, cb, u64(nonce))
		if leadingZeros(ph) >= 12 {
			break
		}
	}

	// Inputs for the deferred transcripts (step 2). Only stdlib values derived from them are given.
	ie := sl.get("hs/xk/initiator_e")
	is := sl.get("hs/xk/initiator_s")
	re := sl.get("hs/xk/responder_e")
	kemSeed := cat(sl.get("hs/xk/initiator_e1_d"), sl.get("hs/xk/initiator_e1_z"))
	m := sl.get("hs/xk/encaps_m")
	dk, err := mlkem.NewDecapsulationKey768(kemSeed)
	must(err)
	ek := dk.EncapsulationKey()
	ss, ct, err := mlkemtest.Encapsulate768(ek, m)
	must(err)
	ss2, err := dk.Decapsulate(ct)
	must(err)
	check(bytes.Equal(ss, ss2), "ML-KEM round trip")

	return O{
		{"description", "Deterministic pieces of the L1 handshake. Full XKhfs/XXhfs message transcripts are deferred (Decision D15)."},
		{"spec", "handshake.md"},
		{"seeds", sl.o},
		{"initialize_symmetric", init},
		{"prologue_example", O{
			{"cb", hx(cb)},
			{"hs_hello", hx(hello)},
			{"prologue", hx(prologue)},
			{"h_after_prologue_xk", hx(hP)},
			{"responder_static_rs", hx(hsPub)},
			{"h_after_premessage_rs_xk", hx(hRS)},
		}},
		{"access", O{
			{"secret", hx(secret)},
			{"h1", hx(h1)},
			{"pass_id", hx(passID)},
			{"proof", hx(proof)},
		}},
		{"payloads", O{
			{"HsInit", O{{"fields", "proto_ver 200, features [1,3,4,5,0x2a2a GREASE], services 1..8 version 2, access{pass_id, proof}, known_card_seq ts-86400, time ts"}, {"encoded", hx(hsInit)}}},
			{"HsResp", O{{"fields", "proto_ver 200, features [1,3,4,5], services [(1,2),(2,2),(3,2)], observed 192.0.2.33:51234, time ts"}, {"encoded", hx(hsResp)}}},
			{"HsFinish", O{{"fields", "card = node-card.json envelope (format illustration)"}, {"encoded_sha256", hx(sha(hsFinish))}, {"encoded_len", len(hsFinish)}}},
		}},
		{"puzzle", O{
			{"cookie_secret_R", hx(rSecret)},
			{"minute", minute},
			{"cookie", hx(cookie)},
			{"difficulty", 12},
			{"nonce", nonce},
			{"hash", hx(ph)},
		}},
		{"deferred_transcripts", O{
			{"status", "to be produced with the implementation in step 2"},
			{"inputs_xk", O{
				{"prologue", "prologue_example.prologue"},
				{"responder_static", "node-card.json hs_sk"},
				{"initiator_e_sk", hx(ie)},
				{"initiator_e_pub", hx(x25519Pub(ie))},
				{"initiator_s_sk", hx(is)},
				{"initiator_s_pub", hx(x25519Pub(is))},
				{"responder_e_sk", hx(re)},
				{"responder_e_pub", hx(x25519Pub(re))},
				{"initiator_e1_seed_dz", hx(kemSeed)},
				{"initiator_e1_ek_sha256", hx(sha(ek.Bytes()))},
				{"encaps_randomness_m", hx(m)},
				{"ekem1_ct_sha256", hx(sha(ct))},
				{"ekem1_ss", hx(ss)},
				{"payloads", "P1 = payloads.HsInit, P2 = payloads.HsResp, P3 = payloads.HsFinish"},
			}},
			{"note", "ML-KEM values are from Go crypto/mlkem (NewDecapsulationKey768) and crypto/mlkem/mlkemtest (Encapsulate768)."},
		}},
	}
}

func leadingZeros(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(x)
	}
	return n
}

func genKDF(card signer, hsPub []byte) any {
	var sl seedLog
	// Session (handshake.md §9, wire.md §8).
	ck := sl.get("session/ck")
	h := sl.get("session/h")
	kI2R, kR2I := noiseHKDF(ck, nil)
	sess, _ := noiseHKDF(ck, []byte("echofog/hs2/session-secret"))
	sid := hkdfSHA(sess, h, []byte("echofog/session2/id"), 16)
	kPath := hkdfSHA(sess, h, []byte("echofog/session2/path-bind"), 32)
	cbNew := sl.get("session/cb_new")
	mac := hmacSHA(kPath, []byte("echofog/session2/path-mac"), cbNew, sid, u64(2), []byte{0})
	pathBind := frame(0x10, 0, cat(sid, qv(2), mac))

	recPT := frame(0x18, 0, cat(qv(4), []byte{0}, []byte("hello over a relay")))
	sealed := cat(u64(0), chachaSeal(kI2R, counterNonce(0), recPT, cat(sid, u64(0))))

	// chat/2 sealed sender key.
	eph := sl.get("chat/eph")
	ikSK := sl.get("chat/recipient_ik")
	ik := x25519Pub(ikSK)
	ephPub := x25519Pub(eph)
	kSeal := hkdfSHA(x25519(eph, ik), nil, cat([]byte("echofog/chat2/seal"), ephPub, ik), 32)

	// mailbox/2 forward sealing to M's handshake key (the node-card.json hs key).
	fe := sl.get("mailbox/forward_eph")
	fePub := x25519Pub(fe)
	kFwd := hkdfSHA(x25519(fe, hsPub), nil, cat([]byte("echofog/mailbox2/forward"), fePub, hsPub), 32)

	// call/2 keys.
	room := sl.get("call/room_secret")
	epoch0 := hkdfSHA(room, nil, cat([]byte("echofog/call2/epoch"), u32(0)), 32)
	kSender := hkdfSHA(epoch0, nil, cat([]byte("echofog/call2/sender-key"), card.nodeID()), 32)

	// DHT keys.
	label := "echofog/card/2"
	dhtCard := sha([]byte("echofog/dht2/key"), []byte{byte(len(label))}, []byte(label), card.nodeID())
	name := "alice.ton"
	dhtName := sha([]byte("echofog/dht2/name-key"), []byte{byte(len(name))}, []byte(name))

	return O{
		{"description", "Key derivations of L2 and the services from fixed inputs. HKDF is HKDF-SHA256 unless marked noise (HMAC-BLAKE2s, handshake.md §2)."},
		{"spec", "handshake.md §9, wire.md §8, services.md, dht.md §2"},
		{"seeds", sl.o},
		{"session", O{
			{"ck", hx(ck)},
			{"h", hx(h)},
			{"split_k_i2r (noise HKDF(ck, empty).o1)", hx(kI2R)},
			{"split_k_r2i (noise HKDF(ck, empty).o2)", hx(kR2I)},
			{"session_secret (noise HKDF(ck, label).o1)", hx(sess)},
			{"sid", hx(sid)},
			{"k_path", hx(kPath)},
			{"cb_new", hx(cbNew)},
			{"path_id", 2},
			{"role", 0},
			{"path_mac", hx(mac)},
			{"PATH_BIND_frame", hx(pathBind)},
		}},
		{"sealed_record", O{
			{"key", "k_i2r"},
			{"n", 0},
			{"plaintext", hx(recPT)},
			{"sealed", hx(sealed)},
			{"record", hx(cat(u16(uint16(len(sealed))), sealed))},
		}},
		{"chat2_seal", O{{"eph_sk", hx(eph)}, {"eph_pub", hx(ephPub)}, {"recipient_ik_sk", hx(ikSK)}, {"recipient_ik", hx(ik)}, {"k", hx(kSeal)}}},
		{"mailbox2_forward", O{{"eph_sk", hx(fe)}, {"eph_pub", hx(fePub)}, {"hs_M", hx(hsPub)}, {"k", hx(kFwd)}}},
		{"call2", O{{"room_secret", hx(room)}, {"epoch0_secret", hx(epoch0)}, {"sender_node_id", hx(card.nodeID())}, {"k_sender", hx(kSender)}}},
		{"dht2", O{
			{"card_key", O{{"label", label}, {"signer_node_id", hx(card.nodeID())}, {"key", hx(dhtCard)}}},
			{"name_key", O{{"name", name}, {"key", hx(dhtName)}}},
		}},
	}
}
