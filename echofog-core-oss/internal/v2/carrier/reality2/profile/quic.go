package profile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// QUIC v1 Initial secrets (RFC 9001 §5.2). The server's Initial packets are protected with keys
// derived from the client's first destination connection ID, so an observer can read the
// ServerHello out of them; nothing later in the handshake is readable.
var quicV1Salt = []byte{
	0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17,
	0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a,
}

const quicV1 = 1

type quicPacket struct {
	kind    string
	version uint32
	dcid    []byte
	scid    []byte
	raw     []byte // the whole packet
	pnOff   int    // offset of the packet number (Initial only)
	length  int    // value of the Length field (Initial only)
}

// splitDatagram cuts a datagram into its coalesced packets.
func splitDatagram(d []byte) ([]quicPacket, error) {
	var out []quicPacket
	for len(d) > 0 {
		if len(out) > 8 {
			return nil, ErrTooBig
		}
		b0 := d[0]
		if b0&0x80 == 0 { // short header: the rest of the datagram
			out = append(out, quicPacket{kind: "1rtt", raw: d})
			return out, nil
		}
		if len(d) < 7 {
			return nil, fmt.Errorf("%w: short QUIC long header", ErrMalformed)
		}
		p := quicPacket{version: binary.BigEndian.Uint32(d[1:5])}
		i := 5
		dl := int(d[i])
		i++
		if dl > 20 || len(d) < i+dl+1 {
			return nil, fmt.Errorf("%w: QUIC DCID", ErrMalformed)
		}
		p.dcid = d[i : i+dl]
		i += dl
		sl := int(d[i])
		i++
		if sl > 20 || len(d) < i+sl {
			return nil, fmt.Errorf("%w: QUIC SCID", ErrMalformed)
		}
		p.scid = d[i : i+sl]
		i += sl
		if p.version == 0 {
			p.kind, p.raw = "vneg", d
			return append(out, p), nil
		}
		switch (b0 >> 4) & 3 {
		case 3:
			p.kind, p.raw = "retry", d
			return append(out, p), nil
		case 0:
			p.kind = "initial"
			tl, n, ok := varint(d[i:])
			if !ok || uint64(len(d)-i-n) < tl {
				return nil, fmt.Errorf("%w: Initial token", ErrMalformed)
			}
			i += n + int(tl)
		case 1:
			p.kind = "0rtt"
		case 2:
			p.kind = "handshake"
		}
		l, n, ok := varint(d[i:])
		if !ok || uint64(len(d)-i-n) < l {
			return nil, fmt.Errorf("%w: QUIC Length", ErrMalformed)
		}
		i += n
		p.pnOff, p.length = i, int(l)
		end := i + int(l)
		p.raw = d[:end]
		out = append(out, p)
		d = d[end:]
	}
	return out, nil
}

func varint(b []byte) (v uint64, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	n = 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0, false
	}
	v = uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, true
}

func expandLabel(secret []byte, label string, n int) []byte {
	full := "tls13 " + label
	info := binary.BigEndian.AppendUint16(nil, uint16(n))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	out := make([]byte, n)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, secret, info), out)
	return out
}

// serverInitialCrypto decrypts a server Initial packet and returns its CRYPTO fragments by
// offset. A ServerHello with a post-quantum key share does not fit one packet.
func serverInitialCrypto(p quicPacket, clientDCID []byte) (map[int][]byte, error) {
	secret := hkdf.Extract(sha256.New, clientDCID, quicV1Salt)
	s := expandLabel(secret, "server in", 32)
	key, iv, hp := expandLabel(s, "quic key", 16), expandLabel(s, "quic iv", 12), expandLabel(s, "quic hp", 16)

	pkt := append([]byte(nil), p.raw...)
	if p.pnOff+4+16 > len(pkt) {
		return nil, fmt.Errorf("%w: Initial too short for a header-protection sample", ErrMalformed)
	}
	blk, err := aes.NewCipher(hp)
	if err != nil {
		return nil, err
	}
	var mask [16]byte
	blk.Encrypt(mask[:], pkt[p.pnOff+4:p.pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&3) + 1
	if p.pnOff+pnLen > len(pkt) {
		return nil, fmt.Errorf("%w: packet number", ErrMalformed)
	}
	var pn uint64
	for i := 0; i < pnLen; i++ {
		pkt[p.pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(pkt[p.pnOff+i])
	}
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[11-i] ^= byte(pn >> (8 * i))
	}
	blk2, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk2)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, pkt[p.pnOff+pnLen:], pkt[:p.pnOff+pnLen])
	if err != nil {
		return nil, fmt.Errorf("%w: server Initial does not decrypt", ErrMalformed)
	}
	return cryptoFrags(plain), nil
}

// cryptoFrags returns the CRYPTO frames of a decrypted Initial payload by offset.
func cryptoFrags(plain []byte) map[int][]byte {
	frags := map[int][]byte{}
	for len(plain) > 0 {
		t, n, ok := varint(plain)
		if !ok {
			break
		}
		plain = plain[n:]
		switch t {
		case 0x00, 0x01: // PADDING, PING
		case 0x02, 0x03: // ACK
			var v [4]uint64
			for i := range v {
				x, m, ok := varint(plain)
				if !ok {
					return frags
				}
				v[i], plain = x, plain[m:]
			}
			for r := uint64(0); r < v[2]; r++ {
				for k := 0; k < 2; k++ {
					_, m, ok := varint(plain)
					if !ok {
						return frags
					}
					plain = plain[m:]
				}
			}
			if t == 0x03 {
				for k := 0; k < 3; k++ {
					_, m, ok := varint(plain)
					if !ok {
						return frags
					}
					plain = plain[m:]
				}
			}
		case 0x06: // CRYPTO
			off, m, ok := varint(plain)
			if !ok {
				return frags
			}
			plain = plain[m:]
			l, m, ok := varint(plain)
			if !ok || uint64(len(plain)-m) < l || off > maxCaptureBytes {
				return frags
			}
			frags[int(off)] = plain[m : m+int(l)]
			plain = plain[m+int(l):]
		default:
			return frags
		}
	}
	return frags
}

// joinCrypto returns the contiguous CRYPTO data from offset 0.
func joinCrypto(frags map[int][]byte) []byte {
	var out []byte
	for {
		f, ok := frags[len(out)]
		if !ok || len(f) == 0 {
			return out
		}
		out = append(out, f...)
	}
}

// ObserveQUIC turns the datagrams of one QUIC connection, in order, into a Vector. The first
// client datagram must be the client's first Initial: its destination connection ID keys the
// server's Initial packets, which carry the ServerHello. The capture must end before the
// client's first request.
func ObserveQUIC(events []Event) (Vector, error) {
	v := Vector{Transport: QUIC}
	total := 0
	for _, e := range events {
		total += len(e.Data)
	}
	if total > maxCaptureBytes {
		return v, ErrTooBig
	}
	var clientDCID []byte
	haveClient := false
	frags := map[int][]byte{}
	unit := 0
	sawSCID := false
	for _, e := range events {
		if !e.FromServer {
			pk, err := splitDatagram(e.Data)
			if err == nil && len(pk) > 0 && pk[0].kind == "initial" {
				// After a Retry the client's Initial carries a new DCID, which keys the server's.
				clientDCID, haveClient = append([]byte(nil), pk[0].dcid...), true
			} else if !haveClient {
				return v, fmt.Errorf("%w: first client datagram is not an Initial", ErrMalformed)
			}
			continue
		}
		if !haveClient {
			return v, fmt.Errorf("%w: server datagram before the client's Initial", ErrMalformed)
		}
		pk, err := splitDatagram(e.Data)
		if err != nil {
			return v, err
		}
		for _, p := range pk {
			it := Item{Kind: p.kind, Unit: unit, Len: len(p.raw)}
			if p.kind == "1rtt" {
				v.Post = append(v.Post, it)
			} else {
				v.Flight = append(v.Flight, it)
			}
			if !sawSCID && p.kind != "1rtt" && p.kind != "vneg" {
				v.SCIDLen, sawSCID = len(p.scid), true
			}
			if p.kind == "initial" && p.version == quicV1 {
				if c, err := serverInitialCrypto(p, clientDCID); err == nil {
					for off, d := range c {
						frags[off] = d
					}
				}
			}
		}
		unit++
		if len(v.Flight)+len(v.Post) > maxItems {
			return v, ErrTooBig
		}
	}
	// A QUIC ServerHello can be preceded by nothing else in the Initial CRYPTO stream.
	return finishHello(v, joinCrypto(frags))
}
