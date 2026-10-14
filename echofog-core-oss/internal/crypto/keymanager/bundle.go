package keymanager

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/storage"
	"golang.org/x/crypto/curve25519"
)

const (
	SignatureContext = "ECHOFOG_PREKEY_BINDING_V1"
	OPKBatchSize     = 20
	MaxBundleOPKs    = 32
)

var (
	ErrOPKGenerationFailed = errors.New("failed to generate opk")
	ErrSaveOPKFailed       = errors.New("failed to save opk to storage")
	ErrGetIdentityFailed   = errors.New("failed to get identity key")
	ErrGetEncryptFailed    = errors.New("failed to get encryption key")
	ErrGetSPKFailed        = errors.New("failed to get signed prekey")
	ErrEnclaveOpenFailed   = errors.New("failed to open key enclave")
	ErrOPKNotFound         = errors.New("one-time prekey not found or already used")
	ErrBundleExpired       = errors.New("bundle expired")
	ErrBundleFromFuture    = errors.New("bundle from far future")
	ErrInvalidSignature    = errors.New("invalid bundle signature")
)

type PublicBundle struct {
	PeerID       [32]byte
	IdentityKey  [32]byte
	SignedPreKey [32]byte
	OneTimeKeys  [][32]byte
	CreatedAt    int64
	Signature    [64]byte
}

type OPKStorage struct {
	Keys map[[32]byte]*memguard.Enclave
}

func GenerateOPKBatch(hardwareStorage storage.Storage) ([][32]byte, *OPKStorage, error) {
	pubList := make([][32]byte, OPKBatchSize)
	inMemStorage := &OPKStorage{
		Keys: make(map[[32]byte]*memguard.Enclave),
	}

	for i := 0; i < OPKBatchSize; i++ {
		privBuf := memguard.NewBuffer(32)

		if _, err := rand.Read(privBuf.Bytes()); err != nil {
			privBuf.Destroy()
			return nil, nil, ErrOPKGenerationFailed
		}

		var xPriv [32]byte
		copy(xPriv[:], privBuf.Bytes())

		var pubKey [32]byte
		curve25519.ScalarBaseMult(&pubKey, &xPriv)
		memguard.WipeBytes(xPriv[:])

		pubList[i] = pubKey

		dbKey := append([]byte("opk:"), pubKey[:]...)
		if err := hardwareStorage.Set(dbKey, privBuf.Bytes()); err != nil {
			slog.Error("Error to save OPK key in storage", "err", err)
		}

		inMemStorage.Keys[pubKey] = privBuf.Seal()
	}

	return pubList, inMemStorage, nil
}

func (am *AccountManager) CreateMyBundle(opkPubs [][32]byte) (*PublicBundle, error) {
	idPrivEnc, idPub, err := am.GetIdentityKey()
	if err != nil {
		return nil, ErrGetIdentityFailed
	}

	_, mTermPubSlice, err := am.GetEncryptionKey()
	if err != nil {
		return nil, ErrGetEncryptFailed
	}
	var mTermPub [32]byte
	copy(mTermPub[:], mTermPubSlice)

	_, spkPubSlice, err := am.GetSignedPreKey()
	if err != nil {
		return nil, ErrGetSPKFailed
	}
	var spkPub [32]byte
	copy(spkPub[:], spkPubSlice)

	var peerID [32]byte
	copy(peerID[:], idPub)

	timestamp := time.Now().Unix()
	bindingData := serializeBundleForSigning(timestamp, mTermPub, spkPub, opkPubs)

	lockedIdPriv, err := idPrivEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}

	tmpPriv := make([]byte, ed25519.PrivateKeySize)
	copy(tmpPriv, lockedIdPriv.Bytes())
	signature := ed25519.Sign(tmpPriv, bindingData)
	memguard.WipeBytes(tmpPriv)

	lockedIdPriv.Destroy()

	var sig [64]byte
	copy(sig[:], signature)

	return &PublicBundle{
		PeerID:       peerID,
		IdentityKey:  mTermPub,
		SignedPreKey: spkPub,
		OneTimeKeys:  opkPubs,
		CreatedAt:    timestamp,
		Signature:    sig,
	}, nil
}

// CreateJITBundle makes a bundle with one fresh one-time prekey, for a peer that asks directly.
func (am *AccountManager) CreateJITBundle() (*PublicBundle, error) {
	return am.CreateBundleWithOPKs(1)
}

// CreateBundleWithOPKs makes a signed bundle with n fresh one-time prekeys. A bundle published to
// the network carries several, so that peers starting sessions from it rarely pick the same one.
func (am *AccountManager) CreateBundleWithOPKs(n int) (*PublicBundle, error) {
	if n < 1 || n > MaxBundleOPKs {
		return nil, ErrOPKGenerationFailed
	}
	opkPubs := make([][32]byte, 0, n)
	for i := 0; i < n; i++ {
		opkPub, opkPrivEnc, err := GenerateSingleOPK()
		if err != nil {
			return nil, err
		}
		lockedOpk, err := opkPrivEnc.Open()
		if err != nil {
			return nil, ErrEnclaveOpenFailed
		}
		err = am.storeOPK(opkPub, lockedOpk.Bytes())
		lockedOpk.Destroy()
		if err != nil {
			return nil, ErrSaveOPKFailed
		}
		opkPubs = append(opkPubs, opkPub)
	}
	return am.CreateMyBundle(opkPubs)
}

// ConsumeOPK returns the private half of a one-time prekey and deletes it.
func (am *AccountManager) ConsumeOPK(pubKey [32]byte) (*memguard.Enclave, error) {
	enc, err := am.OpenOPK(pubKey)
	if err != nil {
		return nil, err
	}
	if err := am.DeleteOPK(pubKey); err != nil {
		slog.Warn("failed to delete OPK from DB", "err", err)
	}
	return enc, nil
}

func GenerateSingleOPK() ([32]byte, *memguard.Enclave, error) {
	privBuf := memguard.NewBuffer(32)

	if _, err := rand.Read(privBuf.Bytes()); err != nil {
		privBuf.Destroy()
		return [32]byte{}, nil, ErrOPKGenerationFailed
	}

	var xPriv [32]byte
	copy(xPriv[:], privBuf.Bytes())

	var pubKey [32]byte
	curve25519.ScalarBaseMult(&pubKey, &xPriv)
	memguard.WipeBytes(xPriv[:])

	return pubKey, privBuf.Seal(), nil
}

func VerifyBundle(bundle *PublicBundle) error {
	now := time.Now().Unix()

	if now-bundle.CreatedAt > 30*24*3600 {
		return ErrBundleExpired
	}

	if bundle.CreatedAt > now+60 {
		return ErrBundleFromFuture
	}

	if len(bundle.OneTimeKeys) > MaxBundleOPKs {
		return ErrInvalidSignature
	}

	bindingData := serializeBundleForSigning(bundle.CreatedAt, bundle.IdentityKey, bundle.SignedPreKey, bundle.OneTimeKeys)

	if !ed25519.Verify(bundle.PeerID[:], bindingData, bundle.Signature[:]) {
		return ErrInvalidSignature
	}

	return nil
}

func serializeBundleForSigning(timestamp int64, mtk, spk [32]byte, opks [][32]byte) []byte {
	var buf bytes.Buffer

	buf.WriteString(SignatureContext)

	timeBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timeBytes, uint64(timestamp))
	buf.Write(timeBytes)
	buf.Write(mtk[:])
	buf.Write(spk[:])

	for _, key := range opks {
		buf.Write(key[:])
	}

	return buf.Bytes()
}
