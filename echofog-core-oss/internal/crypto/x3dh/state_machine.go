package x3dh

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	// MaxSkip is how many message keys one step may skip (messages lost or still on the way).
	MaxSkip = 1000
	// MaxSkippedTotal bounds the skipped keys a session keeps; the oldest chains go first.
	MaxSkippedTotal = 2000
	// MaxSkippedChains: keys of older ratchet steps are dropped, their messages will not come.
	MaxSkippedChains = 4
)

var (
	ErrInvalidCiphertext = errors.New("ciphertext too short or invalid")
	ErrTooManySkipped    = errors.New("too many skipped messages")
	ErrRatchetCrypto     = errors.New("ratchet cryptographic operation failed")
	ErrAeadInit          = errors.New("failed to init aead cipher")
	ErrOldMessage        = errors.New("message key already used or dropped")
	ErrNoAssociatedData  = errors.New("session has no associated data")
)

type X3DHSession struct {
	RemotePeerID types.PeerID
	RootKey      *memguard.Enclave
	SendChain    ChainState
	RecvChain    ChainState
	// PrevSendCount is how many messages the previous sending chain carried (PN of the header).
	PrevSendCount uint32

	LocalRatchetPub  [32]byte
	LocalRatchetPriv *memguard.Enclave
	RemoteRatchetPub [32]byte
	SkippedKeys      map[[32]byte]map[uint32]*memguard.Enclave
	SkippedOrder     [][32]byte // ratchet keys of SkippedKeys, oldest first

	IsInitiator bool

	// AD is IK_initiator || IK_responder; every message authenticates it with its header.
	AD []byte
	// InitEK is the ephemeral key of the init packet that started the session: a repeat of that
	// packet belongs to this session.
	InitEK [32]byte
	// InitMsgKey (responder only) opens the message of the init packet again, so a repeat of the
	// packet (its receipt got lost) can be confirmed. Dropped at the first ratchet step.
	InitMsgKey *memguard.Enclave
}

type ChainState struct {
	Key   *memguard.Enclave
	Index uint32
}

func NewX3DHSession(
	sharedSecretEnc *memguard.Enclave,
	ephemeralKeyPrivEnc *memguard.Enclave,
	remotePeerID types.PeerID,
	remoteRatchetKey [32]byte,
	isInitiator bool,
) (*X3DHSession, error) {
	lockedShared, err := sharedSecretEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}

	kdf := hkdf.New(sha256.New, lockedShared.Bytes(), nil, []byte(infoInitChains))

	k1Buf := memguard.NewBuffer(32)
	k2Buf := memguard.NewBuffer(32)

	if _, err := io.ReadFull(kdf, k1Buf.Bytes()); err != nil {
		k1Buf.Destroy()
		k2Buf.Destroy()
		lockedShared.Destroy()
		return nil, ErrHKDFFailed
	}
	if _, err := io.ReadFull(kdf, k2Buf.Bytes()); err != nil {
		k1Buf.Destroy()
		k2Buf.Destroy()
		lockedShared.Destroy()
		return nil, ErrHKDFFailed
	}
	lockedShared.Destroy()

	lockedEph, err := ephemeralKeyPrivEnc.Open()
	if err != nil {
		k1Buf.Destroy()
		k2Buf.Destroy()
		return nil, ErrEnclaveOpenFailed
	}

	var localRatchetPub [32]byte
	var xPriv [32]byte
	copy(xPriv[:], lockedEph.Bytes())
	curve25519.ScalarBaseMult(&localRatchetPub, &xPriv)
	memguard.WipeBytes(xPriv[:])
	lockedEph.Destroy()

	var sendChainKeyEnc, recvChainKeyEnc, rootKeyEnc *memguard.Enclave
	rootKeyEnc = sharedSecretEnc

	if isInitiator {
		sendChainKeyEnc = k1Buf.Seal()
		recvChainKeyEnc = k2Buf.Seal()
	} else {
		recvChainKeyEnc = k1Buf.Seal()
		k2Buf.Destroy()

		dhSecretEnc, err := calculateEDCHSecret(ephemeralKeyPrivEnc, remoteRatchetKey)
		if err != nil {
			return nil, err
		}

		rootKeyEnc, sendChainKeyEnc, err = rootKDF(rootKeyEnc, dhSecretEnc)
		if err != nil {
			return nil, err
		}
	}

	return &X3DHSession{
		RemotePeerID:     remotePeerID,
		RootKey:          rootKeyEnc,
		LocalRatchetPriv: ephemeralKeyPrivEnc,
		SendChain: ChainState{
			Key:   sendChainKeyEnc,
			Index: 0,
		},
		RecvChain: ChainState{
			Key:   recvChainKeyEnc,
			Index: 0,
		},
		LocalRatchetPub:  localRatchetPub,
		RemoteRatchetPub: remoteRatchetKey,
		SkippedKeys:      make(map[[32]byte]map[uint32]*memguard.Enclave),
		IsInitiator:      isInitiator,
	}, nil
}

// headerAD is what the AEAD authenticates besides the ciphertext: the session's AD and the header.
func (x *X3DHSession) headerAD(h types.RatchetHeader) []byte {
	out := make([]byte, 0, len(x.AD)+40)
	out = append(out, x.AD...)
	out = append(out, h.DH[:]...)
	out = binary.BigEndian.AppendUint32(out, h.PN)
	return binary.BigEndian.AppendUint32(out, h.N)
}

func (x *X3DHSession) RatchetEncrypt(plaintext []byte) (types.EncryptedMessage, error) {
	if len(x.AD) == 0 {
		return types.EncryptedMessage{}, ErrNoAssociatedData
	}
	messageKeyEnc, nextChainKeyEnc, err := kdfChain(x.SendChain.Key)
	if err != nil {
		return types.EncryptedMessage{}, err
	}

	header := types.RatchetHeader{
		DH: x.LocalRatchetPub,
		PN: x.PrevSendCount,
		N:  x.SendChain.Index,
	}

	lockedMsgKey, err := messageKeyEnc.Open()
	if err != nil {
		return types.EncryptedMessage{}, ErrEnclaveOpenFailed
	}
	defer lockedMsgKey.Destroy()

	aead, err := chacha20poly1305.NewX(lockedMsgKey.Bytes())
	if err != nil {
		return types.EncryptedMessage{}, ErrAeadInit
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return types.EncryptedMessage{}, ErrRatchetCrypto
	}

	cipherText := aead.Seal(nonce, nonce, plaintext, x.headerAD(header))

	x.SendChain.Key = nextChainKeyEnc
	x.SendChain.Index++

	return types.EncryptedMessage{Header: header, Ciphertext: cipherText}, nil
}

// RatchetDecrypt opens msg. When it fails the session is left exactly as it was, so a forged or
// damaged message cannot move the ratchet.
func (x *X3DHSession) RatchetDecrypt(msg types.EncryptedMessage) ([]byte, error) {
	if len(x.AD) == 0 {
		return nil, ErrNoAssociatedData
	}
	saved := x.snapshot()
	plain, err := x.ratchetDecrypt(msg)
	if err != nil {
		*x = saved
		return nil, err
	}
	return plain, nil
}

// snapshot copies the state; enclaves are never changed in place, so sharing them is safe.
func (x *X3DHSession) snapshot() X3DHSession {
	c := *x
	c.SkippedKeys = make(map[[32]byte]map[uint32]*memguard.Enclave, len(x.SkippedKeys))
	for dh, keys := range x.SkippedKeys {
		m := make(map[uint32]*memguard.Enclave, len(keys))
		for n, k := range keys {
			m[n] = k
		}
		c.SkippedKeys[dh] = m
	}
	c.SkippedOrder = append([][32]byte(nil), x.SkippedOrder...)
	c.AD = append([]byte(nil), x.AD...)
	return c
}

func (x *X3DHSession) ratchetDecrypt(msg types.EncryptedMessage) ([]byte, error) {
	ad := x.headerAD(msg.Header)
	if keys, ok := x.SkippedKeys[msg.Header.DH]; ok {
		if msgKeyEnc, found := keys[msg.Header.N]; found {
			plain, err := decryptSecure(msgKeyEnc, msg.Ciphertext, ad)
			if err != nil {
				return nil, err
			}
			delete(keys, msg.Header.N)
			if len(keys) == 0 {
				x.dropSkippedChain(msg.Header.DH)
			}
			return plain, nil
		}
	}

	if msg.Header.DH != x.RemoteRatchetPub {
		if err := x.skipMessageKeys(msg.Header.PN); err != nil {
			return nil, err
		}

		dhSecretEnc, err := calculateEDCHSecret(x.LocalRatchetPriv, msg.Header.DH)
		if err != nil {
			return nil, err
		}

		newRootEnc, newRecvChainEnc, err := rootKDF(x.RootKey, dhSecretEnc)
		if err != nil {
			return nil, err
		}

		x.RootKey = newRootEnc
		x.InitMsgKey = nil
		x.RecvChain = ChainState{Key: newRecvChainEnc, Index: 0}
		x.RemoteRatchetPub = msg.Header.DH

		privBuf := memguard.NewBuffer(32)
		if _, err := io.ReadFull(rand.Reader, privBuf.Bytes()); err != nil {
			privBuf.Destroy()
			return nil, ErrRatchetCrypto
		}

		var newLocalPub [32]byte
		var privArr [32]byte
		copy(privArr[:], privBuf.Bytes())
		curve25519.ScalarBaseMult(&newLocalPub, &privArr)
		memguard.WipeBytes(privArr[:])

		x.LocalRatchetPriv = privBuf.Seal()
		x.LocalRatchetPub = newLocalPub

		dhSecretSendEnc, err := calculateEDCHSecret(x.LocalRatchetPriv, x.RemoteRatchetPub)
		if err != nil {
			return nil, err
		}

		newRoot2Enc, newSendChainEnc, err := rootKDF(x.RootKey, dhSecretSendEnc)
		if err != nil {
			return nil, err
		}

		x.RootKey = newRoot2Enc
		x.PrevSendCount = x.SendChain.Index
		x.SendChain = ChainState{Key: newSendChainEnc, Index: 0}
	}

	if msg.Header.N < x.RecvChain.Index {
		return nil, ErrOldMessage
	}
	if err := x.skipMessageKeys(msg.Header.N); err != nil {
		return nil, err
	}

	messageKeyEnc, nextChainKeyEnc, err := kdfChain(x.RecvChain.Key)
	if err != nil {
		return nil, err
	}

	x.RecvChain.Key = nextChainKeyEnc
	x.RecvChain.Index++

	return decryptSecure(messageKeyEnc, msg.Ciphertext, ad)
}

// ReadInitAgain opens a repeat of the message that came with the init packet.
func (x *X3DHSession) ReadInitAgain(msg types.EncryptedMessage) ([]byte, error) {
	if x.InitMsgKey == nil || msg.Header.DH != x.InitEK || msg.Header.N != 0 {
		return nil, ErrOldMessage
	}
	return decryptSecure(x.InitMsgKey, msg.Ciphertext, x.headerAD(msg.Header))
}

func decryptSecure(keyEnc *memguard.Enclave, ciphertext, ad []byte) ([]byte, error) {
	lockedKey, err := keyEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}
	defer lockedKey.Destroy()

	aead, err := chacha20poly1305.NewX(lockedKey.Bytes())
	if err != nil {
		return nil, ErrAeadInit
	}

	nonceSize := aead.NonceSize()
	if len(ciphertext) < nonceSize+aead.Overhead() {
		return nil, ErrInvalidCiphertext
	}

	nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aead.Open(nil, nonce, actualCiphertext, ad)
}

// skipMessageKeys stores the keys of the receiving chain up to (not including) until, for messages
// that are late or lost.
func (x *X3DHSession) skipMessageKeys(until uint32) error {
	if until <= x.RecvChain.Index {
		return nil
	}
	if until-x.RecvChain.Index > MaxSkip {
		return ErrTooManySkipped
	}
	if x.RecvChain.Key == nil {
		return ErrRatchetCrypto
	}

	for x.RecvChain.Index < until {
		messageKeyEnc, nextChainKeyEnc, err := kdfChain(x.RecvChain.Key)
		if err != nil {
			return err
		}

		if x.SkippedKeys[x.RemoteRatchetPub] == nil {
			x.SkippedKeys[x.RemoteRatchetPub] = make(map[uint32]*memguard.Enclave)
			x.SkippedOrder = append(x.SkippedOrder, x.RemoteRatchetPub)
		}
		x.SkippedKeys[x.RemoteRatchetPub][x.RecvChain.Index] = messageKeyEnc

		x.RecvChain.Key = nextChainKeyEnc
		x.RecvChain.Index++
	}
	x.trimSkipped()
	return nil
}

// trimSkipped drops the oldest skipped keys beyond the limits: they weaken forward secrecy and
// messages that late will not come.
func (x *X3DHSession) trimSkipped() {
	for len(x.SkippedOrder) > MaxSkippedChains {
		x.dropSkippedChain(x.SkippedOrder[0])
	}
	total := 0
	for _, keys := range x.SkippedKeys {
		total += len(keys)
	}
	for total > MaxSkippedTotal && len(x.SkippedOrder) > 0 {
		oldest := x.SkippedKeys[x.SkippedOrder[0]]
		if total-len(oldest) >= MaxSkippedTotal || len(x.SkippedOrder) > 1 {
			total -= len(oldest)
			x.dropSkippedChain(x.SkippedOrder[0])
			continue
		}
		// a single chain over the limit: drop its lowest numbers
		for n := range oldest {
			if total <= MaxSkippedTotal {
				break
			}
			delete(oldest, n)
			total--
		}
	}
}

func (x *X3DHSession) dropSkippedChain(dh [32]byte) {
	delete(x.SkippedKeys, dh)
	for i, k := range x.SkippedOrder {
		if k == dh {
			x.SkippedOrder = append(x.SkippedOrder[:i:i], x.SkippedOrder[i+1:]...)
			break
		}
	}
}

func kdfChain(chainKeyEnc *memguard.Enclave) (*memguard.Enclave, *memguard.Enclave, error) {
	lockedChain, err := chainKeyEnc.Open()
	if err != nil {
		return nil, nil, ErrEnclaveOpenFailed
	}
	defer lockedChain.Destroy()

	h1 := hmac.New(sha256.New, lockedChain.Bytes())
	h1.Write([]byte{0x01})
	msgHash := h1.Sum(nil)

	h2 := hmac.New(sha256.New, lockedChain.Bytes())
	h2.Write([]byte{0x02})
	nextHash := h2.Sum(nil)

	msgBuf := memguard.NewBufferFromBytes(msgHash)
	nextBuf := memguard.NewBufferFromBytes(nextHash)

	memguard.WipeBytes(msgHash)
	memguard.WipeBytes(nextHash)

	return msgBuf.Seal(), nextBuf.Seal(), nil
}

func calculateEDCHSecret(myPrivEnc *memguard.Enclave, remotePub [32]byte) (*memguard.Enclave, error) {
	lockedPriv, err := myPrivEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}
	defer lockedPriv.Destroy()

	secret, err := curve25519.X25519(lockedPriv.Bytes(), remotePub[:])
	if err != nil {
		return nil, ErrDHComputation
	}
	defer memguard.WipeBytes(secret)

	return memguard.NewEnclave(secret), nil
}

// rootKDF is KDF_RK of the Double Ratchet: HKDF with the root key as salt and the DH output as
// input key material.
func rootKDF(rootKeyEnc, edchSecretEnc *memguard.Enclave) (*memguard.Enclave, *memguard.Enclave, error) {
	lockedRoot, err := rootKeyEnc.Open()
	if err != nil {
		return nil, nil, ErrEnclaveOpenFailed
	}
	defer lockedRoot.Destroy()

	lockedDH, err := edchSecretEnc.Open()
	if err != nil {
		return nil, nil, ErrEnclaveOpenFailed
	}
	defer lockedDH.Destroy()

	kdf := hkdf.New(sha256.New, lockedDH.Bytes(), lockedRoot.Bytes(), []byte(infoRoot))

	newRootBuf := memguard.NewBuffer(32)
	newChainBuf := memguard.NewBuffer(32)

	if _, err := io.ReadFull(kdf, newRootBuf.Bytes()); err != nil {
		newRootBuf.Destroy()
		newChainBuf.Destroy()
		return nil, nil, ErrHKDFFailed
	}

	if _, err := io.ReadFull(kdf, newChainBuf.Bytes()); err != nil {
		newRootBuf.Destroy()
		newChainBuf.Destroy()
		return nil, nil, ErrHKDFFailed
	}

	return newRootBuf.Seal(), newChainBuf.Seal(), nil
}
