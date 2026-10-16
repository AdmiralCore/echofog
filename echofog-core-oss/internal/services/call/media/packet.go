package media

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type FrameKind byte

const (
	FrameAudio FrameKind = 0x01
	FrameVideo FrameKind = 0x02
	// FramePulse is an empty hop-by-hop keepalive on a stream tree link. It is never forwarded
	// or delivered; it only proves the feeder is alive while the stream itself is idle.
	FramePulse FrameKind = 0x03
)

const (
	packetVersion = 2

	// DatagramBudget is the largest fragment that is sure to fit one QUIC datagram. quic-go starts
	// every connection at 1280-byte packets and only grows them after path MTU discovery; until
	// then SendDatagram refuses payloads over 1280 - 1 (type) - 20 (longest connection ID) - 16
	// (AEAD tag) = 1243 bytes, and the peer wrapper spends one byte of that on the message type.
	// UDP obfuscation (internal/network/dpi) wraps the socket under QUIC, so its 12 bytes grow
	// the UDP packet but do not shrink this budget.
	DatagramBudget = 1280 - 1 - 20 - 16 - 1
	// MaxFragmentPayload is the ciphertext one fragment carries with the longest stream ID;
	// shorter IDs leave room for more (see fragmentPayload).
	MaxFragmentPayload = DatagramBudget - fixedHeaderLen - fragFieldsLen - MaxStreamIDLen
	// MaxFragments bounds the size of a single media unit (~18 KB of ciphertext).
	MaxFragments = 16
	// MaxStreamIDLen bounds the stream identifier carried in every fragment.
	MaxStreamIDLen = 64

	// fixed header: kind(1) version(1) seq(4) ts(4) broadcaster(32) streamIDLen(1)
	fixedHeaderLen = 1 + 1 + 4 + 4 + 32 + 1
	// fragment trailer of the header: fragIdx(1) fragCount(1)
	fragFieldsLen = 2
	keyLen        = 32
	gcmNonceLen   = 12
	gcmTagLen     = 16
)

var (
	ErrPacketTooShort   = errors.New("packet data too short")
	ErrBadPacketVersion = errors.New("unsupported media packet version")
	ErrBadFragment      = errors.New("invalid fragment index or count")
	ErrStreamIDTooLong  = errors.New("stream id is too long")
	ErrUnitTooLarge     = errors.New("media unit exceeds the maximum fragment count")
	ErrDecryptionFailed = errors.New("media decryption failed")
	ErrEncryptionFailed = errors.New("media encryption failed")
	ErrBadKey           = errors.New("room key must be 32 bytes for AES-256-GCM")
)

// MediaPacket is one fragment on the wire, or a fully reassembled and decrypted unit when
// handed to a LocalMediaConsumer (FragCount == 1, Payload is plaintext).
type MediaPacket struct {
	Kind        FrameKind
	Seq         uint32
	Timestamp   uint32
	Broadcaster types.PeerID
	StreamID    string
	FragIdx     uint8
	FragCount   uint8
	Payload     []byte
}

// unitHeader serialises the fields shared by all fragments of a unit. It is also the AES-GCM
// additional data, so a relay cannot re-label who sent a unit, for which stream or with which seq.
func (p *MediaPacket) unitHeader() []byte {
	buf := make([]byte, fixedHeaderLen+len(p.StreamID))
	buf[0] = byte(p.Kind)
	buf[1] = packetVersion
	binary.BigEndian.PutUint32(buf[2:6], p.Seq)
	binary.BigEndian.PutUint32(buf[6:10], p.Timestamp)
	copy(buf[10:42], p.Broadcaster[:])
	buf[42] = byte(len(p.StreamID))
	copy(buf[fixedHeaderLen:], p.StreamID)
	return buf
}

// Encode serialises one fragment.
// Layout: [kind][version][seq:4][ts:4][broadcaster:32][streamIDLen:1][streamID][fragIdx][fragCount][payload]
func (p *MediaPacket) Encode() []byte {
	hdr := p.unitHeader()
	buf := make([]byte, 0, len(hdr)+fragFieldsLen+len(p.Payload))
	buf = append(buf, hdr...)
	fragCount := p.FragCount
	if fragCount == 0 {
		fragCount = 1
	}
	buf = append(buf, p.FragIdx, fragCount)
	return append(buf, p.Payload...)
}

// Decode parses one fragment. The payload aliases data.
func Decode(data []byte) (*MediaPacket, error) {
	if len(data) < fixedHeaderLen+fragFieldsLen {
		return nil, ErrPacketTooShort
	}
	if data[1] != packetVersion {
		return nil, ErrBadPacketVersion
	}
	p := &MediaPacket{
		Kind:      FrameKind(data[0]),
		Seq:       binary.BigEndian.Uint32(data[2:6]),
		Timestamp: binary.BigEndian.Uint32(data[6:10]),
	}
	copy(p.Broadcaster[:], data[10:42])
	idLen := int(data[42])
	if idLen > MaxStreamIDLen {
		return nil, ErrStreamIDTooLong
	}
	off := fixedHeaderLen
	if len(data) < off+idLen+fragFieldsLen {
		return nil, ErrPacketTooShort
	}
	p.StreamID = string(data[off : off+idLen])
	off += idLen
	p.FragIdx, p.FragCount = data[off], data[off+1]
	off += fragFieldsLen
	if p.FragCount == 0 || p.FragCount > MaxFragments || p.FragIdx >= p.FragCount {
		return nil, ErrBadFragment
	}
	p.Payload = data[off:]
	return p, nil
}

// SealUnit encrypts a media unit with the room key and splits it into wire fragments.
func SealUnit(unit *MediaPacket, key []byte) ([][]byte, error) {
	if len(unit.StreamID) > MaxStreamIDLen {
		return nil, ErrStreamIDTooLong
	}
	ct, err := EncryptPayload(unit.Payload, key, unit.unitHeader())
	if err != nil {
		return nil, err
	}
	limit := fragmentPayload(len(unit.StreamID))
	count := (len(ct) + limit - 1) / limit
	if count > MaxFragments {
		return nil, ErrUnitTooLarge
	}
	// split evenly instead of leaving a runt tail fragment
	size := (len(ct) + count - 1) / count
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		end := min((i+1)*size, len(ct))
		frag := *unit
		frag.FragIdx, frag.FragCount = uint8(i), uint8(count)
		frag.Payload = ct[i*size : end]
		out = append(out, frag.Encode())
	}
	return out, nil
}

// fragmentPayload is how much ciphertext fits next to the header of a stream with this ID length.
func fragmentPayload(streamIDLen int) int {
	return DatagramBudget - fixedHeaderLen - fragFieldsLen - streamIDLen
}

// SealOverhead is what sealing adds to a unit in its first fragment: header, nonce and tag. A unit
// of at most DatagramBudget - SealOverhead(len(streamID)) bytes travels as one fragment.
func SealOverhead(streamIDLen int) int {
	return fixedHeaderLen + fragFieldsLen + streamIDLen + gcmNonceLen + gcmTagLen
}

// OpenUnit authenticates and decrypts a reassembled unit; hdr supplies the shared header fields.
func OpenUnit(hdr *MediaPacket, ciphertext []byte, key []byte) ([]byte, error) {
	return DecryptPayload(ciphertext, key, hdr.unitHeader())
}

// EncryptPayload encrypts with AES-256-GCM, binding the ciphertext to aad. Output: nonce||sealed.
func EncryptPayload(payload []byte, key []byte, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize(), gcm.NonceSize()+len(payload)+gcm.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, ErrEncryptionFailed
	}
	return gcm.Seal(nonce, nonce, payload, aad), nil
}

// DecryptPayload reverses EncryptPayload; it fails if the ciphertext or aad were modified.
func DecryptPayload(ciphertext []byte, key []byte, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns+gcm.Overhead() {
		return nil, ErrDecryptionFailed
	}
	out, err := gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return out, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Equals checks equality for tests.
func (p *MediaPacket) Equals(other *MediaPacket) bool {
	if p.Kind != other.Kind || p.Seq != other.Seq || p.Timestamp != other.Timestamp || p.StreamID != other.StreamID {
		return false
	}
	if p.Broadcaster != other.Broadcaster || p.FragIdx != other.FragIdx || p.FragCount != other.FragCount {
		return false
	}
	return bytes.Equal(p.Payload, other.Payload)
}
