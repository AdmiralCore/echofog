package filexfer

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// A frame on the wire: [version][kind][16-byte transfer ID][24-byte nonce][sealed body]. The first
// 18 bytes are authenticated as associated data, so neither the kind nor the transfer can be
// swapped. Frames from sender to receiver and back are sealed with different keys, so a relay
// cannot reflect a frame back to the peer that sent it.
const (
	frameVersion = 1
	idLen        = 16
	headerLen    = 2 + idLen
	nonceLen     = chacha20poly1305.NonceSizeX
	overhead     = headerLen + nonceLen + chacha20poly1305.Overhead
)

type frameKind byte

const (
	kindWant   frameKind = 1 // receiver → sender: [count u16][index u32]...
	kindChunk  frameKind = 2 // sender → receiver: [index u32][proof count u8][proof hashes][data]
	kindCancel frameKind = 3 // either way: [reason u8]
	kindDone   frameKind = 4 // receiver → sender: everything arrived and checked
)

// Why a transfer was called off, as carried by a cancel frame.
const (
	reasonCancelled   byte = 1 // the user stopped it
	reasonDeclined    byte = 2 // the receiver did not want the file
	reasonUnavailable byte = 3 // the sender no longer has the file (deleted, changed, offer withdrawn)
)

// maxWant bounds the indices one want frame may ask for.
const maxWant = 256

// keys are the two directions' AEADs of one transfer.
type keys struct {
	toReceiver cipher.AEAD
	toSender   cipher.AEAD
}

func deriveKeys(secret []byte) (keys, error) {
	if len(secret) != 32 {
		return keys{}, errors.New("file key must be 32 bytes")
	}
	derive := func(label string) (cipher.AEAD, error) {
		m := hmac.New(sha256.New, secret)
		m.Write([]byte("echofog/filexfer/v1/" + label))
		return chacha20poly1305.NewX(m.Sum(nil))
	}
	var k keys
	var err error
	if k.toReceiver, err = derive("to-receiver"); err != nil {
		return keys{}, err
	}
	if k.toSender, err = derive("to-sender"); err != nil {
		return keys{}, err
	}
	return k, nil
}

func seal(aead cipher.AEAD, kind frameKind, id [idLen]byte, body []byte) []byte {
	out := make([]byte, headerLen+nonceLen, overhead+len(body))
	out[0] = frameVersion
	out[1] = byte(kind)
	copy(out[2:headerLen], id[:])
	_, _ = rand.Read(out[headerLen : headerLen+nonceLen])
	return aead.Seal(out, out[headerLen:headerLen+nonceLen], body, out[:headerLen])
}

// parseHeader reads the clear part of a frame.
func parseHeader(frame []byte) (kind frameKind, id [idLen]byte, ok bool) {
	if len(frame) < overhead || frame[0] != frameVersion {
		return 0, id, false
	}
	copy(id[:], frame[2:headerLen])
	return frameKind(frame[1]), id, true
}

func open(aead cipher.AEAD, frame []byte) ([]byte, error) {
	nonce := frame[headerLen : headerLen+nonceLen]
	return aead.Open(nil, nonce, frame[headerLen+nonceLen:], frame[:headerLen])
}

func encodeWant(idx []int) []byte {
	b := make([]byte, 2+4*len(idx))
	binary.BigEndian.PutUint16(b, uint16(len(idx)))
	for i, v := range idx {
		binary.BigEndian.PutUint32(b[2+4*i:], uint32(v))
	}
	return b
}

func decodeWant(b []byte) ([]int, error) {
	if len(b) < 2 {
		return nil, errors.New("short want")
	}
	n := int(binary.BigEndian.Uint16(b))
	if n > maxWant || len(b) != 2+4*n {
		return nil, errors.New("malformed want")
	}
	out := make([]int, n)
	for i := range out {
		out[i] = int(binary.BigEndian.Uint32(b[2+4*i:]))
	}
	return out, nil
}

func encodeChunk(i int, proof []Hash, data []byte) []byte {
	b := make([]byte, 5+32*len(proof)+len(data))
	binary.BigEndian.PutUint32(b, uint32(i))
	b[4] = byte(len(proof))
	for k, p := range proof {
		copy(b[5+32*k:], p[:])
	}
	copy(b[5+32*len(proof):], data)
	return b
}

func decodeChunk(b []byte) (i int, proof []Hash, data []byte, err error) {
	if len(b) < 5 {
		return 0, nil, nil, errors.New("short chunk")
	}
	i = int(binary.BigEndian.Uint32(b))
	np := int(b[4])
	if len(b) < 5+32*np {
		return 0, nil, nil, errors.New("short proof")
	}
	proof = make([]Hash, np)
	for k := range proof {
		copy(proof[k][:], b[5+32*k:])
	}
	return i, proof, b[5+32*np:], nil
}
