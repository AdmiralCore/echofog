package handshake

import (
	"encoding/binary"
	"errors"
)

// HS_HELLO flags (handshake.md §3.1).
const (
	FlagPuzzle      = 0x01 // a puzzle solution follows
	FlagResponderID = 0x02 // the prologue binds an expected responder device ID
)

// ErrHello is returned for a malformed HS_HELLO payload.
var ErrHello = errors.New("handshake: malformed HS_HELLO")

// Hello is the HS_HELLO payload.
type Hello struct {
	Offered []uint16
	Chosen  uint16
	Flags   uint8
	Cookie  []byte // 16 bytes when Flags&FlagPuzzle
	Nonce   uint64
}

func (h *Hello) check() error {
	if len(h.Offered) < 1 || len(h.Offered) > 8 {
		return ErrHello
	}
	// B3: at most one GREASE value, not only GREASE, and chosen is the first non-GREASE entry.
	grease := 0
	for _, s := range h.Offered {
		if IsGrease(s) {
			grease++
		}
	}
	if grease > 1 || grease == len(h.Offered) || h.Chosen != firstNonGrease(h.Offered) {
		return ErrHello
	}
	found := false
	for i, s := range h.Offered {
		if s == h.Chosen {
			found = true
		}
		for _, t := range h.Offered[:i] {
			if s == t {
				return ErrHello
			}
		}
	}
	if !found || IsGrease(h.Chosen) {
		return ErrHello
	}
	if h.Flags&FlagPuzzle != 0 && len(h.Cookie) != 16 {
		return ErrHello
	}
	return nil
}

func firstNonGrease(list []uint16) uint16 {
	for _, s := range list {
		if !IsGrease(s) {
			return s
		}
	}
	return 0
}

// Marshal encodes the payload.
func (h *Hello) Marshal() ([]byte, error) {
	if err := h.check(); err != nil {
		return nil, err
	}
	b := []byte{byte(len(h.Offered))}
	for _, s := range h.Offered {
		b = binary.BigEndian.AppendUint16(b, s)
	}
	b = binary.BigEndian.AppendUint16(b, h.Chosen)
	b = append(b, h.Flags)
	if h.Flags&FlagPuzzle != 0 {
		b = append(b, h.Cookie...)
		b = binary.BigEndian.AppendUint64(b, h.Nonce)
	}
	return b, nil
}

// ParseHello decodes an HS_HELLO payload. Reserved flag bits are kept but ignored.
func ParseHello(b []byte) (*Hello, error) {
	if len(b) < 1 || len(b) > MaxHelloLen {
		return nil, ErrHello
	}
	n := int(b[0])
	if n < 1 || n > 8 || len(b) < 1+2*n+3 {
		return nil, ErrHello
	}
	h := &Hello{Offered: make([]uint16, n)}
	for i := 0; i < n; i++ {
		h.Offered[i] = binary.BigEndian.Uint16(b[1+2*i:])
	}
	p := 1 + 2*n
	h.Chosen = binary.BigEndian.Uint16(b[p:])
	h.Flags = b[p+2]
	rest := b[p+3:]
	if h.Flags&FlagPuzzle != 0 {
		if len(rest) != 24 {
			return nil, ErrHello
		}
		h.Cookie = append([]byte(nil), rest[:16]...)
		h.Nonce = binary.BigEndian.Uint64(rest[16:])
	} else if len(rest) != 0 {
		return nil, ErrHello
	}
	if err := h.check(); err != nil {
		return nil, err
	}
	return h, nil
}

// Retry is the HS_RETRY payload (handshake.md §6).
type Retry struct {
	Cookie     [16]byte
	Difficulty uint8
}

// MaxDifficulty is the highest puzzle difficulty an initiator accepts.
const MaxDifficulty = 24

// ErrRetry is returned for a malformed HS_RETRY payload or a difficulty above 24.
var ErrRetry = errors.New("handshake: malformed HS_RETRY")

// Marshal encodes the payload.
func (r *Retry) Marshal() []byte { return append(r.Cookie[:len(r.Cookie):len(r.Cookie)], r.Difficulty) }

// ParseRetry decodes an HS_RETRY payload and refuses a difficulty above 24.
func ParseRetry(b []byte) (*Retry, error) {
	if len(b) != 17 || b[16] > MaxDifficulty {
		return nil, ErrRetry
	}
	r := &Retry{Difficulty: b[16]}
	copy(r.Cookie[:], b)
	return r, nil
}
