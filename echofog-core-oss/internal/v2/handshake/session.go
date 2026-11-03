package handshake

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/noise"
)

// Errors of the handshake state machines.
var (
	ErrState      = errors.New("handshake: call out of order")
	ErrClockSkew  = errors.New("handshake: peer clock differs by more than 600 s")
	ErrVersion    = errors.New("handshake: peer proto_ver below 200")
	ErrPeerCard   = errors.New("handshake: peer card invalid or does not match the handshake")
	ErrKeyBinding = errors.New("handshake: hs key already bound to another device")
	ErrSelection  = errors.New("handshake: responder selected something that was not offered")
	ErrRetries    = errors.New("handshake: too many retries")
	ErrConfig     = errors.New("handshake: invalid configuration")
)

// Timeouts of handshake.md §11. The state machines do not run timers; the carrier code applies
// these to its reads.
const (
	MessageTimeout   = 5 * time.Second
	HandshakeTimeout = 10 * time.Second
	MaxClockSkew     = 600 * time.Second
	EarlyWindow      = 60 * time.Second
	ReplayWindow     = 180 * time.Second
)

// Session is the result of a completed handshake (handshake.md §9).
type Session struct {
	Suite     uint16
	Initiator bool
	// KI2R and KR2I are the hybrid Noise transport keys, used only for sealed records and
	// datagrams (wire.md §8.4).
	KI2R, KR2I    *noise.CipherState
	SessionSecret []byte // keys path binding
	SID           []byte // 16 bytes
	H             []byte // final handshake hash
	RemoteStatic  []byte
	Peer          *CardInfo // verified card of the peer
	Features      []uint32  // negotiated (GREASE removed)
	Services      []ServiceChoice
	PeerProtoVer  uint32
	PeerTime      uint64
	Observed      []byte // initiator: its address as seen by the responder

	EarlyStatus   uint32 // initiator: P2.early_status
	EarlyResponse []byte // initiator: P2.early
	EarlyRequest  []byte // responder: the processed P1.early

	// Unverified is set on an XX session to a bare address without an expected device ID: every
	// card and address learned in it is only a candidate (handshake.md §4.2).
	Unverified bool
	// ResponderCardSent reports whether P2 carried the responder's card.
	ResponderCardSent bool
}

func newSession(hs *noise.HandshakeState, suite uint16, initiator bool) (*Session, error) {
	i2r, r2i, err := hs.Split()
	if err != nil {
		return nil, err
	}
	ck, h := hs.CK(), hs.H()
	sec := SessionSecret(ck)
	s := &Session{
		Suite: suite, Initiator: initiator,
		KI2R: i2r, KR2I: r2i,
		SessionSecret: sec,
		SID:           SessionID(sec, h),
		H:             h,
		RemoteStatic:  hs.RemoteStatic(),
	}
	for i := range ck {
		ck[i] = 0
	}
	hs.Erase() // ephemerals, ck and the KEM key pair (handshake.md §9)
	return s, nil
}

// KeyBinder remembers which device ID each hs key belongs to (handshake.md §7).
type KeyBinder interface {
	Bind(hsKey []byte, deviceID [32]byte) error
}

// KeyBindings is an in-memory KeyBinder.
type KeyBindings struct {
	mu sync.Mutex
	m  map[[32]byte][32]byte
}

// NewKeyBindings returns an empty binding table.
func NewKeyBindings() *KeyBindings { return &KeyBindings{m: map[[32]byte][32]byte{}} }

// Bind records hsKey → deviceID, or fails if hsKey is bound to another device.
func (b *KeyBindings) Bind(hsKey []byte, deviceID [32]byte) error {
	if len(hsKey) != 32 {
		return ErrKeyBinding
	}
	var k [32]byte
	copy(k[:], hsKey)
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.m[k]; ok && d != deviceID {
		return ErrKeyBinding
	}
	b.m[k] = deviceID
	return nil
}

func bindCard(b KeyBinder, c *CardInfo) error {
	if b == nil {
		return nil
	}
	if err := b.Bind(c.HsKey, c.DeviceID); err != nil {
		return err
	}
	if c.PrevHsKey != nil {
		return b.Bind(c.PrevHsKey, c.DeviceID)
	}
	return nil
}

// ReplayCache is the 0-RTT replay cache keyed by the initiator's ephemeral key (handshake.md §10).
type ReplayCache struct {
	mu sync.Mutex
	m  map[[32]byte]time.Time
}

// NewReplayCache returns an empty cache.
func NewReplayCache() *ReplayCache { return &ReplayCache{m: map[[32]byte]time.Time{}} }

// Seen reports whether key was recorded in the last 180 s, and records it otherwise.
func (c *ReplayCache) Seen(key []byte, now time.Time) bool {
	var k [32]byte
	copy(k[:], key)
	c.mu.Lock()
	defer c.mu.Unlock()
	for x, t := range c.m {
		if now.Sub(t) > ReplayWindow {
			delete(c.m, x)
		}
	}
	if t, ok := c.m[k]; ok && now.Sub(t) <= ReplayWindow {
		return true
	}
	c.m[k] = now
	return false
}

func absDiff(a, b int64) time.Duration {
	d := a - b
	if d < 0 {
		d = -d
	}
	return time.Duration(d) * time.Second
}

func ctEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

func sortedNoGrease(vs []uint32) []uint32 {
	var out []uint32
	for _, v := range vs {
		if !IsGrease32(v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func suiteDetail(s []uint16) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%04x", x)
	}
	return out
}
