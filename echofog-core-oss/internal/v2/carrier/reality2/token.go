package reality2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/hkdf"
)

// The token of wire.md §3.2. k_tok is derived from the ECDH secret of the client's key share
// and the responder's masking key, so both sides compute it without ever sending it; the token
// is an AES-256-GCM box whose associated data binds it to CH0 (the ClientHello with a zeroed
// random) and whose info binds the key to the address the client dialed.
const (
	// TokenKeyLabel is the HKDF info prefix of the token key (registries.md §7.2).
	TokenKeyLabel = "echofog/reality2/token-key"
	// HelloLabel is the hash prefix of the ClientHello binding (registries.md §7.2).
	HelloLabel = "echofog/reality2/hello"

	// TokenLen is the token size: a 16-byte plaintext and a 16-byte GCM tag, the size of an
	// ordinary TLS ClientHello random.
	TokenLen = 32
	// TokenPlaintextLen is the plaintext: version byte, timestamp, zero padding.
	TokenPlaintextLen = 16
	// TokenVersion is the token plaintext's first byte.
	TokenVersion = 0x01

	// TokenTSWindow is how far the token timestamp may be from the server's clock
	// (wire.md §3.3 step 2).
	TokenTSWindow = 300 * time.Second

	// randomOffset is where ClientHello.random starts in the handshake message: handshake type
	// (1) and 3-byte length, then legacy_version (2).
	randomOffset = 6
	// maxAddressLen is the largest address the token info can carry (u8 length).
	maxAddressLen = 255
)

// TLS named groups of the shares the token can be bound to (RFC 8446, draft-ietf-tls-ecdhe-mlkem).
const (
	groupX25519         = 0x001d
	groupX25519MLKEM768 = 0x11ec
	// mlkemEncapKeyLen is the ML-KEM-768 encapsulation key, the deterministic prefix of the
	// hybrid share; the trailing 32 bytes are the X25519 public key.
	mlkemEncapKeyLen = 1184
	// extKeyShare is the TLS 1.3 key_share extension.
	extKeyShare = 51
)

// Token errors. They never distinguish "no such masking key" from "wrong address" from "wrong
// timestamp": a stranger sees the same cover answer in every case.
var (
	// ErrHello reports a ClientHello that is incomplete or malformed.
	ErrHello = errors.New("reality2: malformed ClientHello")
	// ErrToken reports a token that does not open or has the wrong shape.
	ErrToken = errors.New("reality2: token rejected")
	// ErrTokenStale reports a valid token outside the timestamp window.
	ErrTokenStale = errors.New("reality2: token timestamp outside the window")
	// ErrAddress reports an address that the token info cannot carry.
	ErrAddress = errors.New("reality2: unusable address")
)

// Hello is a parsed ClientHello: the 32-byte token it carries in random, CH0 (the same message
// with the random zeroed, the binding of wire.md §3.2) and C, the client's X25519 public key.
type Hello struct {
	Token []byte
	CH0   []byte
	C     []byte
}

// AddressString is the `a` of wire.md §3.2: host:port exactly as the card writes it. IPv6
// literals get the brackets net.JoinHostPort adds; both sides use this function, so the token is
// stable.
func AddressString(host string, port uint32) string {
	return net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10))
}

// TokenKey returns k_tok:
//
//	HKDF-SHA256(ikm = z, salt = empty,
//	            info = "echofog/reality2/token-key" ‖ maskPub ‖ C ‖ u8(len(a)) ‖ a, L = 32)
//
// z is the X25519 shared secret, maskPub the responder's masking public key and C the client's
// X25519 public key. nil means the inputs or the address are unusable.
func TokenKey(z, maskPub, C []byte, addr string) []byte {
	if len(z) != 32 || len(maskPub) != 32 || len(C) != 32 || len(addr) == 0 || len(addr) > maxAddressLen {
		return nil
	}
	info := make([]byte, 0, len(TokenKeyLabel)+32+32+1+len(addr))
	info = append(info, TokenKeyLabel...)
	info = append(info, maskPub...)
	info = append(info, C...)
	info = append(info, byte(len(addr)))
	info = append(info, addr...)
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, z, nil, info), key); err != nil {
		return nil
	}
	return key
}

// HelloAAD is aad = SHA-256("echofog/reality2/hello" ‖ CH0).
func HelloAAD(ch0 []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(HelloLabel))
	h.Write(ch0)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// SealToken builds the 32-byte token: AES-256-GCM(k_tok, nonce = 0^12,
// pt = 0x01 ‖ ts(u32) ‖ 0^11, aad). The nonce is fixed because k_tok is used exactly once: the
// client's X25519 key share is fresh for every ClientHello (wire.md §3.1, §3.2).
func SealToken(kTok []byte, aad [32]byte, ts uint32) ([]byte, error) {
	if len(kTok) != 32 {
		return nil, fmt.Errorf("%w: key length %d", ErrToken, len(kTok))
	}
	aead, err := tokenAEAD(kTok)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, TokenPlaintextLen)
	pt[0] = TokenVersion
	binary.BigEndian.PutUint32(pt[1:5], ts)
	var nonce [12]byte
	return aead.Seal(nil, nonce[:], pt, aad[:]), nil
}

// OpenToken opens a token and returns its timestamp. It checks the version byte, the zero
// padding and |ts − now| ≤ 300 s; a replay is a separate question, answered by ReplayCache.
func OpenToken(kTok []byte, aad [32]byte, token []byte, now time.Time) (uint32, error) {
	if len(kTok) != 32 || len(token) != TokenLen {
		return 0, ErrToken
	}
	aead, err := tokenAEAD(kTok)
	if err != nil {
		return 0, err
	}
	var nonce [12]byte
	pt, err := aead.Open(nil, nonce[:], token, aad[:])
	if err != nil || len(pt) != TokenPlaintextLen {
		return 0, ErrToken
	}
	if pt[0] != TokenVersion {
		return 0, ErrToken
	}
	for _, b := range pt[5:] {
		if b != 0 {
			return 0, ErrToken
		}
	}
	ts := binary.BigEndian.Uint32(pt[1:5])
	delta := now.Unix() - int64(ts)
	if delta < 0 {
		delta = -delta
	}
	if delta > int64(TokenTSWindow/time.Second) {
		return 0, ErrTokenStale
	}
	return ts, nil
}

func tokenAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToken, err)
	}
	return cipher.NewGCM(block)
}

// ParseHello parses a ClientHello handshake message (type byte first, no record or QUIC
// framing) and returns the token from its random, CH0 and C. C is the X25519 key share entry,
// or, when there is none, the last 32 bytes of the X25519MLKEM768 entry (wire.md §3.1).
//
// Any malformed or truncated input is ErrHello; the parser never panics on wire data.
func ParseHello(msg []byte) (*Hello, error) { return parseHello(msg, false) }

// ParseHelloTCP is ParseHello for reality2-tcp (wire.md §4.1): the token is the 32-byte
// legacy_session_id and ClientHello.random stays random, so CH0 is the message with the
// session id zeroed. A hello whose session id is not 32 bytes carries no token (ErrHello).
func ParseHelloTCP(msg []byte) (*Hello, error) { return parseHello(msg, true) }

func parseHello(msg []byte, inSessionID bool) (*Hello, error) {
	if len(msg) < 4+2+32 || msg[0] != 1 {
		return nil, ErrHello
	}
	bodyLen := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
	end := 4 + bodyLen
	if bodyLen < 2+32 || end > len(msg) {
		return nil, ErrHello
	}
	msg = msg[:end]

	p := 4 + 2 // past the handshake header and legacy_version
	token := append([]byte(nil), msg[p:p+32]...)
	p += 32
	if p >= len(msg) {
		return nil, ErrHello
	}
	idLen := int(msg[p])
	p++
	if p+idLen > len(msg) {
		return nil, ErrHello
	}
	sidOff := p
	if inSessionID {
		if idLen != 32 {
			return nil, ErrHello
		}
		token = append([]byte(nil), msg[sidOff:sidOff+32]...)
	}
	p += idLen // legacy_session_id (QUIC: empty)
	if p+2 > len(msg) {
		return nil, ErrHello
	}
	p += 2 + (int(msg[p])<<8 | int(msg[p+1])) // cipher suites
	if p >= len(msg) {
		return nil, ErrHello
	}
	p += 1 + int(msg[p]) // compression methods
	if p+2 > len(msg) {
		return nil, ErrHello
	}
	extEnd := p + 2 + (int(msg[p])<<8 | int(msg[p+1]))
	if extEnd > len(msg) || extEnd < p+2 {
		return nil, ErrHello
	}
	p += 2

	var x25519, hybrid []byte
	for p+4 <= extEnd {
		typ := int(msg[p])<<8 | int(msg[p+1])
		l := int(msg[p+2])<<8 | int(msg[p+3])
		p += 4
		if p+l > extEnd {
			return nil, ErrHello
		}
		if typ == extKeyShare {
			var ok bool
			x25519, hybrid, ok = parseKeyShare(msg[p : p+l])
			if !ok {
				return nil, ErrHello
			}
		}
		p += l
	}
	C := x25519
	if C == nil {
		C = hybrid
	}
	if C == nil {
		return nil, ErrHello
	}

	ch0 := append([]byte(nil), msg...)
	if inSessionID {
		clear(ch0[sidOff : sidOff+32])
	} else {
		clear(ch0[randomOffset : randomOffset+32])
	}
	return &Hello{Token: token, CH0: ch0, C: C}, nil
}

// parseKeyShare reads a key_share extension body and returns the X25519 share and, when
// present, the X25519MLKEM768 share's trailing X25519 key.
func parseKeyShare(body []byte) (x25519, hybrid []byte, ok bool) {
	if len(body) < 2 {
		return nil, nil, false
	}
	n := int(body[0])<<8 | int(body[1])
	if n > len(body)-2 {
		return nil, nil, false
	}
	for p := 2; p+4 <= 2+n; {
		group := int(body[p])<<8 | int(body[p+1])
		l := int(body[p+2])<<8 | int(body[p+3])
		p += 4
		if p+l > 2+n {
			return nil, nil, false
		}
		switch group {
		case groupX25519:
			if l != 32 {
				return nil, nil, false
			}
			x25519 = append([]byte(nil), body[p:p+l]...)
		case groupX25519MLKEM768:
			const hybridLen = mlkemEncapKeyLen + 32
			if l != hybridLen {
				return nil, nil, false
			}
			hybrid = append([]byte(nil), body[p+l-32:p+l]...)
		}
		p += l
	}
	return x25519, hybrid, true
}
