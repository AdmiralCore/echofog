package mimicry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	quicgo "github.com/apernet/quic-go"
	"golang.org/x/crypto/hkdf"
)

var (
	quicV1Salt = []byte{
		0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3,
		0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad,
		0xcc, 0xbb, 0x7f, 0x0a,
	}
	quicV1 = uint32(0x00000001)
)

var (
	errNotInitialPacket = errors.New("quic_reality: not a QUIC v1 Initial packet")
	errMalformedPacket  = errors.New("quic_reality: malformed QUIC packet")
)

func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 4+len(full))
	info = binary.BigEndian.AppendUint16(info, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, length)
	_, _ = hkdf.Expand(sha256.New, secret, info).Read(out)
	return out
}

func readVarint(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, false
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, true
}

// IsQUICInitial checks if a packet begins with a QUIC v1 Initial header.
func IsQUICInitial(b []byte) bool {
	if len(b) < 7 || b[0]&0x80 == 0 {
		return false
	}
	version := binary.BigEndian.Uint32(b[1:5])
	if version != quicV1 {
		return false
	}
	return (b[0]>>4)&0x03 == 0
}

type initialHeader struct {
	dcid    []byte
	scid    []byte
	tokOff  int // offset where token length varint starts
	tokLen  int // token length in bytes
	tokLenN int // bytes of token length varint
	pnOff   int
	pnLen   int
	length  int
	end     int
	mask    [16]byte
	hdr0    byte
	pn      uint64
	hdr     []byte
}

func parseAndUnmaskInitial(b []byte, isClient bool) (*initialHeader, []byte, []byte, []byte, error) {
	if !IsQUICInitial(b) {
		return nil, nil, nil, nil, errNotInitialPacket
	}
	off := 5
	dcidLen := int(b[off])
	off++
	if dcidLen > 20 || len(b) < off+dcidLen+1 {
		return nil, nil, nil, nil, errMalformedPacket
	}
	dcid := b[off : off+dcidLen]
	off += dcidLen

	scidLen := int(b[off])
	off++
	if scidLen > 20 || len(b) < off+scidLen {
		return nil, nil, nil, nil, errMalformedPacket
	}
	scid := b[off : off+scidLen]
	off += scidLen

	tokOff := off
	tokLen, n, ok := readVarint(b[off:])
	if !ok || uint64(len(b)-off-n) < tokLen {
		return nil, nil, nil, nil, errMalformedPacket
	}
	tokLenN := n
	off += n + int(tokLen)

	length, n, ok := readVarint(b[off:])
	if !ok {
		return nil, nil, nil, nil, errMalformedPacket
	}
	off += n
	pnOff := off
	end := pnOff + int(length)
	if length < 20 || end > len(b) {
		return nil, nil, nil, nil, errMalformedPacket
	}
	if len(b) < pnOff+20 {
		return nil, nil, nil, nil, errMalformedPacket
	}

	secretSeed := dcid
	label := "client in"
	if !isClient {
		secretSeed = scid
		label = "server in"
	}

	initialSecret := hkdf.Extract(sha256.New, secretSeed, quicV1Salt)
	secret := hkdfExpandLabel(initialSecret, label, 32)
	key := hkdfExpandLabel(secret, "quic key", 16)
	iv := hkdfExpandLabel(secret, "quic iv", 12)
	hp := hkdfExpandLabel(secret, "quic hp", 16)

	hb, err := aes.NewCipher(hp)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var mask [16]byte
	hb.Encrypt(mask[:], b[pnOff+4:pnOff+20])

	hdr0 := b[0] ^ (mask[0] & 0x0f)
	pnLen := int(hdr0&0x03) + 1
	if pnOff+pnLen > end {
		return nil, nil, nil, nil, errMalformedPacket
	}

	hdr := append([]byte(nil), b[:pnOff+pnLen]...)
	hdr[0] = hdr0
	var pn uint64
	for i := 0; i < pnLen; i++ {
		hdr[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(hdr[pnOff+i])
	}

	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	plain, err := aead.Open(nil, nonce, b[pnOff+pnLen:end], hdr)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	h := &initialHeader{
		dcid:    dcid,
		scid:    scid,
		tokOff:  tokOff,
		tokLen:  int(tokLen),
		tokLenN: tokLenN,
		pnOff:   pnOff,
		pnLen:   pnLen,
		length:  int(length),
		end:     end,
		mask:    mask,
		hdr0:    hdr0,
		pn:      pn,
		hdr:     hdr,
	}
	return h, plain, key, iv, nil
}

// cryptoFrags collects CRYPTO frame fragments from an Initial's plaintext, keyed by stream offset.
// Chrome's ClientHello (with ML-KEM) spans two Initials and quic-go may scatter its CRYPTO frames out
// of order and across datagrams, so a caller reassembling the full hello must merge the fragments of
// every Initial a source sends (see MergeCryptoFrags / AssembleClientHello).
func cryptoFrags(plain []byte) map[int][]byte {
	parts := map[int][]byte{}
	p := 0
	for p < len(plain) {
		ft := plain[p]
		if ft == 0x00 || ft == 0x01 { // PADDING, PING
			p++
			continue
		}
		if ft == 0x06 { // CRYPTO
			p++
			off, n1, ok1 := readVarint(plain[p:])
			if !ok1 {
				break
			}
			p += n1
			length, n2, ok2 := readVarint(plain[p:])
			if !ok2 {
				break
			}
			p += n2
			if uint64(len(plain)-p) < length {
				break
			}
			parts[int(off)] = append([]byte(nil), plain[p:p+int(length)]...)
			p += int(length)
			continue
		}
		break
	}
	return parts
}

// assembleFromFrags returns the bytes covered contiguously from offset 0 by the fragments.
func assembleFromFrags(parts map[int][]byte) []byte {
	var out []byte
	cur := 0
	for {
		chunk, ok := parts[cur]
		if !ok || len(chunk) == 0 {
			break
		}
		out = append(out, chunk...)
		cur += len(chunk)
	}
	return out
}

// QUICInitialCrypto decrypts a client QUIC v1 Initial and returns its DCID and the CRYPTO fragments it
// carries, keyed by stream offset. ok is false if pkt is not a decryptable client Initial. A server
// merges the fragments of a source's Initials to reassemble the ClientHello, which spans two Initials.
func QUICInitialCrypto(pkt []byte) (dcid []byte, frags map[int][]byte, ok bool) {
	h, plain, _, _, err := parseAndUnmaskInitial(pkt, true)
	if err != nil {
		return nil, nil, false
	}
	return append([]byte(nil), h.dcid...), cryptoFrags(plain), true
}

// MergeCryptoFrags merges src into dst (dst may be nil), keeping the fragment already present at an
// offset so a retransmitted Initial is idempotent. It returns the merged map.
func MergeCryptoFrags(dst, src map[int][]byte) map[int][]byte {
	if dst == nil {
		dst = make(map[int][]byte, len(src))
	}
	for off, b := range src {
		if _, seen := dst[off]; !seen {
			dst[off] = b
		}
	}
	return dst
}

// AssembleClientHello returns the ClientHello handshake message reassembled contiguously from offset 0
// out of accumulated CRYPTO fragments.
func AssembleClientHello(frags map[int][]byte) []byte {
	return assembleFromFrags(frags)
}

// QUICClientHelloComplete reports whether a reassembled ClientHello carries the 32-byte random and a
// complete key_share extension — enough to authenticate the stealth token, which is bound to the
// key_share. It is false while the hello is still partial, so the server keeps buffering.
func QUICClientHelloComplete(hello []byte) bool {
	if len(hello) < 38 || hello[0] != 1 {
		return false
	}
	body, ok := quicClientHelloKeyShare(hello)
	if !ok {
		return false
	}
	_, ok = quicMLKEMEncapKey(body)
	return ok
}

// QUIC key share groups and sizes used for the token binding.
const (
	// quicGroupX25519MLKEM768 is the TLS named group for the X25519+ML-KEM-768 hybrid key share.
	quicGroupX25519MLKEM768 = 0x11ec
	// mlkemEncapKeyLen is the size of an ML-KEM-768 encapsulation key (FIPS 203), the deterministic
	// prefix of the hybrid share's data; the trailing 32 bytes are the X25519 public key.
	mlkemEncapKeyLen = 1184
)

// quicMLKEMBinding is the AAD the QUIC stealth token is sealed under: a hash of the ClientHello's
// ML-KEM-768 encapsulation key. The token is carried as the ClientHello's 32-byte random itself, so it
// cannot be bound to that random; binding it to the client's own key share is what stops an on-path
// censor that steals a valid random and runs its OWN handshake. A censor that offers its own key share
// produces a different ML-KEM key, so the token no longer opens and the node hands it to the cover site
// instead of revealing its certificate; a censor that copies the client's ML-KEM public key to pass the
// binding cannot decapsulate the node's response (it lacks the private key), so it still learns nothing.
//
// Only the ML-KEM key is bound, not the X25519 key or the whole key_share: the ML-KEM key is generated
// from a seed (crypto/mlkem) and so can be predicted deterministically, whereas Go's crypto/ecdh ignores
// the supplied rand for X25519, so the X25519 key cannot be reproduced by the predict pass. The node
// prefers the hybrid group, so the ML-KEM key is the one that matters. (The random is read before the
// key share is generated, so the dial predicts the key share and seals the token to it; see
// QUICInitialRand.) The replay cache, keyed on the token bytes, still stops a captured Initial being
// reused. Chrome's first visit to a site sends no QUIC token at all, so keeping the auth in the random
// leaves the Initial's Token field empty, as Chrome's is.
func quicMLKEMBinding(mlkemEncapKey []byte) []byte {
	h := sha256.New()
	h.Write([]byte("echofog-quic-mlkem-binding-v1"))
	h.Write(mlkemEncapKey)
	return h.Sum(nil)
}

// quicMLKEMEncapKey returns the ML-KEM-768 encapsulation key from a key_share extension body: the
// X25519MLKEM768 share's data with its trailing 32-byte X25519 public key removed. ok is false if that
// share is absent or truncated, so the server keeps buffering until the whole ML-KEM key is present.
func quicMLKEMEncapKey(keyShareBody []byte) ([]byte, bool) {
	if len(keyShareBody) < 2 {
		return nil, false
	}
	for p := 2; p+4 <= len(keyShareBody); {
		group := int(keyShareBody[p])<<8 | int(keyShareBody[p+1])
		l := int(keyShareBody[p+2])<<8 | int(keyShareBody[p+3])
		p += 4
		if p+l > len(keyShareBody) {
			return nil, false
		}
		if group == quicGroupX25519MLKEM768 {
			if l < mlkemEncapKeyLen {
				return nil, false
			}
			return keyShareBody[p : p+mlkemEncapKeyLen], true
		}
		p += l
	}
	return nil, false
}

// quicClientHelloKeyShare returns the key_share (51) extension body of a ClientHello handshake message
// (type byte first, no record or QUIC framing): the bytes after the extension's type and length, i.e.
// the client_shares length then each share. ok is false if the hello is truncated before the key_share
// is complete, so the server keeps buffering until it is.
func quicClientHelloKeyShare(msg []byte) ([]byte, bool) {
	const randomOff = 4 + 2 // handshake header, client_version
	const idLenOff = randomOff + 32
	if len(msg) < idLenOff+1 || msg[0] != 1 {
		return nil, false
	}
	p := idLenOff + 1 + int(msg[idLenOff]) // past the session id (empty for QUIC)
	if p+2 > len(msg) {
		return nil, false
	}
	p += 2 + (int(msg[p])<<8 | int(msg[p+1])) // cipher suites
	if p+1 > len(msg) {
		return nil, false
	}
	p += 1 + int(msg[p]) // compression methods
	if p+2 > len(msg) {
		return nil, false
	}
	end := p + 2 + (int(msg[p])<<8 | int(msg[p+1]))
	if end > len(msg) {
		return nil, false // the extensions are not all here yet
	}
	for p += 2; p+4 <= end; {
		typ := int(msg[p])<<8 | int(msg[p+1])
		l := int(msg[p+2])<<8 | int(msg[p+3])
		p += 4
		if p+l > end {
			return nil, false
		}
		if typ == extKeyShare {
			return msg[p : p+l], true
		}
		p += l
	}
	return nil, false
}

// GenerateQUICRandomToken makes a fresh 32-byte stealth token bound to mlkemEncapKey (the ClientHello's
// ML-KEM-768 encapsulation key), to carry as the QUIC ClientHello random. It is exactly StealthTokenLen
// (nonce || AES-GCM(timestamp) || tag), the size of a TLS random, so it is indistinguishable from an
// ordinary ClientHello random on the wire.
func GenerateQUICRandomToken(authKey, mlkemEncapKey []byte) ([]byte, error) {
	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return nil, err
	}
	return GenerateBoundToken(aead, quicMLKEMBinding(mlkemEncapKey))
}

// tokenFirstReader yields token first, then draws from rest. A REALITY QUIC client dial hands this
// to the TLS stack as its randomness source: apernet's ChromeParrot reads the 32-byte ClientHello
// random as its first read, so the token lands there, and every later read (GREASE, key shares,
// ...) is ordinary cryptographic randomness.
type tokenFirstReader struct {
	token []byte
	rest  io.Reader
}

func (r *tokenFirstReader) Read(p []byte) (int, error) {
	if len(r.token) > 0 {
		n := copy(p, r.token)
		r.token = r.token[n:]
		return n, nil
	}
	return r.rest.Read(p)
}

// zeroReader is an endless source of zero bytes; XORed with an AES-CTR keystream it yields the
// keystream itself (see detStream).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// detStream is an unbounded deterministic keystream seeded by seed (AES-256-CTR over zeros). Two
// streams from the same seed yield identical bytes, so a key share predicted from one is reproduced by
// a dial reading from another. seed is single-use and from crypto/rand, so the ephemeral keys it drives
// are as unpredictable as ordinary randomness; it never leaves this process.
func detStream(seed []byte) (io.Reader, error) {
	block, err := aes.NewCipher(seed)
	if err != nil {
		return nil, err
	}
	return cipher.StreamReader{S: cipher.NewCTR(block, make([]byte, aes.BlockSize)), R: zeroReader{}}, nil
}

// QUICInitialRand returns the randomness source for a REALITY QUIC client dial (ChromeParrot): a
// stealth token bound to the dial's own key_share as the first 32 bytes (the ClientHello random),
// cryptographic randomness thereafter. Set it as tls.Config.Rand.
//
// The token is bound to the key_share, but the random is drawn before the stack generates the shares,
// so a single build cannot seal the token to them. Instead this does a deterministic two-pass: a fresh
// seed backs a deterministic keystream for every draw after the 32-byte token; the key_share is
// predicted by building a throwaway ClientHello over that stream (with a placeholder token), the token
// is sealed to it, and the live dial reads the same stream, so the handshake reproduces the identical
// key_share and the token verifies. alpn is the dial's ALPN (its shares do not depend on it, but it is
// passed so the throwaway build matches).
func QUICInitialRand(authKey []byte, alpn []string) (io.Reader, error) {
	seed := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, err
	}
	predict, err := detStream(seed)
	if err != nil {
		return nil, err
	}
	var placeholder [StealthTokenLen]byte
	keyShare, err := quicgo.ChromeParrotKeyShare(&tokenFirstReader{token: placeholder[:], rest: predict}, alpn)
	if err != nil {
		return nil, fmt.Errorf("mimicry: predicting QUIC key share: %w", err)
	}
	mlkem, ok := quicMLKEMEncapKey(keyShare)
	if !ok {
		return nil, errors.New("mimicry: predicted QUIC key share has no ML-KEM entry")
	}
	token, err := GenerateQUICRandomToken(authKey, mlkem)
	if err != nil {
		return nil, err
	}
	live, err := detStream(seed)
	if err != nil {
		return nil, err
	}
	return &tokenFirstReader{token: token, rest: live}, nil
}

// VerifyQUICClientHelloToken checks whether a reassembled ClientHello carries a valid stealth token as
// its random, sealed under authKey and bound to the hello's own key_share. Call it only once the hello
// is QUICClientHelloComplete; the server reassembles both of a source's Initials first, because the
// key_share the token is bound to spans them. It returns false for anyone without the node's key, and
// for a stolen random replayed on a ClientHello with a different key_share.
func VerifyQUICClientHelloToken(hello, authKey []byte) bool {
	return VerifyQUICClientHelloTokenAt(hello, authKey, nowUnix())
}

// VerifyQUICClientHelloTokenAt verifies against a specific time (for tests and replay checks).
func VerifyQUICClientHelloTokenAt(hello, authKey []byte, now int64) bool {
	if len(hello) < 38 || hello[0] != 1 {
		return false
	}
	keyShare, ok := quicClientHelloKeyShare(hello)
	if !ok {
		return false
	}
	mlkem, ok := quicMLKEMEncapKey(keyShare)
	if !ok {
		return false
	}
	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return false
	}
	return verifyStealthToken(aead, hello[6:38], quicMLKEMBinding(mlkem), sharedReplayCache, now) == nil
}

// ExtractInitialDCID returns the DCID from a QUIC Initial packet without decrypting it.
func ExtractInitialDCID(b []byte) ([]byte, bool) {
	if !IsQUICInitial(b) {
		return nil, false
	}
	off := 5
	dcidLen := int(b[off])
	off++
	if dcidLen > 20 || len(b) < off+dcidLen {
		return nil, false
	}
	return b[off : off+dcidLen], true
}
