// Package packet is the noise3 datagram format (docs/protocol/v2/wire.md §2.3–§2.6): the key
// schedule, the Init datagram, data packets with QUIC-style packet-number protection, packet-number
// reconstruction and the replay window.
package packet

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// InitOverhead is repr(32) + sel(16) + ts(4) + flags(1) + inner_len(2) + AEAD tag(16).
	InitOverhead = 71
	// DataOverhead is the masked packet number(4) + AEAD tag(16).
	DataOverhead = 22
	// MinInner is the smallest inner datagram a responder accepts in an Init (wire.md §2.4).
	MinInner = 200
	// MinInitLen is the smallest Init datagram a responder processes (wire.md §2.5 step 1).
	MinInitLen = InitOverhead + MinInner
	// MaxFirstLen is the longest datagram of a flow before the first authenticated responder packet
	// (wire.md §2.4).
	MaxFirstLen = 1200
	// MinCeiling is the smallest datagram ceiling noise3 works with: the lower bound of init_len.
	MinCeiling = 900
	// DefaultDatagramLen is the default outer datagram ceiling: the UDP payload of an IPv4 path of
	// MTU 1200, so no path MTU discovery is needed (wire.md §2.4).
	DefaultDatagramLen = 1172
	// MaxDatagramLen is the hard noise3 payload ceiling of an IPv4 1500-byte path.
	MaxDatagramLen = 1452
	// MaxPacketNumber is the largest packet number a sender may use (wire.md §2.6).
	MaxPacketNumber = 1<<62 - 1
)

// KeyScheduleLabel is the HKDF info prefix of wire.md §2.3.
const KeyScheduleLabel = "echofog/noise3/keys"

// Errors of the datagram format.
var (
	ErrShort        = errors.New("noise3: datagram too short")
	ErrSelector     = errors.New("noise3: selector does not match")
	ErrAuth         = errors.New("noise3: authentication failed")
	ErrMalformed    = errors.New("noise3: malformed plaintext")
	ErrPacketNumber = errors.New("noise3: packet number out of range")
)

// Keys is the noise3 key schedule of one flow (wire.md §2.3).
type Keys struct {
	KSel  [32]byte
	KInit [32]byte
	KI2R  [32]byte
	KR2I  [32]byte
	HPI2R [32]byte
	HPR2I [32]byte
	Sel   [16]byte
}

// Info returns the HKDF info of wire.md §2.3: the label, mask_pub and the dirty u-coordinate.
func Info(maskPub, u []byte) []byte {
	out := make([]byte, 0, len(KeyScheduleLabel)+len(maskPub)+len(u))
	out = append(out, KeyScheduleLabel...)
	out = append(out, maskPub...)
	out = append(out, u...)
	return out
}

// Derive computes the whole key schedule from the ECDH secret, the two public values and the
// representative. An all-zero z aborts (wire.md §2.3).
func Derive(z, maskPub, u, repr []byte) (Keys, error) {
	if len(z) != 32 || len(maskPub) != 32 || len(u) != 32 || len(repr) != 32 {
		return Keys{}, ErrMalformed
	}
	var nonzero byte
	for _, b := range z {
		nonzero |= b
	}
	if nonzero == 0 {
		return Keys{}, ErrAuth
	}
	okm, err := hkdf.Key(sha256.New, z, nil, string(Info(maskPub, u)), 192)
	if err != nil {
		return Keys{}, err
	}
	var k Keys
	copy(k.KSel[:], okm[0:32])
	copy(k.KInit[:], okm[32:64])
	copy(k.KI2R[:], okm[64:96])
	copy(k.KR2I[:], okm[96:128])
	copy(k.HPI2R[:], okm[128:160])
	copy(k.HPR2I[:], okm[160:192])
	mac := hmac.New(sha256.New, k.KSel[:])
	mac.Write(repr)
	copy(k.Sel[:], mac.Sum(nil)[:16])
	return k, nil
}

// Init is the plaintext of an Init datagram.
type Init struct {
	TS    uint32
	Inner []byte
}

// SealInit builds the first datagram of a flow (wire.md §2.4). repr and keys must belong together.
// total is the drawn datagram length; a value below InitOverhead+len(inner) is raised to it, and a
// value above MaxDatagramLen is clamped. An inner datagram that does not fit is an error: it is
// never split.
func SealInit(k Keys, repr []byte, ts uint32, inner []byte, total int) ([]byte, error) {
	if len(repr) != 32 {
		return nil, ErrMalformed
	}
	if len(inner) > 0xffff {
		return nil, ErrMalformed
	}
	need := InitOverhead + len(inner)
	if need > MaxDatagramLen {
		return nil, ErrMalformed
	}
	if total < need {
		total = need
	}
	if total > MaxDatagramLen {
		total = MaxDatagramLen
	}
	pt := make([]byte, 7+len(inner)+total-InitOverhead-len(inner))
	binary.BigEndian.PutUint32(pt[0:4], ts)
	pt[4] = 0 // flags: senders MUST send zero
	binary.BigEndian.PutUint16(pt[5:7], uint16(len(inner)))
	copy(pt[7:], inner)

	aead, err := chacha20poly1305.New(k.KInit[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 48, total)
	copy(out[0:32], repr)
	copy(out[32:48], k.Sel[:])
	var nonce [12]byte
	out = aead.Seal(out, nonce[:], pt, out[0:48])
	return out, nil
}

// OpenInit authenticates and opens an Init datagram. It also checks the selector, so a caller may
// use it directly after the cheap selector pre-check of §2.5.
func OpenInit(k Keys, datagram []byte) (Init, error) {
	if len(datagram) < InitOverhead {
		return Init{}, ErrShort
	}
	if subtle.ConstantTimeCompare(datagram[32:48], k.Sel[:]) != 1 {
		return Init{}, ErrSelector
	}
	aead, err := chacha20poly1305.New(k.KInit[:])
	if err != nil {
		return Init{}, err
	}
	var nonce [12]byte
	pt, err := aead.Open(nil, nonce[:], datagram[48:], datagram[0:48])
	if err != nil {
		return Init{}, ErrAuth
	}
	if len(pt) < 7 {
		return Init{}, ErrMalformed
	}
	n := int(binary.BigEndian.Uint16(pt[5:7]))
	if n > len(pt)-7 {
		return Init{}, ErrMalformed
	}
	return Init{
		TS:    binary.BigEndian.Uint32(pt[0:4]),
		Inner: append([]byte(nil), pt[7:7+n]...),
	}, nil
}

// SealData builds a data packet (wire.md §2.6): the masked 32-bit packet number and the AEAD box.
// pad is the number of padding bytes after the inner datagram.
func SealData(key, hp [32]byte, n uint64, inner []byte, pad int) ([]byte, error) {
	if n > MaxPacketNumber {
		return nil, ErrPacketNumber
	}
	if len(inner) > 0xffff || pad < 0 || DataOverhead+len(inner)+pad > MaxDatagramLen {
		return nil, ErrMalformed
	}
	pt := make([]byte, 2+len(inner)+pad)
	binary.BigEndian.PutUint16(pt[0:2], uint16(len(inner)))
	copy(pt[2:], inner)

	trunc := uint32(n)
	var aad [4]byte
	binary.BigEndian.PutUint32(aad[:], trunc)
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], n)

	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4, 4+len(pt)+16)
	out = aead.Seal(out, nonce[:], pt, aad[:])

	mask, err := hpMask(hp[:], out[4:20])
	if err != nil {
		return nil, err
	}
	out[0] = aad[0] ^ mask[0]
	out[1] = aad[1] ^ mask[1]
	out[2] = aad[2] ^ mask[2]
	out[3] = aad[3] ^ mask[3]
	return out, nil
}

// OpenData authenticates a data packet and reconstructs its full packet number against the
// highest number already received (wire.md §2.6, RFC 9000 Appendix A.3). The caller commits the
// number to a Window after this check.
func OpenData(key, hp [32]byte, datagram []byte, highest uint64) (n uint64, inner []byte, err error) {
	if len(datagram) < DataOverhead {
		return 0, nil, ErrShort
	}
	mask, err := hpMask(hp[:], datagram[4:20])
	if err != nil {
		return 0, nil, err
	}
	var trunc uint32
	for i := 0; i < 4; i++ {
		trunc = trunc<<8 | uint32(datagram[i]^mask[i])
	}
	n = DecodeNumber(highest, trunc, 32)
	if n > MaxPacketNumber {
		return 0, nil, ErrPacketNumber
	}
	var aad [4]byte
	binary.BigEndian.PutUint32(aad[:], uint32(n))
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], n)
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return 0, nil, err
	}
	pt, err := aead.Open(nil, nonce[:], datagram[4:], aad[:])
	if err != nil {
		return 0, nil, ErrAuth
	}
	if len(pt) < 2 {
		return 0, nil, ErrMalformed
	}
	innerLen := int(binary.BigEndian.Uint16(pt[0:2]))
	if innerLen > len(pt)-2 {
		return 0, nil, ErrMalformed
	}
	return n, append([]byte(nil), pt[2:2+innerLen]...), nil
}

// DecodeNumber reconstructs a packet number from its nbits-bit truncation (RFC 9000 Appendix A.3).
// largest is the highest number received so far.
func DecodeNumber(largest uint64, truncated uint32, nbits uint) uint64 {
	win := uint64(1) << nbits
	hwin := win / 2
	mask := win - 1
	expected := largest + 1
	candidate := (expected &^ mask) | uint64(truncated)
	if candidate+hwin <= expected && candidate < (1<<62)-win {
		return candidate + win
	}
	if candidate > expected+hwin && candidate >= win {
		return candidate - win
	}
	return candidate
}

// hpMask returns the first four bytes of the ChaCha20 header-protection mask
// (RFC 9001 §5.4.4, wire.md §2.6).
func hpMask(key, sample []byte) ([]byte, error) {
	if len(sample) < 16 {
		return nil, ErrShort
	}
	c, err := chacha20.NewUnauthenticatedCipher(key, sample[4:16])
	if err != nil {
		return nil, err
	}
	c.SetCounter(binary.LittleEndian.Uint32(sample[0:4]))
	out := make([]byte, 4)
	c.XORKeyStream(out, out)
	return out, nil
}

// Window is the replay bitmap of one direction: 1024 numbers below the highest received
// (wire.md §2.6, §10).
type Window struct {
	highest uint64
	have    bool
	bits    [16]uint64
}

// Highest returns the highest committed packet number, or 0 before the first packet.
func (w *Window) Highest() uint64 { return w.highest }

// Check reports whether n is acceptable: not seen, not more than 1023 behind the highest, and
// below 2^62.
func (w *Window) Check(n uint64) bool {
	if n > MaxPacketNumber {
		return false
	}
	if !w.have {
		return true
	}
	if n > w.highest {
		return true
	}
	d := w.highest - n
	if d >= 1024 {
		return false
	}
	return w.bits[d/64]&(1<<(d%64)) == 0
}

// Commit records an accepted packet number.
func (w *Window) Commit(n uint64) {
	if n > MaxPacketNumber {
		return
	}
	if !w.have {
		w.have = true
		w.highest = n
		w.bits = [16]uint64{1}
		return
	}
	if n > w.highest {
		delta := n - w.highest
		if delta >= 1024 {
			w.bits = [16]uint64{}
		} else {
			words := int(delta / 64)
			rem := uint(delta % 64)
			var next [16]uint64
			for i := 15; i >= 0; i-- {
				if i-words < 0 {
					continue
				}
				v := w.bits[i-words] << rem
				if rem != 0 && i-words-1 >= 0 {
					v |= w.bits[i-words-1] >> (64 - rem)
				}
				next[i] = v
			}
			w.bits = next
		}
		w.highest = n
		w.bits[0] |= 1
		return
	}
	d := w.highest - n
	if d < 1024 {
		w.bits[d/64] |= 1 << (d % 64)
	}
}
