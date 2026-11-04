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
	"golang.org/x/crypto/chacha20"
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
	cross := flag.String("crosscheck", "", "recompute the transcripts of another implementation's handshake-transcript.json and exit")
	flag.Parse()
	if *cross != "" {
		crossCheck(*cross)
		return
	}
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
	card, co := genCard(maskPub3, maskPubR)
	writeFile(*out, "node-card.json", card)
	writeFile(*out, "link.json", genLink(co))
	hsv, tr := genHandshake(co)
	writeFile(*out, "handshake.json", hsv)
	writeFile(*out, "handshake-transcript.json", tr)
	writeFile(*out, "kdf.json", genKDF(co))
	writeFile(*out, "negative.json", genNegative(co))
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
	for _, e := range []string{"4025", "80000025", "c000000000000025", "4001", "800000ff"} {
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
	hello := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0x02})
	add("HS_HELLO", 0x02, 0, hello, "offered [0x0001, 0x0002], chosen 0x0001, flags 0x02 (responder ID bound), no puzzle")
	cookie := pattern(16, 0xa0)
	helloP := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0x03}, cookie, u64(4242))
	add("HS_HELLO (puzzle)", 0x02, 0, helloP, "flags 0x03, cookie a0..af (pattern, see handshake.json puzzle for a real cookie), nonce 4242")
	add("HS_RETRY", 0x04, 0, cat(cookie, []byte{16}), "difficulty 16 (pattern cookie)")
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
	// A key without a representative.
	var noRepr O
	nrBase := sl.get("elligator2/no_representative")
	for i := 0; ; i++ {
		sk := sha(nrBase, u32(uint32(i)))
		dirty, _ := dirtyPub(sk)
		if ellReverse(dirty, 0) == nil {
			noRepr = O{{"seed_base", hx(nrBase)}, {"attempt", i}, {"sk", hx(sk)}, {"dirty_pub", hx(dirty)},
				{"reason", "-2u(u+A) is not a square: the encoder must pick a new key"}}
			break
		}
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
		{"no_representative", noRepr},
		{"rfc9380_crosscheck", O{{"note", "the generator checks its forward map on full field elements against RFC 9380 Appendix J.4.2 (u[0] → Q.x); values big-endian as in the RFC"}, {"cases", rfc}}},
		{"ecdh_check", O{
			{"peer_sk", hx(peer)},
			{"x25519_peer_sk_sample0_dirty_pub", hx(zDirty)},
			{"x25519_peer_sk_sample0_clean_pub", hx(zClean)},
			{"x25519_sample0_sk_peer_pub", hx(zInit)},
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

	z := x25519(k.sk, maskPub)
	uResp := ellForward(k.repr)
	check(bytes.Equal(x25519(maskSK, uResp), z), "responder derives the same z")
	info := cat([]byte("echofog/noise3/keys"), maskPub, k.dirty)
	okm := hkdfSHA(z, nil, info, 192)
	kSel, kInit, kI2R, kR2I, hpI2R, hpR2I := okm[0:32], okm[32:64], okm[64:96], okm[96:128], okm[128:160], okm[160:192]
	sel := hmacSHA(kSel, k.repr)[:16]

	inner := pattern(1200, 0) // synthetic stand-in for a QUIC Initial
	total := 1300
	padLen := total - 71 - len(inner)
	initPlain := cat(u32(uint32(fixedTime)), []byte{0}, u16(uint16(len(inner))), inner, make([]byte, padLen))
	box := chachaSeal(kInit, make([]byte, 12), initPlain, cat(k.repr, sel))
	initDgram := cat(k.repr, sel, box)
	check(len(initDgram) == total, "Init length")
	check(bytes.Equal(chachaOpen(kInit, make([]byte, 12), box, cat(k.repr, sel)), initPlain), "Init opens")

	data := func(dir string, hp, key []byte, n uint64, inner []byte, pad int) O {
		pn := u32(uint32(n))
		pt := cat(u16(uint16(len(inner))), inner, make([]byte, pad))
		ct := chachaSeal(key, counterNonce(n), pt, pn)
		mask := hpMask(hp, ct[:16])
		masked := make([]byte, 4)
		for i := range masked {
			masked[i] = pn[i] ^ mask[i]
		}
		dg := cat(masked, ct)
		// Receiver side: unmask and decode against an expected value.
		m2 := hpMask(hp, dg[4:20])
		var trunc uint32
		for i := 0; i < 4; i++ {
			trunc = trunc<<8 | uint32(dg[i]^m2[i])
		}
		check(decodePN(n, trunc) == n, "packet number decodes")
		return O{{"dir", dir}, {"n", n}, {"inner", hx(inner)}, {"pad_len", pad}, {"pn_plain", hx(pn)},
			{"nonce", hx(counterNonce(n))}, {"plaintext", hx(pt)}, {"sample", hx(ct[:16])}, {"mask", hx(mask[:4])},
			{"datagram", hx(dg)}}
	}
	// Recovery after a long loss run: the receiver expects 1 but gets n = 5000.
	check(decodePN(1, 5000) == 5000, "decode after 4999 lost packets")
	return O{
		{"description", "noise3 Init datagram and data packets for fixed keys. The selector is keyed on the ECDH secret. Data packets use QUIC-style packet-number protection (ChaCha20 mask from a 16-byte ciphertext sample). The inner datagram is a synthetic byte pattern standing in for a QUIC Initial."},
		{"spec", "wire.md §2.3–§2.6"},
		{"seeds", sl.o},
		{"mask_sk", hx(maskSK)},
		{"mask_pub", hx(maskPub)},
		{"ephemeral", O{{"seed_base", hx(base)}, {"attempt", k.attempt}, {"sk", hx(k.sk)}, {"tweak", fmt.Sprintf("%02x", tweak)},
			{"dirty_pub", hx(k.dirty)}, {"repr", hx(k.repr)}}},
		{"z", hx(z)},
		{"hkdf_info", hx(info)},
		{"okm", O{{"k_sel", hx(kSel)}, {"k_init", hx(kInit)}, {"k_i2r", hx(kI2R)}, {"k_r2i", hx(kR2I)}, {"hp_i2r", hx(hpI2R)}, {"hp_r2i", hx(hpR2I)}}},
		{"sel", hx(sel)},
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
			data("i2r", hpI2R, kI2R, 0, pattern(16, 0x10), 5),
			data("r2i", hpR2I, kR2I, 0, pattern(24, 0x30), 0),
			data("i2r", hpI2R, kI2R, 1, nil, 12),
			data("i2r", hpI2R, kI2R, 5000, pattern(8, 0x50), 0),
		}},
		{"pn_decoding", "RFC 9000 Appendix A.3 with 32-bit truncation: the last entry decodes correctly at a receiver whose highest received number is 1 (4998 packets lost)"},
	}, maskPub
}

// hpMask is RFC 9001 §5.4.4: ChaCha20 with counter = LE32(sample[0:4]), nonce = sample[4:16], over 5 zero bytes.
func hpMask(key, sample []byte) []byte {
	c, err := chacha20.NewUnauthenticatedCipher(key, sample[4:16])
	must(err)
	c.SetCounter(binary.LittleEndian.Uint32(sample[0:4]))
	out := make([]byte, 5)
	c.XORKeyStream(out, out)
	return out
}

// decodePN is RFC 9000 Appendix A.3 for a 32-bit truncated packet number; expectedMinus1 is the
// highest number received so far.
func decodePN(largest uint64, truncated uint32) uint64 {
	expected := largest + 1
	const win = uint64(1) << 32
	const hwin = win / 2
	const mask = win - 1
	candidate := (expected &^ mask) | uint64(truncated)
	if candidate+hwin <= expected && candidate < (1<<62)-win {
		return candidate + win
	}
	if candidate > expected+hwin && candidate >= win {
		return candidate - win
	}
	return candidate
}

func genReality() (any, []byte) {
	var sl seedLog
	maskSK := sl.get("reality2/mask_sk")
	maskPub := x25519Pub(maskSK)
	c := sl.get("reality2/client_x25519")
	C := x25519Pub(c)
	z := x25519(c, maskPub)
	check(bytes.Equal(z, x25519(maskSK, C)), "server derives z")
	addr := "192.0.2.10:443"
	info := cat([]byte("echofog/reality2/token-key"), maskPub, C, []byte{byte(len(addr))}, []byte(addr))
	kTok := hkdfSHA(z, nil, info, 32)

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
	// The same hello at another address of the node gives another key, so the token fails there.
	other := hkdfSHA(z, nil, cat([]byte("echofog/reality2/token-key"), maskPub, C, []byte{byte(len("198.51.100.7:443"))}, []byte("198.51.100.7:443")), 32)
	check(!bytes.Equal(other, kTok), "address binding")
	// C taken from an X25519MLKEM768 share (ML-KEM part ‖ X25519 part) when there is no X25519 entry.
	hybrid := cat(pattern(1184, 0x11), C)
	return O{
		{"description", "REALITY v2 token for a fixed masking key, client X25519 key share and dialed address. CH0 is a synthetic ClientHello (type, length, version, zeroed random, then a byte pattern); real ClientHellos are Chrome-shaped."},
		{"spec", "wire.md §3.2"},
		{"seeds", sl.o},
		{"mask_sk", hx(maskSK)},
		{"mask_pub", hx(maskPub)},
		{"client_sk_c", hx(c)},
		{"client_share_C", hx(C)},
		{"dialed_address", addr},
		{"z", hx(z)},
		{"hkdf_info", hx(info)},
		{"k_tok", hx(kTok)},
		{"k_tok_at_198.51.100.7:443", hx(other)},
		{"ch0", hx(ch0)},
		{"aad", hx(aad)},
		{"ts", fixedTime},
		{"plaintext", hx(pt)},
		{"token", hx(token)},
		{"client_hello_with_token", hx(ch)},
		{"placement", "QUIC: ClientHello.random (bytes 6..37 of the handshake message, as here); TCP and ws2: legacy_session_id"},
		{"hybrid_share_example", O{{"note", "X25519MLKEM768 key_exchange = ML-KEM-768 encapsulation key (1184) ‖ X25519 (32); C is the last 32 bytes"}, {"share_sha256", hx(sha(hybrid))}, {"C", hx(hybrid[1184:])}}},
	}, maskPub
}

func genSignedObject() any {
	// RFC 8032 §7.1 TEST 1 key, so the Ed25519 layer can be checked against the RFC.
	rfcSeed := unhex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	s := newSigner(rfcSeed)
	check(hx(s.pub) == "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "RFC 8032 TEST 1 public key")
	check(hx(ed25519.Sign(s.priv, nil)) == "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b", "RFC 8032 TEST 1 signature")
	var sl seedLog
	s2 := newSigner(sl.get("release/second_key"))

	policy := (&pb{}).s(1, "proto").u(2, 200).u(3, 200).b
	art := (&pb{}).s(1, "windows-amd64").s(2, "EchoFog-0.3.0-windows-amd64.zip").by(3, sha([]byte("artifact bytes"))).u(4, 52428800).s(5, "https://example.org/releases/0.3.0/EchoFog-0.3.0-windows-amd64.zip").b
	manifest := (&pb{}).s(1, "0.3.0").u(2, fixedTime).by(3, policy).by(4, art).s(6, "https://example.org/releases/0.3.0").b
	checkCanonicalTop(manifest, map[int]bool{3: true, 4: true, 5: true, 7: true})
	cosigInput := cat([]byte("echofog/release-cosign/2"), sha(manifest))
	cosig := ed25519.Sign(s2.priv, cosigInput)
	cs := (&pb{}).by(1, s2.akey()).by(2, cosig).b
	body := (&pb{}).by(1, manifest).by(2, cs).b
	env, signed, sig := envelope("echofog/release/2", s, fixedTime, fixedTime+30*86400, body)
	label, gotBody := verifyEnvelope(env)
	check(label == "echofog/release/2" && bytes.Equal(gotBody, body), "envelope parses")
	check(ed25519.Verify(s2.pub, cosigInput, cosig), "cosignature")

	// A minimal object with an empty body, to show the smallest envelope.
	envEmpty, _, _ := envelope("echofog/devices/2", s, 1, fixedTime+86400, nil)
	verifyEnvelope(envEmpty)

	return O{
		{"description", "Signed object envelope (fmt 0x02) with the RFC 8032 TEST 1 Ed25519 key. The signature covers every byte before it. The release manifest is co-signed by a second key (threshold 2)."},
		{"spec", "objects.md §1–§2, §9"},
		{"seeds", sl.o},
		{"signer_seed", hx(rfcSeed)},
		{"signer_pub", hx(s.pub)},
		{"signer_akey", hx(s.akey())},
		{"signer_text", "ed25519:" + b64(s.pub)},
		{"node_id", hx(s.nodeID())},
		{"objects", []any{
			O{
				{"label", "echofog/release/2"},
				{"seq", fixedTime},
				{"not_after", fixedTime + 30*86400},
				{"manifest_fields", "Release{version=\"0.3.0\", released_at=ts, policies=[{component=\"proto\", min_supported=200, recommended=200}], artifacts=[{windows-amd64 …}], notes_url}"},
				{"manifest", hx(manifest)},
				{"cosigner_akey", hx(s2.akey())},
				{"cosig_input", hx(cosigInput)},
				{"cosig", hx(cosig)},
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

type cardOut struct {
	public, peer, invite []byte
	signer               signer
	account              signer
	hsPub                []byte
	invMaskPub           []byte
	passID               []byte
	secret               []byte
	initCard, throwCard  []byte
	throwHS              []byte
}

func genCard(maskPub3, maskPubR []byte) (any, cardOut) {
	var sl seedLog
	acct := newSigner(sl.get("card/account_seed"))
	s := newSigner(sl.get("card/device_seed"))
	hsSK := sl.get("card/hs_sk")
	hsPub := x25519Pub(hsSK)
	hsAkey := cat([]byte{0x02}, hsPub)
	prevHS := cat([]byte{0x02}, x25519Pub(sl.get("card/prev_hs_sk")))
	invMaskPub := x25519Pub(sl.get("card/invite_mask_sk"))
	secret := sl.get("link/access_secret")[:16]
	passID := hmacSHA(secret, []byte("echofog/hs2/pass-id"))[:8]

	shape := func(id uint64) []byte { return (&pb{}).u(1, id).b }
	params := func(sni string, shapeID uint64) []byte { return (&pb{}).s(1, sni).by(2, shape(shapeID)).b }
	addr := func(suite uint64, host string, port uint64, url string, mask uint64, prm []byte, flags uint64) []byte {
		return (&pb{}).u(1, suite).s(2, host).u(3, port).s(4, url).u(5, mask).by(6, prm).u(7, flags).b
	}
	notAfter := fixedTime + 6*86400
	mask := func(pub []byte) []byte { return (&pb{}).by(1, cat([]byte{0x02}, pub)).u(2, notAfter+86400).b }
	svc := func(id uint64) []byte { return (&pb{}).u(1, id).packed(2, 2).b }
	exit := (&pb{}).u(1, 500).u(2, 10240).u(3, 100000).s(4, "NL").u(5, 0b1011).packed(7, 1, 2).b

	// The account's device list: one device (this one), primary.
	devBody := (&pb{}).by(1, (&pb{}).by(1, s.akey()).u(2, fixedTime).u(3, 1).b).b
	devEnv, _, _ := envelope("echofog/devices/2", acct, fixedTime, fixedTime+30*86400, devBody)
	verifyEnvelope(devEnv)

	build := func(addrs [][]byte, masks [][]byte, scope uint64, inviteID []byte, flags uint64, withPrev bool, l1 []uint64, withExit bool) []byte {
		p := &pb{}
		p.u(1, 200).by(2, hsAkey).packed(3, l1...)
		for _, a := range addrs {
			p.by(4, a)
		}
		for _, m := range masks {
			p.by(5, mask(m))
		}
		p.u(6, 1).u(7, flags)
		for _, id := range []uint64{1, 2, 5, 6} {
			p.by(8, svc(id))
		}
		p.packed(9, 1, 3, 4, 5, 6)
		if withExit {
			p.by(10, exit)
		}
		p.s(11, "https://shop.example.org/echofog").s(12, "0.3.0")
		p.by(13, acct.akey())
		if withPrev {
			p.by(15, (&pb{}).by(1, prevHS).u(2, notAfter).b)
		}
		p.u(16, scope).by(17, inviteID).by(18, devEnv)
		checkCanonicalTop(p.b, map[int]bool{4: true, 5: true, 8: true, 11: true})
		return p.b
	}
	// Public card: camouflaged addresses only (no noise3, no ws2); one masking key.
	pubBody := build([][]byte{
		addr(2, "192.0.2.10", 443, "", 1, params("www.example.org", 1), 1),
		addr(3, "198.51.100.7", 443, "", 1, params("www.example.org", 2), 0),
	}, [][]byte{maskPubR}, 0, nil, 0b100110, false, []uint64{1, 2}, true)
	pubEnv, pubSigned, pubSig := envelope("echofog/card/2", s, fixedTime, notAfter, pubBody)
	verifyEnvelope(pubEnv)
	// Peer card: adds noise3 and ws2, a second masking key and the previous hs key.
	peerBody := build([][]byte{
		addr(1, "192.0.2.10", 443, "", 2, params("", 1), 0),
		addr(2, "192.0.2.10", 443, "", 1, params("www.example.org", 1), 1),
		addr(3, "198.51.100.7", 443, "", 1, params("www.example.org", 2), 0),
		addr(4, "", 0, "wss://cdn.example.org/ef/3c9a7d", 1, params("inner.example.org", 2), 0),
	}, [][]byte{maskPubR, maskPub3}, 1, nil, 0b100110, true, []uint64{1, 2}, true)
	peerEnv, _, _ := envelope("echofog/card/2", s, fixedTime+1, notAfter, peerBody)
	verifyEnvelope(peerEnv)
	// Invitation card: one noise3 address with the invitation's own masking key.
	invBody := build([][]byte{addr(1, "192.0.2.10", 443, "", 1, params("", 1), 0)},
		[][]byte{invMaskPub}, 2, passID, 0b010001, false, []uint64{1}, false)
	invEnv, _, _ := envelope("echofog/card/2", s, fixedTime+2, fixedTime+30*86400, invBody)
	verifyEnvelope(invEnv)
	check(len(pubEnv) <= 4096 && len(peerEnv) <= 4096 && len(invEnv) <= 4096, "card size")

	// The initiator of the handshake transcripts: a user with its own account and device, and a
	// throwaway identity for XX to a bare address, with its own hs key.
	var isl seedLog
	initHS := cat([]byte{0x02}, x25519Pub(sha([]byte(seedPrefix+"hs/xk/initiator_s"))))
	userCard := func(a, d signer, seq uint64, initHS []byte) []byte {
		dl := (&pb{}).by(1, (&pb{}).by(1, d.akey()).u(2, fixedTime).u(3, 1).b).b
		dlEnv, _, _ := envelope("echofog/devices/2", a, fixedTime, fixedTime+30*86400, dl)
		body := (&pb{}).u(1, 200).by(2, initHS).packed(3, 1, 2).
			by(8, (&pb{}).u(1, 3).packed(2, 2).b).by(8, (&pb{}).u(1, 7).packed(2, 2).b).
			packed(9, 1, 3, 4, 5, 6).s(12, "0.3.0").by(13, a.akey()).u(16, 1).by(18, dlEnv).b
		checkCanonicalTop(body, map[int]bool{8: true})
		env, _, _ := envelope("echofog/card/2", d, seq, fixedTime+6*86400, body)
		verifyEnvelope(env)
		return env
	}
	iAcct, iDev := newSigner(isl.get("initiator/account_seed")), newSigner(isl.get("initiator/device_seed"))
	tAcct, tDev := newSigner(isl.get("initiator/throwaway_account_seed")), newSigner(isl.get("initiator/throwaway_device_seed"))
	initCard := userCard(iAcct, iDev, fixedTime, initHS)
	// A throwaway identity has its own hs key too: reusing the real one would link the sessions.
	throwHS := isl.get("initiator/throwaway_hs_sk")
	throwCard := userCard(tAcct, tDev, fixedTime, cat([]byte{0x02}, x25519Pub(throwHS)))

	return O{
		{"description", "One server's cards in three scopes. Public (DHT, PEX): reality2-quic and reality2-tcp only. Peer (inside sessions): adds noise3 and ws2, a second masking key and a previous hs key. Invite: one noise3 address with the invitation's own masking key, invite_id = pass_id, flags unlisted|invite-scoped. Services are ids 1, 2, 5, 6 = dht, pex, relay, exit."},
		{"spec", "objects.md §3–§4, §10.3"},
		{"seeds", sl.o},
		{"account_seed", hx(acct.seed)},
		{"account_pub", hx(acct.pub)},
		{"account_text", "ed25519:" + b64(acct.pub)},
		{"node_id (SHA-256 of the account akey)", hx(acct.nodeID())},
		{"device_seed", hx(s.seed)},
		{"device_pub", hx(s.pub)},
		{"device_id (SHA-256 of the device akey; the card signer)", hx(s.nodeID())},
		{"device_list", O{{"fields", "DeviceList{devices: [{key = device akey, added = ts, flags = 1 primary}]}, signed by the account key"}, {"body", hx(devBody)}, {"envelope", hx(devEnv)}}},
		{"hs_sk", hx(hsSK)},
		{"hs_akey", hx(hsAkey)},
		{"prev_hs_akey", hx(prevHS)},
		{"mask_keys", O{{"reality (reality2.json)", hx(maskPubR)}, {"noise3 (noise3.json)", hx(maskPub3)}, {"invitation", hx(invMaskPub)}}},
		{"public", O{
			{"fields", "proto_ver 200; l1_suites [1,2]; addrs: reality2-quic 192.0.2.10:443 mask 1 sni www.example.org shape 1 bootstrap, reality2-tcp 198.51.100.7:443 mask 1 shape 2; role server; flags 0x26 (relay, dht-server, exit); services dht/2 pex/2 relay/2 exit/2; features [1,3,4,5,6]; exit offer 500 micro-USDT/MiB, 10240 KiB free, 100000 kbit/s, NL, policy 0x0b, assets [1,2]; shop https://shop.example.org/echofog; release 0.3.0; account = account akey; scope 0; devices = the device list envelope"},
			{"seq", fixedTime}, {"not_after", notAfter},
			{"body", hx(pubBody)}, {"signed_bytes_sha256", hx(sha(pubSigned))}, {"sig", hx(pubSig)},
			{"envelope", hx(pubEnv)}, {"envelope_len", len(pubEnv)}, {"object_id", hx(sha(pubEnv))},
		}},
		{"peer", O{
			{"fields", "as public plus noise3 192.0.2.10:443 mask 2 and ws2 wss://cdn.example.org/ef/3c9a7d sni inner.example.org; prev_hs; scope 1"},
			{"seq", fixedTime + 1}, {"envelope", hx(peerEnv)}, {"envelope_len", len(peerEnv)},
		}},
		{"invite", O{
			{"fields", "noise3 192.0.2.10:443 with the invitation masking key; l1_suites [1] (a hidden node never lists XX); no exit offer; flags 0x11 (unlisted, invite-scoped); scope 2; invite_id = pass_id"},
			{"invite_id", hx(passID)}, {"seq", fixedTime + 2}, {"not_after", fixedTime + 30*86400},
			{"envelope", hx(invEnv)}, {"envelope_len", len(invEnv)},
		}},
		{"initiator", O{
			{"fields", "user (role 0), flags 0, scope peer, no addresses, hs_key = x25519(hs/xk/initiator_s), l1_suites [1,2], services chat/2 files/2, own account and device list"},
			{"seeds", isl.o},
			{"node_id", hx(iAcct.nodeID())}, {"device_id", hx(iDev.nodeID())},
			{"envelope", hx(initCard)}, {"envelope_len", len(initCard)},
		}},
		{"initiator_throwaway", O{
			{"fields", "as initiator, under a fresh throwaway account, device and hs key (XX to a bare address, handshake.md §4.2)"},
			{"hs_sk", hx(throwHS)},
			{"node_id", hx(tAcct.nodeID())}, {"device_id", hx(tDev.nodeID())},
			{"envelope", hx(throwCard)}, {"envelope_len", len(throwCard)},
		}},
	}, cardOut{pubEnv, peerEnv, invEnv, s, acct, hsPub, invMaskPub, passID, secret, initCard, throwCard, throwHS}
}

func linkFor(env []byte) (string, []byte, []byte) {
	raw := cat([]byte{0x00}, env)
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	must(err)
	_, err = w.Write(env)
	must(err)
	must(w.Close())
	comp := cat([]byte{0x01}, z.Bytes())
	inflated, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(comp[1:])), 4097))
	must(err)
	check(bytes.Equal(inflated, env), "inflate round trip")
	best := raw
	if len(comp) < len(raw) {
		best = comp
	}
	return "echofog://join?v=2&card=" + b64(best), raw, comp
}

func genLink(c cardOut) any {
	contact, raw, comp := linkFor(c.public)
	inv, _, invComp := linkFor(c.invite)
	invite := inv + "&s=" + b64(c.secret)
	return O{
		{"description", "Links v2. The DEFLATE bytes are informative (encoders may differ); decoding is normative. The contact link carries the public card of node-card.json, the invitation link its invite-scope card and the access secret."},
		{"spec", "objects.md §10"},
		{"contact", O{
			{"card_envelope_sha256", hx(sha(c.public))},
			{"method0_len", len(raw)},
			{"method1_len", len(comp)},
			{"method1_data", hx(comp)},
			{"link", contact},
		}},
		{"invitation", O{
			{"card_envelope_sha256", hx(sha(c.invite))},
			{"method1_len", len(invComp)},
			{"access_secret", hx(c.secret)},
			{"secret64", b64(c.secret)},
			{"pass_id", hx(c.passID)},
			{"link", invite},
			{"web_form", "https://example.org/join/#" + strings.TrimPrefix(invite, "echofog://")},
		}},
	}
}

func genHandshake(c cardOut) (any, any) {
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
	rid := c.signer.nodeID() // the responder's device ID
	prol := func(hello []byte, bound bool) []byte {
		r := make([]byte, 32)
		if bound {
			r = rid
		}
		return cat([]byte("echofog/hs2/prologue"), cb, r, []byte{byte(len(hello))}, hello)
	}
	// XK to a public node: offered [0x0001, 0x0002] (its public card lists both).
	hello := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0x02})
	prologue := prol(hello, true)
	h := noiseInit(names[0])
	hP := b2s(h, prologue)
	hRS := b2s(hP, c.hsPub)
	// XK to a hidden node: its invitation card lists only 0x0001; GREASE 0x0a0a is inserted before the
	// first larger entry, so it goes last.
	helloHidden := cat([]byte{2}, u16(1), u16(0x0a0a), u16(1), []byte{0x02})
	prologueHidden := prol(helloHidden, true)
	// XX to a bare address: no expected device ID.
	helloXX := cat([]byte{1}, u16(2), u16(2), []byte{0x00})
	prologueXX := prol(helloXX, false)

	// Payloads (canonical protobuf, no pad).
	h1 := sl.get("hs/h1")
	proofExample := hmacSHA(c.secret, []byte("echofog/hs2/access-proof"), h1)
	svcOffer := func(id, v uint64) []byte { return (&pb{}).u(1, id).packed(2, v).b }
	hsInit := func(access []byte, knownSeq uint64) []byte {
		pi := &pb{}
		pi.u(1, 200).packed(2, 1, 3, 4, 5, 0x2a2a)
		for _, id := range []uint64{1, 2, 3, 4, 5, 6, 7, 8} {
			pi.by(3, svcOffer(id, 2))
		}
		pi.by(3, svcOffer(0x3a3a, 1)) // GREASE service
		pi.by(4, access).u(5, knownSeq).u(6, fixedTime)
		checkCanonicalTop(pi.b, map[int]bool{3: true})
		return pi.b
	}
	p1Public := hsInit(nil, fixedTime) // knows the public card (seq = ts)
	p1Hidden := func(h1 []byte) []byte {
		proof := hmacSHA(c.secret, []byte("echofog/hs2/access-proof"), h1)
		return hsInit((&pb{}).by(1, c.passID).by(2, proof).b, fixedTime+2) // knows the invitation card
	}
	p1XX := hsInit(nil, 0)
	choice := func(id uint64) []byte { return (&pb{}).u(1, id).u(2, 2).b }
	hsResp := func(card []byte) []byte {
		pr := &pb{}
		pr.u(1, 200).packed(2, 1, 3, 4, 5)
		for _, id := range []uint64{1, 2, 3} {
			pr.by(3, choice(id))
		}
		pr.by(4, card).by(8, cat([]byte{4}, []byte{192, 0, 2, 33}, u16(51234))).u(9, fixedTime)
		checkCanonicalTop(pr.b, map[int]bool{3: true})
		return pr.b
	}
	p2 := hsResp(nil)
	p2XX := hsResp(c.public)
	p3 := (&pb{}).by(1, c.initCard).b
	p3XX := (&pb{}).by(1, c.throwCard).b

	// Puzzle (uses cb above).
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
	retry := frame(0x04, 0, cat(cookie, []byte{12}))
	helloSol := cat([]byte{2}, u16(1), u16(2), u16(1), []byte{0x03}, cookie, u64(nonce))

	// Transcript keys: one set for all three transcripts.
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
	rsSK := sha([]byte(seedPrefix + "card/hs_sk"))
	check(bytes.Equal(x25519Pub(rsSK), c.hsPub), "responder static")

	base := func(name string, xk bool, prologue []byte, prologueSrc string) hsInputs {
		pn := names[1]
		if xk {
			pn = names[0]
		}
		return hsInputs{name: name, protocolName: pn, xk: xk, prologue: prologue,
			is: is, rs: rsSK, ie: ie, re: re, seed: kemSeed, m: m,
			sources: map[string]string{"prologue": prologueSrc}}
	}
	t1 := base("XKhfs", true, prologue, "handshake.json prologue_xk.prologue")
	t1.p1, t1.p2, t1.p3 = p1Public, p2, p3
	t1.sources["p1"] = "handshake.json payloads.HsInit_xk.encoded (no access: a public responder; known_card_seq = the public card's seq)"
	t1.sources["p2"] = "handshake.json payloads.HsResp.encoded (no card: the initiator's copy is current)"
	t1.sources["p3"] = "handshake.json payloads.HsFinish.encoded (card = node-card.json initiator.envelope)"
	t1.checks = []string{"application-valid: every payload and card check of handshake.md §7 passes"}
	t2 := base("XKhfs-access-proof", true, prologueHidden, "handshake.json prologue_xk_hidden.prologue")
	t2.p1Func, t2.p2, t2.p3 = p1Hidden, p2, p3
	t2.sources["p1"] = "payloads.HsInit_xk_hidden: access{pass_id, proof = HMAC-SHA256(link.json invitation.access_secret, \"echofog/hs2/access-proof\" ‖ messages[0].h_before_payload)}, known_card_seq = the invitation card's seq"
	t2.sources["p2"] = "handshake.json payloads.HsResp.encoded"
	t2.sources["p3"] = "handshake.json payloads.HsFinish.encoded"
	t2.checks = []string{"application-valid: a hidden responder holding link.json's access secret accepts P1.access; the invitation card lists only suite 0x0001"}
	t3 := base("XXhfs", false, prologueXX, "handshake.json prologue_xx_bare_address.prologue")
	t3.p1, t3.p2, t3.p3 = p1XX, p2XX, p3XX
	t3.is = c.throwHS
	t3.sources["p1"] = "handshake.json payloads.HsInit_xx.encoded (no access, no known_card_seq)"
	t3.sources["p2"] = "handshake.json payloads.HsResp_xx.encoded (card = node-card.json public.envelope)"
	t3.sources["p3"] = "handshake.json payloads.HsFinish_xx.encoded (card = node-card.json initiator_throwaway.envelope)"
	t3.checks = []string{"same keys as the XK transcripts except the initiator static, which is the throwaway identity's own hs key (node-card.json initiator_throwaway.hs_sk); bare-address prologue"}
	// hs rotation on the hidden node. The transcript reuses the access-proof P1 (handshake.md §12):
	// it names the current invitation card, while a real rotation would have the initiator hold an
	// older card whose hs_key is now prev_hs. The responder tries its current key first, fails to
	// open P1, and succeeds on the second pass with prev_hs, before its access check.
	prevSK := sha([]byte(seedPrefix + "card/prev_hs_sk"))
	t4 := base("XKhfs-rotation-prev-hs", true, prologueHidden, "handshake.json prologue_xk_hidden.prologue")
	t4.rs = prevSK
	t4.p1Func, t4.p2, t4.p3 = p1Hidden, p2, p3
	t4.sources["p1"] = t2.sources["p1"]
	t4.sources["p2"] = t2.sources["p2"]
	t4.sources["p3"] = t2.sources["p3"]
	t4.checks = []string{"responder_s is node-card.json prev_hs (card/prev_hs_sk); handshake-transcript.json rotation.first_pass shows the failed pass with the current hs key"}
	r1, r2, r3, r4 := runHandshake(t1), runHandshake(t2), runHandshake(t3), runHandshake(t4)
	// First pass of the responder with its current key (node-card.json hs_sk).
	fp := noiseInit(names[0])
	fpCK0 := fp
	fp = b2s(fp, prologueHidden)
	fpPre := b2s(fp, c.hsPub)
	fp = b2s(fpPre, x25519Pub(ie))
	fp = b2s(fp, ek.Bytes())
	fpCK, fpK := noiseHKDF(fpCK0, x25519(rsSK, x25519Pub(ie)))
	aead, err := chacha20poly1305.New(fpK)
	must(err)
	_, openErr := aead.Open(nil, counterNonce(0), r4.msg1[32+1184:], fp)
	check(openErr != nil, "first pass with the current key must fail")
	rotation := O{
		{"note", "XKhfs-rotation-prev-hs, message 1 as received by a responder whose current hs key is node-card.json hs_sk and whose previous key is prev_hs"},
		{"first_pass", O{{"rs", hx(c.hsPub)}, {"h_after_premessage", hx(fpPre)}, {"h1", hx(fp)}, {"ck_after_es", hx(fpCK)}, {"k_after_es", hx(fpK)}, {"p1_open", "fails: the responder starts again with prev_hs"}}},
		{"second_pass", O{{"rs", hx(x25519Pub(prevSK))}, {"h1", hx(r4.h1)}, {"p1_open", "succeeds; only then does the access check run on P1"}}},
	}
	// The live access proof verifies against the transcript's own h1.
	check(bytes.Contains(r2.p1, hmacSHA(c.secret, []byte("echofog/hs2/access-proof"), r2.h1)), "live access proof")

	transcripts := O{
		{"description", "Normative XKhfs and XXhfs handshake transcripts (handshake.md §12, Decision D15): every message's bytes, h/ck/k/n after every token, and the final keys. The generator's Noise code reproduced byte for byte the earlier transcripts on which the two independent step-2 implementations agreed; these application-valid transcripts must be reproduced by both again. What is compared is the Noise message bytes (messages[].bytes) and the final values; the HS_HELLO/HS_MSG frames around them follow from frames.json."},
		{"spec", "handshake.md §2, §4, §5, §9, §12"},
		{"conventions", []string{
			"hex is lowercase; inputs come from handshake.json, node-card.json and link.json (sources named per field)",
			"steps: the state after each token; 'k' is the cipher key ('' when none) and 'n' the next nonce",
			"ML-KEM-768: KEM_GENERATE = crypto/mlkem NewDecapsulationKey768(d ‖ z); KEM_ENCAP with fixed m = FIPS 203 ML-KEM.Encaps_internal(ek, m) (Go crypto/mlkem/mlkemtest.Encapsulate768)",
			"h_before_payload is h right before EncryptAndHash(payload); for message 1 of XK it is h1 of the access proof (handshake.md §5)",
			"payloads carry no pad field",
			"final.session_secret = Noise HKDF(ck, \"echofog/hs2/session-secret\").o1; final.sid = HKDF-SHA256(session_secret, salt = h, info = \"echofog/session2/id\", 16)",
		}},
		{"transcripts", []any{r1.o, r2.o, r3.o, r4.o}},
		{"rotation", rotation},
	}

	hs := O{
		{"description", "Deterministic pieces of the L1 handshake: initial states, prologues, payloads, access proof, puzzle. The full transcripts are in handshake-transcript.json."},
		{"spec", "handshake.md"},
		{"seeds", sl.o},
		{"initialize_symmetric", init},
		{"prologue_xk", O{
			{"cb", hx(cb)},
			{"rid (node-card.json device_id)", hx(rid)},
			{"hs_hello", hx(hello)},
			{"hs_hello_note", "offered [0x0001, 0x0002], chosen 0x0001, flags 0x02 (responder device ID bound); to the public card"},
			{"prologue", hx(prologue)},
			{"h_after_prologue", hx(hP)},
			{"responder_static_rs", hx(c.hsPub)},
			{"h_after_premessage_rs", hx(hRS)},
		}},
		{"prologue_xk_hidden", O{
			{"hs_hello", hx(helloHidden)},
			{"hs_hello_note", "offered [0x0001, 0x0a0a GREASE], chosen 0x0001, flags 0x02; to the invitation card (l1_suites [1])"},
			{"prologue", hx(prologueHidden)},
			{"h_after_prologue", hx(b2s(h, prologueHidden))},
		}},
		{"prologue_xx_bare_address", O{
			{"hs_hello", hx(helloXX)},
			{"prologue", hx(prologueXX)},
			{"h_after_prologue", hx(b2s(noiseInit(names[1]), prologueXX))},
			{"note", "no expected device ID: rid is 32 zero bytes, and the initiator must use a throwaway identity"},
		}},
		{"access_example", O{
			{"note", "the HMAC with a fixed h1 seed; in a handshake h1 is the transcript's h before EncryptAndHash(P1) (handshake-transcript.json XKhfs-access-proof)"},
			{"secret (link.json)", hx(c.secret)},
			{"h1", hx(h1)},
			{"pass_id", hx(c.passID)},
			{"proof", hx(proofExample)},
		}},
		{"payloads", O{
			{"HsInit_xk", O{{"fields", "proto_ver 200, features [1,3,4,5,0x2a2a GREASE], services 1..8 version 2 and GREASE service 0x3a3a version 1, known_card_seq ts (the public card), time ts"}, {"encoded", hx(p1Public)}}},
			{"HsInit_xk_hidden", O{{"fields", "as HsInit_xk plus access{pass_id, proof over the transcript's h1}, known_card_seq ts+2 (the invitation card); the encoding is in handshake-transcript.json"}, {"encoded", hx(r2.p1)}}},
			{"HsInit_xx", O{{"fields", "as HsInit_xk without access and known_card_seq"}, {"encoded", hx(p1XX)}}},
			{"HsResp", O{{"fields", "proto_ver 200, features [1,3,4,5], services [(1,2),(2,2),(3,2)], observed 192.0.2.33:51234, time ts; no card"}, {"encoded", hx(p2)}}},
			{"HsResp_xx", O{{"fields", "as HsResp plus card = node-card.json public.envelope"}, {"encoded", hx(p2XX)}}},
			{"HsFinish", O{{"fields", "card = node-card.json initiator.envelope"}, {"encoded", hx(p3)}}},
			{"HsFinish_xx", O{{"fields", "card = node-card.json initiator_throwaway.envelope"}, {"encoded", hx(p3XX)}}},
		}},
		{"puzzle", O{
			{"cb", "prologue_xk.cb"},
			{"cookie_secret_R", hx(rSecret)},
			{"minute", minute},
			{"cookie", hx(cookie)},
			{"difficulty", 12},
			{"nonce", nonce},
			{"hash", hx(ph)},
			{"HS_RETRY_frame", hx(retry)},
			{"HS_HELLO_with_solution_payload", hx(helloSol)},
		}},
		{"transcript_inputs", O{
			{"note", "the keys of all three transcripts in handshake-transcript.json"},
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
		}},
	}
	return hs, transcripts
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

func genKDF(c cardOut) any {
	var sl seedLog
	card := c.signer
	hsPub := c.hsPub
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
	sealedAAD := cat(sid, u64(0), []byte{1}, qv(0), qv(0)) // stream record, path 0, pipe 0
	sealed := cat(u64(0), chachaSeal(kI2R, counterNonce(0), recPT, sealedAAD))
	record := cat(u16(uint16(1+len(sealed))), []byte{1}, sealed)

	// Relay circuit channel binding and rendezvous ID.
	relayID := sha([]byte("relay node id for the vector"))
	cid := uint64(0x0123456789abcdef)
	cbCircuit := sha([]byte("echofog/hs2/circuit"), relayID, u64(cid))
	day := uint32(fixedTime / 86400)
	personal := sl.get("relay/personal_pass_secret")[:16]
	rendezvous := hmacSHA(personal, []byte("echofog/relay2/rendezvous"), u32(day))

	// chat/2 sealed sender.
	eph := sl.get("chat/eph")
	ikSK := sl.get("chat/recipient_ik")
	ik := x25519Pub(ikSK)
	ephPub := x25519Pub(eph)
	kSeal := hkdfSHA(x25519(eph, ik), nil, cat([]byte("echofog/chat2/seal"), ephPub, ik), 32)
	to := sha([]byte("recipient node id for the vector"))
	inner := (&pb{}).by(1, card.akey()).by(2, pattern(16, 0x70)).u(3, fixedTime).by(5, []byte("ratchet bytes")).b
	sealBox := chachaSeal(kSeal, make([]byte, 12), inner, cat(to, ephPub))
	envelopeChat := (&pb{}).by(1, to).by(2, ephPub).by(3, sealBox).b

	// mailbox/2 forward sealing to M's handshake key and a public-queue proof of work.
	fe := sl.get("mailbox/forward_eph")
	fePub := x25519Pub(fe)
	kFwd := hkdfSHA(x25519(fe, hsPub), nil, cat([]byte("echofog/mailbox2/forward"), fePub, hsPub), 32)
	queue := pattern(16, 0x90)
	blob := envelopeChat
	deposit := (&pb{}).u(1, 9).by(2, queue).by(3, blob).b
	sealedFwd := cat(fePub, chachaSeal(kFwd, make([]byte, 12), deposit, fePub))
	var pow uint64
	var powHash []byte
	for pow = 0; ; pow++ {
		powHash = sha([]byte("echofog/mailbox2/pow"), queue, sha(blob), u64(pow))
		if leadingZeros(powHash) >= 12 {
			break
		}
	}

	// call/2 keys and a signed unit.
	room := sl.get("call/room_secret")
	epoch0 := hkdfSHA(room, nil, cat([]byte("echofog/call2/epoch"), u32(0)), 32)
	joinSalt := sl.get("call/join_salt")[:16]
	kSender := hkdfSHA(epoch0, nil, cat([]byte("echofog/call2/sender-key"), card.nodeID(), joinSalt), 32)
	media := newSigner(sl.get("call/media_key"))
	blk, err := aes.NewCipher(kSender)
	must(err)
	gcm, err := cipher.NewGCM(blk)
	must(err)
	var units []any
	var hashes []byte
	for i := uint32(0); i < 2; i++ {
		seq := 7 + i
		header := cat([]byte{1, 0, 3, 0, 1}, u32(seq), u32(160+20*i))
		nonce := cat([]byte{0, 1, 0, 0}, u32(seq), []byte{0, 0, 0, 0})
		ctu := gcm.Seal(nil, nonce, pattern(40, byte(0xc0+i)), header)
		hu := sha([]byte("echofog/call2/unit"), header, ctu)[:16]
		hashes = append(hashes, hu...)
		units = append(units, O{{"header", hx(header)}, {"note", "audio, epoch 0, sender 3, stream 0, gen 1"}, {"seq", seq},
			{"nonce", hx(nonce)}, {"ciphertext_and_tag", hx(ctu)}, {"h", hx(hu)}})
	}
	authPrefix := cat([]byte{1, 0, 3, 0, 1}, u32(7), []byte{0, 2}, hashes)
	authSig := ed25519.Sign(media.priv, cat([]byte("echofog/call2/batch"), authPrefix))
	callAuth := cat(qv(0x13), authPrefix, authSig)

	// chat/2 PQXDH session key with fixed DH outputs and KEM values.
	dh := func(n string) []byte { return sl.get("chat/pqxdh/" + n) }
	dh1, dh2, dh3, dh4, ssKEM := dh("dh1"), dh("dh2"), dh("dh3"), dh("dh4"), dh("ss")
	kemID, ctHash := dh("kem_key_id"), dh("kem_ct_hash")
	pqxdhIKM := cat(bytes.Repeat([]byte{0xff}, 32), dh1, dh2, dh3, dh4, ssKEM)
	pqxdhSK := hkdfSHA(pqxdhIKM, make([]byte, 32), cat([]byte("echofog/chat2/pqxdh"), kemID, ctHash), 32)

	// DHT keys.
	label := "echofog/card/2"
	dhtCard := sha([]byte("echofog/dht2/key"), u32(day), []byte{byte(len(label))}, []byte(label), card.nodeID())
	dhtRelease := sha([]byte("echofog/dht2/release"), u32(day))

	return O{
		{"description", "Key derivations of L2 and the services from fixed inputs. HKDF is HKDF-SHA256 unless marked noise (HMAC-BLAKE2s, handshake.md §2)."},
		{"spec", "handshake.md §9, wire.md §8, services.md, dht.md §2"},
		{"seeds", sl.o},
		{"session", O{
			{"ck", hx(ck)},
			{"h", hx(h)},
			{"split_k_i2r", hx(kI2R)},
			{"split_k_r2i", hx(kR2I)},
			{"session_secret", hx(sess)},
			{"note", "split_k_i2r/split_k_r2i = noise HKDF(ck, empty) o1/o2; session_secret = noise HKDF(ck, \"echofog/hs2/session-secret\").o1"},
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
			{"aad", hx(sealedAAD)},
			{"record", hx(record)},
			{"note", "aad = sid ‖ u64(n) ‖ kind 1 (ordered stream record) ‖ varint path_id 0 ‖ varint pipe 0; record = u16(len) ‖ kind 1 ‖ sealed"},
		}},
		{"relay2", O{
			{"relay_node_id", hx(relayID)},
			{"cid", fmt.Sprintf("%016x", cid)},
			{"cb_circuit", hx(cbCircuit)},
			{"day", day},
			{"personal_pass_secret", hx(personal)},
			{"rendezvous_id", hx(rendezvous)},
		}},
		{"chat2_seal", O{{"eph_sk", hx(eph)}, {"eph_pub", hx(ephPub)}, {"recipient_ik_sk", hx(ikSK)}, {"recipient_ik", hx(ik)}, {"k", hx(kSeal)},
			{"to", hx(to)}, {"inner", hx(inner)}, {"box", hx(sealBox)}, {"SealedEnvelope", hx(envelopeChat)}}},
		{"mailbox2", O{{"forward_eph_sk", hx(fe)}, {"forward_eph_pub", hx(fePub)}, {"hs_M", hx(hsPub)}, {"k", hx(kFwd)},
			{"deposit", hx(deposit)}, {"deposit_note", "req 9, queue 90..9f, blob = chat2_seal.SealedEnvelope"}, {"sealed", hx(sealedFwd)},
			{"pow_bits", 12}, {"pow", pow}, {"pow_hash", hx(powHash)}}},
		{"call2", O{{"room_secret", hx(room)}, {"epoch0_secret", hx(epoch0)}, {"sender_device_id", hx(card.nodeID())}, {"join_salt", hx(joinSalt)},
			{"k_sender", hx(kSender)}, {"units", units}, {"units_note", "plaintexts are 40 bytes of c0.. and c1.."},
			{"media_key_pub", hx(media.pub)},
			{"call_auth", O{{"note", "first_seq 7, prev_count 0, count 2"}, {"signed_prefix", hx(authPrefix)}, {"sig", hx(authSig)}, {"datagram", hx(callAuth)}}}}},
		{"chat2_pqxdh", O{{"dh1", hx(dh1)}, {"dh2", hx(dh2)}, {"dh3", hx(dh3)}, {"dh4", hx(dh4)}, {"kem_ss", hx(ssKEM)},
			{"kem_key_id", hx(kemID)}, {"kem_ct_hash", hx(ctHash)}, {"note", "synthetic DH outputs; ikm = 0xff×32 ‖ DH1 ‖ DH2 ‖ DH3 ‖ DH4 ‖ SS, salt = 32 zero bytes"},
			{"SK", hx(pqxdhSK)}}},
		{"dht2", O{
			{"day", day},
			{"card_key", O{{"label", label}, {"signer_device_id", hx(card.nodeID())}, {"key", hx(dhtCard)}}},
			{"devices_key", O{{"label", "echofog/devices/2"}, {"signer_node_id", hx(c.account.nodeID())}, {"note", "the account key signs the device list"},
				{"key", hx(sha([]byte("echofog/dht2/key"), u32(day), []byte{byte(len("echofog/devices/2"))}, []byte("echofog/devices/2"), c.account.nodeID()))}}},
			{"release_key", hx(dhtRelease)},
		}},
	}
}

func genNegative(c cardOut) any {
	s := c.signer
	// Non-canonical bodies.
	outOfOrder := cat((&pb{}).u(6, 1).b, (&pb{}).u(1, 200).b)
	longTag := cat([]byte{0x88, 0x00}, []byte{0xc8, 0x01})               // field 1 tag as a 2-byte varint
	unpacked := cat((&pb{}).u(1, 200).b, []byte{0x48, 0x01, 0x48, 0x03}) // features as unpacked varints
	dupScalar := cat((&pb{}).u(1, 200).b, (&pb{}).u(1, 201).b)
	// Signature with S + L (not canonical).
	body := (&pb{}).u(1, 200).b
	env, _, sig := envelope("echofog/card/2", s, 1, fixedTime+86400, body)
	S := leBig(sig[32:])
	L, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
	S.Add(S, L)
	sb := S.Bytes()
	sNew := make([]byte, 32)
	for i := range sb {
		sNew[i] = sb[len(sb)-1-i]
	}
	badSig := cat(env[:len(env)-64], sig[:32], sNew)
	check(!ed25519.Verify(s.pub, env[:len(env)-64], cat(sig[:32], sNew)), "Go rejects S ≥ L")
	// An envelope whose signer is the identity point (small order).
	smallOrder := cat([]byte{0x02, byte(len("echofog/card/2"))}, []byte("echofog/card/2"), []byte{0x01},
		unhex("0100000000000000000000000000000000000000000000000000000000000000"), u64(1), u64(fixedTime+86400), qv(uint64(len(body))), body, make([]byte, 64))
	return O{
		{"description", "Inputs that MUST be rejected."},
		{"spec", "objects.md §2.2–§2.3"},
		{"non_canonical_bodies", []any{
			O{{"body", hx(outOfOrder)}, {"reason", "fields out of order (6 before 1)"}},
			O{{"body", hx(longTag)}, {"reason", "tag of field 1 encoded with a non-minimal varint"}},
			O{{"body", hx(unpacked)}, {"reason", "NodeCard.features (field 9, known packed field) sent unpacked"}},
			O{{"body", hx(dupScalar)}, {"reason", "known non-repeated field 1 appears twice"}},
		}},
		{"signature_s_not_canonical", O{{"envelope", hx(badSig)}, {"reason", "S + L in place of S: RFC 8032 requires S < L"}}},
		{"small_order_signer", O{{"envelope", hx(smallOrder)}, {"reason", "signer key is the identity point (small order)"}}},
		{"noise3_init_out_of_window", O{{"input", "noise3.json init with ts = init.ts - 301 at a responder whose clock is init.ts"}, {"reason", "|ts − now| > 300 s: silently dropped"}}},
		{"reality2_token_wrong_address", O{{"input", "reality2.json client_hello_with_token received on 198.51.100.7:443"}, {"reason", "k_tok differs (reality2.json k_tok_at_198.51.100.7:443): forwarded to the cover"}}},
		{"varint", "see varint.json invalid"},
	}
}
