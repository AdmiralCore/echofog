package handshake

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/echofog/echofog-core/internal/v2/noise"
)

// Labels (registries.md §7.3, §7.4).
const (
	labelExporter      = "EXPORTER-echofog/hs2/channel"
	labelCircuit       = "echofog/hs2/circuit"
	labelPrologue      = "echofog/hs2/prologue"
	labelPassID        = "echofog/hs2/pass-id"
	labelAccessProof   = "echofog/hs2/access-proof"
	labelPuzzle        = "echofog/hs2/puzzle"
	labelCookie        = "echofog/hs2/cookie"
	labelSessionSecret = "echofog/hs2/session-secret"
	labelSessionID     = "echofog/session2/id"
)

func hmacSHA256(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func sha256Of(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// TLSBinding returns cb for a carrier: the TLS exporter with label EXPORTER-echofog/hs2/channel,
// empty context, length 32 (handshake.md §3.2). The connection must be TLS 1.3 without early data.
func TLSBinding(cs *tls.ConnectionState) ([]byte, error) {
	if cs == nil || cs.Version != tls.VersionTLS13 {
		return nil, errors.New("handshake: channel binding needs TLS 1.3")
	}
	// Resumption is allowed; TLS early data is not (handshake.md D5), and crypto/tls never
	// accepts it. QUIC carriers must refuse 0-RTT themselves.
	return cs.ExportKeyingMaterial(labelExporter, []byte{}, 32)
}

// CircuitBinding returns cb for a relay stream circuit (handshake.md §3.2).
func CircuitBinding(relayNodeID []byte, cid uint64) []byte {
	return sha256Of([]byte(labelCircuit), relayNodeID, be64(cid))
}

// Prologue returns "echofog/hs2/prologue" ‖ cb(32) ‖ rid(32) ‖ u8(len(hello)) ‖ hello. A nil
// rid means 32 zero bytes (no bound responder ID).
func Prologue(cb, rid, hello []byte) []byte {
	if len(cb) != 32 {
		panic("handshake: cb must be 32 bytes")
	}
	if rid == nil {
		rid = make([]byte, 32)
	}
	if len(rid) != 32 || len(hello) > 255 {
		panic("handshake: bad rid or hello")
	}
	b := make([]byte, 0, len(labelPrologue)+65+len(hello))
	b = append(b, labelPrologue...)
	b = append(b, cb...)
	b = append(b, rid...)
	b = append(b, byte(len(hello)))
	return append(b, hello...)
}

// PassID returns HMAC-SHA256(S, "echofog/hs2/pass-id")[0:8].
func PassID(secret []byte) []byte { return hmacSHA256(secret, []byte(labelPassID))[:8] }

// AccessProof returns HMAC-SHA256(S, "echofog/hs2/access-proof" ‖ h1).
func AccessProof(secret, h1 []byte) []byte {
	return hmacSHA256(secret, []byte(labelAccessProof), h1)
}

// Cookie returns HMAC-SHA256(R, "echofog/hs2/cookie" ‖ cb ‖ u64(minute))[0:16].
func Cookie(r, cb []byte, minute uint64) []byte {
	return hmacSHA256(r, []byte(labelCookie), cb, be64(minute))[:16]
}

// PuzzleHash returns SHA-256("echofog/hs2/puzzle" ‖ cookie ‖ cb ‖ u64(nonce)).
func PuzzleHash(cookie, cb []byte, nonce uint64) []byte {
	return sha256Of([]byte(labelPuzzle), cookie, cb, be64(nonce))
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x != 0 {
			return n + bits.LeadingZeros8(x)
		}
		n += 8
	}
	return n
}

// PuzzleOK reports whether nonce solves the puzzle at the difficulty.
func PuzzleOK(cookie, cb []byte, nonce uint64, difficulty uint8) bool {
	return leadingZeroBits(PuzzleHash(cookie, cb, nonce)) >= int(difficulty)
}

// puzzleCheckEvery is how many hashes pass between context checks (a power of two).
const puzzleCheckEvery = 1 << 16

// SolvePuzzle returns the smallest nonce that solves the puzzle. Difficulty above 24 is refused.
func SolvePuzzle(cookie, cb []byte, difficulty uint8) (uint64, error) {
	return SolvePuzzleContext(context.Background(), cookie, cb, difficulty)
}

// SolvePuzzleContext is SolvePuzzle that stops when ctx is done.
func SolvePuzzleContext(ctx context.Context, cookie, cb []byte, difficulty uint8) (uint64, error) {
	if difficulty > MaxDifficulty {
		return 0, ErrRetry
	}
	for nonce := uint64(0); ; nonce++ {
		if nonce&(puzzleCheckEvery-1) == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if PuzzleOK(cookie, cb, nonce, difficulty) {
			return nonce, nil
		}
	}
}

// SessionSecret returns Noise HKDF(ck, "echofog/hs2/session-secret", 2).o1 (handshake.md §9).
func SessionSecret(ck []byte) []byte {
	o1, o2 := noise.HKDF2(ck, []byte(labelSessionSecret))
	for i := range o2 {
		o2[i] = 0
	}
	return o1
}

// SessionID returns HKDF-SHA256(ikm = session_secret, salt = h, info = "echofog/session2/id", 16).
func SessionID(sessionSecret, h []byte) []byte {
	out, err := hkdf.Key(sha256.New, sessionSecret, h, labelSessionID, 16)
	if err != nil {
		panic(err) // unreachable: fixed length
	}
	return out
}
