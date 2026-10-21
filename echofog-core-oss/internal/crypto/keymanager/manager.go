package keymanager

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/storage"
	"golang.org/x/crypto/curve25519"
)

const (
	EntropyBitSize        = 128
	BipStandard           = 44
	HardenedIndexStandard = 0x80000000
	FOGCoinType           = 999
	IdentityKeyIndex      = 0
	EncryptionKeyIndex    = 1
	SignedPreKeyIndex     = 2
	WalletKeyIndex        = 3
)

const SignedPreKeyTTL = time.Hour * 24 * 7

type KeyManager struct {
	masterKey *memguard.Enclave
	masterCC  *memguard.Enclave
	storage   storage.Storage
}

func NewKeyManager(rootI []byte, storage storage.Storage) *KeyManager {
	masterKeyCopy := make([]byte, 32)
	copy(masterKeyCopy, rootI[:32])
	masterCCCopy := make([]byte, 32)
	copy(masterCCCopy, rootI[32:])

	return &KeyManager{
		masterKey: memguard.NewEnclave(masterKeyCopy),
		masterCC:  memguard.NewEnclave(masterCCCopy),
		storage:   storage,
	}
}

type AccountManager struct {
	Index      uint32
	AccountKey *memguard.Enclave
	AccountCC  *memguard.Enclave
	storage    storage.Storage

	mu         sync.Mutex
	storageKey *memguard.Enclave
	lastPurge  time.Time

	opkMu      sync.Mutex // the count of one-time prekeys handed out on request
	jitCount   int
	jitCounted bool
}

func NewKeyManagerWithMnemonic(mnemonic []byte, passphrase []byte, storage storage.Storage) (*KeyManager, error) {
	defer memguard.WipeBytes(mnemonic)
	defer memguard.WipeBytes(passphrase)

	seedEnclave, err := SeedFromMnemonic(mnemonic, passphrase)
	if err != nil {
		return nil, fmt.Errorf("seed generation error: %w", err)
	}

	lockedSeed, err := seedEnclave.Open()
	if err != nil {
		return nil, errors.New("memory corruption: unable to open seed enclave")
	}
	defer lockedSeed.Destroy()

	hmacRoot := hmac.New(sha512.New, []byte("ed25519 seed"))
	hmacRoot.Write(lockedSeed.Bytes())
	rootI := hmacRoot.Sum(nil)
	defer memguard.WipeBytes(rootI)

	return &KeyManager{
		masterKey: memguard.NewEnclave(rootI[:32]),
		masterCC:  memguard.NewEnclave(rootI[32:]),
		storage:   storage,
	}, nil
}

func (km *KeyManager) GetAccount(accountIndex uint32) (*AccountManager, error) {
	k44, c44, err := deriveSecureSLIP0010(km.masterKey, km.masterCC, HardenedIndexStandard+BipStandard)
	if err != nil {
		return nil, err
	}

	kCoin, cCoin, err := deriveSecureSLIP0010(k44, c44, HardenedIndexStandard+FOGCoinType)
	if err != nil {
		return nil, err
	}

	accKey, accCC, err := deriveSecureSLIP0010(kCoin, cCoin, HardenedIndexStandard+accountIndex)
	if err != nil {
		return nil, err
	}

	return &AccountManager{
		Index:      accountIndex,
		AccountKey: accKey,
		AccountCC:  accCC,
		storage:    km.storage,
	}, nil
}

func (km *KeyManager) GetSecretKeys() (*memguard.Enclave, *memguard.Enclave) {
	return km.masterKey, km.masterCC
}

func (km *KeyManager) Destroy() {
	km.masterKey = nil
	km.masterCC = nil
}

func (am *AccountManager) GetIdentityKey() (*memguard.Enclave, ed25519.PublicKey, error) {
	identityEnc, _, err := deriveSecureSLIP0010(am.AccountKey, am.AccountCC, HardenedIndexStandard+IdentityKeyIndex)
	if err != nil {
		return nil, nil, err
	}

	lockedIdentity, err := identityEnc.Open()
	if err != nil {
		return nil, nil, err
	}
	defer lockedIdentity.Destroy()

	rawPriv := ed25519.NewKeyFromSeed(lockedIdentity.Bytes())
	defer memguard.WipeBytes(rawPriv)

	pub := rawPriv.Public().(ed25519.PublicKey)
	privEnclave := memguard.NewEnclave(rawPriv)

	return privEnclave, pub, nil
}

func (am *AccountManager) GetEncryptionKey() (*memguard.Enclave, []byte, error) {
	encryptEnc, _, err := deriveSecureSLIP0010(am.AccountKey, am.AccountCC, HardenedIndexStandard+EncryptionKeyIndex)
	if err != nil {
		return nil, nil, err
	}

	lockedEncrypt, err := encryptEnc.Open()
	if err != nil {
		return nil, nil, err
	}
	defer lockedEncrypt.Destroy()

	var xPriv [32]byte
	copy(xPriv[:], lockedEncrypt.Bytes())
	defer memguard.WipeBytes(xPriv[:])

	var xPub [32]byte
	curve25519.ScalarBaseMult(&xPub, &xPriv)

	return memguard.NewEnclave(xPriv[:]), xPub[:], nil
}

func (am *AccountManager) GetTemporaryKey() (*memguard.Enclave, []byte, error) {
	privBuf := memguard.NewBuffer(32)
	defer privBuf.Destroy()

	if _, err := rand.Read(privBuf.Bytes()); err != nil {
		return nil, nil, err
	}

	var xPriv [32]byte
	copy(xPriv[:], privBuf.Bytes())
	defer memguard.WipeBytes(xPriv[:])

	var pubKey [32]byte
	curve25519.ScalarBaseMult(&pubKey, &xPriv)

	return privBuf.Seal(), pubKey[:], nil
}

// GetSignedPreKey returns the current signed prekey, making a new one when it is a week old. The
// replaced one is kept (see SignedPreKeyRetention): bundles that name it are still around.
func (am *AccountManager) GetSignedPreKey() (*memguard.Enclave, []byte, error) {
	am.purgeOld()
	name := []byte(spkCurrentKey)
	if data, err := am.storage.Get(name); err == nil {
		plain, err := am.open(name, data, 40)
		if err == nil && len(plain) == 40 {
			created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
			if time.Since(created) < SignedPreKeyTTL {
				priv, pub := x25519Pair(plain[8:])
				memguard.WipeBytes(plain)
				return priv, pub, nil
			}
			// retire it: still needed for sessions started from bundles that name it
			_, pub := x25519Pair(plain[8:])
			oldName := append([]byte(spkOldPrefix), pub...)
			if sealed, err := am.seal(oldName, timedSecret(time.Now(), plain[8:])); err == nil {
				_ = am.storage.Set(oldName, sealed)
			}
		}
		memguard.WipeBytes(plain)
	}

	privBuf := memguard.NewBuffer(32)
	defer privBuf.Destroy()
	if _, err := rand.Read(privBuf.Bytes()); err != nil {
		return nil, nil, err
	}
	plain := timedSecret(time.Now(), privBuf.Bytes())
	defer memguard.WipeBytes(plain)
	sealed, err := am.seal(name, plain)
	if err != nil {
		return nil, nil, err
	}
	if err := am.storage.Set(name, sealed); err != nil {
		return nil, nil, err
	}
	priv, pub := x25519Pair(privBuf.Bytes())
	return priv, pub, nil
}

// SignedPreKeys returns the private halves of the current signed prekey and of the replaced ones
// that bundles may still name, newest first.
func (am *AccountManager) SignedPreKeys() ([]*memguard.Enclave, error) {
	cur, _, err := am.GetSignedPreKey()
	if err != nil {
		return nil, err
	}
	out := []*memguard.Enclave{cur}
	entries, err := am.storage.FindValues([]byte(spkOldPrefix), 0)
	if err != nil {
		return out, nil
	}
	for _, e := range entries {
		plain, err := am.open(e.Key, e.Value, -1)
		if err != nil || len(plain) != 40 {
			continue
		}
		created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
		if time.Since(created) <= SignedPreKeyRetention {
			out = append(out, memguard.NewEnclave(append([]byte(nil), plain[8:]...)))
		}
		memguard.WipeBytes(plain)
	}
	return out, nil
}

// x25519Pair seals a private key and returns it with its public key.
func x25519Pair(privBytes []byte) (*memguard.Enclave, []byte) {
	var xPriv, xPub [32]byte
	copy(xPriv[:], privBytes)
	curve25519.ScalarBaseMult(&xPub, &xPriv)
	enc := memguard.NewEnclave(xPriv[:]) // wipes xPriv
	return enc, xPub[:]
}

func (am *AccountManager) GetWalletKey() (*memguard.Enclave, error) {
	encryptEnc, _, err := deriveSecureSLIP0010(am.AccountKey, am.AccountCC, HardenedIndexStandard+WalletKeyIndex)
	if err != nil {
		return nil, err
	}
	return encryptEnc, nil
}

func GenerateEphemeralTransportKey() (priv [32]byte, pub [32]byte, err error) {
	if _, err := rand.Read(priv[:]); err != nil {
		return priv, pub, err
	}

	curve25519.ScalarBaseMult(&pub, &priv)
	return priv, pub, nil
}

func (am *AccountManager) CalculateSharedSecret(remoteBundle *PublicBundle) (*memguard.Enclave, error) {
	myPrivEnc, _, err := am.GetEncryptionKey()
	if err != nil {
		return nil, err
	}

	lockedPriv, err := myPrivEnc.Open()
	if err != nil {
		return nil, err
	}
	defer lockedPriv.Destroy()

	var myPriv [32]byte
	copy(myPriv[:], lockedPriv.Bytes())
	defer memguard.WipeBytes(myPriv[:])

	var remotePub [32]byte
	copy(remotePub[:], remoteBundle.SignedPreKey[:])

	sharedSecret, err := curve25519.X25519(myPriv[:], remotePub[:])
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(sharedSecret)

	return memguard.NewEnclave(sharedSecret), nil
}

func deriveSecureSLIP0010(parentKeyEnc, chainCodeEnc *memguard.Enclave, index uint32) (*memguard.Enclave, *memguard.Enclave, error) {
	if index < HardenedIndexStandard {
		return nil, nil, errors.New("SLIP-0010 requires hardened index")
	}

	pKey, err := parentKeyEnc.Open()
	if err != nil {
		return nil, nil, err
	}
	defer pKey.Destroy()

	pCC, err := chainCodeEnc.Open()
	if err != nil {
		return nil, nil, err
	}
	defer pCC.Destroy()

	data := make([]byte, 37)
	data[0] = 0
	copy(data[1:33], pKey.Bytes())
	binary.BigEndian.PutUint32(data[33:], index)
	defer memguard.WipeBytes(data)

	h := hmac.New(sha512.New, pCC.Bytes())
	h.Write(data)
	I := h.Sum(nil)
	defer memguard.WipeBytes(I)

	return memguard.NewEnclave(I[:32]), memguard.NewEnclave(I[32:]), nil
}
