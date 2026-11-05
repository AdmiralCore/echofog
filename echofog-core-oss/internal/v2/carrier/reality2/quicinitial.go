package reality2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

// The server gate must decide on the ClientHello before quic-go sees the packet, so it
// decrypts QUIC v1 Initial packets itself (RFC 9001 §5.2) and reassembles the CRYPTO
// stream. This is a port of the v1 decoder (internal/network/mimicry/quic_reality.go)
// into v2; no v1 network package is imported (AD1).
const (
	quicV1Version = 0x00000001
	// maxInitialCrypto bounds one source's reassembled ClientHello.
	maxInitialCrypto = 16 << 10
)

var (
	quicV1Salt = []byte{
		0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3,
		0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad,
		0xcc, 0xbb, 0x7f, 0x0a,
	}
	errNotInitial = errors.New("reality2: not a QUIC v1 Initial packet")
	errMalformed  = errors.New("reality2: malformed QUIC packet")
)

// isQUICInitial reports whether b starts with a QUIC v1 Initial long header.
func isQUICInitial(b []byte) bool {
	if len(b) < 7 || b[0]&0x80 == 0 {
		return false
	}
	if binary.BigEndian.Uint32(b[1:5]) != quicV1Version {
		return false
	}
	return (b[0]>>4)&0x03 == 0
}

// initialPacket is what the gate needs from one client Initial: the connection IDs and the
// CRYPTO fragments it carries, keyed by their stream offset.
type initialPacket struct {
	dcid  []byte
	scid  []byte
	frags map[int][]byte
}

// parseClientInitial decrypts a client QUIC v1 Initial packet and extracts its CRYPTO frames.
func parseClientInitial(pkt []byte) (*initialPacket, error) {
	if !isQUICInitial(pkt) {
		return nil, errNotInitial
	}
	off := 5
	dcidLen := int(pkt[off])
	off++
	if dcidLen > 20 || len(pkt) < off+dcidLen+1 {
		return nil, errMalformed
	}
	dcid := pkt[off : off+dcidLen]
	off += dcidLen
	scidLen := int(pkt[off])
	off++
	if scidLen > 20 || len(pkt) < off+scidLen {
		return nil, errMalformed
	}
	scid := pkt[off : off+scidLen]
	off += scidLen

	tokLen, n, ok := readVarint(pkt[off:])
	if !ok || uint64(len(pkt)-off-n) < tokLen {
		return nil, errMalformed
	}
	off += n + int(tokLen)
	length, n, ok := readVarint(pkt[off:])
	if !ok || length < 20 {
		return nil, errMalformed
	}
	off += n
	pnOff := off
	end := pnOff + int(length)
	if end > len(pkt) || pnOff+20 > len(pkt) {
		return nil, errMalformed
	}

	// client_initial_secret = HKDF-Extract(salt, DCID), then the client handshake keys.
	secret := hkdfExpandLabel(hkdf.Extract(sha256.New, dcid, quicV1Salt), "client in", 32)
	key := hkdfExpandLabel(secret, "quic key", 16)
	iv := hkdfExpandLabel(secret, "quic iv", 12)
	hp := hkdfExpandLabel(secret, "quic hp", 16)

	hpb, err := aes.NewCipher(hp)
	if err != nil {
		return nil, errMalformed
	}
	mask := make([]byte, 16)
	hpb.Encrypt(mask, pkt[pnOff+4:pnOff+20])
	hdr0 := pkt[0] ^ (mask[0] & 0x0f)
	pnLen := int(hdr0&0x03) + 1
	if pnOff+pnLen > end {
		return nil, errMalformed
	}
	hdr := append([]byte(nil), pkt[:pnOff+pnLen]...)
	hdr[0] = hdr0
	var pn uint64
	for i := 0; i < pnLen; i++ {
		hdr[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(hdr[pnOff+i])
	}
	nonce := make([]byte, 12)
	copy(nonce, iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, errMalformed
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, errMalformed
	}
	plain, err := aead.Open(nil, nonce, pkt[pnOff+pnLen:end], hdr)
	if err != nil {
		return nil, errMalformed
	}
	return &initialPacket{
		dcid:  append([]byte(nil), dcid...),
		scid:  append([]byte(nil), scid...),
		frags: cryptoFrames(plain),
	}, nil
}

// cryptoFrames collects the CRYPTO frames of one Initial's plaintext. It stops at the first
// frame it does not understand: everything after an unknown frame is not needed for the
// ClientHello, and a partial reassembly already fails the token check.
func cryptoFrames(plain []byte) map[int][]byte {
	parts := map[int][]byte{}
	for p := 0; p < len(plain); {
		switch plain[p] {
		case 0x00, 0x01: // PADDING, PING
			p++
			continue
		case 0x06: // CRYPTO
			p++
			off, n, ok := readVarint(plain[p:])
			if !ok {
				return parts
			}
			p += n
			length, n, ok := readVarint(plain[p:])
			if !ok || uint64(len(plain)-p-n) < length {
				return parts
			}
			p += n
			parts[int(off)] = append([]byte(nil), plain[p:p+int(length)]...)
			p += int(length)
		default:
			return parts
		}
	}
	return parts
}

// mergeCrypto keeps the first fragment seen at an offset, so a retransmitted Initial is
// idempotent.
func mergeCrypto(dst, src map[int][]byte) map[int][]byte {
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

// assemblesClientHello returns the contiguous ClientHello from offset 0, and whether the
// handshake message is complete (its 24-bit length is satisfied).
func assembledClientHello(frags map[int][]byte) ([]byte, bool) {
	var out []byte
	for off := 0; ; {
		b, ok := frags[off]
		if !ok || len(b) == 0 {
			break
		}
		if len(out)+len(b) > maxInitialCrypto {
			return nil, false
		}
		out = append(out, b...)
		off += len(b)
	}
	if len(out) < 4 || out[0] != 1 {
		return nil, false
	}
	n := int(out[1])<<16 | int(out[2])<<8 | int(out[3])
	if n > maxInitialCrypto || len(out) < 4+n {
		return nil, false
	}
	return out[:4+n], true
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

func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := binary.BigEndian.AppendUint16(nil, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, length)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, secret, info), out)
	return out
}
