package mimicry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

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

// readAllCryptoData collects all contiguous CRYPTO frame data from plain.
func readAllCryptoData(plain []byte) []byte {
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
	// Assemble contiguous bytes starting at offset 0
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

// quicRandomBinding is the fixed AAD the QUIC stealth token is sealed under. The token is carried
// as the ClientHello's 32-byte random itself, so it cannot be bound to that random; instead it is
// sealed with the node's per-node auth key (DeriveAEAD) under this constant AAD, and the replay
// cache (keyed on the token bytes) stops a captured Initial being reused. Tampering with the random
// breaks the token's own AEAD tag, so a forged ClientHello fails to authenticate and is handed to
// the cover site. Chrome's first visit to a site sends no QUIC token at all, so keeping the auth in
// the random leaves the Initial's Token field empty, as Chrome's is.
func quicRandomBinding() []byte {
	h := sha256.Sum256([]byte("echofog-quic-random-token-v1"))
	return h[:]
}

// GenerateQUICRandomToken makes a fresh 32-byte stealth token to carry as a QUIC ClientHello random.
// It is exactly StealthTokenLen (nonce || AES-GCM(timestamp) || tag), the size of a TLS random, so
// it is indistinguishable from an ordinary ClientHello random on the wire.
func GenerateQUICRandomToken(authKey []byte) ([]byte, error) {
	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return nil, err
	}
	return GenerateBoundToken(aead, quicRandomBinding())
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

// QUICInitialRand returns the randomness source for a REALITY QUIC client dial (ChromeParrot): the
// freshly generated stealth token as the first 32 bytes, cryptographic randomness thereafter. Set
// it as tls.Config.Rand; the ClientHello random then carries the token and the server authenticates
// the dial from it, without a QUIC Token-field artefact that a real Chrome would never send.
func QUICInitialRand(authKey []byte) (io.Reader, error) {
	token, err := GenerateQUICRandomToken(authKey)
	if err != nil {
		return nil, err
	}
	return &tokenFirstReader{token: token, rest: rand.Reader}, nil
}

// VerifyQUICInitialRandomToken checks whether a client Initial carries a valid stealth token as its
// ClientHello random, sealed under authKey. It returns the DCID either way (so the caller can track
// the connection), and false for anyone without the node's key: a stranger, scanner or active probe
// the caller then hands to the cover site.
func VerifyQUICInitialRandomToken(pkt []byte, authKey []byte) (valid bool, dcid []byte) {
	return VerifyQUICInitialRandomTokenAt(pkt, authKey, nowUnix())
}

// VerifyQUICInitialRandomTokenAt verifies against a specific time (for tests and replay checks).
func VerifyQUICInitialRandomTokenAt(pkt []byte, authKey []byte, now int64) (valid bool, dcid []byte) {
	h, plain, _, _, err := parseAndUnmaskInitial(pkt, true)
	if err != nil {
		return false, nil
	}
	clientHello := readAllCryptoData(plain)
	if len(clientHello) < 38 || clientHello[0] != 1 {
		return false, h.dcid
	}
	random := clientHello[6:38]
	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return false, h.dcid
	}
	if verifyStealthToken(aead, random, quicRandomBinding(), sharedReplayCache, now) == nil {
		return true, h.dcid
	}
	return false, h.dcid
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
