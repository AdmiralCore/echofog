package censor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"

	"golang.org/x/crypto/hkdf"
)

// QUIC v1 Initial packets are encrypted with keys anyone can derive from the packet itself
// (RFC 9001 §5.2): that is how GFW and TSPU read the SNI of a QUIC connection, and how they tell
// real QUIC from noise that only looks like it.

var quicV1Salt = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}

const quicV1 = 0x00000001

func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 4+len(full))
	info = binary.BigEndian.AppendUint16(info, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0) // empty context
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

// quicInitial is what the Initial packets of one datagram carried.
type quicInitial struct {
	isLongHeader bool
	version      uint32
	decrypted    bool           // an Initial opened with the keys of its own DCID: real QUIC
	crypto       map[int][]byte // CRYPTO frame data by offset
}

var errNotInitial = errors.New("not a QUIC v1 Initial")

// openInitials reads the client Initial packets coalesced in one datagram.
func openInitials(dgram []byte) quicInitial {
	res := quicInitial{crypto: map[int][]byte{}}
	if len(dgram) == 0 || dgram[0]&0x80 == 0 {
		return res
	}
	res.isLongHeader = true
	for len(dgram) > 0 && dgram[0]&0x80 != 0 {
		n, err := openOne(dgram, &res)
		if err != nil || n <= 0 {
			return res
		}
		dgram = dgram[n:]
	}
	return res
}

// openOne opens the Initial at the start of b and returns its length.
func openOne(b []byte, res *quicInitial) (int, error) {
	if len(b) < 7 {
		return 0, errNotInitial
	}
	res.version = binary.BigEndian.Uint32(b[1:5])
	if res.version != quicV1 || (b[0]>>4)&0x3 != 0 {
		return 0, errNotInitial
	}
	off := 5
	dcidLen := int(b[off])
	off++
	if dcidLen > 20 || len(b) < off+dcidLen+1 {
		return 0, errNotInitial
	}
	dcid := b[off : off+dcidLen]
	off += dcidLen
	scidLen := int(b[off])
	off++
	if scidLen > 20 || len(b) < off+scidLen {
		return 0, errNotInitial
	}
	off += scidLen
	tokLen, n, ok := readVarint(b[off:])
	if !ok || uint64(len(b)-off-n) < tokLen {
		return 0, errNotInitial
	}
	off += n + int(tokLen)
	length, n, ok := readVarint(b[off:])
	if !ok {
		return 0, errNotInitial
	}
	off += n
	pnOff := off
	end := pnOff + int(length)
	if length < 20 || end > len(b) {
		return 0, errNotInitial
	}

	initial := hkdf.Extract(sha256.New, dcid, quicV1Salt)
	client := hkdfExpandLabel(initial, "client in", 32)
	key := hkdfExpandLabel(client, "quic key", 16)
	iv := hkdfExpandLabel(client, "quic iv", 12)
	hp := hkdfExpandLabel(client, "quic hp", 16)

	hb, err := aes.NewCipher(hp)
	if err != nil {
		return 0, err
	}
	sample := b[pnOff+4 : pnOff+20]
	var mask [16]byte
	hb.Encrypt(mask[:], sample)
	hdr := append([]byte(nil), b[:pnOff+4]...)
	hdr[0] ^= mask[0] & 0x0f
	pnLen := int(hdr[0]&0x03) + 1
	var pn uint64
	for i := 0; i < pnLen; i++ {
		hdr[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(hdr[pnOff+i])
	}
	hdr = hdr[:pnOff+pnLen]

	kb, err := aes.NewCipher(key)
	if err != nil {
		return 0, err
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return 0, err
	}
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	plain, err := aead.Open(nil, nonce, b[pnOff+pnLen:end], hdr)
	if err != nil {
		return end, errNotInitial
	}
	res.decrypted = true
	readCryptoFrames(plain, res.crypto)
	return end, nil
}

// readCryptoFrames collects the CRYPTO frames of a decrypted Initial payload.
func readCryptoFrames(p []byte, into map[int][]byte) {
	for len(p) > 0 {
		t := p[0]
		switch {
		case t == 0x00 || t == 0x01: // PADDING, PING
			p = p[1:]
		case t == 0x06: // CRYPTO
			o, n1, ok1 := readVarint(p[1:])
			if !ok1 {
				return
			}
			l, n2, ok2 := readVarint(p[1+n1:])
			start := 1 + n1 + n2
			if !ok2 || uint64(len(p)-start) < l {
				return
			}
			into[int(o)] = append([]byte(nil), p[start:start+int(l)]...)
			p = p[start+int(l):]
		case t == 0x02 || t == 0x03: // ACK: not expected from a client's first Initials
			return
		default:
			return
		}
	}
}

// assembleCrypto joins CRYPTO data from offset 0 for as long as it is contiguous.
func assembleCrypto(parts map[int][]byte) []byte {
	offs := make([]int, 0, len(parts))
	for o := range parts {
		offs = append(offs, o)
	}
	sort.Ints(offs)
	var out []byte
	for _, o := range offs {
		if o > len(out) {
			break
		}
		if end := o + len(parts[o]); end > len(out) {
			out = append(out, parts[o][len(out)-o:]...)
		}
	}
	return out
}
