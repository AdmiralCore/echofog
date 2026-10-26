package mimicry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"

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

func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 64:
		return append(b, byte(v))
	case v < 16384:
		return binary.BigEndian.AppendUint16(b, uint16(v)|0x4000)
	case v < 1073741824:
		return binary.BigEndian.AppendUint32(b, uint32(v)|0x80000000)
	default:
		return binary.BigEndian.AppendUint64(b, v|0xc000000000000000)
	}
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

// quicHelloBinding binds the stealth token to the ClientHello's 32-byte random and the QUIC DCID.
func quicHelloBinding(random, dcid []byte) []byte {
	h := sha256.New()
	h.Write([]byte("echofog-quic-hello-binding-v1"))
	h.Write(random)
	h.Write(dcid)
	return h.Sum(nil)
}

// InjectInitialToken inserts a 32-byte stealth authentication token into a client's Initial packet header.
// The token is cryptographically bound to the ClientHello random and the connection's DCID.
func InjectInitialToken(pkt []byte, authKey []byte) (newPkt []byte, dcid []byte, ok bool) {
	h, plain, key, iv, err := parseAndUnmaskInitial(pkt, true)
	if err != nil {
		return nil, nil, false
	}
	clientHello := readAllCryptoData(plain)
	if len(clientHello) < 38 || clientHello[0] != 1 {
		return nil, nil, false
	}
	random := clientHello[6:38]
	bound := quicHelloBinding(random, h.dcid)

	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return nil, nil, false
	}
	token, err := GenerateBoundToken(aead, bound)
	if err != nil {
		return nil, nil, false
	}

	// Reconstruct the packet with the token in the Initial header:
	// b[:tokOff] | varint(32) | token(32) | length_varint | pn | payload | rest
	var out []byte
	out = append(out, pkt[:h.tokOff]...)
	out = appendVarint(out, uint64(len(token)))
	out = append(out, token...)

	newPNOff := len(out)
	// Append length varint covering packet number + ciphertext
	out = appendVarint(out, uint64(h.length))
	actualPNOff := len(out)

	// Build new unmasked header for AEAD authentication
	newHdr := append([]byte(nil), out...)
	newHdr[0] = h.hdr0
	for i := 0; i < h.pnLen; i++ {
		newHdr = append(newHdr, byte(h.pn>>(8*(h.pnLen-1-i))))
	}

	// Re-encrypt payload with the new header as AAD
	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, false
	}
	gaead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, nil, false
	}
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(h.pn >> (8 * i))
	}
	sealed := gaead.Seal(nil, nonce, plain, newHdr)

	// Append packet number bytes (temporarily unmasked)
	for i := 0; i < h.pnLen; i++ {
		out = append(out, byte(h.pn>>(8*(h.pnLen-1-i))))
	}
	out = append(out, sealed...)

	// Append any trailing coalesced packets in the datagram
	if len(pkt) > h.end {
		out = append(out, pkt[h.end:]...)
	}

	// Re-apply header protection on sample from payload
	initialSecret := hkdf.Extract(sha256.New, h.dcid, quicV1Salt)
	secret := hkdfExpandLabel(initialSecret, "client in", 32)
	hpKey := hkdfExpandLabel(secret, "quic hp", 16)
	hb, err := aes.NewCipher(hpKey)
	if err != nil {
		return nil, nil, false
	}
	var mask [16]byte
	hb.Encrypt(mask[:], out[actualPNOff+4:actualPNOff+20])

	out[0] = h.hdr0 ^ (mask[0] & 0x0f)
	for i := 0; i < h.pnLen; i++ {
		out[actualPNOff+i] ^= mask[1+i]
	}

	_ = newPNOff
	return out, h.dcid, true
}

// StripInitialToken removes the stealth token from an authenticated Initial packet header,
// returning the clean Initial packet with Token Length = 0 for the standard quic-go listener.
func StripInitialToken(pkt []byte) (cleanPkt []byte, ok bool) {
	h, plain, key, iv, err := parseAndUnmaskInitial(pkt, true)
	if err != nil || h.tokLen != StealthTokenLen {
		return pkt, false
	}

	// Build clean packet with tokLen = 0:
	var out []byte
	out = append(out, pkt[:h.tokOff]...)
	out = append(out, 0x00) // token length = 0

	out = appendVarint(out, uint64(h.length))
	actualPNOff := len(out)

	cleanHdr := append([]byte(nil), out...)
	cleanHdr[0] = h.hdr0
	for i := 0; i < h.pnLen; i++ {
		cleanHdr = append(cleanHdr, byte(h.pn>>(8*(h.pnLen-1-i))))
	}

	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, false
	}
	gaead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, false
	}
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(h.pn >> (8 * i))
	}
	sealed := gaead.Seal(nil, nonce, plain, cleanHdr)

	for i := 0; i < h.pnLen; i++ {
		out = append(out, byte(h.pn>>(8*(h.pnLen-1-i))))
	}
	out = append(out, sealed...)

	if len(pkt) > h.end {
		out = append(out, pkt[h.end:]...)
	}

	initialSecret := hkdf.Extract(sha256.New, h.dcid, quicV1Salt)
	secret := hkdfExpandLabel(initialSecret, "client in", 32)
	hpKey := hkdfExpandLabel(secret, "quic hp", 16)
	hb, err := aes.NewCipher(hpKey)
	if err != nil {
		return nil, false
	}
	var mask [16]byte
	hb.Encrypt(mask[:], out[actualPNOff+4:actualPNOff+20])

	out[0] = h.hdr0 ^ (mask[0] & 0x0f)
	for i := 0; i < h.pnLen; i++ {
		out[actualPNOff+i] ^= mask[1+i]
	}

	return out, true
}

// VerifyQUICInitialToken verifies whether an incoming Initial packet carries a valid stealth token
// bound to its ClientHello (checked either in Initial Token field or in legacy_session_id).
func VerifyQUICInitialToken(pkt []byte, authKey []byte) (valid bool, dcid []byte) {
	return VerifyQUICInitialTokenAt(pkt, authKey, nowUnix())
}

// VerifyQUICInitialTokenAt verifies the token against a specific timestamp (useful for tests and replay checks).
func VerifyQUICInitialTokenAt(pkt []byte, authKey []byte, now int64) (valid bool, dcid []byte) {
	h, plain, _, _, err := parseAndUnmaskInitial(pkt, true)
	if err != nil {
		return false, nil
	}
	clientHello := readAllCryptoData(plain)
	if len(clientHello) < 38 || clientHello[0] != 1 {
		return false, h.dcid
	}

	random := clientHello[6:38]
	bound := quicHelloBinding(random, h.dcid)

	aead, err := DeriveAEAD(authKey)
	if err != nil {
		return false, h.dcid
	}

	// 1. Check in QUIC Initial Token field
	if h.tokLen == StealthTokenLen {
		token := pkt[h.tokOff+h.tokLenN : h.tokOff+h.tokLenN+StealthTokenLen]
		if err := verifyStealthToken(aead, token, bound, sharedReplayCache, now); err == nil {
			return true, h.dcid
		}
	}

	// 2. Also check if helloBinding (TCP-style with key_share) matches
	if b2, ok := helloBinding(clientHello); ok {
		if h.tokLen == StealthTokenLen {
			token := pkt[h.tokOff+h.tokLenN : h.tokOff+h.tokLenN+StealthTokenLen]
			if err := verifyStealthToken(aead, token, b2, sharedReplayCache, now); err == nil {
				return true, h.dcid
			}
		}
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
