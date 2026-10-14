package types

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type PeerID [32]byte

type IdentityPublicKey [32]byte
type IdentityPrivateKey [64]byte
type Signature [64]byte

type RatchetPublicKey [32]byte
type RatchetPrivateKey [32]byte

type ChainKey [32]byte

type MessageKey [32]byte

type SessionID [16]byte
type MessageID [16]byte
type ContentHash [32]byte

var emptySessionID = SessionID{}

func (s SessionID) IsEmpty() bool {
	return s == emptySessionID
}

func (s SessionID) String() string {
	return hex.EncodeToString(s[:])
}

type PeerPublicKey [32]byte
type PeerPrivateKey [64]byte // ed25519

type EncryptionKey [32]byte //x25519

const HOPLIMIT = 20

type EncryptedMessage struct {
	Header     RatchetHeader
	Ciphertext []byte
}

type RatchetHeader struct {
	DH RatchetPublicKey // The sender's current public key
	PN uint32           // Number of messages in the previous thread
	N  uint32           // Message number in the current thread
}

type X3DHInit struct {
	EphemeralKey [32]byte
	IdentityKey  [32]byte
	OneTimeKeyID *[32]byte

	SecureMessage EncryptedMessage
}

type PrivKeyGeneric interface {
	ed25519.PrivateKey | []byte
}

func ToPrivKey[T PrivKeyGeneric](key T) (PeerPrivateKey, error) {
	var privKey [64]byte
	if len(key) != 64 {
		return privKey, errors.New("invalid priv key len")
	}
	copy(privKey[:], key)
	return privKey, nil
}

func ToPeerID(b []byte) (PeerID, error) {
	var id PeerID
	if len(b) != 32 {
		return id, fmt.Errorf("wrong length: %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}

func PeerPubKeyToID(pubKey PeerPublicKey) PeerID {
	return sha256.Sum256(pubKey[:])
}

func ToPeerPubKey(b []byte) (PeerPublicKey, error) {
	var pub PeerPublicKey
	if len(b) != 32 {
		return pub, fmt.Errorf("wrong length for public key: %d", len(b))
	}
	copy(pub[:], b)
	return pub, nil
}

func NewMessageID() []byte {
	id := uuid.New()
	res := make([]byte, 16)
	copy(res, id[:])
	return res
}

func ParseMessageID(data []byte) (MessageID, error) {
	bytes, err := uuid.FromBytes(data)
	if err != nil {
		return [16]byte{}, err
	}

	return MessageID(bytes), nil
}

func (id PeerID) String() string {
	return hex.EncodeToString(id[:])
}

func (id PeerID) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(id[:])), nil
}

func (id *PeerID) UnmarshalText(text []byte) error {
	b, err := hex.DecodeString(string(text))
	if err != nil {
		return err
	}
	if len(b) != 32 {
		return fmt.Errorf("invalid peer id length: %d", len(b))
	}
	copy(id[:], b)
	return nil
}

func (k IdentityPublicKey) String() string {
	return hex.EncodeToString(k[:])
}

func (k RatchetPublicKey) String() string {
	return hex.EncodeToString(k[:])
}

func (k RatchetPublicKey) Bytes() []byte {
	return k[:]
}

func ToRatchetHeader(DH []byte, PN, N uint32) RatchetHeader {
	var ratchetPubKey RatchetPublicKey
	copy(ratchetPubKey[:], DH)
	return RatchetHeader{
		DH: ratchetPubKey,
		PN: PN,
		N:  N,
	}
}

func ToX3DHInitPacket(EphemeralKey, IdentityKey, OneTimeKeyID []byte, encryptedMessage EncryptedMessage) X3DHInit {
	initPacket := X3DHInit{
		SecureMessage: encryptedMessage,
	}
	copy(initPacket.IdentityKey[:], IdentityKey)
	copy(initPacket.EphemeralKey[:], EphemeralKey)
	if len(OneTimeKeyID) == 32 {
		var otk [32]byte
		copy(otk[:], OneTimeKeyID)

		initPacket.OneTimeKeyID = &otk
	}
	return initPacket
}

func (id PeerID) ToBytes() []byte {
	res := make([]byte, 32)
	copy(res, id[:])
	return res
}
